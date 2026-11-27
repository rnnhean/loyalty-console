// Package gateway —— API 网关。
//
// 它不处理业务，只做「转发 + 顺手加点动作」。本项目的网关承担六件事：
//
//  1. 路由         /api/catalog/* → catalog 域，/api/benefit/* → benefit 域 …
//  2. 认证鉴权     验 HMAC 签名，把 operator 塞进 context 向下传，各域不再各写一遍
//  3. 限流         按路由 + 按调用方，令牌桶
//  4. 协议转换     对外 JSON/REST，对内直接调用领域服务（本实现是进程内，等价 gRPC 语义）
//  5. 聚合 BFF     一个页面要 3 个域的接口，网关拼成 1 个响应（见 aggregate.go）
//  6. 横切能力     trace_id、结构化日志、Prometheus 指标、CORS、panic 恢复、超时
//
// 它存在的理由（本质）：**单一入口（choke point）**。
// 没有这个关口，你既统计不了全站 QPS，也没法统一限流和鉴权。
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/time/rate"

	"github.com/example/loyalty-console/server/internal/observ"
	"github.com/example/loyalty-console/server/pkg/trace"
)

// ErrUnauthorized 鉴权失败
var ErrUnauthorized = errors.New("unauthorized")

// ErrRateLimited 触发限流
var ErrRateLimited = errors.New("rate limited")

// ── 中间件 ─────────────────────────────────────────────────

// Trace 生成/透传 trace_id，并写回响应头。
// 必须是最外层中间件，否则后面任何日志都没有 trace_id。
func Trace() gin.HandlerFunc {
	return func(c *gin.Context) {
		incoming := c.GetHeader(trace.HeaderName)
		var id string
		var ctx = c.Request.Context()
		if incoming != "" && len(incoming) <= 64 {
			// 校验形态，防止把日志注入串塞进来
			if isValidTraceID(incoming) {
				ctx, id = trace.WithContext(ctx, incoming), incoming
			}
		}
		if id == "" {
			ctx, id = trace.Ensure(ctx)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Header(trace.ResponseHeader, id)
		c.Next()
	}
}

func isValidTraceID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') || r == '-') {
			return false
		}
	}
	return true
}

// accessLogSkip 是不打访问日志的路径。
//
// /metrics 是 Prometheus 每 15 秒抓一次，/healthz 是探针每秒一次——
// 这两类请求占了日志量的大头，但排查问题时毫无价值。
// 不过滤的话 ES 索引会被它们刷满，真正的业务请求被埋在里面。
var accessLogSkip = map[string]bool{
	"/metrics": true,
	"/healthz": true,
	"/livez":   true,
	"/readyz":  true,
}

// AccessLog 记录每个 HTTP 请求。
//
// 必须在 Trace 之后挂：日志要带 trace_id，而 trace_id 是 Trace 中间件注入的。
//
// 只记「请求已经处理完」的最终状态（c.Next() 之后），
// 因为在之前拿不到最终 status 和耗时。
//
// 路径用 c.FullPath()（路由模板 /documents/:id）而不是真实路径，
// 否则每个 id 都是不同的值，ES 里按 path 聚合会得到一片散点。
func AccessLog(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		if accessLogSkip[c.Request.URL.Path] {
			c.Next()
			return
		}
		start := time.Now()
		c.Next()

		route := c.FullPath()
		if route == "" {
			// 没匹配上任何路由（404）：用真实路径，但要截断，
			// 防止构造超长 URL 把日志撑爆
			route = c.Request.URL.Path
			if len(route) > 200 {
				route = route[:200]
			}
		}

		status := c.Writer.Status()
		// service 由 logger 自己带（main 里 .With("service","gateway")），
		// 这里再传一次会在同一条 JSON 里出现两个 service 键。
		attrs := []any{
			"method", c.Request.Method,
			"path", route,
			"status", status,
			"bytes", c.Writer.Size(),
			"latency_ms", float64(time.Since(start).Microseconds()) / 1000.0,
			"client_ip", c.ClientIP(),
			"trace_id", trace.FromContext(c.Request.Context()),
		}
		// 失败请求带上错误信息；成功请求不记，省一半体积
		if errs := c.Errors.ByType(gin.ErrorTypePrivate).String(); errs != "" {
			attrs = append(attrs, "error", errs)
		}

		// 5xx 用 Error 级别：告警规则按 level:error 筛选，
		// 4xx 是客户端问题（多半是 token 过期），用 Warn 就够。
		switch {
		case status >= 500:
			logger.Error("http_request", attrs...)
		case status >= 400:
			logger.Warn("http_request", attrs...)
		default:
			logger.Info("http_request", attrs...)
		}
	}
}

// Metrics 记录 RED 三件套。
//
// routeOf 返回用于指标标签的路由模板。
//
// 用 c.FullPath() 而不是 c.Request.URL.Path：后者会把 /spu/123、/spu/456
// 分成两条时间序列，指标基数爆炸。这是 Prometheus 使用中最常见的错误之一。
// 路由未匹配时退回 "unmatched"，避免原始 path 直接进标签。
func routeOf(c *gin.Context) string {
	if p := c.FullPath(); p != "" {
		return p
	}
	return "unmatched"
}

// probeRoute 判断该路由是否属于机器定时轮询的探针接口。
//
// Prometheus 每 15s 抓一次 /metrics、编排每 10s 探一次 /healthz，
// 一天下来同样的日志有上万条，会把真正的请求淹掉——查问题时要先翻过
// 好几屏心跳才看得到一条业务访问，等于日志不可用。
//
// 注意只跳过日志、不跳过指标：指标是聚合值（次数累加在同一个时间序列上），
// 不会因为条数多而撑爆；而且探针本身的可观测性要靠 up / 抓取间隔来判断。
func probeRoute(route string) bool {
	switch route {
	case "/metrics", "/healthz", "/readyz":
		return true
	}
	return false
}

func Metrics() gin.HandlerFunc {
	return func(c *gin.Context) {
		// gin 先完成路由匹配再执行 handler 链，所以这里 FullPath 已经可用。
		route := routeOf(c)
		method := c.Request.Method

		observ.InFlight.Inc()
		start := time.Now()

		c.Next()

		observ.InFlight.Dec()
		elapsed := time.Since(start).Seconds()
		status := strconv.Itoa(c.Writer.Status())

		observ.RequestTotal.WithLabelValues(route, method, status).Inc()
		observ.RequestDuration.WithLabelValues(route, method).Observe(elapsed)

		// 探针接口只记指标、不记日志，否则每天上万条心跳会淹掉业务日志。
		// 但它们仍然保留 trace_id 之外的全部指标，出问题时看 up / 抓取失败即可。
		if probeRoute(route) {
			return
		}

		// 结构化日志：每条都带 trace_id，这是能 grep 定位的前提
		lv := slog.LevelInfo
		if c.Writer.Status() >= 500 {
			lv = slog.LevelError
		} else if c.Writer.Status() >= 400 {
			lv = slog.LevelWarn
		}
		Logger.LogAttrs(c.Request.Context(), lv, "http_request",
			slog.String("trace_id", trace.FromContext(c.Request.Context())),
			slog.String("route", route),
			slog.String("method", method),
			slog.Int("status", c.Writer.Status()),
			slog.Float64("duration_ms", float64(elapsed*1000)),
			slog.String("client_ip", c.ClientIP()),
		)
	}
}

// Logger 全局 logger，由 main 注入。
// Logger 是包级 logger。
//
// 必须带 service 字段：这个 logger 打在启动、中间件降级等场景，
// 不带 service 的话这些日志进 ES 后会落进 logs-unknown 索引，
// 排查时按服务筛选根本找不到它们。
var Logger = observ.NewLogger("info").With("service", "gateway")

// Recover 捕获 panic，返回 500 而不是让进程崩掉。
// 网关是单点，它崩了全站就崩了，所以这一层必须兜住。
func Recover() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				Logger.ErrorContext(c.Request.Context(), "panic_recovered",
					slog.String("trace_id", trace.FromContext(c.Request.Context())),
					slog.Any("panic", r),
					slog.String("stack", string(debug.Stack())),
				)
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"code": 500, "message": "服务内部错误",
					"trace_id": trace.FromContext(c.Request.Context()),
				})
			}
		}()
		c.Next()
	}
}

// CORS 允许前端开发服务器跨域访问。
// 生产环境应该收紧 Allow-Origin 到具体域名，这里为本地开发放开。
func CORS(allowed []string) gin.HandlerFunc {
	set := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		set[o] = struct{}{}
	}
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if _, ok := set[origin]; ok {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Allow-Credentials", "true")
			c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Content-Type,Authorization,"+trace.HeaderName)
			c.Header("Access-Control-Expose-Headers", trace.ResponseHeader)
			c.Header("Access-Control-Max-Age", "86400")
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// ── 限流 ───────────────────────────────────────────────────

// limiterBucket 每个 key 一个令牌桶。
//
// 为什么不是全局一个桶：全局桶会让某个调用方把配额吃光，
// 影响到所有人。按 key 分桶才能做「按调用方限流」。
type limiterBucket struct {
	lim  *rate.Limiter
	seen time.Time
}

// RateLimiter 按「路由 + 调用方」维度的令牌桶限流器。
//
// 这里体现 Go 相对 Node 的一个实际差异：桶的读写需要并发安全，
// Node 单线程不需要锁（但也因此无法利用多核），Go 用 RWMutex 保护 map，
// 读多写少用 RWMutex 而不是 Mutex。
type RateLimiter struct {
	mu      sync.RWMutex
	buckets map[string]*limiterBucket
	rps     rate.Limit
	burst   int
	stop    chan struct{}
	once    sync.Once
}

// NewRateLimiter 创建限流器并启动后台清理。
//
// 后台 goroutine 的存在理由：不清理的话 buckets 会随 key 无限增长，内存泄漏。
// 这是 Go 里常见的「起了 goroutine 就得想清楚怎么停」——stop channel + Once。
func NewRateLimiter(rps float64, burst int) *RateLimiter {
	r := &RateLimiter{
		buckets: make(map[string]*limiterBucket),
		rps:     rate.Limit(rps),
		burst:   burst,
		stop:    make(chan struct{}),
	}
	go r.janitor(5 * time.Minute)
	return r
}

// janitor 定期清掉长时间没用的桶。
func (r *RateLimiter) janitor(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case now := <-t.C:
			r.janitorOnce(every, now)
		}
	}
}

// janitorOnce 执行一轮清理。
//
// 单独抽出来是为了可测：直接测 janitor 得等 5 分钟，
// 而且清理逻辑（阈值判定）本身很容易写错 -- 比如用 time.Since 而不是
// 传入 now，测试就没法构造"很久以前"的时间点。
//
// 顺便说明为什么阈值是 3*every 而不是 every：幂等/重试的调用方可能
// 间隔几分钟才来一次，清太急会把刚建好的桶删掉，下次又新建 --
// 不泄漏，但白白浪费内存分配。留 3 倍余量。
func (r *RateLimiter) janitorOnce(every time.Duration, now ...time.Time) {
	var t time.Time
	if len(now) > 0 {
		t = now[0]
	} else {
		t = time.Now()
	}
	r.mu.Lock()
	for k, b := range r.buckets {
		if t.Sub(b.seen) > 3*every {
			delete(r.buckets, k)
		}
	}
	r.mu.Unlock()
}

// Close 停止后台清理。
func (r *RateLimiter) Close() {
	r.once.Do(func() { close(r.stop) })
}

// Allow 判断是否放行。
func (r *RateLimiter) Allow(key string) bool {
	r.mu.RLock()
	b, ok := r.buckets[key]
	r.mu.RUnlock()

	if !ok {
		r.mu.Lock()
		// 双重检查：可能在上面的 RUnlock 与 Lock 之间被别的 goroutine 建好了
		if b, ok = r.buckets[key]; !ok {
			b = &limiterBucket{lim: rate.NewLimiter(r.rps, r.burst)}
			r.buckets[key] = b
		}
		r.mu.Unlock()
	}
	r.mu.Lock()
	b.seen = time.Now()
	r.mu.Unlock()
	return b.lim.Allow()
}

// RateLimit 中间件。key = 路由 + 调用方（没有调用方就按 IP）。
//
// 这一层防的是「**单个来源**刷接口」：一个 IP 或一个调用方 id
// 把某条路由的配额吃光。它按来源分桶，所以攻击者换 IP 就绕过。
func RateLimit(rl *RateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		caller := c.GetHeader("X-Caller-Id")
		if caller == "" {
			caller = c.ClientIP()
		}
		route := routeOf(c)
		if !rl.Allow(route + "|" + caller) {
			observ.RateLimitedTotal.WithLabelValues(route, "caller").Inc()
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"code": 429, "message": "请求过于频繁，请稍后重试",
				"trace_id": trace.FromContext(c.Request.Context()),
			})
			return
		}
		c.Next()
	}
}

// RateLimitByKey 按「路由 + 业务 ID」限流。
//
// 和 RateLimit 是两个正交的维度，不是替代关系：
//
//	RateLimit       按**来源**分桶，10 台机器各刷 1 次同一个 ID → 放行，因为它按来源看谁都没超
//	RateLimitByKey  按**目标**分桶，10 台机器各刷 1 次同一个 ID → 合并计数，第 11 次就打回
//
// 只做前者挡不住「肉鸡/多 IP 一起刷同一条数据」，只做后者挡不住
// 「一个来源轮着刷 10000 个 ID」。两条都挂才补得齐。
//
// keyFn 从请求里取业务标识。取不到（空串）时**直接放行**：
// 参数校验是 handler 的职责，限流层不做校验；而且空 key 会让所有
// 非法请求共用一个桶，等于给攻击者一个免费的全局限流开关。
//
// 注意这层的桶按业务 ID 建，key 空间和业务表一样大，
// 所以 janitor 的清理周期是它不泄漏的唯一保障——桶存在 map 里，
// 单机内存上限取决于单位时间内被访问过的不同 ID 数，而不是总量。
func RateLimitByKey(rl *RateLimiter, keyFn func(*gin.Context) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := keyFn(c)
		if id == "" {
			c.Next()
			return
		}
		route := routeOf(c)
		if !rl.Allow(route + "|biz|" + id) {
			observ.RateLimitedTotal.WithLabelValues(route, "biz_key").Inc()
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"code": 429, "message": "该数据查询过于频繁，请稍后重试",
				"trace_id": trace.FromContext(c.Request.Context()),
			})
			return
		}
		c.Next()
	}
}

// KeyFromJSON 从 JSON body 的整数字段取业务 ID（写接口用）。
//
// 读 body 之后必须把内容塞回去，否则后续 handler 的 ShouldBindJSON 拿到空 body。
// 这是「中间件读 body」的经典坑：net/http 的 Body 是一次性流。
func KeyFromJSON(field string) func(*gin.Context) string {
	return func(c *gin.Context) string {
		if c.Request.Body == nil || c.Request.Method == http.MethodGet {
			return ""
		}
		// 只在 body 还没被读过的时候读：已经读过说明有人缓存了，
		// 再读一次会得到空内容，反而破坏下游。
		if _, read := c.Get("__body_cached"); read {
			return ""
		}
		raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 4096))
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		c.Set("__body_cached", true)
		if err != nil || len(raw) == 0 {
			return ""
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return ""
		}
		v, ok := m[field]
		if !ok {
			return ""
		}
		// 用 json.Number 而不是 float64：ID 是 64 位整数，
		// 走 float64 会在 2^53 以上丢精度，两个不同的 ID 可能变成同一个 key。
		var n json.Number
		if err := json.Unmarshal(v, &n); err != nil {
			return ""
		}
		return n.String()
	}
}

// KeyFromQuery 从查询串取业务 ID（读接口用）。
func KeyFromQuery(field string) func(*gin.Context) string {
	return func(c *gin.Context) string {
		return c.Query(field)
	}
}

// KeyFromParam 从路径参数取业务 ID，如 /export/jobs/:id。
func KeyFromParam(field string) func(*gin.Context) string {
	return func(c *gin.Context) string {
		return c.Param(field)
	}
}

// ── 认证 ───────────────────────────────────────────────────

// Operator 经过鉴权后放进 context 的调用方身份。
type Operator struct {
	ID   string
	Role string
}

type opKey struct{}

// OperatorFrom 从 context 取调用方。
func OperatorFrom(c *gin.Context) (Operator, bool) {
	v, ok := c.Get("__operator")
	if !ok {
		return Operator{}, false
	}
	op, ok := v.(Operator)
	return op, ok
}

// Auth 简易鉴权：校验请求头里的 token。
//
// 真实系统这里会是 JWT 验签或调鉴权服务，本实现保留**结构**：
// 鉴权在网关做一次，operator 放 context 向下传，各领域服务不再重复鉴权。
// 这就是网关存在的第二个理由——横切关注点只写一次。
func Auth(verify func(token string) (Operator, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		// /healthz /metrics / 登录 不需要鉴权
		p := c.Request.URL.Path
		if p == "/healthz" || p == "/metrics" || p == "/api/auth/login" {
			c.Next()
			return
		}
		tok := c.GetHeader("Authorization")
		if len(tok) > 7 && tok[:7] == "Bearer " {
			tok = tok[7:]
		}
		op, err := verify(tok)
		if err != nil {
			// 鉴权失败必须计数：AuthFailureSpike 告警靠它发现「有人在扫 token」。
			// 只打日志的话，日志量大了没人会主动聚合看，告警也永远收不到数据。
			observ.AuthFailTotal.WithLabelValues(routeOf(c)).Inc()
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code": 401, "message": "登录已失效，请重新登录",
				"trace_id": trace.FromContext(c.Request.Context()),
			})
			return
		}
		c.Set("__operator", op)
		c.Next()
	}
}

// ── 超时 ───────────────────────────────────────────────────

// Timeout 给请求加 deadline。
//
// 用 context.WithTimeout 而不是 time.AfterFunc：前者会把取消信号
// 一路传下去，下游的 SQL / Redis 调用能感知到并提前放弃。
// 这是 Go 里「取消要沿调用链传播」的基本做法，Node 里靠 AbortSignal 做同一件事。
func Timeout(d time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), d)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
		if ctx.Err() == context.DeadlineExceeded && !c.Writer.Written() {
			c.AbortWithStatusJSON(http.StatusGatewayTimeout, gin.H{
				"code": 504, "message": "请求超时",
				"trace_id": trace.FromContext(c.Request.Context()),
			})
		}
	}
}

// ── 路由装配 ───────────────────────────────────────────────

// Mount 把健康检查与指标端点挂上。
func Mount(r *gin.Engine) {
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.HandlerFor(observ.Registry, promhttp.HandlerOpts{EnableOpenMetrics: true})))
}
