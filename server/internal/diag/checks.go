package diag

import (
	"context"
	"fmt"
	"time"
)

// ── orphan_rules ─────────────────────────────────────────────

// orphanRules：启用了、却没挂到任何权益包上的规则。
//
// 判据是 group_uid = 0 且 lifecycle_state = 1（启用）。
// 注意列定义是 BIGINT NOT NULL DEFAULT 0，所以「未挂载」是等于 0，
// 不是 IS NULL —— 写成 IS NULL 会永远返回空集，
// 而空集会被判成 ok，也就是把「全都没挂」报成「一切正常」。这是最危险的一种错。
//
// 只在内存里过滤、不在 SQL 里过滤，是为了让这条判定能被单测直接覆盖。
func orphanRules(ctx context.Context, ds DataSource, _ Params) (Result, error) {
	// 多取一条用来判断「是不是被 limit 截断了」。
	rules, err := ds.ListActiveRules(ctx, scanLimit+limitProbe)
	if err != nil {
		return Result{}, err
	}

	truncated := len(rules) > scanLimit
	if truncated {
		rules = rules[:scanLimit]
	}

	res := newResult(len(rules))
	for _, r := range rules {
		if r.GroupUID != 0 {
			continue
		}
		res.Findings = append(res.Findings, Finding{
			ID:       fmt.Sprintf("rule:%d", r.UID),
			Severity: SeverityHigh,
			Title:    fmt.Sprintf("「%s」没有挂到任何权益包", r.Title),
			Detail:   "这条规则处于启用状态，但 group_uid 为 0，不会对任何会员生效。",
			Evidence: map[string]any{
				"rule_uid":   r.UID,
				"rule_title": r.Title,
				"group_uid":  r.GroupUID,
			},
			Link: fmt.Sprintf("/benefit/rules?focus=%d", r.UID),
		})
	}

	finish(&res, truncated, scanLimit)
	return res, nil
}

// ── voucher_health ───────────────────────────────────────────

// voucherHealth：券批次的状态与发放进度是否自相矛盾。
//
// 只报三类「不管怎么解释都是错的」情况，不报需要业务判断的：
//
//   - 已删除的批次已经发出去过券 → 券还在用户手里，批次却当它不存在了
//   - 启用中的批次一条都没发、且配额为 0 → 建了但没配量，等于死批次
//   - 发放数超过总配额 → 超发，账对不上
//
// 反例（故意不报）：草稿状态的批次 quota_issued=0，是正常的；
// 「发了很少」也不是问题——那是运营自己的节奏，诊断不该替人打分。
func voucherHealth(ctx context.Context, ds DataSource, _ Params) (Result, error) {
	batches, err := ds.ListVoucherBatches(ctx, scanLimit+limitProbe)
	if err != nil {
		return Result{}, err
	}

	truncated := len(batches) > scanLimit
	if truncated {
		batches = batches[:scanLimit]
	}

	res := newResult(len(batches))
	for _, b := range batches {
		// 已删除但有发放记录：券已经出去了，批次却按删除态处理。
		if b.LifecycleState == 3 && b.QuotaIssued > 0 {
			res.Findings = append(res.Findings, Finding{
				ID:       fmt.Sprintf("batch:%d", b.UID),
				Severity: SeverityHigh,
				Title:    fmt.Sprintf("「%s」已删除，但已发出 %d 张券", b.Title, b.QuotaIssued),
				Detail:   "批次记录处于已删除状态，但发放记录还在。这些券在用户端仍然可用，而批次在后台已经不可见，后续无法核销对账。",
				Evidence: map[string]any{
					"batch_uid":    b.UID,
					"batch_title":  b.Title,
					"quota_issued": b.QuotaIssued,
					"lifecycle":    b.LifecycleState,
				},
				Link: fmt.Sprintf("/operation/vouchers?focus=%d", b.UID),
			})
			continue
		}

		// 启用中、配额为 0：建了批次但没配量，永远发不出去。
		if b.State == 1 && b.QuotaTotal == 0 {
			res.Findings = append(res.Findings, Finding{
				ID:       fmt.Sprintf("batch:%d", b.UID),
				Severity: SeverityMid,
				Title:    fmt.Sprintf("「%s」已启用但总配额为 0", b.Title),
				Detail:   "批次处于启用状态，但 quota_total 为 0，任何领取请求都会失败。要么补上配额，要么先停用。",
				Evidence: map[string]any{
					"batch_uid":   b.UID,
					"batch_title": b.Title,
					"quota_total": b.QuotaTotal,
					"state":       b.State,
				},
				Link: fmt.Sprintf("/operation/vouchers?focus=%d", b.UID),
			})
			continue
		}

		// 超发：发放数大于总配额，账面上解释不通。
		if b.QuotaIssued > b.QuotaTotal {
			res.Findings = append(res.Findings, Finding{
				ID:       fmt.Sprintf("batch:%d", b.UID),
				Severity: SeverityHigh,
				Title:    fmt.Sprintf("「%s」发放数超过总配额（%d / %d）", b.Title, b.QuotaIssued, b.QuotaTotal),
				Detail:   "quota_issued 大于 quota_total，说明发放链路没有正确扣减配额，或者配额被事后调小。两种情况都需要人工核对。",
				Evidence: map[string]any{
					"batch_uid":    b.UID,
					"batch_title":  b.Title,
					"quota_total":  b.QuotaTotal,
					"quota_issued": b.QuotaIssued,
				},
				Link: fmt.Sprintf("/operation/vouchers?focus=%d", b.UID),
			})
		}
	}

	finish(&res, truncated, scanLimit)
	return res, nil
}

// ── slot_bindings ────────────────────────────────────────────

// slotBindings：启用中的推广位没绑产品。
//
// 判据只有一条：lifecycle_state = 1（启用）且 product_title 为空。
// 推广位的作用是把素材投到某个产品的某个位置，没有产品就没有落点——
// 这种配置一定是错的，不存在「先空着待补」的合理解释
// （真要待补，应该留在草稿态而不是启用）。
func slotBindings(ctx context.Context, ds DataSource, _ Params) (Result, error) {
	slots, err := ds.ListActiveSlots(ctx, scanLimit+limitProbe)
	if err != nil {
		return Result{}, err
	}

	truncated := len(slots) > scanLimit
	if truncated {
		slots = slots[:scanLimit]
	}

	res := newResult(len(slots))
	for _, s := range slots {
		if s.Product != "" {
			continue
		}
		res.Findings = append(res.Findings, Finding{
			ID:       fmt.Sprintf("slot:%d", s.UID),
			Severity: SeverityMid,
			Title:    fmt.Sprintf("「%s」已启用但没有绑定产品", s.Title),
			Detail:   "推广位处于启用状态，但 product_title 为空。没有产品归属的推广位不会命中任何端上的投放位。",
			Evidence: map[string]any{
				"slot_uid":      s.UID,
				"slot_title":    s.Title,
				"product_title": s.Product,
			},
			Link: fmt.Sprintf("/operation/slots?focus=%d", s.UID),
		})
	}

	finish(&res, truncated, scanLimit)
	return res, nil
}

// ── pricing_visibility ───────────────────────────────────────

// pricingVisibility：已发布的定价策略，却在时间窗外面。
//
// publish_state 的语义是 0 未发布 / 4 已发布（见 02-schema.sql）。
// 写成 `== 1` 会把所有已发布策略当成未发布，这条规则就永远报不出东西。
//
// 时间窗两端都可能为空（DATETIME NULL），空表示该侧不限。
// 这类策略不算问题——「永久有效」是合法配置。
func pricingVisibility(ctx context.Context, ds DataSource, p Params) (Result, error) {
	product := paramString(p, "product")

	policies, err := ds.ListPublishedPolicies(ctx, scanLimit+limitProbe)
	if err != nil {
		return Result{}, err
	}

	truncated := len(policies) > scanLimit
	if truncated {
		policies = policies[:scanLimit]
	}

	now := ds.Now()
	res := newResult(0)
	sawProduct := false

	for _, pol := range policies {
		// 带产品参数时只诊断该产品；未带则全扫。
		// 这里过滤的是数据范围，不是判定依据，所以放在计数之前。
		if product != "" && pol.Product != product {
			continue
		}
		sawProduct = true
		res.Coverage.Checked++

		// 已发布但已经过期。
		if pol.QuietlyExpired(now) {
			res.Findings = append(res.Findings, Finding{
				ID:       fmt.Sprintf("policy:%d", pol.UID),
				Severity: SeverityHigh,
				Title:    fmt.Sprintf("「%s」仍是已发布状态，但已在 %s 过期", pol.Title, pol.EffectiveTo.Format("2006-01-02 15:04")),
				Detail:   "定价策略的生效区间已经结束，但 publish_state 还是 4（已发布）。前端会继续把它当作生效策略展示，而实际计价可能已经走到别的策略上。",
				Evidence: map[string]any{
					"policy_uid":    pol.UID,
					"policy_title":  pol.Title,
					"product_title": pol.Product,
					"effective_to":  pol.EffectiveTo.Format("2006-01-02 15:04"),
					"publish_state": pol.PublishState,
				},
				Link: fmt.Sprintf("/catalog/pricing?focus=%d", pol.UID),
			})
			continue
		}

		// 已发布但还没到生效时间。
		if pol.QuietlyPending(now) {
			res.Findings = append(res.Findings, Finding{
				ID:       fmt.Sprintf("policy:%d", pol.UID),
				Severity: SeverityMid,
				Title:    fmt.Sprintf("「%s」标记为已发布，但要到 %s 才生效", pol.Title, pol.EffectiveFrom.Format("2006-01-02 15:04")),
				Detail:   "策略处于已发布状态，但生效时间还没到。如果这是预期的时间预售，可以忽略；如果是手动发布时忘了改生效时间，这段时间内的计价不会走这条策略。",
				Evidence: map[string]any{
					"policy_uid":     pol.UID,
					"policy_title":   pol.Title,
					"product_title":  pol.Product,
					"effective_from": pol.EffectiveFrom.Format("2006-01-02 15:04"),
					"publish_state":  pol.PublishState,
				},
				Link: fmt.Sprintf("/catalog/pricing?focus=%d", pol.UID),
			})
		}
	}

	// 指定了产品却一条都没扫到：不能报 ok——「这个产品没有已发布策略」
	// 和「这个产品名写错了、根本不存在」在数据上长得一样。
	// 必须先于 finish 处理，否则 finish 会把「扫到 0 条」当成正常的空表报 ok。
	if product != "" && !sawProduct {
		res.Verdict = VerdictInconclusive
		res.Confidence = 0.3
		res.Coverage.Missing = append(res.Coverage.Missing,
			fmt.Sprintf("产品「%s」下没有任何已发布的定价策略，无法判断是产品名写错还是确实未配置", product))
		if truncated {
			res.Coverage.Missing = append(res.Coverage.Missing,
				fmt.Sprintf("数据量超过 %d 条，本次只扫描了前 %d 条，其余未检查", scanLimit, scanLimit))
		}
		return res, nil
	}

	finish(&res, truncated, scanLimit)
	return res, nil
}

// ── 时间窗判定 ───────────────────────────────────────────────

// QuietlyExpired 已发布但生效区间已经结束。
// 两端都为空时永远不成立——「不限时间」是合法配置，不是问题。
func (p PricingPolicy) QuietlyExpired(now time.Time) bool {
	return p.PublishState == 4 && p.HasTo && now.After(p.EffectiveTo)
}

// QuietlyPending 已发布但生效时间还没到。
func (p PricingPolicy) QuietlyPending(now time.Time) bool {
	return p.PublishState == 4 && p.HasFrom && now.Before(p.EffectiveFrom)
}

// paramString 取字符串参数。缺失或类型不对都当空串处理——
// 调用方自己决定「空」意味着什么，别在这里 panic。
func paramString(p Params, key string) string {
	if p == nil {
		return ""
	}
	v, ok := p[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}
