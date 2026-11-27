package rule

import (
	"encoding/json"
	"testing"
)

func mustExpr(t *testing.T, s string) Expr {
	t.Helper()
	var e Expr
	if err := json.Unmarshal([]byte(s), &e); err != nil {
		t.Fatalf("解析表达式失败: %v", err)
	}
	return e
}

func TestEvalLeafOperators(t *testing.T) {
	data := map[string]any{"tier.state": 1, "tier.rank": 3, "rule.unit": "次"}
	cases := []struct {
		name string
		expr string
		want bool
	}{
		{"eq 数字", `{"field":"tier.state","op":"eq","value":1}`, true},
		{"eq 不匹配", `{"field":"tier.state","op":"eq","value":2}`, false},
		{"ne", `{"field":"tier.state","op":"ne","value":2}`, true},
		{"gt", `{"field":"tier.rank","op":"gt","value":2}`, true},
		{"gte 边界", `{"field":"tier.rank","op":"gte","value":3}`, true},
		{"lt 边界不成立", `{"field":"tier.rank","op":"lt","value":3}`, false},
		{"in", `{"field":"rule.unit","op":"in","value":["次","天"]}`, true},
		{"nin", `{"field":"rule.unit","op":"nin","value":["天"]}`, true},
		{"exists 真", `{"field":"tier.state","op":"exists","value":true}`, true},
		{"exists 假", `{"field":"missing.field","op":"exists","value":false}`, true},
		{"字段缺失时 eq 不命中", `{"field":"missing.field","op":"eq","value":1}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Eval(mustExpr(t, c.expr), data).Hit
			if got != c.want {
				t.Fatalf("期望 %v 实际 %v", c.want, got)
			}
		})
	}
}

func TestEvalCombinators(t *testing.T) {
	data := map[string]any{"a": 1, "b": 2, "c": 3}
	cases := []struct {
		name string
		expr string
		want bool
	}{
		{"all 全真", `{"all":[{"field":"a","op":"eq","value":1},{"field":"b","op":"eq","value":2}]}`, true},
		{"all 一假", `{"all":[{"field":"a","op":"eq","value":1},{"field":"b","op":"eq","value":9}]}`, false},
		{"any 一真", `{"any":[{"field":"a","op":"eq","value":9},{"field":"c","op":"eq","value":3}]}`, true},
		{"any 全假", `{"any":[{"field":"a","op":"eq","value":9}]}`, false},
		{"not", `{"not":{"field":"a","op":"eq","value":1}}`, false},
		{"嵌套 all+any", `{"all":[{"field":"a","op":"eq","value":1},{"any":[{"field":"b","op":"eq","value":9},{"field":"c","op":"eq","value":3}]}]}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Eval(mustExpr(t, c.expr), data).Hit
			if got != c.want {
				t.Fatalf("期望 %v 实际 %v", c.want, got)
			}
		})
	}
}

// 求值必须留下可解释的轨迹——这是 explain_rule 工具的全部价值所在。
func TestEvalTraceExplainsMiss(t *testing.T) {
	expr := mustExpr(t, `{"all":[{"field":"tier.state","op":"eq","value":1},{"field":"tier.online","op":"eq","value":1}]}`)
	res := Eval(expr, map[string]any{"tier.state": 1, "tier.online": 0})
	if res.Hit {
		t.Fatal("不应命中")
	}
	var found bool
	for _, st := range res.Steps {
		if st.Field == "tier.online" && !st.Hit {
			found = true
			if st.Actual != 0 {
				t.Fatalf("trace 里的实际值应是 0，实际 %v", st.Actual)
			}
		}
	}
	if !found {
		t.Fatal("trace 里没有记录未命中的那一步")
	}
}

// 缺少字段要单独标记，不能和「值不匹配」混为一谈。
func TestEvalMissingFieldDistinguished(t *testing.T) {
	expr := mustExpr(t, `{"field":"not.exists","op":"eq","value":1}`)
	res := Eval(expr, map[string]any{})
	if len(res.MissingFields) != 1 || res.MissingFields[0] != "not.exists" {
		t.Fatalf("应记录缺失字段，实际 %v", res.MissingFields)
	}
}

func TestValidateRejectsBadShapes(t *testing.T) {
	bad := []string{
		`{}`,                                                       // 空节点
		`{"field":"a","op":"delete","value":1}`,                     // 非法算子
		`{"field":"a","op":"in","value":1}`,                         // in 必须是数组
		`{"field":"a","op":"eq"}`,                                   // eq 必须带 value
		`{"field":"a","op":"eq","value":1,"all":[{"field":"b","op":"eq","value":1}]}`, // 叶子与组合混用
	}
	for i, s := range bad {
		if err := mustExpr(t, s).Validate(); err == nil {
			t.Fatalf("第 %d 个非法表达式没有被拒绝: %s", i, s)
		}
	}
	good := []string{
		`{"field":"a","op":"eq","value":1}`,
		`{"all":[{"field":"a","op":"eq","value":1}]}`,
		`{"not":{"field":"a","op":"exists","value":true}}`,
	}
	for i, s := range good {
		if err := mustExpr(t, s).Validate(); err != nil {
			t.Fatalf("第 %d 个合法表达式被拒绝: %v", i, err)
		}
	}
}

// 深嵌套必须被拒绝：规则来自 HTTP，属于不可信输入。
func TestValidateDepthLimit(t *testing.T) {
	expr := `{"field":"a","op":"eq","value":1}`
	for i := 0; i < MaxDepth+2; i++ {
		expr = `{"not":` + expr + `}`
	}
	if err := mustExpr(t, expr).Validate(); err == nil {
		t.Fatal("超过深度上限的表达式应被拒绝")
	}
}

func TestSummary(t *testing.T) {
	res := Eval(mustExpr(t, `{"field":"tier.online","op":"eq","value":1}`), map[string]any{"tier.online": 0})
	if got := Summary(res); got == "命中" || got == "" {
		t.Fatalf("未命中时摘要应说明原因，实际 %q", got)
	}
}
