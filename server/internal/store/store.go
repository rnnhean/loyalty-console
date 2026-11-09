// Package store —— 数据访问层。
//
// 这里集中体现 Go 在资源管理上的几个实际 advantage：
//   - database/sql 自带连接池，生命周期由 context 控制
//   - sync.RWMutex 保护本地热点缓存（读多写少）
//   - singleflight 合并并发重复查询，防缓存击穿
//   - errgroup 并发扇出，任一失败快速返回
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"github.com/example/loyalty-console/server/internal/observ"
)

// Store 聚合所有数据源句柄。
type Store struct {
	DB    *sql.DB
	RDB   *redis.Client
	group singleflight.Group

	// 本地热点缓存：字典项、产品目录这类「读多写极少」的数据。
	// 用 RWMutex 而不是 Mutex —— 读并发是写的成百上千倍。
	cacheMu sync.RWMutex
	cache   map[string]cacheEntry
}

type cacheEntry struct {
	val     any
	expires time.Time
}

// Config 数据源配置。
type Config struct {
	MySQLDSN      string
	RedisAddr     string
	RedisPassword string
	RedisDB       int
	MaxOpenConn   int
	MaxIdleConn   int
	ConnMaxLife   time.Duration
}

// Open 建立连接。
func Open(ctx context.Context, cfg Config) (*Store, error) {
	db, err := sql.Open("mysql", cfg.MySQLDSN)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	// 连接池参数必须显式设置：
	// 不设 MaxOpen 默认无限，突发流量会把 DB 连接打满；
	// 不设 ConnMaxLifetime 会撞上 MySQL 的 wait_timeout 导致 "invalid connection"。
	db.SetMaxOpenConns(cfg.MaxOpenConn)
	db.SetMaxIdleConns(cfg.MaxIdleConn)
	db.SetConnMaxLifetime(cfg.ConnMaxLife)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.RedisAddr,
		Password:     cfg.RedisPassword,
		DB:           cfg.RedisDB,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		PoolSize:     20,
	})
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}

	return &Store{DB: db, RDB: rdb, cache: make(map[string]cacheEntry)}, nil
}

// Close 释放资源。defer 里调用，保证进程退出前连接干净关闭。
func (s *Store) Close() error {
	var first error
	if s.DB != nil {
		if err := s.DB.Close(); err != nil {
			first = err
		}
	}
	if s.RDB != nil {
		if err := s.RDB.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ── 本地热点缓存 ───────────────────────────────────────────

// GetOrLoad 两级缓存：本地 → Redis → 数据库。
//
// **值一律是 []byte**（通常是 JSON）。这条约定是硬性的，原因有两个：
//   - go-redis 的 Get().Result() 返回 Go 的 string，不是 []byte；如果上层
//     拿 []byte 做类型断言，进程重启、本地缓存清空、从 Redis 读回的那一刻
//     就会 panic / 返回 500。这个坑真实踩过一次。
//   - 本地缓存与 Redis 必须共用同一份编码，否则「本地拿出来能用、Redis
//     拿回来不能用」，而且只在重启后才暴露。
//
// 两级 TTL 故意不同：本地短、Redis 长。
// 本地是每个实例一份，过期后回到 Redis 拿 —— 这是 Redis 层唯一的读取时机。
// 如果两者相同，本地命中永远盖住 Redis，Redis 就只写不读，等于白存。
//
// singleflight 在这里是**防击穿**的关键：
// 缓存过期的瞬间，100 个并发请求会同时穿透到 DB。singleflight 让
// 只有一个 goroutine 真正查询，其余等它的结果。
//
// 这是 Go 相比 TS 后端的一个真实差异：Node 里要自己维护 pending Promise 的
// map 来做同样的事，Go 有标准化的 singleflight 库，且天然并发安全。
func (s *Store) GetOrLoad(ctx context.Context, ns, key string, ttl time.Duration, load func(context.Context) ([]byte, error)) ([]byte, error) {
	ck := ns + ":" + key
	// 本地 TTL 取 Redis TTL 的 1/5，下限 1 秒、上限 30 秒：
	// 既让 Redis 层有机会被真正读到，又不至于频繁回 Redis。
	localTTL := ttl / 5
	if localTTL > 30*time.Second {
		localTTL = 30 * time.Second
	}
	if localTTL < time.Second {
		localTTL = time.Second
	}

	// 1. 本地缓存
	s.cacheMu.RLock()
	e, ok := s.cache[ck]
	s.cacheMu.RUnlock()
	if ok && time.Now().Before(e.expires) {
		observ.CacheHits.WithLabelValues("local").Inc()
		if b, ok := e.val.([]byte); ok {
			return b, nil
		}
	}

	// 2. singleflight 合并
	v, err, shared := s.group.Do(ck, func() (any, error) {
		// 双重检查：可能在等锁期间已被别的 goroutine 填充
		s.cacheMu.RLock()
		e, ok := s.cache[ck]
		s.cacheMu.RUnlock()
		if ok && time.Now().Before(e.expires) {
			return e.val, nil
		}

		// 3. Redis
		if s.RDB != nil {
			raw, rerr := s.RDB.Get(ctx, ck).Result()
			if rerr == nil {
				observ.CacheHits.WithLabelValues("redis").Inc()
				b := []byte(raw) // 统一成 []byte，见函数头注释
				s.setLocal(ck, b, localTTL)
				return b, nil
			}
			if !errors.Is(rerr, redis.Nil) {
				// Redis 故障不应影响主流程，降级到 DB
				observ.CacheMisses.WithLabelValues("redis_error").Inc()
			}
		}

		// 4. 回源
		observ.CacheMisses.WithLabelValues(ns).Inc()
		b, lerr := load(ctx)
		if lerr != nil {
			return nil, lerr
		}
		s.setLocal(ck, b, localTTL)
		if s.RDB != nil {
			// Redis 写失败不影响返回，只是下次还要回源
			_ = s.RDB.Set(ctx, ck, b, ttl).Err()
		}
		return b, nil
	})
	if shared {
		observ.SingleflightCollapsed.WithLabelValues(ns).Inc()
	}
	if err != nil {
		return nil, err
	}
	b, ok := v.([]byte)
	if !ok {
		return nil, fmt.Errorf("cache %s: value is %T, want []byte", ck, v)
	}
	return b, nil
}

func (s *Store) setLocal(k string, v any, ttl time.Duration) {
	s.cacheMu.Lock()
	s.cache[k] = cacheEntry{val: v, expires: time.Now().Add(ttl)}
	s.cacheMu.Unlock()
}

// Invalidate 主动失效。写操作后调用，避免读到旧值。
func (s *Store) Invalidate(keys ...string) {
	s.cacheMu.Lock()
	for _, k := range keys {
		delete(s.cache, k)
	}
	s.cacheMu.Unlock()
}
