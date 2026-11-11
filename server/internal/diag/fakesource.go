package diag

import (
	"context"
	"time"
)

// FakeSource 是 DataSource 的内存实现，给本包与 cmd/gateway 的单测共用。
//
// 放在非 _test.go 文件里是有意的：cmd/gateway 的 handler 测试也需要它，
// 而 _test.go 里的类型导不出包。代价是这个类型会进生产二进制——
// 一个几十行的结构体，换取「handler 测试不打数据库」，值。
type FakeSource struct {
	Rules    []EntitlementRule
	Batches  []VoucherBatch
	Slots    []PromotionSlot
	Policies []PricingPolicy
	Tiers    []Tier
	Groups   []Group
	Tasks    []TaskThreshold
	Entities []NamedEntity
	Toggles  []FeatureToggle
	// Now_ 为零值时固定用 2026-11-11 12:00 UTC，让时间相关用例可复现。
	Now_ time.Time
	Err  error
}

var _ DataSource = (*FakeSource)(nil)

func (f *FakeSource) ListActiveRules(_ context.Context, limit int) ([]EntitlementRule, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return clip(f.Rules, limit), nil
}

func (f *FakeSource) ListVoucherBatches(_ context.Context, limit int) ([]VoucherBatch, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return clip(f.Batches, limit), nil
}

func (f *FakeSource) ListActiveSlots(_ context.Context, limit int) ([]PromotionSlot, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return clip(f.Slots, limit), nil
}

func (f *FakeSource) ListPublishedPolicies(_ context.Context, limit int) ([]PricingPolicy, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return clip(f.Policies, limit), nil
}

func (f *FakeSource) Now() time.Time {
	if f.Now_.IsZero() {
		return time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	}
	return f.Now_
}

func (f *FakeSource) ListTiers(_ context.Context, product string, limit int) ([]Tier, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return clip(filterByProduct(f.Tiers, product, func(t Tier) string { return t.Product }), limit), nil
}

func (f *FakeSource) ListGroups(_ context.Context, product string, limit int) ([]Group, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return clip(filterByProduct(f.Groups, product, func(g Group) string { return g.Product }), limit), nil
}

func (f *FakeSource) ListTasks(_ context.Context, product string, limit int) ([]TaskThreshold, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return clip(filterByProduct(f.Tasks, product, func(t TaskThreshold) string { return t.Product }), limit), nil
}

func (f *FakeSource) ListNamedEntities(_ context.Context, limit int) ([]NamedEntity, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return clip(f.Entities, limit), nil
}

func (f *FakeSource) ListToggles(_ context.Context, limit int) ([]FeatureToggle, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return clip(f.Toggles, limit), nil
}

// filterByProduct 复刻 SQL 里 `product_title = ?` 的语义。
// product 为空表示不过滤——这是数据源的行为，不是规则的判定。
func filterByProduct[T any](in []T, product string, get func(T) string) []T {
	if product == "" {
		return in
	}
	out := make([]T, 0, len(in))
	for _, v := range in {
		if get(v) == product {
			out = append(out, v)
		}
	}
	return out
}

// clip 模拟 SQL 的 LIMIT。真实实现是数据库截断，假实现得把这件事做出来，
// 否则「取 limit+1 条判断截断」的逻辑在测试里根本走不到。
func clip[T any](in []T, limit int) []T {
	if limit > 0 && len(in) > limit {
		return in[:limit]
	}
	return in
}
