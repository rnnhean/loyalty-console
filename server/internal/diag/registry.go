package diag

import (
	"context"
	"fmt"
)

// Registry 是诊断能力的全部清单。
// agent 侧的 /api/diag/checks 直接读它，所以这里的 Name 一旦上线就不能改——
// 模型和评测集里都写死了这些字符串。
var Registry = []Check{
	{
		Name:      "orphan_rules",
		Title:     "孤儿权益规则",
		Domain:    "benefit",
		CostHint:  "low",
		TimeoutMS: 6000,
		Run:       orphanRules,
	},
	{
		Name:      "voucher_health",
		Title:     "券批次健康度",
		Domain:    "operation",
		CostHint:  "low",
		TimeoutMS: 6000,
		Run:       voucherHealth,
	},
	{
		Name:      "slot_bindings",
		Title:     "推广位绑定",
		Domain:    "operation",
		CostHint:  "low",
		TimeoutMS: 6000,
		Run:       slotBindings,
	},
	{
		Name:     "pricing_visibility",
		Title:    "定价策略可见性",
		Domain:   "catalog",
		CostHint: "low",
		Params: map[string]ParamSpec{
			// 不标必填：不带参数时扫全部产品，带了就只看该产品。
			// 这是「缩小范围」而不是「缺参数就没法跑」。
			"product": {Type: "string", Required: false},
		},
		TimeoutMS: 6000,
		Run:       pricingVisibility,
	},
	{
		Name:     "benefit_chain",
		Title:    "权益链路完整性",
		Domain:   "benefit",
		CostHint: "mid",
		Params: map[string]ParamSpec{
			// 这条必须指定产品：链路是「某产品下的层级→包→规则」，
			// 没有起点就没法查。默认取第一个产品是错的——
			// 它会让一个看起来很确定的结论建立在一次未经授权的猜测上。
			"product": {Type: "string", Required: true},
		},
		TimeoutMS: 6000,
		Run:       benefitChain,
	},
	{
		Name:      "test_data_scan",
		Title:     "测试数据残留",
		Domain:    "setting",
		CostHint:  "mid",
		TimeoutMS: 8000,
		Run:       testDataScan,
	},
	{
		Name:     "change_impact",
		Title:    "开关改动影响面",
		Domain:   "setting",
		CostHint: "low",
		Params: map[string]ParamSpec{
			"toggle_key": {Type: "string", Required: true},
		},
		TimeoutMS: 6000,
		Run:       changeImpact,
	},
	{
		// site_audit 聚合其他规则，所以它要读 Registry。
		// 但把 siteAudit 直接写在这里会造成 Go 的初始化环
		// （Registry → siteAudit → Registry），编译不过。
		// 解法：字面量里留 nil，在 init() 里补上——init 跑在包初始化完成之后，
		// 这时 Registry 已经是最终值了。Validate() 在 run() 里调用，晚于 init。
		Name:      "site_audit",
		Title:     "整体体检",
		Domain:    "all",
		CostHint:  "high",
		TimeoutMS: 20000,
	},
}

// siteAuditCheckName 是自省规则的名字。
// siteAudit 遍历 Registry 时必须跳过它自己，否则无限递归。
const siteAuditCheckName = "site_audit"

func init() {
	for i := range Registry {
		if Registry[i].Name != siteAuditCheckName {
			continue
		}
		Registry[i].Run = func(ctx context.Context, ds DataSource, _ Params) (Result, error) {
			return siteAudit(ctx, ds)
		}
	}
}

// Find 按名字取规则。名字不存在时返回 false，调用方负责转成 404，
// 不要用 panic——模型编一个规则名是常见情况，不是程序错误。
func Find(name string) (Check, bool) {
	for _, c := range Registry {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

// Validate 在启动时调用：配置错应该在启动时炸，而不是等用户点进来才发现。
func Validate() error {
	seen := make(map[string]struct{}, len(Registry))
	for _, c := range Registry {
		if c.Name == "" {
			return fmt.Errorf("diag: 存在没有名称的规则")
		}
		if _, dup := seen[c.Name]; dup {
			return fmt.Errorf("diag: 规则名重复 %q", c.Name)
		}
		seen[c.Name] = struct{}{}
		if c.Run == nil {
			return fmt.Errorf("diag: 规则 %q 没有 Run", c.Name)
		}
		if c.TimeoutMS <= 0 {
			return fmt.Errorf("diag: 规则 %q 的 TimeoutMS 必须大于 0", c.Name)
		}
		if c.CostHint != "" && c.CostHint != "low" && c.CostHint != "mid" && c.CostHint != "high" {
			return fmt.Errorf("diag: 规则 %q 的 CostHint %q 不是 low/mid/high", c.Name, c.CostHint)
		}
	}
	return nil
}
