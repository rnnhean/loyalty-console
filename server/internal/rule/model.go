// Package rule 承载可配置的运营规则。
//
// 和 diag 包的分工：
//   - diag 是**代码里写死**的确定性体检，改判定要发版；
//   - rule 是**运营在页面上配**的条件表达式，改条件不用发版。
//
// 两者的共同底线一样：**判定归代码，说话归模型**。
// 求值器只认闭集算子，不接受任意脚本、不做字符串拼接，
// 所以模型或运营都不可能通过规则把注入带进系统。
package rule

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Op 是算子闭集。没有 eval、没有正则回调、没有函数调用——
// 一旦允许「表达式里执行代码」，规则配置就变成了远程代码执行入口。
type Op string

const (
	OpEQ     Op = "eq"
	OpNE     Op = "ne"
	OpGT     Op = "gt"
	OpGTE    Op = "gte"
	OpLT     Op = "lt"
	OpLTE    Op = "lte"
	OpIn     Op = "in"
	OpNIn    Op = "nin"
	OpExists Op = "exists"
)

var validOps = map[Op]bool{
	OpEQ: true, OpNE: true, OpGT: true, OpGTE: true,
	OpLT: true, OpLTE: true, OpIn: true, OpNIn: true, OpExists: true,
}

// MaxDepth 限制嵌套层数。
// 不限制的话，一条构造出来的深嵌套规则能让求值递归爆栈——
// 而规则是运营通过 HTTP 提交的，属于不可信输入。
const MaxDepth = 8

// Expr 同时表达叶子条件和组合节点，用一个结构体而不是接口，
// 是因为它要能直接 JSON 往返（前端编辑器、DB 存储、模型读取都用同一份形状）。
//
//	叶子： {"field":"tier.state","op":"eq","value":1}
//	组合： {"all":[{...},{...}]}
//	      {"any":[{...}]}
//	      {"not":{...}}
type Expr struct {
	Field string          `json:"field,omitempty"`
	Op    Op              `json:"op,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`

	All []Expr `json:"all,omitempty"`
	Any []Expr `json:"any,omitempty"`
	Not *Expr  `json:"not,omitempty"`
}

// IsLeaf 判断是不是叶子条件。
func (e Expr) IsLeaf() bool { return e.Field != "" }

// Validate 校验规则形状。
// 传进来的可能是运营手填的、也可能是模型生成的，一律按不可信处理。
func (e Expr) Validate() error { return e.validate(1) }

func (e Expr) validate(depth int) error {
	if depth > MaxDepth {
		return fmt.Errorf("规则嵌套超过 %d 层", MaxDepth)
	}
	leaf := e.Field != ""
	groups := 0
	if len(e.All) > 0 {
		groups++
	}
	if len(e.Any) > 0 {
		groups++
	}
	if e.Not != nil {
		groups++
	}

	switch {
	case leaf && groups > 0:
		return errors.New("同一个节点不能既是叶子又是组合")
	case !leaf && groups == 0:
		return errors.New("节点必须至少包含 field 或 all/any/not 之一")
	case groups > 1:
		return errors.New("all / any / not 只能出现一个")
	}

	if leaf {
		if !validOps[e.Op] {
			return fmt.Errorf("不支持的算子 %q", e.Op)
		}
		if e.Op != OpExists && len(e.Value) == 0 {
			return fmt.Errorf("算子 %q 必须带 value", e.Op)
		}
		if (e.Op == OpIn || e.Op == OpNIn) && !isJSONArray(e.Value) {
			return fmt.Errorf("算子 %q 的 value 必须是数组", e.Op)
		}
		return nil
	}

	// 组合节点：空数组也要拒绝，否则 all:[] 恒真、any:[] 恒假，语义容易被误用
	if len(e.All) > 0 {
		for i := range e.All {
			if err := e.All[i].validate(depth + 1); err != nil {
				return fmt.Errorf("all[%d]: %w", i, err)
			}
		}
	}
	if len(e.Any) > 0 {
		for i := range e.Any {
			if err := e.Any[i].validate(depth + 1); err != nil {
				return fmt.Errorf("any[%d]: %w", i, err)
			}
		}
	}
	if e.Not != nil {
		if err := e.Not.validate(depth + 1); err != nil {
			return fmt.Errorf("not: %w", err)
		}
	}
	return nil
}

// Step 是一次求值的可解释步骤。
//
// 为什么要 trace：运营最常见的追问是「这条规则为什么没命中」，
// 而只回一个 false 什么都说明不了。每一步都带上「实际值 vs 期望值」，
// Agent 才能把这串东西组织成人话。
type Step struct {
	Field    string `json:"field,omitempty"`
	Op       Op     `json:"op,omitempty"`
	Expected string `json:"expected,omitempty"`
	Actual   any    `json:"actual,omitempty"`
	Present  bool   `json:"present"`
	Hit      bool   `json:"hit"`
	Note     string `json:"note,omitempty"`
}

// Result 是一条规则的求值结果。
type Result struct {
	Hit   bool   `json:"hit"`
	Steps []Step `json:"steps"`
	// MissingFields 是求值时取不到值的字段。
	// 和「值不匹配」必须分开——前者是数据缺，后者是条件不满足，
	// 混在一起会让运营以为规则写错了。
	MissingFields []string `json:"missing_fields,omitempty"`
}

// Eval 求值。data 是已经被展平的字段表，键支持点路径（"tier.state"）。
func Eval(e Expr, data map[string]any) Result {
	var res Result
	res.Hit = evalNode(e, data, &res)
	return res
}

func evalNode(e Expr, data map[string]any, res *Result) bool {
	if e.IsLeaf() {
		return evalLeaf(e, data, res)
	}
	if len(e.All) > 0 {
		// 全部求值完再短路，是为了让 trace 完整
		hit := true
		for _, sub := range e.All {
			if !evalNode(sub, data, res) {
				hit = false
			}
		}
		return hit
	}
	if len(e.Any) > 0 {
		hit := false
		for _, sub := range e.Any {
			if evalNode(sub, data, res) {
				hit = true
			}
		}
		return hit
	}
	if e.Not != nil {
		return !evalNode(*e.Not, data, res)
	}
	// 校验过的表达式不会走到这里；保底返回 false 而不是 panic，
	// 因为求值发生在请求路径上，panic 会被 Recover 中间件转成 500，
	// 而一条坏规则不该让整个接口不可用。
	res.Steps = append(res.Steps, Step{Note: "空节点，按不命中处理"})
	return false
}

func evalLeaf(e Expr, data map[string]any, res *Result) bool {
	actual, present := lookup(data, e.Field)
	step := Step{
		Field:    e.Field,
		Op:       e.Op,
		Expected: string(e.Value),
		Actual:   actual,
		Present:  present,
	}
	if !present {
		res.MissingFields = append(res.MissingFields, e.Field)
	}

	var hit bool
	switch e.Op {
	case OpExists:
		want := true
		_ = json.Unmarshal(e.Value, &want)
		hit = present == want
	case OpEQ:
		hit = present && equal(actual, decode(e.Value))
	case OpNE:
		hit = present && !equal(actual, decode(e.Value))
	case OpGT, OpGTE, OpLT, OpLTE:
		if present {
			hit = compare(actual, decode(e.Value), e.Op)
		}
	case OpIn, OpNIn:
		if present {
			hit = inSet(actual, e.Value)
			if e.Op == OpNIn {
				hit = !hit
			}
		}
	}
	step.Hit = hit
	res.Steps = append(res.Steps, step)
	return hit
}

// lookup 取值，支持点路径。
// 同时支持「整键匹配」和「逐层下钻」：数据里既有扁平键 "tier.state"，
// 也可能是嵌套 map，两种写法都要能用。
func lookup(data map[string]any, path string) (any, bool) {
	if v, ok := data[path]; ok {
		return v, true
	}
	cur := any(data)
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func decode(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

// equal 做宽松相等：JSON 解出来的数字都是 float64，
// 而数据源里可能是 int64，直接 == 会比出错误结果。
func equal(a, b any) bool {
	if af, ok := toFloat(a); ok {
		if bf, ok2 := toFloat(b); ok2 {
			return math.Abs(af-bf) < 1e-9
		}
		return false
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

func compare(a, b any, op Op) bool {
	af, ok1 := toFloat(a)
	bf, ok2 := toFloat(b)
	if !ok1 || !ok2 {
		// 非数字退化成字典序比较——字符串枚举（如 state 名称）也会用到
		as, bs := fmt.Sprint(a), fmt.Sprint(b)
		switch op {
		case OpGT:
			return as > bs
		case OpGTE:
			return as >= bs
		case OpLT:
			return as < bs
		case OpLTE:
			return as <= bs
		}
		return false
	}
	switch op {
	case OpGT:
		return af > bf
	case OpGTE:
		return af >= bf
	case OpLT:
		return af < bf
	case OpLTE:
		return af <= bf
	}
	return false
}

func inSet(v any, raw json.RawMessage) bool {
	var arr []any
	if err := json.Unmarshal(raw, &arr); err != nil {
		return false
	}
	for _, item := range arr {
		if equal(v, item) {
			return true
		}
	}
	return false
}

func isJSONArray(raw json.RawMessage) bool {
	var arr []any
	return json.Unmarshal(raw, &arr) == nil
}
