package observ

import "github.com/prometheus/client_golang/prometheus"

// ── 规则引擎 ─────────────────────────────────────────────

// RuleEvalTotal 求值次数。
//
// 按 result 切分而不是只记总数：命中率突然变化，
// 要么是数据变了（正常），要么是有人改错了规则（事故）。
// 这两种情况的处置完全不同，所以必须能分开看。
var RuleEvalTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "rule", Name: "eval_total", Help: "规则求值次数，按结果切分"},
	[]string{"result"},
)

// RuleEvalDuration 求值耗时。
// 规则是逐行求值的，复杂度直接和规则条数、嵌套深度相关；
// P99 涨说明有人在写深嵌套，而不是机器变慢。
var RuleEvalDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "mc", Subsystem: "rule", Name: "eval_duration_seconds",
		Help:    "单次规则求值耗时",
		Buckets: []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5},
	},
	[]string{"scope"}, // single = 单条解释，batch = 试运行整批
)

// ── 定时作业 ─────────────────────────────────────────────

// JobRunTotal 作业执行次数。
//
// 标签名用 job_name 而不是 job：job 是 Prometheus 抓取配置保留的标签，
// 自定义指标里再用同名标签会被 target 的 job 覆盖（实测面板上全变成 "gateway"）。
var JobRunTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "job", Name: "run_total", Help: "定时作业执行次数，按作业/结果切分"},
	[]string{"job_name", "result"},
)

// JobDuration 作业耗时。
// 分桶上限取到 120 秒：清理类作业可能在几十万行上跑，
// 这个区间里出现样本说明该考虑分批了。
var JobDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "mc", Subsystem: "job", Name: "duration_seconds",
		Help:    "定时作业耗时",
		Buckets: []float64{0.01, 0.1, 0.5, 1, 5, 15, 30, 60, 120},
	},
	[]string{"job_name"},
)

// JobSkippedTotal 因并发策略被跳过的次数。
// 持续走高说明作业间隔配得太密，或者上一次跑得太慢。
var JobSkippedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "job", Name: "skipped_total", Help: "因并发策略被跳过的作业次数"},
	[]string{"job_name", "reason"},
)

// ── 字段脱敏 ─────────────────────────────────────────────

// MaskAppliedTotal 脱敏生效次数，按来源切分。
// source=local 占比高，说明 Python 识别服务不稳——这是一个
// 用指标就能发现的依赖问题，不用等用户投诉。
var MaskAppliedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "mask", Name: "applied_total", Help: "脱敏生效次数，按 来源/类型 切分"},
	[]string{"source", "kind"},
)

// MaskPlaintextTotal 明文访问次数。
// 这是审计指标：正常应为 0 或极低，突然上涨必须能查到是谁。
var MaskPlaintextTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{Namespace: "mc", Subsystem: "mask", Name: "plaintext_total", Help: "明文访问次数，按字段切分"},
	[]string{"field"},
)

func init() {
	Registry.MustRegister(
		RuleEvalTotal, RuleEvalDuration,
		JobRunTotal, JobDuration, JobSkippedTotal,
		MaskAppliedTotal, MaskPlaintextTotal,
	)
}
