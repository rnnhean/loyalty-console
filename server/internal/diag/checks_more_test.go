package diag

import (
	"context"
	"sort"
	"testing"
)

// ── benefit_chain ────────────────────────────────────────────

func TestBenefitChain_MissingProduct(t *testing.T) {
	// 缺参数绝不能默认取第一个产品：那会让结论建立在一个未经授权的猜测上。
	check, _ := Find("benefit_chain")
	res, err := check.Run(context.Background(), &FakeSource{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Verdict != VerdictInconclusive {
		t.Errorf("verdict = %q, want inconclusive", res.Verdict)
	}
	if len(res.Coverage.Missing) == 0 {
		t.Fatal("缺参数必须写清 missing")
	}
}

func TestBenefitChain(t *testing.T) {
	product := "文档云 Pro"
	cases := []struct {
		name        string
		ds          *FakeSource
		wantVerdict Verdict
		wantIDs     []string
	}{
		{
			name: "命中：产品下没有层级",
			ds: &FakeSource{
				Tiers:  []Tier{},
				Groups: []Group{{UID: 1, Title: "包", Product: product, LifecycleState: 1}},
			},
			wantVerdict: VerdictIssue,
			wantIDs:     []string{"tier:missing"},
		},
		{
			name: "命中：有层级但没权益包",
			ds: &FakeSource{
				Tiers:  []Tier{{UID: 1, Label: "SVIP", Product: product, LifecycleState: 1}},
				Groups: []Group{},
			},
			wantVerdict: VerdictIssue,
			wantIDs:     []string{"group:missing"},
		},
		{
			name: "命中：权益包挂到不存在的层级",
			ds: &FakeSource{
				Tiers:  []Tier{{UID: 1, Label: "SVIP", Product: product, LifecycleState: 1}},
				Groups: []Group{{UID: 5, Title: "错挂包", Product: product, TierUID: 999, LifecycleState: 1}},
			},
			wantVerdict: VerdictIssue,
			wantIDs:     []string{"group:5"},
		},
		{
			name: "命中：任务发奖包不存在",
			ds: &FakeSource{
				Tiers:  []Tier{{UID: 1, Label: "SVIP", Product: product, LifecycleState: 1}},
				Groups: []Group{{UID: 5, Title: "正常包", Product: product, TierUID: 1, LifecycleState: 1}},
				Tasks:  []TaskThreshold{{UID: 9, Title: "签到任务", Product: product, RewardGroupUID: 777, LifecycleState: 1}},
			},
			wantVerdict: VerdictIssue,
			wantIDs:     []string{"task:9"},
		},
		{
			// 只报最靠上的断点：任务包也挂了，但不该一起报——
			// 修完上层的错，下层可能是对的。
			name: "只报第一个断点",
			ds: &FakeSource{
				Tiers:  []Tier{{UID: 1, Label: "SVIP", Product: product, LifecycleState: 1}},
				Groups: []Group{{UID: 5, Title: "错挂包", Product: product, TierUID: 999, LifecycleState: 1}},
				Tasks:  []TaskThreshold{{UID: 9, Title: "签到任务", Product: product, RewardGroupUID: 777, LifecycleState: 1}},
			},
			wantVerdict: VerdictIssue,
			wantIDs:     []string{"group:5"},
		},
		{
			name: "未命中：链路完整",
			ds: &FakeSource{
				Tiers:  []Tier{{UID: 1, Label: "SVIP", Product: product, LifecycleState: 1}},
				Groups: []Group{{UID: 5, Title: "包", Product: product, TierUID: 1, LifecycleState: 1}},
				Tasks:  []TaskThreshold{{UID: 9, Title: "任务", Product: product, RewardGroupUID: 5, LifecycleState: 1}},
			},
			wantVerdict: VerdictOK,
			wantIDs:     []string{},
		},
		{
			// 通用包（tier_uid=0）和不发奖的任务（reward_group_uid=0）都是合法配置。
			name: "未命中：通用包与不发奖任务",
			ds: &FakeSource{
				Tiers:  []Tier{{UID: 1, Label: "SVIP", Product: product, LifecycleState: 1}},
				Groups: []Group{{UID: 5, Title: "通用包", Product: product, TierUID: 0, LifecycleState: 1}},
				Tasks:  []TaskThreshold{{UID: 9, Title: "进度任务", Product: product, RewardGroupUID: 0, LifecycleState: 1}},
			},
			wantVerdict: VerdictOK,
			wantIDs:     []string{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			check, _ := Find("benefit_chain")
			res, err := check.Run(context.Background(), c.ds, Params{"product": product})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Verdict != c.wantVerdict {
				t.Errorf("verdict = %q, want %q", res.Verdict, c.wantVerdict)
			}
			if got := sortedIDs(res.Findings); !equalStrings(got, c.wantIDs) {
				t.Errorf("ids = %v, want %v", got, c.wantIDs)
			}
		})
	}
}

// ── test_data_scan ───────────────────────────────────────────

func TestTestDataScan(t *testing.T) {
	cases := []struct {
		name        string
		ents        []NamedEntity
		wantVerdict Verdict
		wantIDs     []string
	}{
		{
			name: "命中：启用中的测试批次",
			ents: []NamedEntity{
				{Table: "batch", UID: 5005, Name: "测试批次-勿投", Enabled: true},
				{Table: "batch", UID: 5001, Name: "双十一 20 元券", Enabled: true},
			},
			wantVerdict: VerdictIssue,
			wantIDs:     []string{"batch:5005"},
		},
		{
			// 停用的测试数据是正常的——运营造完数据用完会停掉。
			// 只有「启用的」才会被真实用户看到。
			name: "未命中：测试数据已停用",
			ents: []NamedEntity{
				{Table: "batch", UID: 5005, Name: "测试批次-勿投", Enabled: false},
			},
			wantVerdict: VerdictOK,
			wantIDs:     []string{},
		},
		{
			name: "命中：纯数字名称",
			ents: []NamedEntity{
				{Table: "spu", UID: 42, Name: "123456", Enabled: true},
			},
			wantVerdict: VerdictIssue,
			wantIDs:     []string{"spu:42"},
		},
		{
			// 「临时」「占位」这类词在真实数据里也常见，
			// 误报会让运营不信第一条，后面的结论就都不看了。
			name: "未命中：真人名字里带常规词",
			ents: []NamedEntity{
				{Table: "group", UID: 3, Name: "临时会员专享礼包", Enabled: true},
				{Table: "rule", UID: 4, Name: "年度会员", Enabled: true},
			},
			wantVerdict: VerdictOK,
			wantIDs:     []string{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			check, _ := Find("test_data_scan")
			res, err := check.Run(context.Background(), &FakeSource{Entities: c.ents}, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Verdict != c.wantVerdict {
				t.Errorf("verdict = %q, want %q", res.Verdict, c.wantVerdict)
			}
			if got := sortedIDs(res.Findings); !equalStrings(got, c.wantIDs) {
				t.Errorf("ids = %v, want %v", got, c.wantIDs)
			}
		})
	}
}

// ── change_impact ────────────────────────────────────────────

func TestChangeImpact(t *testing.T) {
	cases := []struct {
		name        string
		toggles     []FeatureToggle
		params      Params
		wantVerdict Verdict
		wantIDs     []string
	}{
		{
			name: "命中：同产品有相关开关",
			toggles: []FeatureToggle{
				{UID: 1, Key: "checkout.v2", Product: "文档云 Pro", Value: "on"},
				{UID: 2, Key: "checkout.retry", Product: "文档云 Pro", Value: "off"},
			},
			params:      Params{"toggle_key": "checkout.v2"},
			wantVerdict: VerdictIssue,
			wantIDs:     []string{"toggle:1"},
		},
		{
			name: "未命中：产品下只有它自己",
			toggles: []FeatureToggle{
				{UID: 1, Key: "checkout.v2", Product: "文档云 Pro", Value: "on"},
				{UID: 2, Key: "other", Product: "超级会员", Value: "on"},
			},
			params:      Params{"toggle_key": "checkout.v2"},
			wantVerdict: VerdictOK,
			wantIDs:     []string{},
		},
		{
			// key 写错和 key 已被删除在数据上一样，不能报 ok。
			name: "开关不存在：inconclusive",
			toggles: []FeatureToggle{
				{UID: 1, Key: "checkout.v2", Product: "文档云 Pro"},
			},
			params:      Params{"toggle_key": "no.such.key"},
			wantVerdict: VerdictInconclusive,
			wantIDs:     []string{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			check, _ := Find("change_impact")
			res, err := check.Run(context.Background(), &FakeSource{Toggles: c.toggles}, c.params)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Verdict != c.wantVerdict {
				t.Errorf("verdict = %q, want %q", res.Verdict, c.wantVerdict)
			}
			if got := sortedIDs(res.Findings); !equalStrings(got, c.wantIDs) {
				t.Errorf("ids = %v, want %v", got, c.wantIDs)
			}
		})
	}
}

func TestChangeImpact_MissingKey(t *testing.T) {
	check, _ := Find("change_impact")
	res, err := check.Run(context.Background(), &FakeSource{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Verdict != VerdictInconclusive || len(res.Coverage.Missing) == 0 {
		t.Errorf("缺 toggle_key 必须 inconclusive 带 missing，got %q / %v", res.Verdict, res.Coverage.Missing)
	}
}

// ── site_audit ───────────────────────────────────────────────

// 聚合规则的基本契约：把所有子规则的 finding 汇总，且不能递归调到自己。
func TestSiteAudit(t *testing.T) {
	ds := &FakeSource{
		Rules: []EntitlementRule{
			{UID: 101, Title: "生日礼", GroupUID: 0, LifecycleState: 1},
		},
		Slots: []PromotionSlot{
			{UID: 7004, Title: "浮层", Product: "", LifecycleState: 1},
		},
	}
	check, ok := Find("site_audit")
	if !ok {
		t.Fatal("site_audit 未注册")
	}
	res, err := check.Run(context.Background(), ds, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Verdict != VerdictIssue {
		t.Errorf("verdict = %q, want issue", res.Verdict)
	}

	ids := sortedIDs(res.Findings)
	want := []string{"orphan_rules/rule:101", "slot_bindings/slot:7004"}
	if !equalStrings(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}

	// 带参数的规则在这里没参数，应当贡献 missing 而不是静默跳过。
	if len(res.Coverage.Missing) == 0 {
		t.Error("缺参数的子规则必须留下 missing 痕迹，不能装没事")
	}
}

func sortedIDs(fs []Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.ID)
	}
	sort.Strings(out)
	return out
}
