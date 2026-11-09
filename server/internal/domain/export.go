package domain

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/example/loyalty-console/server/internal/exporter"
)

// ── 导出数据源 ─────────────────────────────────────────────
//
// 这里刻意用 keyset 分页（WHERE uid > ?）而不是 OFFSET：
// 导出过程中如果有新数据写入，OFFSET 分页会让后面的行整体后移，
// 表现为「同一行导出两次、另一行漏掉」。keyset 不受插入影响。

const exportBatch = 500

// spuSource 商品导出源。
func (svc *CatalogService) SPUSource(keyword string) exporter.Source[SPURow] {
	var last int64
	where, args := "voided = 0", []any{}
	if keyword != "" {
		where += " AND spu_title LIKE ?"
		args = append(args, likeParam(keyword))
	}
	return exporter.FuncSource[SPURow](func(ctx context.Context) ([]SPURow, error) {
		q := "SELECT spu_uid, spu_title, spu_kind, retail_amount, lifecycle_state, created_at " +
			"FROM cat_spu_archive WHERE " + where + " AND spu_uid > ? ORDER BY spu_uid LIMIT ?"
		rows, err := svc.S.DB.QueryContext(ctx, q, append(args, last, exportBatch)...)
		if err != nil {
			return nil, err
		}
		items, err := scanRows(rows, func(r *sql.Rows) (SPURow, error) {
			var v SPURow
			var ct sql.NullTime
			err := r.Scan(&v.UID, &v.Title, &v.Kind, &v.Amount, &v.State, &ct)
			v.CreatedAt = fmtTime(ct)
			return v, err
		})
		if err != nil {
			return nil, err
		}
		if len(items) > 0 {
			last = items[len(items)-1].UID
		}
		return items, nil
	})
}

// tierSource 会员层级导出源。
func (svc *BenefitService) TierSource(product string) exporter.Source[TierRow] {
	var last int64
	where, args := "voided = 0", []any{}
	if product != "" {
		where += " AND product_title = ?"
		args = append(args, product)
	}
	return exporter.FuncSource[TierRow](func(ctx context.Context) ([]TierRow, error) {
		q := "SELECT tier_uid, tier_label, product_title, tier_rank, lifecycle_state, online_state, updated_at " +
			"FROM bft_tier_profile WHERE " + where + " AND tier_uid > ? ORDER BY tier_uid LIMIT ?"
		rows, err := svc.S.DB.QueryContext(ctx, q, append(args, last, exportBatch)...)
		if err != nil {
			return nil, err
		}
		items, err := scanRows(rows, func(r *sql.Rows) (TierRow, error) {
			var v TierRow
			var ut sql.NullTime
			err := r.Scan(&v.UID, &v.Label, &v.Product, &v.Rank, &v.State, &v.Online, &ut)
			v.Updated = fmtTime(ut)
			return v, err
		})
		if err != nil {
			return nil, err
		}
		if len(items) > 0 {
			last = items[len(items)-1].UID
		}
		return items, nil
	})
}

// 导出列定义。集中在一处，前端和运维要「加一列」时只改这里。
var (
	SPUColumns = []exporter.Column[SPURow]{
		{Header: "ID", Value: exporter.Int64(func(v SPURow) int64 { return v.UID })},
		{Header: "商品名称", Value: func(v SPURow) string { return v.Title }},
		{Header: "类型", Value: exporter.Int(func(v SPURow) int { return v.Kind })},
		{Header: "标价", Value: exporter.Money(func(v SPURow) float64 { return v.Amount })},
		{Header: "状态", Value: exporter.Int(func(v SPURow) int { return v.State })},
		{Header: "创建时间", Value: func(v SPURow) string { return v.CreatedAt }},
	}

	TierColumns = []exporter.Column[TierRow]{
		{Header: "ID", Value: exporter.Int64(func(v TierRow) int64 { return v.UID })},
		{Header: "层级名称", Value: func(v TierRow) string { return v.Label }},
		{Header: "所属产品", Value: func(v TierRow) string { return v.Product }},
		{Header: "等级", Value: exporter.Int(func(v TierRow) int { return v.Rank })},
		{Header: "启用状态", Value: exporter.Int(func(v TierRow) int { return v.State })},
		{Header: "上线状态", Value: exporter.Int(func(v TierRow) int { return v.Online })},
		{Header: "更新时间", Value: func(v TierRow) string { return v.Updated }},
	}
)

// ── 权益到期回收 ───────────────────────────────────────────
//
// 场景：会员权益到了有效期终点，要批量置为失效。
//
// 为什么这件事必须走延迟队列而不是定时扫表：
//   - 扫表要处理「已经扫过的不能重复处理」，需要额外的游标表；
//   - 扫表的时间精度受扫描间隔限制（每 5 分钟扫一次，最坏晚 5 分钟）；
//   - 数据量大时全表扫描本身就是负担。
// 延迟队列把「什么时候执行」交给中间件，应用只处理到期的那一条。
//
// 又为什么必须配分布式锁：
//   三个实例各起一个消费者，同一条到期任务会被投递给其中一个 ——
//   RabbitMQ 保证了一条消息只给一个消费者。但**同一批业务数据**
//   可能被多条任务同时触及（同一个权益的多条到期记录），
//   这时锁保证「处置逻辑对同一资源是串行的」。

// ExpireTask 权益到期的任务载荷。
type ExpireTask struct {
	// RuleUID 权益规则 ID。同一规则的到期处理要互斥。
	RuleUID int64 `json:"rule_uid"`
	// Product 产品，用于审计定位。
	Product string `json:"product"`
	// Kind 到期类型：tier（层级）/ group（权益组）
	Kind string `json:"kind"`
}

// ExpireOutcome 一次到期处理的结果。
type ExpireOutcome struct {
	RuleUID  int64  `json:"rule_uid"`
	Affected int64  `json:"affected"`
	Skipped  bool   `json:"skipped"` // 锁没抢到，另一个实例在处理
	Reason   string `json:"reason,omitempty"`
}

// ExpireBenefitRule 把一个权益规则置为失效。
//
// 幂等性来自 SQL 本身：WHERE lifecycle_state = 1 保证重复执行时
// 第二次 Affected = 0，不会把已失效的又「失效」一遍。
// 这是**最便宜的幂等**，比引入额外的状态表好得多。
//
// inTx 在提交之前被调用，用来登记要发的领域事件。为什么不用「先提交、
// 再发 Kafka」：两次操作之间进程重启，库里已经失效、下游永远收不到通知，
// 而且没有任何报错。把事件登记放进同一个事务，才谈得上「要么都成」。
//
// 注意 inTx 拿到的是事务句柄，必须用它写库，不能用 svc.S.DB ——
// 用 DB 会另开一条连接，原子性立刻失效，而代码上看不出区别。
//
// 关于并发安全：调用方还会套一层 Redis 分布式锁，但**正确性不依赖它**。
// WHERE lifecycle_state = 1 是一条条件更新，两个事务即使同时进来，
// InnoDB 的行锁也只会让一个拿到 affected=1，另一个拿到 0。
// 锁的作用只是把「同一规则的重复处理」串行化、省掉无谓的争用；
// 它失效（主从切换、租约过期）时正确性依然成立。
// 这就是「用数据库约束兜底、锁只做优化」——反过来（只靠锁保证正确）
// 一旦锁失效就会重复扣减，而这类问题往往只在故障当天暴露。
func (svc *BenefitService) ExpireBenefitRule(
	ctx context.Context, ruleUID int64, inTx func(ctx context.Context, tx *sql.Tx, affected int64) error,
) (int64, error) {
	tx, err := svc.S.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE bft_entitlement_rule
		    SET lifecycle_state = 0, updated_at = ?
		  WHERE rule_uid = ? AND lifecycle_state = 1`,
		time.Now(), ruleUID)
	if err != nil {
		return 0, fmt.Errorf("expire rule %d: %w", ruleUID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("expire rule %d: %w", ruleUID, err)
	}

	// affected = 0 表示这条规则已经失效过（重复投递），
	// 此时不登记事件 —— 否则下游会收到两条「到期」。
	if affected > 0 && inTx != nil {
		if err := inTx(ctx, tx, affected); err != nil {
			return 0, err
		}
	}
	return affected, tx.Commit()
}
