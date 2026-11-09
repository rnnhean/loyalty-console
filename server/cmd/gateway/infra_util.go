package main

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/example/loyalty-console/server/internal/gateway"
	"github.com/example/loyalty-console/server/pkg/trace"
)

// ── 小型工具 ───────────────────────────────────────────────

func marshalJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func marshalJSONErr(v any) ([]byte, error) { return json.Marshal(v) }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func withTrace(ctx context.Context, h gin.H) gin.H {
	h["trace_id"] = trace.FromContext(ctx)
	return h
}

func operatorID(c *gin.Context) (string, bool) {
	op, ok := gateway.OperatorFrom(c)
	if !ok {
		return "", false
	}
	return op.ID, true
}

// ── 中间件健康快照 ─────────────────────────────────────────
//
// 每个组件的状态用「最近一次探活结果」表示，而不是每次请求都去 ping：
// 探活在后台每秒跑一次，请求路径上只是读内存。
// 这个取舍的理由是——健康检查被高频调用（K8s 每几秒一次），
// 每次都打 Redis/RabbitMQ 会成为负担，而秒级的陈旧度完全可以接受。

type componentHealth struct {
	OK        bool      `json:"ok"`
	LastCheck time.Time `json:"last_check"`
	Detail    string    `json:"detail,omitempty"`
}

type infraState struct {
	mu   sync.RWMutex
	data map[string]componentHealth
}

func newInfraState() *infraState {
	return &infraState{data: make(map[string]componentHealth)}
}

func (s *infraState) set(name string, ok bool, detail string) {
	s.mu.Lock()
	s.data[name] = componentHealth{OK: ok, LastCheck: time.Now(), Detail: detail}
	s.mu.Unlock()
}

func (s *infraState) snapshot() map[string]componentHealth {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]componentHealth, len(s.data))
	for k, v := range s.data {
		out[k] = v
	}
	return out
}
