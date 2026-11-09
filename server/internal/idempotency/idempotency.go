// Package idempotency —— 幂等键。
//
// 解决什么问题：网络超时下，客户端会重试；重试可能落到另一个实例上。
// 没有幂等保护时，一次「保存权益规则」可能真的被保存两次。
//
// 实现选择：**Redis SET NX 占位 + 状态机**，而不是数据库唯一索引。
//   - 唯一索引能防重复，但只对「已经写完」的重复有效；两个并发请求同时
//     通过校验、同时插入时，总有一个会拿到 DuplicateKey 并且已经产生了副作用
//     （比如发了消息）。而且拿不到「第一次请求的结果」用于回放。
//   - Redis 占位在业务执行前就拦住第二个请求，并且可以缓存第一次的响应体，
//     让重试请求拿到完全一样的返回（这才是真正的幂等）。
//
// 三种状态：
//
//	""        不存在
//	"running" 有请求正在处理
//	"done:<响应摘要>" 已完成，可直接回放
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/example/loyalty-console/server/internal/observ"
)

// ErrInFlight 同一个幂等键的请求正在处理中。
var ErrInFlight = errors.New("idempotency: request in flight")

// Guard 幂等守卫。
type Guard struct {
	rdb *redis.Client
	// 处理中状态的 TTL。设短一些：进程如果崩了，这个键自动过期，
	// 客户端重试还能继续，不会被永久卡死。
	runningTTL time.Duration
	// 完成状态的 TTL。这是「重试窗口」，过了窗口再发就当成新请求。
	doneTTL time.Duration
}

// NewGuard 构造。
func NewGuard(rdb *redis.Client) *Guard {
	return &Guard{rdb: rdb, runningTTL: 30 * time.Second, doneTTL: 10 * time.Minute}
}

// Outcome 抢占结果。
type Outcome int

const (
	// Proceed 抢到了，调用方应该执行业务。
	Proceed Outcome = iota
	// Replay 之前已经完成过，直接用 Cached 返回。
	Replay
	// Busy 有别的请求正在处理同一件事。
	Busy
)

// Begin 尝试为 key 占位。
//
// 返回 Proceed 时，调用方**必须**在结束时调用 Finish，否则该 key 会卡到
// runningTTL 过期，期间所有重试都被判为 Busy。
func (g *Guard) Begin(ctx context.Context, key string) (Outcome, []byte, error) {
	// 先看有没有已完成的记录
	v, err := g.rdb.Get(ctx, doneKey(key)).Bytes()
	if err == nil {
		observ.IdempotencyOutcome.WithLabelValues("replay").Inc()
		return Replay, v, nil
	}
	if !errors.Is(err, redis.Nil) {
		// Redis 故障：降级为「放行」。宁可重复一次，也不要因为缓存挂了
		// 把整个写入接口卡死。这是有意的取舍，不是疏漏。
		observ.IdempotencyOutcome.WithLabelValues("degraded").Inc()
		return Proceed, nil, nil
	}

	ok, err := g.rdb.SetNX(ctx, runningKey(key), "1", g.runningTTL).Result()
	if err != nil {
		observ.IdempotencyOutcome.WithLabelValues("degraded").Inc()
		return Proceed, nil, nil // 同上，降级放行
	}
	if !ok {
		observ.IdempotencyOutcome.WithLabelValues("busy").Inc()
		return Busy, nil, nil
	}
	observ.IdempotencyOutcome.WithLabelValues("proceed").Inc()
	return Proceed, nil, nil
}

// Finish 记录完成结果，供后续重试回放。
//
// 失败时也调用（body 传错误摘要）：重试应该拿到同样的失败，
// 而不是重新执行一遍可能部分成功的操作。
func (g *Guard) Finish(ctx context.Context, key string, body []byte) {
	// 先写完成态再删处理态，顺序不能反：
	// 反过来会出现「running 已删、done 还没写」的空窗，此时重试会被当成新请求。
	if err := g.rdb.Set(ctx, doneKey(key), body, g.doneTTL).Err(); err != nil {
		return
	}
	g.rdb.Del(ctx, runningKey(key))
}

// Abort 放弃占位（业务根本没执行，例如参数校验失败）。
func (g *Guard) Abort(ctx context.Context, key string) {
	g.rdb.Del(ctx, runningKey(key))
}

func runningKey(k string) string { return "idem:running:" + k }

func doneKey(k string) string { return "idem:done:" + k }

// Fingerprint 由「调用方 + 业务动作 + 请求体」算出幂等键。
//
// 为什么要带调用方：不同运维同时提交同样的内容，是两次独立操作，
// 不应该互相顶掉。
func Fingerprint(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}
