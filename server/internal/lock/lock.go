// Package lock —— 基于 Redis 的分布式锁。
//
// 什么时候真的需要它：**多实例部署下，同一件「只能有一个执行者」的事**。
// 典型场景是定时任务（到期权益回收、对账、缓存预热）——
//
//	单实例时：一个 goroutine 跑就够了；
//	三实例时：三个 goroutine 同时跑，同一条记录被处理三次。
//
// 本地部署用不到它，但公网多副本部署一定会撞上，所以这里实现完整版本。
//
// 实现要点（每一条对应一个真实的坑）：
//
//  1. SET NX PX 而不是 SETNX + EXPIRE
//     两条命令之间进程挂掉，key 就永远不过期 —— 死锁。
//     Redis 2.6.12 起 SET 支持 NX+EX 原子组合。
//
//  2. value 必须是随机 token，释放时用 Lua 比对
//     「先 GET 判断是我的锁，再 DEL」不是原子的：判断完、DEL 前，
//     锁可能已经过期并被别人拿走，这时 DEL 掉的是别人的锁。
//     Lua 脚本在 Redis 里单线程执行，才真正做到「比对+删除」原子。
//
//  3. 续期（watchdog）必须由持有者主动做
//     业务执行时间不确定，锁的 TTL 要么设很长（挂了就长期锁死），
//     要么设短+续期。这里选短 TTL + 后台续期。
package lock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/example/loyalty-console/server/internal/observ"
)

// ErrNotAcquired 抢锁失败。
var ErrNotAcquired = errors.New("lock: not acquired")

// ErrLost 续期时发现锁已经不属于自己（过期后被别人拿走）。
var ErrLost = errors.New("lock: ownership lost")

// releaseScript 释放锁的原子脚本。
//
// KEYS[1] = lock key
// ARGV[1] = 持有者的随机 token
// 返回 1 表示确实删掉了自己的锁，0 表示锁已经不属于自己。
const releaseScript = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("DEL", KEYS[1])
else
    return 0
end`

// renewScript 续期的原子脚本，同样是「确认持有 → 才续」。
const renewScript = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("PEXPIRE", KEYS[1], ARGV[2])
else
    return 0
end`

// Mutex 一把已持有的锁。
//
// 不要 copy 这个结构体（含 sync.Mutex 与 atomic）。
type Mutex struct {
	rdb   *redis.Client
	key   string
	token string
	ttl   time.Duration
	scope string
	// acquiredAt 用于统计持锁时长
	acquiredAt time.Time

	stop     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once
	lost     atomic.Bool
}

// Locker 锁工厂，持有连接与默认 TTL。
type Locker struct {
	rdb *redis.Client
	ttl time.Duration
}

// NewLocker 构造。
//
// ttl 不宜太短：网络抖动导致的续期延迟会误判为丢锁。
// 10~30 秒是常见区间，这里默认 15s，续期间隔取 ttl/3。
func NewLocker(rdb *redis.Client, ttl time.Duration) *Locker {
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	return &Locker{rdb: rdb, ttl: ttl}
}

// Acquire 抢锁，抢不到立刻返回 ErrNotAcquired（不阻塞）。
//
// 为什么不做阻塞重试：定时任务场景下，「抢不到」意味着别的实例正在干，
// 这一轮直接跳过就好；阻塞重试会让多个实例排队等同一件事，反而更糟。
// 需要等待的调用方自己在外层循环。
func (l *Locker) Acquire(ctx context.Context, key string) (*Mutex, error) {
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	ok, err := l.rdb.SetNX(ctx, key, token, l.ttl).Result()
	if err != nil {
		// Redis 不可用要单独计数：这时候是「锁机制失效」，
		// 和「有人正常抢着锁」是两回事，混在一起看会误判。
		observ.LockAcquire.WithLabelValues("error").Inc()
		return nil, fmt.Errorf("lock acquire: %w", err)
	}
	if !ok {
		observ.LockAcquire.WithLabelValues("busy").Inc()
		return nil, ErrNotAcquired
	}
	observ.LockAcquire.WithLabelValues("ok").Inc()

	m := &Mutex{
		rdb:        l.rdb,
		key:        key,
		token:      token,
		ttl:        l.ttl,
		scope:      scopeOf(key),
		acquiredAt: time.Now(),
		stop:       make(chan struct{}),
		stopped:    make(chan struct{}),
	}
	go m.watchdog()
	return m, nil
}

// scopeOf 从锁 key 里取「业务范围」作为指标的 label 值。
//
// 直接用完整 key 做 label 是经典事故：key 里带订单号/用户 id 时，
// 时间序列数量会随数据量线性增长，几周就把 Prometheus 打爆。
// 这里只取第一段（约定 key 形如 "scope:id"）。
func scopeOf(key string) string {
	if i := strings.IndexByte(key, ':'); i > 0 {
		return key[:i]
	}
	return key
}

// watchdog 周期性续期。
//
// 起 goroutine 就必须能停下来，否则进程退出时它还在跑（goroutine 泄漏）。
// 这里用 stop channel + stopped channel 做「通知停止 → 等它真的退出」，
// 而不是 close(stop) 就返回 —— 后者会让 Release 与续期产生竞态。
func (m *Mutex) watchdog() {
	defer close(m.stopped)
	interval := m.ttl / 3
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			n, err := m.rdb.Eval(ctx, renewScript, []string{m.key}, m.token, m.ttl.Milliseconds()).Int()
			cancel()
			if err != nil {
				// Redis 抖一下不算丢锁，下一轮再续；
				// 连续失败到 TTL 过期才会真的丢，那种情况会在下一次 Evaluate 时发现。
				continue
			}
			if n == 0 {
				m.lost.Store(true)
				observ.LockLost.WithLabelValues(m.scope).Inc()
				return
			}
		}
	}
}

// Lost 报告锁是否已经丢失。
//
// 长任务的执行方应该在关键节点检查它 —— 锁丢了还继续写库，
// 等于两个实例同时写同一批数据。
func (m *Mutex) Lost() bool { return m.lost.Load() }

// Release 停止续期并释放锁。可重复调用。
func (m *Mutex) Release(ctx context.Context) error {
	m.stopOnce.Do(func() {
		close(m.stop)
		<-m.stopped // 等续期 goroutine 真的退出，避免它和释放打架
	})
	// 持锁时长在退出时记一次：超过 TTL 的持锁说明临界区太长了，
	// 靠 watchdog 撑着虽然不出错，但已经不是「短临界区」的设计意图。
	observ.LockHoldDuration.WithLabelValues(m.scope).Observe(time.Since(m.acquiredAt).Seconds())

	n, err := m.rdb.Eval(ctx, releaseScript, []string{m.key}, m.token).Int()
	if err != nil {
		return fmt.Errorf("lock release: %w", err)
	}
	if n == 0 && !m.lost.Load() {
		// 没删掉说明锁已经因过期被别人拿走了。这不算错误调用方要处理，
		// 但调用方可以通过 Lost() 感知。
		m.lost.Store(true)
		observ.LockLost.WithLabelValues(m.scope).Inc()
	}
	return nil
}

// Do 便捷包装：抢到锁才执行，抢不到返回 ErrNotAcquired。
func (l *Locker) Do(ctx context.Context, key string, fn func(context.Context) error) error {
	m, err := l.Acquire(ctx, key)
	if err != nil {
		return err
	}
	defer func() {
		// 释放用独立的 ctx：请求 ctx 可能已经超时，用它释放会失败。
		rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = m.Release(rctx)
	}()
	return fn(ctx)
}

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
