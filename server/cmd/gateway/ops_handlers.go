package main

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/example/loyalty-console/server/internal/gateway"
	"github.com/example/loyalty-console/server/internal/masking"
	"github.com/example/loyalty-console/server/internal/rule"
	"github.com/example/loyalty-console/server/internal/scheduler"
)

// opsDeps 三块新能力的依赖集合。
// 单独一个结构体而不是往 main 里塞一堆闭包变量：
// 这些 handler 要在测试里单独构造（不需要起完整网关）。
type opsDeps struct {
	Rules     *rule.Service
	Jobs      *scheduler.Store
	Runner    *scheduler.Runner
	Masking   *masking.Service
	MaskField []string // 允许预览脱敏的字段白名单
}

// ── 规则 ─────────────────────────────────────────────────

func listOpsRules(deps *opsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		items, err := deps.Rules.Store.List(c.Request.Context(), c.Query("domain"), 200)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "查询规则失败"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "items": items, "total": len(items)})
	}
}

func getRule(deps *opsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		r, err := deps.Rules.Store.Get(c.Request.Context(), id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "规则不存在"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "item": r})
	}
}

func createRule(deps *opsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var r rule.Rule
		if err := c.ShouldBindJSON(&r); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "请求体格式不正确"})
			return
		}
		id, err := deps.Rules.Create(c.Request.Context(), r)
		if err != nil {
			// 校验失败是 400（运营写错了），不是 500
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "id": id})
	}
}

func updateRule(deps *opsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		var r rule.Rule
		if err := c.ShouldBindJSON(&r); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "请求体格式不正确"})
			return
		}
		r.ID = id
		if err := deps.Rules.Update(c.Request.Context(), r); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0})
	}
}

func deleteRule(deps *opsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if err := deps.Rules.Store.Delete(c.Request.Context(), id); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "规则不存在"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0})
	}
}

// dryRunRules 用样例数据试跑。请求体可传自定义 data，不传就用内置样例。
func dryRunRules(deps *opsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			Domain string         `json:"domain"`
			Data   map[string]any `json:"data"`
		}
		_ = c.ShouldBindJSON(&body)
		if len(body.Data) == 0 {
			body.Data = rule.SampleData()
		}
		stats, err := deps.Rules.DryRun(c.Request.Context(), body.Domain, body.Data)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "试运行失败"})
			return
		}
		hit := 0
		for _, s := range stats {
			if s.Hit {
				hit++
			}
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "items": stats, "total": len(stats), "hit": hit})
	}
}

// explainRule 解释单条规则。
func explainRule(deps *opsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		var body struct {
			Data map[string]any `json:"data"`
		}
		_ = c.ShouldBindJSON(&body)
		if len(body.Data) == 0 {
			body.Data = rule.SampleData()
		}
		res, r, err := deps.Rules.Explain(c.Request.Context(), id, body.Data)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "规则不存在"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"code": 0, "rule": r.Name, "hit": res.Hit,
			"summary": rule.Summary(res), "steps": res.Steps,
			"missing_fields": res.MissingFields,
		})
	}
}

func ruleVersions(deps *opsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		v, err := deps.Rules.Store.Versions(c.Request.Context(), id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "查询版本失败"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "items": v})
	}
}

// ── 定时作业 ─────────────────────────────────────────────

func listJobs(deps *opsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		items, err := deps.Jobs.List(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "查询作业失败"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "items": items, "commands": deps.Runner.Commands()})
	}
}

func createJob(deps *opsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var j scheduler.Job
		if err := c.ShouldBindJSON(&j); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "请求体格式不正确"})
			return
		}
		if j.Concurrency == "" {
			j.Concurrency = string(scheduler.ConcurrencySkip)
		}
		id, err := deps.Jobs.Create(c.Request.Context(), j)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "id": id})
	}
}

func updateJob(deps *opsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		var j scheduler.Job
		if err := c.ShouldBindJSON(&j); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "请求体格式不正确"})
			return
		}
		j.ID = id
		if err := deps.Jobs.Update(c.Request.Context(), j); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0})
	}
}

func deleteJob(deps *opsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if err := deps.Jobs.Delete(c.Request.Context(), id); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "作业不存在"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0})
	}
}

// runJobNow 手动触发。运维最常干的事，必须有。
func runJobNow(deps *opsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		out, err := deps.Runner.RunNow(c.Request.Context(), id)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "output": out})
	}
}

func listJobRuns(deps *opsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Query("job_id"), 10, 64)
		rows, err := deps.Jobs.Runs(c.Request.Context(), id, 50)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "查询执行历史失败"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "items": rows})
	}
}

// ── 脱敏 ─────────────────────────────────────────────────

func listMaskPolicies(deps *opsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		items, err := deps.Masking.ListPolicies(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "查询策略失败"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "items": items})
	}
}

func upsertMaskPolicy(deps *opsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var p masking.Policy
		if err := c.ShouldBindJSON(&p); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "请求体格式不正确"})
			return
		}
		if err := deps.Masking.UpsertPolicy(c.Request.Context(), p); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0})
	}
}

func deleteMaskPolicy(deps *opsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := deps.Masking.DeletePolicy(c.Request.Context(), c.Param("field")); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0})
	}
}

// previewMask 预览一个值的脱敏效果。
//
// 只接受白名单字段名，且**不落审计**——预览是配置过程的一部分，
// 不是真实数据访问。真看明文才需要理由和审计。
func previewMask(deps *opsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			Field string `json:"field"`
			Value string `json:"value"`
		}
		if err := c.ShouldBindJSON(&body); err != nil || body.Field == "" {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "field 与 value 必填"})
			return
		}
		allowed := false
		for _, f := range deps.MaskField {
			if f == body.Field {
				allowed = true
				break
			}
		}
		if !allowed {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "该字段不在脱敏策略范围内"})
			return
		}
		policies, _ := deps.Masking.ListPolicies(c.Request.Context())
		var p *masking.Policy
		for i := range policies {
			if policies[i].Field == body.Field {
				p = &policies[i]
				break
			}
		}
		op, _ := gateway.OperatorFrom(c)
		res := deps.Masking.Apply(c.Request.Context(), p, body.Field, body.Value, op.Role)
		c.JSON(http.StatusOK, gin.H{"code": 0, "result": res})
	}
}

func listMaskAudit(deps *opsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		items, err := deps.Masking.RecentAudit(c.Request.Context(), 50)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "查询审计失败"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "items": items})
	}
}

// requestPlaintext 管理员申请查看明文：写审计并返回是否放行。
// 真正返回明文由调用方（列表接口）按策略决定，这里只负责留痕。
func requestPlaintext(deps *opsDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			Field  string `json:"field"`
			Reason string `json:"reason"`
			Count  int    `json:"count"`
		}
		if err := c.ShouldBindJSON(&body); err != nil || body.Field == "" || body.Reason == "" {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "field 与 reason 必填"})
			return
		}
		op, _ := gateway.OperatorFrom(c)
		if err := deps.Masking.Audit(c.Request.Context(), op.ID, op.Role, body.Field, body.Reason, body.Count); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "记录审计失败"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "granted": op.Role == "admin"})
	}
}
