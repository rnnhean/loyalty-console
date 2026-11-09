// Package trace —— trace_id 贯穿全链路。
//
// 这是「前后端监控不变成两座孤岛」的关键一环：
// 前端生成 id → 请求头带上 → 网关读取 → 写进 context → 每条日志带上 → 响应头回传。
//
// 用户报错时只要给出 trace_id，就能 grep 到那一次请求的全部日志。
package trace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

// HeaderName 前后端约定的请求头。用 X- 前缀是历史惯例，
// 虽然 RFC 6648 建议不再新增 X- 头，但监控场景下各家 SDK 都这么做，跟共识走。
const HeaderName = "X-Trace-Id"

// ResponseHeader 网关把最终采用的 trace_id 回传给前端，
// 前端出错上报时把它一起带上。
const ResponseHeader = "X-Trace-Id"

type ctxKey struct{}

// FromContext 取出当前请求的 trace_id。没有则返回空串。
func FromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}

// WithContext 把 trace_id 放进 context。
func WithContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// New 生成一个 16 字节（32 位十六进制）的 trace id，
// 与 W3C trace-context 的 trace-id 长度一致，将来要接 OTel 能直接复用。
func New() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 极端情况下 rand 不可用（几乎不可能），退化成固定值而不是 panic：
		// 监控链路挂掉不应该拖垮业务请求。
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// Ensure 返回已有的 trace_id，没有就新建一个。
func Ensure(ctx context.Context) (context.Context, string) {
	if id := FromContext(ctx); id != "" {
		return ctx, id
	}
	id := New()
	return WithContext(ctx, id), id
}
