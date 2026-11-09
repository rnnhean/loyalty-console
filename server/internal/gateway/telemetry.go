package gateway

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/example/loyalty-console/server/internal/observ"
	"github.com/example/loyalty-console/server/pkg/trace"
)

// ── 前端监控上报 ───────────────────────────────────────────
//
// 设计前提：本服务要部署在公网，前端上报端点是一个**匿名可达的写接口**。
// 这类端点有三个典型风险，必须在设计时就挡住，而不是等被打爆了再补：
//
//  1. 指标基数爆炸 —— 攻击者构造 kind/name 随机值，一次请求就能在
//     Prometheus 里造出上千条时间序列，把内存和存储吃光。
//     → label 用白名单枚举，不在白名单里的直接丢弃。
//
//  2. 日志注入 / 敏感数据回灌 —— 上报体里可能带页面 URL（含 query 上的
//     token）、堆栈（含用户输入）。原样落日志等于把凭据写进磁盘。
//     → 只保留 pathname，丢弃 query 与 hash；字段做长度截断。
//
//  3. 被当作免费存储 —— 不限流的话，任何人都能无限 POST。
//     → 走网关的 RateLimiter（按 IP 分桶），并限制单次事件数与 body 大小。

// 白名单：kind 决定事件类型，只有这几类会被记录。
var allowedKinds = map[string]struct{}{
	"page_view":      {},
	"web_vital":      {},
	"js_error":       {},
	"api_error":      {},
	"resource_error": {},
	"unhandled":      {},
}

// 白名单：name 的合法取值（随 kind 而定，统一收在这里便于审计）。
var allowedNames = map[string]struct{}{
	// web_vital
	"LCP": {}, "CLS": {}, "INP": {}, "FCP": {}, "TTFB": {},
	// api_error / resource_error
	"fetch": {}, "xhr": {}, "script": {}, "link": {}, "img": {},
	// page_view 用固定值
	"page": {},
}

// rating 只接受这四档（Web Vitals 官方分级）。
var allowedRatings = map[string]struct{}{
	"good": {}, "needs-improvement": {}, "poor": {}, "unknown": {},
}

// beaconEvent 单条上报。
type beaconEvent struct {
	Kind    string  `json:"kind"`
	Name    string  `json:"name"`
	Value   float64 `json:"value"`
	Rating  string  `json:"rating"`
	TraceID string  `json:"trace_id"`
	URL     string  `json:"url"`
}

type beaconPayload struct {
	Events []beaconEvent `json:"events"`
}

const (
	maxBeaconBody   = 64 << 10 // 64KB，正常一批 20 条事件不超过 8KB
	maxBeaconEvents = 50
	maxURLStored    = 256
)

// Telemetry 处理 POST /api/telemetry/beacon。
//
// 返回 204 无 body：前端用 sendBeacon 发送，不读响应。
// 即便某些事件被丢弃也返回 204 —— 让前端重试没有意义，
// 反而会在页面上线首日因个别字段不合法产生重试风暴。
func Telemetry() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		tid := trace.FromContext(ctx)

		if c.Request.ContentLength > maxBeaconBody {
			c.AbortWithStatus(http.StatusRequestEntityTooLarge)
			return
		}

		var p beaconPayload
		if err := c.ShouldBindJSON(&p); err != nil {
			c.Status(http.StatusNoContent)
			return
		}
		if len(p.Events) > maxBeaconEvents {
			p.Events = p.Events[:maxBeaconEvents]
		}

		kept := 0
		for _, e := range p.Events {
			if _, ok := allowedKinds[e.Kind]; !ok {
				continue
			}
			if e.Name == "" {
				e.Name = "page"
			}
			if _, ok := allowedNames[e.Name]; !ok {
				continue
			}
			if _, ok := allowedRatings[e.Rating]; !ok {
				e.Rating = "unknown"
			}
			kept++

			if e.Kind == "web_vital" {
				// 异常值直接丢：负数或超过 1 小时的数值只会污染分位数
				if e.Value < 0 || e.Value > 3600 {
					continue
				}
				observ.FrontendVital.WithLabelValues(e.Name).Observe(e.Value)
				continue
			}

			// path 只对 page_view 记：PV 需要按页面拆，其他事件的 url
			// 没有聚合价值，给了只会增加基数。
			// safePath 已经把 query 丢掉，这里不会带出 token。
			pagePath := ""
			if e.Kind == "page_view" {
				pagePath = safePath(e.URL)
			}
			observ.FrontendEvents.WithLabelValues(e.Kind, e.Name, e.Rating, pagePath).Inc()

			// 错误类事件写一行日志，带上 trace_id 与收敛后的路径，
			// 用于「用户报错 → 用 trace_id 串起前后端」。
			if e.Kind == "js_error" || e.Kind == "unhandled" || e.Kind == "api_error" {
				Logger.WarnContext(ctx, "frontend_event",
					"kind", e.Kind,
					"name", e.Name,
					"path", safePath(e.URL),
					"trace_id", firstNonEmpty(e.TraceID, tid),
				)
			}
		}

		c.Status(http.StatusNoContent)
	}
}

// safePath 只保留 pathname。
//
// 为什么连 query 都要丢：后台系统常见的链接形如
//
//	/trade?order=xxx&_t=<token>
//
// query 里混着凭据的概率远高于想象。日志一旦落盘就很难追溯清理，
// 所以默认「不留」，而不是「先存下来再想办法处理」。
func safePath(raw string) string {
	if raw == "" {
		return ""
	}
	// 去掉 protocol+host，保留 path
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			raw = rest[j:]
		} else {
			raw = "/"
		}
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	// ★ 切完可能是空串：形如 "https://example.com?token=x" 的输入，
	//   第一步切出 "?token=x"，第二步按 '?' 再切就什么都不剩了。
	//   空 path 会变成一个独立的标签值（Grafana 里显示成空行），
	//   既污染 PV 按页面聚合，又让"只有首页"这种情况无法统计。
	//   path 的语义是"URL 的路径部分"，不存在就是根路径 "/"。
	if raw == "" {
		return "/"
	}
	if len(raw) > maxURLStored {
		raw = truncateUTF8(raw, maxURLStored)
	}
	return raw
}

// truncateUTF8 按字节上限截断，但保证结果是合法 UTF-8。
//
// 为什么不能直接 raw[:n]：UTF-8 里一个汉字占 3 字节，n 正好落在字符中间时
// 会切出半个字符。写进日志就是 "�"，写进 Prometheus label 就是一个和原页面
// 对不上的新序列 —— 既难看又让「PV 按页面聚合」出现一条永远只有零星的杂项。
//
// 做法是往回退到字符起始字节。UTF-8 的后续字节最高两位恒为 10（0x80~0xBF），
// 起始字节最高两位一定不是 10，所以 utf8.RuneStart 是精确判据，
// 不需要先解码整串、也不会有"退到 0 就死循环"的问题（n 递减到 0 时循环条件先失败）。
//
// 复杂度上只往回退最多 3 个字节（UTF-8 最长 4 字节），不是逐字符重扫。
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
