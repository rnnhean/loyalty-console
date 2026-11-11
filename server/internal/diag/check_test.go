package diag

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 单测统一用 diag.FakeSource（fakesource.go），handler 测试也复用它。

func findingIDs(fs []Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.ID)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ── orphan_rules ─────────────────────────────────────────────

func TestOrphanRules(t *testing.T) {
	cases := []struct {
		name        string
		ds          DataSource
		wantVerdict Verdict
		wantIDs     []string
		wantChecked int
	}{
		{
			name: "命中：启用但 group_uid 为 0",
			ds: &FakeSource{Rules: []EntitlementRule{
				{UID: 101, Title: "生日礼", GroupUID: 0, LifecycleState: 1},
				{UID: 102, Title: "专属客服", GroupUID: 9, LifecycleState: 1},
			}},
			wantVerdict: VerdictIssue,
			wantIDs:     []string{"rule:101"},
			wantChecked: 2,
		},
		{
			name: "未命中：全部都挂上了",
			ds: &FakeSource{Rules: []EntitlementRule{
				{UID: 201, Title: "生日礼", GroupUID: 3, LifecycleState: 1},
			}},
			wantVerdict: VerdictOK,
			wantChecked: 1,
		},
		{
			// 空表是「确实没有」，不是「没查到」——必须是 ok + checked:0。
			// 如果这里返回 inconclusive，会让运营以为系统坏了；
			// 反过来，如果查不到时返回 ok，会把故障报成「一切正常」。
			name:        "空表：ok 且 checked 为 0",
			ds:          &FakeSource{Rules: []EntitlementRule{}},
			wantVerdict: VerdictOK,
			wantChecked: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			check, ok := Find("orphan_rules")
			if !ok {
				t.Fatal("orphan_rules 未注册")
			}
			res, err := check.Run(context.Background(), c.ds, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Verdict != c.wantVerdict {
				t.Errorf("verdict = %q, want %q", res.Verdict, c.wantVerdict)
			}
			if res.Coverage.Checked != c.wantChecked {
				t.Errorf("checked = %d, want %d", res.Coverage.Checked, c.wantChecked)
			}
			if got := findingIDs(res.Findings); !equalStrings(got, c.wantIDs) {
				t.Errorf("finding ids = %v, want %v", got, c.wantIDs)
			}
		})
	}
}

// ── voucher_health ───────────────────────────────────────────

func TestVoucherHealth(t *testing.T) {
	cases := []struct {
		name        string
		ds          DataSource
		wantVerdict Verdict
		wantIDs     []string
		wantChecked int
	}{
		{
			name: "命中：已删除但发过券",
			ds: &FakeSource{Batches: []VoucherBatch{
				{UID: 5004, Title: "员工内购福利券", QuotaTotal: 300, QuotaIssued: 300, State: 3, LifecycleState: 3},
			}},
			wantVerdict: VerdictIssue,
			wantIDs:     []string{"batch:5004"},
			wantChecked: 1,
		},
		{
			name: "命中：启用但配额为 0",
			ds: &FakeSource{Batches: []VoucherBatch{
				{UID: 5006, Title: "空批次", QuotaTotal: 0, QuotaIssued: 0, State: 1, LifecycleState: 1},
			}},
			wantVerdict: VerdictIssue,
			wantIDs:     []string{"batch:5006"},
			wantChecked: 1,
		},
		{
			name: "命中：超发",
			ds: &FakeSource{Batches: []VoucherBatch{
				{UID: 5007, Title: "超发批次", QuotaTotal: 100, QuotaIssued: 130, State: 2, LifecycleState: 1},
			}},
			wantVerdict: VerdictIssue,
			wantIDs:     []string{"batch:5007"},
			wantChecked: 1,
		},
		{
			// 草稿批次一条没发、已停用批次发了一部分——都是正常节奏，不该报。
			name: "未命中：草稿与停用都是合法状态",
			ds: &FakeSource{Batches: []VoucherBatch{
				{UID: 5001, QuotaTotal: 5000, QuotaIssued: 1832, State: 2, LifecycleState: 1},
				{UID: 5003, QuotaTotal: 800, QuotaIssued: 96, State: 1, LifecycleState: 1},
				{UID: 5005, QuotaTotal: 10, QuotaIssued: 0, State: 0, LifecycleState: 0},
			}},
			wantVerdict: VerdictOK,
			wantChecked: 3,
		},
		{
			name:        "空表：ok 且 checked 为 0",
			ds:          &FakeSource{Batches: []VoucherBatch{}},
			wantVerdict: VerdictOK,
			wantChecked: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			check, _ := Find("voucher_health")
			res, err := check.Run(context.Background(), c.ds, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Verdict != c.wantVerdict {
				t.Errorf("verdict = %q, want %q", res.Verdict, c.wantVerdict)
			}
			if res.Coverage.Checked != c.wantChecked {
				t.Errorf("checked = %d, want %d", res.Coverage.Checked, c.wantChecked)
			}
			if got := findingIDs(res.Findings); !equalStrings(got, c.wantIDs) {
				t.Errorf("finding ids = %v, want %v", got, c.wantIDs)
			}
		})
	}
}

// ── slot_bindings ────────────────────────────────────────────

func TestSlotBindings(t *testing.T) {
	cases := []struct {
		name        string
		ds          DataSource
		wantVerdict Verdict
		wantIDs     []string
		wantChecked int
	}{
		{
			name: "命中：启用但没绑产品",
			ds: &FakeSource{Slots: []PromotionSlot{
				{UID: 7004, Title: "首页浮层", Product: "", LifecycleState: 1},
				{UID: 7001, Title: "首页首屏 Banner", Product: "文档云 Pro", LifecycleState: 1},
			}},
			wantVerdict: VerdictIssue,
			wantIDs:     []string{"slot:7004"},
			wantChecked: 2,
		},
		{
			name: "未命中：全部绑了产品",
			ds: &FakeSource{Slots: []PromotionSlot{
				{UID: 7002, Title: "收银台弹窗", Product: "文档云 Pro", LifecycleState: 1},
			}},
			wantVerdict: VerdictOK,
			wantChecked: 1,
		},
		{
			name:        "空表：ok 且 checked 为 0",
			ds:          &FakeSource{Slots: []PromotionSlot{}},
			wantVerdict: VerdictOK,
			wantChecked: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			check, _ := Find("slot_bindings")
			res, err := check.Run(context.Background(), c.ds, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Verdict != c.wantVerdict {
				t.Errorf("verdict = %q, want %q", res.Verdict, c.wantVerdict)
			}
			if res.Coverage.Checked != c.wantChecked {
				t.Errorf("checked = %d, want %d", res.Coverage.Checked, c.wantChecked)
			}
			if got := findingIDs(res.Findings); !equalStrings(got, c.wantIDs) {
				t.Errorf("finding ids = %v, want %v", got, c.wantIDs)
			}
		})
	}
}

// ── pricing_visibility ───────────────────────────────────────

func TestPricingVisibility(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name        string
		ds          DataSource
		params      Params
		wantVerdict Verdict
		wantIDs     []string
		wantChecked int
	}{
		{
			name: "命中：已发布但已过期",
			ds: &FakeSource{Now_: now, Policies: []PricingPolicy{
				{UID: 11, Title: "双十一价", Product: "文档云 Pro", PublishState: 4, LifecycleState: 1,
					HasTo: true, EffectiveTo: time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)},
			}},
			wantVerdict: VerdictIssue,
			wantIDs:     []string{"policy:11"},
			wantChecked: 1,
		},
		{
			name: "未命中：时间窗覆盖当下",
			ds: &FakeSource{Now_: now, Policies: []PricingPolicy{
				{UID: 12, Title: "现行价", Product: "文档云 Pro", PublishState: 4, LifecycleState: 1,
					HasFrom: true, EffectiveFrom: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
					HasTo: true, EffectiveTo: time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)},
			}},
			wantVerdict: VerdictOK,
			wantChecked: 1,
		},
		{
			// 「永久有效」是合法配置，不能报。
			name: "未命中：两端都不限时间",
			ds: &FakeSource{Now_: now, Policies: []PricingPolicy{
				{UID: 13, Title: "长期价", Product: "超级会员", PublishState: 4, LifecycleState: 1},
			}},
			wantVerdict: VerdictOK,
			wantChecked: 1,
		},
		{
			// 关键回归：publish_state 是 4 而不是 1。
			// 如果实现里写成 == 1，这条会被判成「未命中」，规则永远报不出东西。
			name: "未命中：未发布的过期策略不归这条规则管",
			ds: &FakeSource{Now_: now, Policies: []PricingPolicy{
				{UID: 14, Title: "草稿价", Product: "文档云 Pro", PublishState: 0, LifecycleState: 1,
					HasTo: true, EffectiveTo: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
			}},
			wantVerdict: VerdictOK,
			wantChecked: 1,
		},
		{
			name: "带产品参数：只扫该产品",
			ds: &FakeSource{Now_: now, Policies: []PricingPolicy{
				{UID: 15, Title: "文档云过期价", Product: "文档云 Pro", PublishState: 4, LifecycleState: 1,
					HasTo: true, EffectiveTo: time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)},
				{UID: 16, Title: "其他产品", Product: "超级会员", PublishState: 4, LifecycleState: 1,
					HasTo: true, EffectiveTo: time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)},
			}},
			params:      Params{"product": "文档云 Pro"},
			wantVerdict: VerdictIssue,
			wantIDs:     []string{"policy:15"},
			wantChecked: 1,
		},
		{
			// 产品名写错时，数据上和「该产品确实没配策略」完全一样。
			// 这时候报 ok 等于替使用者做了一个它无权做的判断。
			name: "产品不存在：inconclusive 而不是 ok",
			ds: &FakeSource{Now_: now, Policies: []PricingPolicy{
				{UID: 17, Title: "现行价", Product: "文档云 Pro", PublishState: 4, LifecycleState: 1},
			}},
			params:      Params{"product": "不存在的产品"},
			wantVerdict: VerdictInconclusive,
			wantChecked: 0,
		},
		{
			name:        "空表且未指定产品：ok 且 checked 为 0",
			ds:          &FakeSource{Now_: now, Policies: []PricingPolicy{}},
			wantVerdict: VerdictOK,
			wantChecked: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			check, _ := Find("pricing_visibility")
			res, err := check.Run(context.Background(), c.ds, c.params)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Verdict != c.wantVerdict {
				t.Errorf("verdict = %q, want %q", res.Verdict, c.wantVerdict)
			}
			if res.Coverage.Checked != c.wantChecked {
				t.Errorf("checked = %d, want %d", res.Coverage.Checked, c.wantChecked)
			}
			if got := findingIDs(res.Findings); !equalStrings(got, c.wantIDs) {
				t.Errorf("finding ids = %v, want %v", got, c.wantIDs)
			}
			if res.Verdict == VerdictInconclusive && len(res.Coverage.Missing) == 0 {
				t.Error("inconclusive 必须带非空 coverage.missing")
			}
		})
	}
}

// ── 横切：数据源报错与截断 ───────────────────────────────────

// 数据源报错必须是 error，不能变成「没发现问题」。
// 这两者混淆的后果是：库挂了，运营看到的是绿色的「一切正常」。
func TestAllChecks_DataSourceError(t *testing.T) {
	for _, name := range []string{"orphan_rules", "voucher_health", "slot_bindings", "pricing_visibility"} {
		t.Run(name, func(t *testing.T) {
			check, _ := Find(name)
			_, err := check.Run(context.Background(), &FakeSource{Err: errors.New("db down")}, nil)
			if err == nil {
				t.Fatal("数据源报错必须返回 error，不能被吞成空结果")
			}
		})
	}
}

// 超过扫描上限时必须降级成 inconclusive，而不是报 ok。
// 场景：库里 6000 条规则，只扫了前 5000 条且这 5000 条都正常，
// 剩下 1000 条里可能全是孤儿——说「一切正常」就是骗人。
func TestAllChecks_TruncatedMustNotReportOK(t *testing.T) {
	const n = scanLimit + 1000

	rules := make([]EntitlementRule, 0, n)
	for i := 1; i <= n; i++ {
		rules = append(rules, EntitlementRule{UID: int64(i), GroupUID: 7, LifecycleState: 1})
	}
	batches := make([]VoucherBatch, 0, n)
	for i := 1; i <= n; i++ {
		batches = append(batches, VoucherBatch{UID: int64(i), QuotaTotal: 100, QuotaIssued: 10, State: 2, LifecycleState: 1})
	}
	slots := make([]PromotionSlot, 0, n)
	for i := 1; i <= n; i++ {
		slots = append(slots, PromotionSlot{UID: int64(i), Product: "文档云 Pro", LifecycleState: 1})
	}

	ds := &FakeSource{Rules: rules, Batches: batches, Slots: slots}

	for _, name := range []string{"orphan_rules", "voucher_health", "slot_bindings"} {
		t.Run(name, func(t *testing.T) {
			check, _ := Find(name)
			res, err := check.Run(context.Background(), ds, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Verdict != VerdictInconclusive {
				t.Errorf("verdict = %q, want inconclusive（被截断时不许报 ok）", res.Verdict)
			}
			if len(res.Coverage.Missing) == 0 {
				t.Error("inconclusive 必须说明缺了什么，Coverage.Missing 不能为空")
			}
			if res.Coverage.Checked != scanLimit {
				t.Errorf("checked = %d, want %d", res.Coverage.Checked, scanLimit)
			}
		})
	}
}

// 截断但已经查到问题时：结论仍是 issue，但置信度要降、缺口要写明。
func TestTruncatedWithFinding(t *testing.T) {
	rules := make([]EntitlementRule, 0, scanLimit+2)
	rules = append(rules, EntitlementRule{UID: 1, Title: "孤儿", GroupUID: 0, LifecycleState: 1})
	for i := 2; i <= scanLimit+2; i++ {
		rules = append(rules, EntitlementRule{UID: int64(i), GroupUID: 7, LifecycleState: 1})
	}

	check, _ := Find("orphan_rules")
	res, err := check.Run(context.Background(), &FakeSource{Rules: rules}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Verdict != VerdictIssue {
		t.Errorf("verdict = %q, want issue", res.Verdict)
	}
	if res.Confidence >= 1 {
		t.Errorf("confidence = %v，截断时必须降低", res.Confidence)
	}
	if len(res.Coverage.Missing) == 0 {
		t.Error("截断必须写进 Coverage.Missing")
	}
}
