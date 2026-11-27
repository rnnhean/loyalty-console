package rule

import (
	"encoding/json"
	"strings"
	"testing"
)

// ValidateForSave 是保存前的第一道闸门：运营提交的垃圾规则必须在这里被拦住，
// 而不是等到求值时才炸。每个分支都要有对应用例。
func TestValidateForSave(t *testing.T) {
	ok := Rule{
		Name:   "正常规则",
		Domain: "operation",
		Action: "打标签",
		Expr:   mustExpr(t, `{"field":"a","op":"eq","value":1}`),
	}
	if err := ValidateForSave(ok); err != nil {
		t.Fatalf("合法规则被拒绝: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*Rule)
		want   string
	}{
		{"空名称", func(r *Rule) { r.Name = "" }, "规则名不能为空"},
		{"名称超长", func(r *Rule) { r.Name = strings.Repeat("规", 65) }, "不能超过 64"},
		{"空域", func(r *Rule) { r.Domain = "" }, "所属域"},
		{"空动作", func(r *Rule) { r.Action = "" }, "动作"},
		{"动作超长", func(r *Rule) { r.Action = strings.Repeat("a", 201) }, "不能超过 200"},
		{"空表达式", func(r *Rule) { r.Expr = Expr{} }, "至少包含"},
		{"非法算子", func(r *Rule) {
			var e Expr
			_ = json.Unmarshal([]byte(`{"field":"a","op":"drop_table","value":1}`), &e)
			r.Expr = e
		}, "不支持的算子"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := ok
			c.mutate(&r)
			err := ValidateForSave(r)
			if err == nil {
				t.Fatalf("应被拒绝")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误信息应包含 %q，实际 %q", c.want, err.Error())
			}
		})
	}
}

// 空组合节点必须被拒绝：all:[] 恒真、any:[] 恒假，语义太容易被误用。
func TestValidateRejectsEmptyGroups(t *testing.T) {
	for _, raw := range []string{`{"all":[]}`, `{"any":[]}`} {
		var e Expr
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			t.Fatal(err)
		}
		if err := e.Validate(); err == nil {
			t.Fatalf("%s 应被拒绝", raw)
		}
	}
}

// 内置样例必须能命中种子规则，否则运营点开「试运行」看到的是一片「未命中」，
// 会以为功能坏了。（实测踩过：全 0 样例导致模型连调三次 simulate_rules）
func TestSampleDataHitsSeedRules(t *testing.T) {
	data := SampleData()
	rules := []string{
		`{"all":[{"field":"member.login_days","op":"gte","value":30},{"field":"tier.state","op":"eq","value":1}]}`,
		`{"all":[{"field":"tier.rank","op":"gte","value":3},{"field":"tier.online","op":"eq","value":0}]}`,
	}
	for i, raw := range rules {
		if !Eval(mustExpr(t, raw), data).Hit {
			t.Fatalf("第 %d 条种子规则用内置样例应命中", i+1)
		}
	}
}

// 数值比较要兼容各种来源类型：JSON 解出来是 float64，
// 数据源里可能是 int64 / int，字符串形式的数字也要能比。
func TestCompareAcrossTypes(t *testing.T) {
	cases := []struct {
		a, b any
		op   Op
		want bool
	}{
		{float64(30), 30, OpGTE, true},
		{int64(45), float64(30), OpGT, true},
		{int(5), int64(10), OpLT, true},
		{"45", 30, OpGT, true},   // 字符串数字
		{"45", "30", OpGT, true}, // 两边都是字符串
		{"b", "a", OpGT, true},   // 纯字符串走字典序
		{"on", "off", OpGTE, true},
		{true, 1, OpEQ, false}, // 布尔和数字不相等
	}
	for i, c := range cases {
		if got := compare(c.a, c.b, c.op); got != c.want {
			t.Fatalf("第 %d 组 compare(%v,%v,%s) 期望 %v 实际 %v", i, c.a, c.b, c.op, c.want, got)
		}
	}
}

func TestToFloatRejectsNonNumeric(t *testing.T) {
	for _, v := range []any{"abc", true, nil, struct{}{}} {
		if _, ok := toFloat(v); ok {
			t.Fatalf("%v 不应被当成数字", v)
		}
	}
	if f, ok := toFloat(json.Number("3.5")); !ok || f != 3.5 {
		t.Fatal("json.Number 应能转成数字")
	}
}

// Summary 要能在四种情形下都给出可读原因，不能只回「未命中」。
func TestSummaryAllBranches(t *testing.T) {
	hit := Eval(mustExpr(t, `{"field":"a","op":"eq","value":1}`), map[string]any{"a": 1})
	if Summary(hit) != "命中" {
		t.Fatalf("命中时摘要应为「命中」，实际 %q", Summary(hit))
	}

	missing := Eval(mustExpr(t, `{"field":"nope","op":"eq","value":1}`), map[string]any{})
	if !strings.Contains(Summary(missing), "缺少字段") {
		t.Fatalf("缺字段时应说明缺什么，实际 %q", Summary(missing))
	}

	mismatch := Eval(mustExpr(t, `{"field":"a","op":"eq","value":9}`), map[string]any{"a": 1})
	s := Summary(mismatch)
	if !strings.Contains(s, "a") || !strings.Contains(s, "实际") {
		t.Fatalf("不匹配时应给出字段、实际值与期望，实际 %q", s)
	}
}

// 点路径取值：既支持扁平键 "tier.state"，也要能从嵌套 map 里钻取。
func TestLookupBothShapes(t *testing.T) {
	flat := map[string]any{"tier.state": 1}
	if v, ok := lookup(flat, "tier.state"); !ok || v != 1 {
		t.Fatal("扁平键应能取到")
	}
	nested := map[string]any{"tier": map[string]any{"state": 1}}
	if v, ok := lookup(nested, "tier.state"); !ok || v != 1 {
		t.Fatal("嵌套 map 应能钻取")
	}
	if _, ok := lookup(nested, "tier.nope"); ok {
		t.Fatal("不存在的路径应返回 false")
	}
}
