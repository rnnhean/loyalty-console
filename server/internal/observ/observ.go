// Package observ —— 可观测性基础设施。
//
// 设计取舍（个人/单体规模，不引 OTel Collector）：
//   - Metrics  → Prometheus，暴露 /metrics，用 Histogram 出分位数
//   - Logs     → 结构化 JSON（slog），每条必带 trace_id
//   - Traces   → 不引分布式追踪，只做「trace_id 贯穿」：
//     前端生成 → 网关接收 → context 传递 → 日志与响应头回传
//
// 为什么不做完整 trace：单体服务内 Span 的意义有限，真正有价值的
// 是「用户截图 → 拿到 trace_id → grep 日志定位」，这一条链路成本极低、收益最高。
package observ

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry 独立的指标注册表。多个进程（gateway / admin）各自持有，
// 避免全局默认注册表在测试里重复注册 panic。
var Registry = prometheus.NewRegistry()

// ── RED 指标：Rate / Errors / Duration ──────────────────────

// RequestTotal 请求计数（Counter 只增不减）
var RequestTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "mc",
		Subsystem: "http",
		Name:      "requests_total",
		Help:      "HTTP 请求总数，按 路由/方法/状态码 切分",
	},
	[]string{"route", "method", "status"},
)

// RequestDuration 耗时分布。
//
// ⚠️ 必须用 Histogram 而不是 Summary：
// 分位数（p95/p99）要能跨实例聚合，Summary 的分位数合不了。
// 桶的划分依据实测——本系统接口 P99 在 800ms 以内，超过 2s 视为异常，
// 所以 2s 以上只留一个 5s 兜底桶，避免长尾把桶撑爆。
var RequestDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "mc",
		Subsystem: "http",
		Name:      "request_duration_seconds",
		Help:      "HTTP 请求耗时",
		Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	},
	[]string{"route", "method"},
)

// InFlight 当前正在处理的请求数（Gauge 可增可减，反映饱和度）
var InFlight = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "mc",
		Subsystem: "http",
		Name:      "in_flight_requests",
		Help:      "正在处理中的请求数",
	},
)

// ── 业务指标 ───────────────────────────────────────────────

// CacheHits / CacheMisses 缓存命中率。命中率是判断缓存是否真的有用的唯一依据，
// 只加缓存不看命中率等于蒙眼优化。
var CacheHits = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "cache", Name: "hits_total", Help: "缓存命中"},
	[]string{"namespace"},
)
var CacheMisses = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "cache", Name: "misses_total", Help: "缓存未命中"},
	[]string{"namespace"},
)

// SingleflightCollapsed 被 singleflight 合并掉的重复请求数。
// 这个数字直接说明「防击穿措施挽回了多少次重复查询」。
var SingleflightCollapsed = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "cache", Name: "singleflight_collapsed_total", Help: "被合并的重复请求"},
	[]string{"key"},
)

// ErrgroupFanout 并发聚合的子任务数与耗时。
// 用来看「并发真的带来了加速，还是被某一个慢任务拖死」。
var FanoutDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "mc", Subsystem: "aggregate", Name: "fanout_duration_seconds",
		Help: "并发聚合总耗时", Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2},
	},
	[]string{"aggregate"},
)

// FanoutBlocks 单次聚合扇出的子任务数。
// 和 FanoutDuration 一起看：块数多而耗时不涨，才是真并发；
// 块数多、耗时长，说明撞到了 SetLimit 或连接池上限。
var FanoutBlocks = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "mc", Subsystem: "aggregate", Name: "fanout_blocks",
		Help: "单次聚合的子任务数", Buckets: []float64{1, 2, 4, 6, 8, 12, 20},
	},
	[]string{"aggregate"},
)

// DegradedTotal 聚合时降级的区块数（某个子查询失败但仍返回了部分数据）。
// 这是「用户看到页面成功了，但其实少了一块」的唯一信号，
// 只看 HTTP 200 是看不出来的，必须有这个计数器才可能发现。
var DegradedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "aggregate", Name: "degraded_total", Help: "聚合降级的区块数"},
	[]string{"aggregate", "block"},
)

// RateLimitedTotal 被限流拒绝的请求数。
//
// scope 区分是哪一层拦下的：
//
//	caller  —— 按「路由 + 调用方(IP)」限流，防的是单个来源刷接口
//	biz_key —— 按「路由 + 业务 ID」限流，防的是大量来源一起刷同一个 ID
//
// 两个 label 必须分开，否则分不清「有人在爆破」和「某条数据被打爆」：
// 前者应该封 IP，后者应该扩缓存或加锁，处置手段完全不同。
var RateLimitedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "gateway", Name: "rate_limited_total", Help: "被限流的请求"},
	[]string{"route", "scope"},
)

// AuthFailTotal 鉴权失败数。公网部署下这是最重要的安全指标之一：
// 短时间内飙升通常意味着有人在扫 token。
var AuthFailTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "gateway", Name: "auth_fail_total", Help: "鉴权失败次数"},
	[]string{"route"},
)

// ── 前端上报指标 ───────────────────────────────────────────
//
// 前端通过网关上报（不直连 Prometheus），网关把事件转成计数器。
// 这样有三个好处：不必在前端暴露写权限；上报流量走同一套限流与鉴权；
// 前端 JS 事件不会污染 /metrics 的基数（route/label 都在网关侧白名单约束）。

// FrontendEvents 前端事件计数。
// kind 是**白名单枚举**（page_view / web_vital / js_error / api_error / resource_error），
// 不直接透传上报值 —— 否则任意人构造请求就能把指标基数打爆。
//
// path 标签只对 page_view 有值，并且已经在网关侧 safePath() 收窄过：
// 只保留 pathname、丢掉 query（query 里混着 token 的概率很高）。
// 其他 kind 一律传 ""，避免把 url 也变成基数来源。
var FrontendEvents = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "frontend", Name: "events_total", Help: "前端上报事件"},
	[]string{"kind", "name", "rating", "path"},
)

// FrontendVital 核心 Web 指标分布。
// 用 Histogram 而不是 Counter：LCP/CLS 要看分位数才算得清「有多少次体验是坏的」，
// 只看平均值会被海量快页面稀释。
//
// ★ 单位是**毫秒**，不是秒。
//   web-vitals 库给的原生单位就是毫秒（LCP 2500 表示 2.5 秒），
//   上报表里也是毫秒。桶必须按毫秒划，否则全部落进 +Inf，
//   分位数算出来是最后那个桶的边界，等于没有数据 —— 这个错很隐蔽：
//   指标看着有、面板却永远 No data，因为 quantile 和「良好率」都算不出来。
//
//   各指标的判定线（Google 官方口径，毫秒）：
//     LCP 2500 / FCP 1800 / TTFB 800 / INP 200
//
// ★ CLS 是**无量纲比值**（0~1），和上面四个毫秒量纲不同。
//   它落进最低那几档桶，数值本身可读，但不要拿它跟毫秒指标比大小。
//   要精确看 CLS 的分布，用下面两个 0.1 / 0.25 的小桶。
//
// metric 标签只接受 LCP/CLS/INP/FCP/TTFB 五个枚举值（网关侧白名单约束），
// 所以这里的基数上限是 5。
var FrontendVital = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "mc", Subsystem: "frontend", Name: "vital_value",
		Help: "Core Web Vitals 取值（毫秒；CLS 为单位比值）",
		Buckets: []float64{
			// CLS 的量程（无量纲）
			0.1, 0.25,
			// 毫秒量程：覆盖「极快 → 不可接受」
			200, 500, 800, 1200, 1800, 2500, 4000, 10000,
		},
	},
	[]string{"metric"},
)

// ── 分布式锁 ───────────────────────────────────────────────
//
// 三个数必须一起看，单看任何一个都会误判：
//   acquire 成功数涨 → 有并发在抢锁（正常）
//   acquire 失败数涨 → 锁竞争激烈 或 有任务卡住没释放
//   lost 出现       → 严重：锁的租约过期了但业务还在跑，
//                     意味着同一份工作可能被两个实例同时做

// LockAcquire 锁获取结果。result ∈ {ok, busy, error}
var LockAcquire = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "lock", Name: "acquire_total", Help: "分布式锁获取结果"},
	[]string{"result"},
)

// LockHoldDuration 持锁时长。持续逼近租约 TTL 说明业务执行太慢，
// 靠 watchdog 续租撑着 —— 那时候锁已经不再是「短临界区」了，架构该改。
var LockHoldDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "mc", Subsystem: "lock", Name: "hold_duration_seconds",
		Help: "持锁时长", Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 15, 60},
	},
	[]string{"scope"},
)

// LockLost 租约丢失次数。**这个数字只要非零就要报警**，
// 它代表「我们以为只有一个人在改，但实际上不是」。
var LockLost = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "lock", Name: "lost_total", Help: "锁租约丢失（业务仍在执行）"},
	[]string{"scope"},
)

// ── 幂等 ───────────────────────────────────────────────────

// IdempotencyOutcome 幂等判定结果。
// replay 比例高说明客户端重试多（网络差，或前端连点了）；
// busy 比例高说明同一个用户的并发请求在互相顶。
var IdempotencyOutcome = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "idempotency", Name: "outcome_total", Help: "幂等判定结果"},
	[]string{"outcome"},
)

// ── 消息队列 ───────────────────────────────────────────────

// KafkaPublished 生产消息结果
var KafkaPublished = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "mq_kafka", Name: "published_total", Help: "Kafka 消息生产"},
	[]string{"topic", "result"},
)

// KafkaConsumed 消费结果。dlq 是重试耗尽后放弃的消息数，
// 非零意味着有事件没被正确处理，需要人工介入。
var KafkaConsumed = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "mq_kafka", Name: "consumed_total", Help: "Kafka 消息消费"},
	[]string{"topic", "result"},
)

// KafkaLag 消费滞后（最后一次抓取的 offset 差）。
// 单实例实现里用不上，改成消费组后这个指标就是「消费者是否跟得上」的唯一依据。
var KafkaLag = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{Namespace: "mc", Subsystem: "mq_kafka", Name: "lag", Help: "消费滞后条数"},
	[]string{"topic", "partition"},
)

// KafkaHandleDuration 单条消息处理耗时。
// 必须小于 max poll interval，否则消费者会被踢出组、触发 rebalance。
var KafkaHandleDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "mc", Subsystem: "mq_kafka", Name: "handle_duration_seconds",
		Help: "消息处理耗时", Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
	},
	[]string{"topic"},
)

// RabbitPublished 延迟任务投递
var RabbitPublished = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "mq_rabbit", Name: "published_total", Help: "RabbitMQ 任务投递"},
	[]string{"queue", "result"},
)

// RabbitConsumed 延迟任务消费
var RabbitConsumed = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "mq_rabbit", Name: "consumed_total", Help: "RabbitMQ 任务消费"},
	[]string{"queue", "result"},
)

// RabbitDeadLettered 进入死信的任务数。
// 延迟队列的超时回收是**按时触发**的，死信意味着到期了但没处理成功，
// 权益就不会被回收 —— 这是资损风险的直接信号。
var RabbitDeadLettered = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "mq_rabbit", Name: "dead_lettered_total", Help: "死信任务数"},
	[]string{"queue"},
)

// ── 对象存储 ───────────────────────────────────────────────

// BlobOperation 对象存储操作。bytes 只对 upload 有意义（Histogram 会因为
// 0 值把分位数拉低，所以不用 Histogram，用 Summary 的 count/sum 手算均值）。
var BlobOperation = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "blob", Name: "operations_total", Help: "对象存储操作"},
	[]string{"op", "result"},
)

// BlobBytes 上传/下载字节数
var BlobBytes = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "blob", Name: "bytes_total", Help: "对象存储字节数"},
	[]string{"op"},
)

// ExportJobs 异步导出任务状态。
// queued → running → done/failed。卡在 running 不动的任务数
// 是「导出功能用户觉得坏了但错误率是 0」的唯一暴露点。
var ExportJobs = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "export", Name: "jobs_total", Help: "导出任务"},
	[]string{"status"},
)

// ExportRows 单次导出的行数分布，用来判断「导出慢」是因为数据多还是因为 SQL 慢。
var ExportRows = prometheus.NewHistogram(
	prometheus.HistogramOpts{
		Namespace: "mc", Subsystem: "export", Name: "rows",
		Help: "单次导出行数", Buckets: []float64{100, 1000, 5000, 20000, 50000, 200000},
	},
)

// OutboxFlushed 发件箱补发结果。result: ok / retry / dropped
var OutboxFlushed = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "mc", Subsystem: "outbox", Name: "flushed_total",
		Help: "发件箱补发次数，按结果切分",
	},
	[]string{"result"},
)

// OutboxBacklog 当前待发条数。
//
// 用 Gauge 而不是 Counter：积压是一个「此刻多少」的量。
// 它的用途是判断补发协程是否跟得上 —— 持续上涨说明下游堵了，
// 而不是单看某个报警阈值。
var OutboxBacklog = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "mc", Subsystem: "outbox", Name: "backlog",
		Help: "发件箱中待补发的事件条数",
	},
)

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		RequestTotal, RequestDuration, InFlight, RateLimitedTotal, AuthFailTotal,
		CacheHits, CacheMisses, SingleflightCollapsed, FanoutDuration, FanoutBlocks, DegradedTotal,
		FrontendEvents, FrontendVital,
		LockAcquire, LockHoldDuration, LockLost, IdempotencyOutcome,
		KafkaPublished, KafkaConsumed, KafkaLag, KafkaHandleDuration,
		RabbitPublished, RabbitConsumed, RabbitDeadLettered,
		BlobOperation, BlobBytes, ExportJobs, ExportRows,
		OutboxFlushed, OutboxBacklog,
	)
}

// MetricsHandler 返回 /metrics 端点。
func MetricsHandler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{EnableOpenMetrics: true})
}

// ── 结构化日志 ─────────────────────────────────────────────

// NewLogger 返回 JSON 结构化 logger。
//
// 为什么用 slog 而不是 zap/logrus：Go 1.21+ 标准库自带，零依赖，
// 换实现只要换 Handler。个人项目没必要为日志引第三方。
func NewLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: lv,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// 时间统一成 RFC3339，便于 Grafana 按时间范围查
			if a.Key == slog.TimeKey {
				return slog.String("ts", a.Value.Time().Format(time.RFC3339Nano))
			}
			return a
		},
	})
	return slog.New(h)
}
