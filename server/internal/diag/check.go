// Package diag 承载运营诊断的确定性判定。
//
// 原则只有一条：判定归代码，说话归模型。
// 每个 Check.Run 都是纯函数——给定数据源就返回结论，不读配置、不发网络请求、
// 不依赖 *gin.Context。模型只负责挑哪几条规则跑，以及把结论组织成人话。
package diag

import (
	"context"
	"time"
)

// Verdict 是三值枚举，没有第四种「不知道」。
// 查得到就是 ok / issue；查不到就是 inconclusive，且必须说明缺了什么。
type Verdict string

const (
	VerdictOK           Verdict = "ok"
	VerdictIssue        Verdict = "issue"
	VerdictInconclusive Verdict = "inconclusive"
)

type Severity string

const (
	SeverityHigh Severity = "high"
	SeverityMid  Severity = "mid"
	SeverityLow  Severity = "low"
)

// Finding 是一条可核验的问题。
// Evidence 里的字段必须来自真实查询结果——模型不许往里加东西，
// 前端也不许拿它做展示之外的用途。
type Finding struct {
	ID       string         `json:"id"`
	Severity Severity       `json:"severity"`
	Title    string         `json:"title"`
	Detail   string         `json:"detail"`
	Evidence map[string]any `json:"evidence"`
	// Link 是给人点的，不是给模型读的：必须能直接落到具体行
	Link string `json:"link"`
}

// Coverage 说明这次到底查了多少、跳过多少、缺了什么。
// Checked 必须等于实际扫过的行数，不能是估的——
// inconclusive 的说服力全靠它。
type Coverage struct {
	Checked int      `json:"checked"`
	Skipped int      `json:"skipped"`
	Missing []string `json:"missing"`
}

type Result struct {
	Verdict    Verdict   `json:"verdict"`
	Confidence float64   `json:"confidence"`
	Findings   []Finding `json:"findings"`
	Coverage   Coverage  `json:"coverage"`
}

type ParamSpec struct {
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

type Params map[string]any

// EntitlementRule 对应 bft_entitlement_rule 的诊断所需字段。
// 只带判定要用的列——多带一列，就多一份「模型拿它编故事」的机会。
type EntitlementRule struct {
	UID            int64
	Title          string
	GroupUID       int64
	BenefitKind    int8
	LifecycleState int8
}

// VoucherBatch 对应 ops_voucher_batch。
// batch_state 与 lifecycle_state 同为 0 草稿 / 1 启用 / 2 停用 / 3 已删除，
// 但两者独立维护：批次可以被停用而记录本身仍是启用态（反之亦然）。
type VoucherBatch struct {
	UID            int64
	Title          string
	CampaignUID    int64
	FaceAmount     float64
	QuotaTotal     int
	QuotaIssued    int
	State          int8
	LifecycleState int8
}

// PromotionSlot 对应 ops_promotion_slot。
type PromotionSlot struct {
	UID            int64
	Title          string
	Product        string
	LifecycleState int8
}

// PricingPolicy 对应 cat_pricing_policy。
// PublishState 只有 0 未发布 / 4 已发布两种有意义的值（不是 0/1）——
// 按「1 = 已发布」写会把所有已发布策略判成未发布。
type PricingPolicy struct {
	UID            int64
	Title          string
	Product        string
	PublishState   int8
	LifecycleState int8
	HasFrom        bool
	HasTo          bool
	EffectiveFrom  time.Time
	EffectiveTo    time.Time
}

// Tier 对应 bft_tier_profile：会员层级。
type Tier struct {
	UID            int64
	Label          string
	Product        string
	LifecycleState int8
}

// Group 对应 bft_entitlement_group：权益包。tier_uid = 0 表示不属于任何层级。
type Group struct {
	UID            int64
	Title          string
	Product        string
	TierUID        int64
	LifecycleState int8
}

// TaskThreshold 对应 bft_task_threshold。
// RewardGroupUID 指向权益包——指向不存在的包是典型的断链。
type TaskThreshold struct {
	UID            int64
	Title          string
	Product        string
	RewardGroupUID int64
	LifecycleState int8
}

// FeatureToggle 对应 cfg_feature_toggle。
type FeatureToggle struct {
	UID     int64
	Key     string
	Product string
	Value   string
}

// NamedEntity 是「有名字的业务对象」的统称，用于测试数据扫描。
// 只带扫描要用的列：表名、主键、名称、是否启用。
type NamedEntity struct {
	Table   string // 逻辑表名，用作 finding id 前缀
	UID     int64
	Name    string
	Enabled bool
}

// DataSource 是规则取数的唯一入口。
//
// 故意不把「group_uid = 0」写进查询条件：那是判定，属于规则函数。
// 数据源只负责交出「当前启用的规则」，判断哪条是孤儿由 orphanRules 自己来。
// 这样单测能用内存假实现覆盖判定逻辑，不需要数据库。
type DataSource interface {
	ListActiveRules(ctx context.Context, limit int) ([]EntitlementRule, error)
	ListVoucherBatches(ctx context.Context, limit int) ([]VoucherBatch, error)
	ListActiveSlots(ctx context.Context, limit int) ([]PromotionSlot, error)
	ListPublishedPolicies(ctx context.Context, limit int) ([]PricingPolicy, error)

	// 以下三个按产品过滤；product 为空表示不过滤（只看启用中的行）。
	ListTiers(ctx context.Context, product string, limit int) ([]Tier, error)
	ListGroups(ctx context.Context, product string, limit int) ([]Group, error)
	ListTasks(ctx context.Context, product string, limit int) ([]TaskThreshold, error)

	// ListNamedEntities 跨表扫描「像测试数据」的对象。
	ListNamedEntities(ctx context.Context, limit int) ([]NamedEntity, error)
	ListToggles(ctx context.Context, limit int) ([]FeatureToggle, error)

	// Now 由数据源提供而不是规则里调 time.Now()：
	// 规则必须是纯函数，否则「已过期」这类判定没法在单测里固定下来——
	// 测试会在某个整点前后随机失败，而且没人看得懂为什么。
	Now() time.Time
}

type Check struct {
	Name      string
	Title     string
	Domain    string
	Params    map[string]ParamSpec
	CostHint  string // low | mid | high
	TimeoutMS int
	// Run 的 error 只表示「没跑成」（数据源报错、超时）。
	// 业务上的「有问题」是 Findings 非空，不是 error。
	Run func(ctx context.Context, ds DataSource, p Params) (Result, error)
}
