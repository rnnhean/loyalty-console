package diag

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SQLSource 是 DataSource 的 MySQL 实现。
//
// 刻意不复用 domain 层的 List*：那些方法带分页、带 UI 排序、
// 还夹着各自的业务过滤，语义是为「列表页」服务的。诊断要的是
// 「当前生效的全集」，走同一条 SQL 会在两边改需求时互相牵连——
// 列表页加个默认筛选，诊断就静默少查了一半数据，而且看不出来。
type SQLSource struct {
	DB *sql.DB
	// now 可注入。生产为 nil 时用 time.Now；测试可以塞一个固定时刻，
	// 否则「已过期」这类用例会在某个整点前后随机失败。
	now func() time.Time
}

var _ DataSource = (*SQLSource)(nil)

// Now 实现 DataSource。
func (s *SQLSource) Now() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// ListActiveRules 取「启用」的权益规则。
//
// voided = 1 是软删除，必须排除；lifecycle_state = 1 是启用。
// 这里不加 group_uid 条件——判定归规则函数。
func (s *SQLSource) ListActiveRules(ctx context.Context, limit int) ([]EntitlementRule, error) {
	const q = `SELECT rule_uid, rule_title, group_uid, benefit_kind, lifecycle_state
FROM bft_entitlement_rule
WHERE voided = 0 AND lifecycle_state = 1
ORDER BY rule_uid
LIMIT ?`

	return queryRows(s, ctx, q, []any{limit}, func(rows *sql.Rows) (EntitlementRule, error) {
		var r EntitlementRule
		err := rows.Scan(&r.UID, &r.Title, &r.GroupUID, &r.BenefitKind, &r.LifecycleState)
		return r, err
	})
}

// ListVoucherBatches 取未软删除的券批次。
//
// 不过滤 lifecycle_state：已删除但有发放记录的批次恰恰是最该报的那类。
// 过滤掉就等于「看不见就不算问题」。
func (s *SQLSource) ListVoucherBatches(ctx context.Context, limit int) ([]VoucherBatch, error) {
	const q = `SELECT batch_uid, batch_title, campaign_uid, face_amount,
       quota_total, quota_issued, batch_state, lifecycle_state
FROM ops_voucher_batch
WHERE voided = 0
ORDER BY batch_uid
LIMIT ?`

	return queryRows(s, ctx, q, []any{limit}, func(rows *sql.Rows) (VoucherBatch, error) {
		var b VoucherBatch
		err := rows.Scan(&b.UID, &b.Title, &b.CampaignUID, &b.FaceAmount,
			&b.QuotaTotal, &b.QuotaIssued, &b.State, &b.LifecycleState)
		return b, err
	})
}

// ListActiveSlots 取启用中的推广位。
func (s *SQLSource) ListActiveSlots(ctx context.Context, limit int) ([]PromotionSlot, error) {
	const q = `SELECT slot_uid, slot_title, product_title, lifecycle_state
FROM ops_promotion_slot
WHERE voided = 0 AND lifecycle_state = 1
ORDER BY slot_uid
LIMIT ?`

	return queryRows(s, ctx, q, []any{limit}, func(rows *sql.Rows) (PromotionSlot, error) {
		var p PromotionSlot
		err := rows.Scan(&p.UID, &p.Title, &p.Product, &p.LifecycleState)
		return p, err
	})
}

// ListPublishedPolicies 取已发布的定价策略。
//
// publish_state = 4 是「已发布」（见 02-schema.sql 的列注释）。
// 生效时间两端是 DATETIME NULL，用 sql.NullTime 接，
// 不能直接扫进 time.Time——NULL 会让整条查询报错。
func (s *SQLSource) ListPublishedPolicies(ctx context.Context, limit int) ([]PricingPolicy, error) {
	const q = `SELECT policy_uid, policy_title, product_title, publish_state, lifecycle_state,
       effective_from, effective_to
FROM cat_pricing_policy
WHERE voided = 0 AND publish_state = 4
ORDER BY policy_uid
LIMIT ?`

	return queryRows(s, ctx, q, []any{limit}, func(rows *sql.Rows) (PricingPolicy, error) {
		var p PricingPolicy
		var from, to sql.NullTime
		if err := rows.Scan(&p.UID, &p.Title, &p.Product, &p.PublishState, &p.LifecycleState, &from, &to); err != nil {
			return p, err
		}
		p.HasFrom = from.Valid
		if from.Valid {
			p.EffectiveFrom = from.Time
		}
		p.HasTo = to.Valid
		if to.Valid {
			p.EffectiveTo = to.Time
		}
		return p, nil
	})
}

// queryRows 是四个查询共用的执行 + 扫描骨架。
// 抽出来只是为了避免四次重复的 rows.Close / rows.Err 处理——
// 那类代码漏一处就是连接泄漏，而泄漏在压测前都发现不了。
func queryRows[T any](s *SQLSource, ctx context.Context, q string, args []any, scan func(*sql.Rows) (T, error)) ([]T, error) {
	if s == nil || s.DB == nil {
		return nil, fmt.Errorf("diag: 数据源未初始化")
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("diag: 查询必须带 LIMIT 参数")
	}

	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("diag: 查询失败: %w", err)
	}
	defer rows.Close()

	out := make([]T, 0, 64)
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("diag: 扫描失败: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("diag: 遍历失败: %w", err)
	}
	return out, nil
}

// ── 层级 / 权益包 / 任务阈值 ───────────────────────────
//
// 三个方法都按 product_title 过滤，product 为空则不过滤。
// 过滤放在 SQL 里（而不是取回内存再筛）是因为这三张表都可能很大，
// 而「某产品的链路」是个天然的高选择性条件。

// ListTiers 取启用中的会员层级。
func (s *SQLSource) ListTiers(ctx context.Context, product string, limit int) ([]Tier, error) {
	q := `SELECT tier_uid, tier_label, product_title, lifecycle_state
FROM bft_tier_profile
WHERE voided = 0 AND lifecycle_state = 1`
	args := []any{}
	if product != "" {
		q += ` AND product_title = ?`
		args = append(args, product)
	}
	q += ` ORDER BY tier_rank, tier_uid LIMIT ?`
	args = append(args, limit)

	return queryRows(s, ctx, q, args, func(rows *sql.Rows) (Tier, error) {
		var t Tier
		err := rows.Scan(&t.UID, &t.Label, &t.Product, &t.LifecycleState)
		return t, err
	})
}

// ListGroups 取启用中的权益包。
func (s *SQLSource) ListGroups(ctx context.Context, product string, limit int) ([]Group, error) {
	q := `SELECT group_uid, group_title, product_title, tier_uid, lifecycle_state
FROM bft_entitlement_group
WHERE voided = 0 AND lifecycle_state = 1`
	args := []any{}
	if product != "" {
		q += ` AND product_title = ?`
		args = append(args, product)
	}
	q += ` ORDER BY group_uid LIMIT ?`
	args = append(args, limit)

	return queryRows(s, ctx, q, args, func(rows *sql.Rows) (Group, error) {
		var g Group
		err := rows.Scan(&g.UID, &g.Title, &g.Product, &g.TierUID, &g.LifecycleState)
		return g, err
	})
}

// ListTasks 取启用中的任务阈值。
func (s *SQLSource) ListTasks(ctx context.Context, product string, limit int) ([]TaskThreshold, error) {
	q := `SELECT task_uid, task_title, product_title, reward_group_uid, lifecycle_state
FROM bft_task_threshold
WHERE voided = 0 AND lifecycle_state = 1`
	args := []any{}
	if product != "" {
		q += ` AND product_title = ?`
		args = append(args, product)
	}
	q += ` ORDER BY task_uid LIMIT ?`
	args = append(args, limit)

	return queryRows(s, ctx, q, args, func(rows *sql.Rows) (TaskThreshold, error) {
		var t TaskThreshold
		err := rows.Scan(&t.UID, &t.Title, &t.Product, &t.RewardGroupUID, &t.LifecycleState)
		return t, err
	})
}

// ── 测试数据扫描 / 开关 ────────────────────────────────

// ListNamedEntities 跨表取「名称 + 启用状态」，用于找残留的测试数据。
//
// 用 UNION ALL 一次查完，而不是在 Go 里循环五次查询：
// 五张表各一次往返，在远程库上就是 5 倍延迟，而这条规则要跑在 8 秒预算里。
// 表名在这里硬编码进 SQL 字符串——它不来自用户输入，不存在注入面。
func (s *SQLSource) ListNamedEntities(ctx context.Context, limit int) ([]NamedEntity, error) {
	const q = `
SELECT 'rule' AS tbl, rule_uid AS uid, rule_title AS name, (lifecycle_state = 1) AS enabled
  FROM bft_entitlement_rule WHERE voided = 0
UNION ALL
SELECT 'group', group_uid, group_title, (lifecycle_state = 1)
  FROM bft_entitlement_group WHERE voided = 0
UNION ALL
SELECT 'tier', tier_uid, tier_label, (lifecycle_state = 1)
  FROM bft_tier_profile WHERE voided = 0
UNION ALL
SELECT 'task', task_uid, task_title, (lifecycle_state = 1)
  FROM bft_task_threshold WHERE voided = 0
UNION ALL
SELECT 'batch', batch_uid, batch_title, (lifecycle_state = 1)
  FROM ops_voucher_batch WHERE voided = 0
UNION ALL
SELECT 'slot', slot_uid, slot_title, (lifecycle_state = 1)
  FROM ops_promotion_slot WHERE voided = 0
UNION ALL
SELECT 'campaign', campaign_uid, campaign_title, (lifecycle_state = 1)
  FROM ops_campaign_plan WHERE voided = 0
UNION ALL
SELECT 'policy', policy_uid, policy_title, (lifecycle_state = 1)
  FROM cat_pricing_policy WHERE voided = 0
LIMIT ?`

	return queryRows(s, ctx, q, []any{limit}, func(rows *sql.Rows) (NamedEntity, error) {
		var e NamedEntity
		err := rows.Scan(&e.Table, &e.UID, &e.Name, &e.Enabled)
		return e, err
	})
}

// ListToggles 取全部开关。cfg_feature_toggle 没有 lifecycle_state 列，
// 只有 voided，所以这里不按启用态过滤。
func (s *SQLSource) ListToggles(ctx context.Context, limit int) ([]FeatureToggle, error) {
	const q = `SELECT toggle_uid, toggle_key, product_title, toggle_value
FROM cfg_feature_toggle
WHERE voided = 0
ORDER BY toggle_uid
LIMIT ?`

	return queryRows(s, ctx, q, []any{limit}, func(rows *sql.Rows) (FeatureToggle, error) {
		var t FeatureToggle
		err := rows.Scan(&t.UID, &t.Key, &t.Product, &t.Value)
		return t, err
	})
}
