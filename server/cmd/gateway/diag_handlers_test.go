package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/example/loyalty-console/server/internal/diag"
)

// handler 测试用 httptest + FakeSource：不打数据库、不启任何容器。
func newDiagRouter(ds diag.DataSource) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/diag/checks", listChecks())
	r.GET("/api/diag/run/:name", runCheck(ds))
	r.POST("/api/diag/batch", runBatch(ds))
	return r
}

func doReq(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// 规则目录必须等于 Registry 的实际内容——agent 靠它决定「有哪些工具」，
// 少一条就是工具不见，多一条就是幻觉。
func TestDiagChecks(t *testing.T) {
	r := newDiagRouter(&diag.FakeSource{})
	w := doReq(r, http.MethodGet, "/api/diag/checks", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		Code  int `json:"code"`
		Total int `json:"total"`
		Items []struct {
			Name      string `json:"name"`
			Title     string `json:"title"`
			CostHint  string `json:"cost_hint"`
			TimeoutMS int    `json:"timeout_ms"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != len(diag.Registry) || len(resp.Items) != len(diag.Registry) {
		t.Fatalf("total = %d / items = %d, want %d", resp.Total, len(resp.Items), len(diag.Registry))
	}
	for _, it := range resp.Items {
		if it.Name == "" || it.TimeoutMS <= 0 {
			t.Errorf("规则条目不完整: %+v", it)
		}
		if it.CostHint != "low" && it.CostHint != "mid" && it.CostHint != "high" {
			t.Errorf("cost_hint 非法: %q", it.CostHint)
		}
	}
}

func TestDiagRun(t *testing.T) {
	ds := &diag.FakeSource{
		Rules: []diag.EntitlementRule{
			{UID: 101, Title: "生日礼", GroupUID: 0, LifecycleState: 1},
			{UID: 102, Title: "专属客服", GroupUID: 9, LifecycleState: 1},
		},
	}
	r := newDiagRouter(ds)
	w := doReq(r, http.MethodGet, "/api/diag/run/orphan_rules", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			Check    string `json:"check"`
			Verdict  string `json:"verdict"`
			Findings []struct {
				ID string `json:"id"`
			} `json:"findings"`
			Coverage struct {
				Checked int `json:"checked"`
			} `json:"coverage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.Verdict != "issue" {
		t.Errorf("verdict = %q, want issue", resp.Data.Verdict)
	}
	if len(resp.Data.Findings) != 1 || resp.Data.Findings[0].ID != "rule:101" {
		t.Errorf("findings = %+v", resp.Data.Findings)
	}
	if resp.Data.Coverage.Checked != 2 {
		t.Errorf("checked = %d, want 2", resp.Data.Coverage.Checked)
	}
}

// 模型编一个不存在的规则名是常态。这必须是干净的 400，不能是 500，
// 也不能 panic——500 会触发 Sentry 告警，把「模型幻觉」变成「运维事故」。
func TestDiagRunUnknownCheck(t *testing.T) {
	r := newDiagRouter(&diag.FakeSource{})
	w := doReq(r, http.MethodGet, "/api/diag/run/no_such_check", "")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"code":400`) {
		t.Errorf("body = %s", w.Body.String())
	}
}

// 数据源报错时不能返回 200 + 一个空结论。
// 那是把「库挂了」渲染成「没问题」，运维永远查不出来。
func TestDiagRunDataSourceError(t *testing.T) {
	r := newDiagRouter(&diag.FakeSource{Err: errors.New("db down")})
	w := doReq(r, http.MethodGet, "/api/diag/run/orphan_rules", "")

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	// 内部错误不能透出去：公网部署下，SQL 报错是最好的侦察情报。
	if strings.Contains(w.Body.String(), "db down") {
		t.Errorf("内部错误泄漏到响应: %s", w.Body.String())
	}
}

func TestDiagBatch(t *testing.T) {
	ds := &diag.FakeSource{
		Rules: []diag.EntitlementRule{
			{UID: 101, Title: "生日礼", GroupUID: 0, LifecycleState: 1},
		},
		Slots: []diag.PromotionSlot{
			{UID: 7001, Title: "首页 Banner", Product: "文档云 Pro", LifecycleState: 1},
		},
	}
	r := newDiagRouter(ds)
	w := doReq(r, http.MethodPost, "/api/diag/batch", `{"checks":["orphan_rules","slot_bindings"]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Partial bool `json:"partial"`
			Results []struct {
				Check   string `json:"check"`
				Verdict string `json:"verdict"`
			} `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.Partial {
		t.Error("全部成功时 partial 必须是 false")
	}
	if len(resp.Data.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(resp.Data.Results))
	}
	if resp.Data.Results[0].Verdict != "issue" || resp.Data.Results[1].Verdict != "ok" {
		t.Errorf("verdicts = %q / %q", resp.Data.Results[0].Verdict, resp.Data.Results[1].Verdict)
	}
}

// partial 是这个接口的核心语义：单条失败不算整体失败。
// 失败项的 verdict 必须为空、error 必须非空，否则前端会把「没查到」当「没问题」。
func TestDiagBatchPartial(t *testing.T) {
	r := newDiagRouter(&diag.FakeSource{})
	w := doReq(r, http.MethodPost, "/api/diag/batch", `{"checks":["no_such_check"]}`)

	var resp struct {
		Data struct {
			Partial bool `json:"partial"`
			Results []struct {
				Check   string `json:"check"`
				Verdict string `json:"verdict"`
				Error   string `json:"error"`
			} `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Data.Partial {
		t.Error("有失败项时 partial 必须是 true")
	}
	if len(resp.Data.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(resp.Data.Results))
	}
	got := resp.Data.Results[0]
	if got.Verdict != "" {
		t.Errorf("失败项的 verdict 必须为空（序列化成 null），got %q", got.Verdict)
	}
	if got.Error == "" {
		t.Error("失败项必须带 error 标签")
	}
}

func TestDiagBatchValidation(t *testing.T) {
	r := newDiagRouter(&diag.FakeSource{})

	cases := []struct {
		name string
		body string
	}{
		{name: "空 checks", body: `{"checks":[]}`},
		{name: "超过上限", body: `{"checks":["a","b","c","d","e","f","g","h","i"]}`},
		{name: "格式错误", body: `not json`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := doReq(r, http.MethodPost, "/api/diag/batch", c.body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
		})
	}
}

// 带参数的规则：查询串里的 product 要能传到规则里。
func TestDiagRunWithParams(t *testing.T) {
	ds := &diag.FakeSource{
		Policies: []diag.PricingPolicy{
			{UID: 11, Title: "文档云过期价", Product: "文档云 Pro", PublishState: 4, LifecycleState: 1,
				HasTo: true, EffectiveTo: mustTime("2026-11-05T00:00:00Z")},
			{UID: 12, Title: "超级会员过期价", Product: "超级会员", PublishState: 4, LifecycleState: 1,
				HasTo: true, EffectiveTo: mustTime("2026-11-05T00:00:00Z")},
		},
	}
	r := newDiagRouter(ds)
	w := doReq(r, http.MethodGet, "/api/diag/run/pricing_visibility?product=%E6%96%87%E6%A1%A3%E4%BA%91%20Pro", "")

	var resp struct {
		Data struct {
			Findings []struct {
				ID string `json:"id"`
			} `json:"findings"`
			Coverage struct {
				Checked int `json:"checked"`
			} `json:"coverage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 只应命中该产品那一条：参数没传进去的话会同时命中两条，checked 会是 2。
	if resp.Data.Coverage.Checked != 1 {
		t.Errorf("checked = %d, want 1（product 参数没生效）", resp.Data.Coverage.Checked)
	}
	if len(resp.Data.Findings) != 1 || resp.Data.Findings[0].ID != "policy:11" {
		t.Errorf("findings = %+v", resp.Data.Findings)
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
