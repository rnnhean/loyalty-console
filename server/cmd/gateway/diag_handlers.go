package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/example/loyalty-console/server/internal/diag"
	"github.com/example/loyalty-console/server/internal/observ"
	"github.com/example/loyalty-console/server/pkg/trace"
)

// ── 诊断接口 ───────────────────────────────────────────────
//
// 三个接口的分工：
//
//	GET  /api/diag/checks   规则目录，agent 启动拉一次
//	GET  /api/diag/run/:name 单条规则，对话里「按需再查一项」走这里
//	POST /api/diag/batch    体检，一次跑多条
//
// 三者都不做跨请求缓存。理由：配置随时会被人改，
// 5 秒前的结论可能已经过期，而用户会以为它是最新的。

// maxBatchSize 一次 batch 最多跑几条。
// 不是性能限制，是「防模型一次要 20 项」——那既超出 45 秒预算，
// 也让报告变得没人读。
const maxBatchSize = 8

type diagCheckItem struct {
	Name      string                    `json:"name"`
	Title     string                    `json:"title"`
	Domain    string                    `json:"domain"`
	Params    map[string]diag.ParamSpec `json:"params,omitempty"`
	CostHint  string                    `json:"cost_hint"`
	TimeoutMS int                       `json:"timeout_ms"`
}

// listChecks 规则目录。
func listChecks() gin.HandlerFunc {
	return func(c *gin.Context) {
		items := make([]diagCheckItem, 0, len(diag.Registry))
		for _, ch := range diag.Registry {
			items = append(items, diagCheckItem{
				Name:      ch.Name,
				Title:     ch.Title,
				Domain:    ch.Domain,
				Params:    ch.Params,
				CostHint:  ch.CostHint,
				TimeoutMS: ch.TimeoutMS,
			})
		}
		c.JSON(http.StatusOK, gin.H{
			"code":     0,
			"total":    len(items),
			"items":    items,
			"trace_id": trace.FromContext(c.Request.Context()),
		})
	}
}

type diagRunResponse struct {
	Check      string         `json:"check"`
	Verdict    diag.Verdict   `json:"verdict"`
	Confidence float64        `json:"confidence"`
	Findings   []diag.Finding `json:"findings"`
	Coverage   diag.Coverage  `json:"coverage"`
	Error      string         `json:"error,omitempty"`
}

// runCheck 执行单条规则。
func runCheck(ds diag.DataSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		ch, found := diag.Find(name)
		if !found {
			// 未知规则名是 400 不是 404：资源是「诊断规则集合」，
			// 集合存在，是这个元素不在集合里。
			// 对 agent 来说两者都是「这个工具不可用」，但 400 不会触发
			// 前端的「接口不存在」全局提示。
			c.JSON(http.StatusBadRequest, gin.H{
				"code":     400,
				"message":  "未知的诊断规则",
				"trace_id": trace.FromContext(c.Request.Context()),
			})
			return
		}

		params := paramsFromQuery(c, ch)
		started := time.Now()
		res, err := runWithTimeout(c.Request.Context(), ch, ds, params)
		observeCheck(ch.Name, res, err, started)
		if err != nil {
			fail(c, err)
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"code": 0,
			"data": diagRunResponse{
				Check:      ch.Name,
				Verdict:    res.Verdict,
				Confidence: res.Confidence,
				Findings:   res.Findings,
				Coverage:   res.Coverage,
			},
			"trace_id": trace.FromContext(c.Request.Context()),
		})
	}
}

type diagBatchRequest struct {
	Checks []string          `json:"checks"`
	Params map[string]string `json:"params"`
}

type diagBatchResponse struct {
	Partial bool              `json:"partial"`
	Results []diagRunResponse `json:"results"`
}

// runBatch 体检：一次跑多条。
//
// 单条失败不算整体失败——这是这个接口的核心语义。
// 客户端拿 partial=true 就知道「有结果但不能全信」，
// 失败的那条 verdict 为空字符串，带一个机器可读的 error 标签。
func runBatch(ds diag.DataSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req diagBatchRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":     400,
				"message":  "请求格式不正确",
				"trace_id": trace.FromContext(c.Request.Context()),
			})
			return
		}
		if len(req.Checks) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":     400,
				"message":  "checks 不能为空",
				"trace_id": trace.FromContext(c.Request.Context()),
			})
			return
		}
		if len(req.Checks) > maxBatchSize {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":     400,
				"message":  fmt.Sprintf("一次最多 %d 条", maxBatchSize),
				"trace_id": trace.FromContext(c.Request.Context()),
			})
			return
		}

		out := diagBatchResponse{
			Partial: false,
			Results: make([]diagRunResponse, 0, len(req.Checks)),
		}

		// 串行执行，不并发。
		//
		// 理由：这个接口在体检路径上，一次最多 8 条、单条 6 秒上限，
		// 串行最坏 48 秒会顶到总预算——所以这里必须设一个总截止时间，
		// 而不是靠并发去抢时间。并发会让多条规则同时打数据库，
		// 在库本来就慢的时候反而更容易一起超时。
		ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
		defer cancel()

		for _, name := range req.Checks {
			ch, found := diag.Find(name)
			if !found {
				// 这里不埋点：name 来自请求体，是任意字符串。
				// 拿它当 label 等于把指标维度交给外部输入，
				// 几百次乱填的请求就能让 Prometheus 的时间序列爆掉。
				out.Partial = true
				out.Results = append(out.Results, diagRunResponse{
					Check:   name,
					Verdict: "", // 空结论：这条没跑成，前端要按「没能执行」渲染，不能当 ok
					Error:   "unknown_check",
				})
				continue
			}

			started := time.Now()
			res, err := runWithTimeout(ctx, ch, ds, toParams(req.Params))
			observeCheck(ch.Name, res, err, started)
			if err != nil {
				out.Partial = true
				out.Results = append(out.Results, diagRunResponse{
					Check:   name,
					Verdict: "",
					Error:   errorLabel(err),
				})
				continue
			}
			out.Results = append(out.Results, diagRunResponse{
				Check:      ch.Name,
				Verdict:    res.Verdict,
				Confidence: res.Confidence,
				Findings:   res.Findings,
				Coverage:   res.Coverage,
			})
		}

		c.JSON(http.StatusOK, gin.H{
			"code":     0,
			"data":     out,
			"trace_id": trace.FromContext(c.Request.Context()),
		})
	}
}

// runWithTimeout 给单条规则套上它自己声明的超时。
func runWithTimeout(ctx context.Context, ch diag.Check, ds diag.DataSource, p diag.Params) (diag.Result, error) {
	rctx, cancel := context.WithTimeout(ctx, time.Duration(ch.TimeoutMS)*time.Millisecond)
	defer cancel()
	return ch.Run(rctx, ds, p)
}

// observeCheck 把一条规则的执行结果记进指标。
//
// 埋点放在 handler 而不是规则实现里，是为了守住纯函数那条线：
// 规则只做判定，规则作者加一条新规则时不必碰 observ 包，
// 它的单元测试也不用为了 metrics 引入全局状态。
//
// 规则名只来自 diag.Registry（调用方已确认 found），不会含外部输入，
// 所以可以安全地当 label。——「结论是 inconclusive 且 missing 非空」
// 才算一次覆盖缺失；如果规则自己已经失败，缺失是必然的，不再重复计数。
func observeCheck(name string, res diag.Result, err error, started time.Time) {
	verdict := string(res.Verdict)
	if err != nil {
		verdict = "error"
	}
	observ.CheckTotal.WithLabelValues(name, verdict).Inc()
	observ.CheckDuration.WithLabelValues(name).Observe(time.Since(started).Seconds())
	if err == nil && len(res.Coverage.Missing) > 0 {
		observ.CoverageMissingTotal.WithLabelValues(name).Inc()
	}
}

// errorLabel 把内部错误压成一个短标签。
//
// 这个字符串会进模型上下文，所以只给机器看的枚举，不塞堆栈——
// 堆栈里可能有 SQL、表名、连接串片段。
func errorLabel(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case err == nil:
		return ""
	default:
		return "db_error"
	}
}

// paramsFromQuery 从 query string 拼参数。
// 规则声明了哪些参数就取哪些，没声明的忽略——
// 不让调用方往规则里塞任意键。
func paramsFromQuery(c *gin.Context, ch diag.Check) diag.Params {
	if len(ch.Params) == 0 {
		return nil
	}
	out := make(diag.Params, len(ch.Params))
	for key := range ch.Params {
		if v := c.Query(key); v != "" {
			out[key] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func toParams(m map[string]string) diag.Params {
	if len(m) == 0 {
		return nil
	}
	out := make(diag.Params, len(m))
	for k, v := range m {
		if v != "" {
			out[k] = v
		}
	}
	return out
}
