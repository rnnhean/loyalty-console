package diag

import "fmt"

// scanLimit 是单条规则一次最多扫多少行。
//
// 选 5000 的理由：这是「诊断」不是「导出」，要的是「够不够发现问题」，
// 不是「扫完整个库」。运营的诊断请求有 45 秒总预算，一条规则吃掉 6 秒，
// 超过这个量级说明该走后台任务，而不是在对话里等。
const scanLimit = 5000

// limitProbe 取 limit+1 条用来判断是否被截断。
//
// 不这么做的话，规则数刚好超过上限时 checked 等于上限，
// 报告看起来是「全查完了、没问题」——而实际上后面那些根本没扫。
// 这种「假的完整性」比 inconclusive 危险得多。
const limitProbe = 1

// finish 统一收口一条规则的结论。
//
// 把「有 finding 就是 issue、没有就是 ok」和「截断必须降级」两件事
// 放在一个函数里，是为了让每条规则都逃不掉截断处理——
// 靠每个规则作者自己记得写，迟早会漏一条，而漏掉的那条会在
// 数据量涨上去之后突然开始撒谎。
func finish(res *Result, truncated bool, limit int) {
	if truncated {
		// Skipped 不填具体数字：不知道总共多少条，编一个数比不填更糟。
		// 缺口写在 Missing 里，由人读。
		res.Coverage.Missing = append(res.Coverage.Missing,
			fmt.Sprintf("数据量超过 %d 条，本次只扫描了前 %d 条，其余未检查", limit, limit))
		if res.Confidence > 0.5 {
			res.Confidence = 0.5
		}
		// 只扫了一部分却说「没问题」，等于给出了错误的安心感——
		// 这时即使一条 finding 都没有，也不能报 ok。
		if len(res.Findings) == 0 {
			res.Verdict = VerdictInconclusive
			return
		}
	}

	if len(res.Findings) > 0 {
		res.Verdict = VerdictIssue
	} else {
		res.Verdict = VerdictOK
	}
}

// newResult 造一个干净的 Result。
// Missing 必须初始化成空切片而不是 nil：nil 序列化成 JSON 是 null，
// 前端判 `Array.isArray` 会失败，而 `null` 和 `[]` 在这里语义不同。
func newResult(checked int) Result {
	return Result{
		Confidence: 1,
		Findings:   make([]Finding, 0, 4),
		Coverage:   Coverage{Checked: checked, Missing: []string{}},
	}
}
