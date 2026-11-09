package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrDuplicateEvent 该事件已被同一消费组处理过。
//
// 它不是错误，是「正常去重」的信号：Kafka 是 at-least-once，
// 重复投递是常态。调用方收到它应当当作成功并继续消费下一条。
var ErrDuplicateEvent = errors.New("duplicate event")

// ── 交易审计 ───────────────────────────────────────────────
//
// 审计是「事后追溯」：谁在什么时候改了哪条规则、改前改后是什么。
// 它有两个硬要求，决定了实现方式：
//
//  1. **不能丢**。审计记录丢了，等于操作无法追溯。所以不能只靠
//     异步落库 —— 异步就得接受「消息发出去但消费失败」。
//     本实现走「先同步写 DB，再发事件通知下游报表/风控」，DB 是事实来源。
//
//  2. **不能慢**。审计是写操作的附带动作，不能因为它让保存变慢。
//     所以写入走单条 INSERT（无事务、无索引冲突检查），
//     下游通知走 Kafka 异步。

// AuditRow 一条审计记录。
type AuditRow struct {
	UID       int64           `json:"uid"`
	Target    string          `json:"target_kind"`
	Ref       string          `json:"target_ref"`
	Action    string          `json:"action_kind"`
	Operator  string          `json:"operator_ref"`
	Detail    json.RawMessage `json:"detail,omitempty"`
	CreatedAt string          `json:"created_at"`
}

// RecordAudit 写一条审计。调用方必须传非空 action / target。
//
// 这个函数故意不返回业务错误以外的信息：审计失败要能被上层感知，
// 但错误内容不应该泄漏到响应里（可能是 SQL 细节）。
func (svc *TradeService) RecordAudit(ctx context.Context, target, ref, action, operator string, detail any) (int64, error) {
	if target == "" || action == "" {
		return 0, fmt.Errorf("audit requires target and action")
	}
	var payload []byte
	if detail != nil {
		b, err := json.Marshal(detail)
		if err != nil {
			return 0, fmt.Errorf("marshal audit detail: %w", err)
		}
		payload = b
	}

	res, err := svc.S.DB.ExecContext(ctx,
		`INSERT INTO trd_audit_trace (target_kind, target_ref, action_kind, operator_ref, detail_payload)
		 VALUES (?, ?, ?, ?, ?)`,
		target, ref, action, operator, nullJSON(payload))
	if err != nil {
		return 0, fmt.Errorf("insert audit: %w", err)
	}
	return res.LastInsertId()
}

func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// RecordAuditOnce 消费侧写审计：按 (event_id, consumer_group) 去重，
// 去重占位与审计写入在**同一个事务**里。
//
// 为什么不能用「先插去重表、再写业务、失败再删去重表」：
// 两步之间进程被 kill，去重表里留着位、业务没写，重投时被判为
// 已处理 —— 这条事件永久丢失，且不会有任何报错。
//
// 为什么也不接受「先写业务、最后插去重表」：
// 若去重表插入失败（比如表满），业务已经提交，回滚不了；
// 若插入后发现重复，业务已经重复写入了一份。
//
// 唯一自洽的做法是把两者放进一个事务：要么都成，要么都滚回去重来。
// 这也是「重复处理」和「丢失处理」只能二选一时，选重复 —— 因为
// 去重表保证重复会在事务里被发现，而不是靠事后对账。
func (svc *TradeService) RecordAuditOnce(
	ctx context.Context, eventID, group, target, ref, action, operator string, detail any,
) (int64, error) {
	if eventID == "" || group == "" {
		return 0, fmt.Errorf("dedup requires event id and consumer group")
	}
	if target == "" || action == "" {
		return 0, fmt.Errorf("audit requires target and action")
	}

	var payload []byte
	if detail != nil {
		b, err := json.Marshal(detail)
		if err != nil {
			return 0, fmt.Errorf("marshal audit detail: %w", err)
		}
		payload = b
	}

	tx, err := svc.S.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// INSERT IGNORE + 判断影响行数，比「插了再捕获 1062」更省事：
	// 不用把驱动错误码带进 domain 层。代价是它同时吞掉别的警告，
	// 这里只用在「主键冲突即重复」的单一场合，可以接受。
	res, err := tx.ExecContext(ctx,
		`INSERT IGNORE INTO msg_consumed_event (event_id, consumer_group) VALUES (?, ?)`,
		eventID, group)
	if err != nil {
		return 0, fmt.Errorf("mark consumed: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrDuplicateEvent
	}

	res, err = tx.ExecContext(ctx,
		`INSERT INTO trd_audit_trace (target_kind, target_ref, action_kind, operator_ref, detail_payload)
		 VALUES (?, ?, ?, ?, ?)`,
		target, ref, action, operator, nullJSON(payload))
	if err != nil {
		return 0, fmt.Errorf("insert audit: %w", err)
	}
	uid, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return uid, tx.Commit()
}

// ListAudit 审计列表，可按目标对象过滤。
func (svc *TradeService) ListAudit(ctx context.Context, target, ref string, p Page) (ListResult[AuditRow], error) {
	p.Normalize()
	res := ListResult[AuditRow]{Items: make([]AuditRow, 0), Page: p.Num, Size: p.Size}

	where, args := "1 = 1", []any{}
	if target != "" {
		where += " AND target_kind = ?"
		args = append(args, target)
	}
	if ref != "" {
		where += " AND target_ref = ?"
		args = append(args, ref)
	}

	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM trd_audit_trace WHERE "+where, args...).Scan(&res.Total); err != nil {
		return res, err
	}

	rows, err := svc.S.DB.QueryContext(ctx,
		"SELECT trace_uid, target_kind, target_ref, action_kind, operator_ref, detail_payload, created_at "+
			"FROM trd_audit_trace WHERE "+where+" ORDER BY trace_uid DESC LIMIT ? OFFSET ?",
		append(args, p.Size, p.Offset())...)
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (AuditRow, error) {
		var v AuditRow
		var detail sql.NullString
		var ct sql.NullTime
		if err := r.Scan(&v.UID, &v.Target, &v.Ref, &v.Action, &v.Operator, &detail, &ct); err != nil {
			return v, err
		}
		if detail.Valid {
			v.Detail = json.RawMessage(detail.String)
		}
		v.CreatedAt = fmtTime(ct)
		return v, nil
	})
	return res, err
}

// ── 券批次（带并发安全的核销） ─────────────────────────────

// VoucherBatchRow 券批次。
type VoucherBatchRow struct {
	UID       int64   `json:"uid"`
	Title     string  `json:"title"`
	Campaign  int64   `json:"campaign_uid"`
	Face      float64 `json:"face_amount"`
	Total     int     `json:"quota_total"`
	Issued    int     `json:"quota_issued"`
	State     int     `json:"batch_state"`
	CreatedAt string  `json:"created_at"`
}

// ListVouchers 券批次列表。
func (svc *OperationService) ListVouchers(ctx context.Context, p Page) (ListResult[VoucherBatchRow], error) {
	p.Normalize()
	res := ListResult[VoucherBatchRow]{Items: make([]VoucherBatchRow, 0), Page: p.Num, Size: p.Size}
	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM ops_voucher_batch WHERE voided = 0").Scan(&res.Total); err != nil {
		return res, err
	}
	rows, err := svc.S.DB.QueryContext(ctx,
		"SELECT batch_uid, batch_title, campaign_uid, face_amount, quota_total, quota_issued, batch_state, created_at "+
			"FROM ops_voucher_batch WHERE voided = 0 ORDER BY batch_uid DESC LIMIT ? OFFSET ?",
		p.Size, p.Offset())
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (VoucherBatchRow, error) {
		var v VoucherBatchRow
		var ct sql.NullTime
		err := r.Scan(&v.UID, &v.Title, &v.Campaign, &v.Face, &v.Total, &v.Issued, &v.State, &ct)
		v.CreatedAt = fmtTime(ct)
		return v, err
	})
	return res, err
}

// IssueVoucher 领一张券。
//
// 这里是本项目里**并发正确性最敏感**的一处：限量的券，100 个人同时领，
// 必须有明确的机制保证「不会超发」。三种写法对比：
//
//	① 先查后改（错误示范）
//	   SELECT quota_issued, quota_total → 判断 → UPDATE quota_issued = n+1
//	   两个事务都读到 issued=99、total=100，都判断成功，最后 issued=100 但
//	   实际发了 101 张 —— 典型的丢失更新。
//
//	② SELECT ... FOR UPDATE（本项目采用）
//	   行锁把并发串行化，读到的值在事务提交前不会被别人改。
//	   优点：逻辑直观、可读性好；缺点：热点行会成为串行瓶颈。
//
//	③ UPDATE ... WHERE quota_issued < quota_total（最优）
//	   单条原子语句，不需要显式加锁，MySQL 内部用行锁保证。
//	   缺点：拿不到「更新前是多少」用于审计。
//
//	本项目选 ②：券批次不是高并发场景（管理台手动发券），
//	可读性和审计需求优先于峰值吞吐。这是一个有意识的取舍。
func (svc *OperationService) IssueVoucher(ctx context.Context, batchUID int64) (int, error) {
	tx, err := svc.S.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, err
	}
	// defer Rollback 是安全的：事务已提交时 Rollback 返回 ErrTxDone，忽略即可。
	defer func() { _ = tx.Rollback() }()

	var issued, total int
	// FOR UPDATE 在这里是必需的：没有它，"读-判断-写" 三步之间会被并发插入。
	err = tx.QueryRowContext(ctx,
		"SELECT quota_issued, quota_total FROM ops_voucher_batch WHERE batch_uid = ? AND voided = 0 FOR UPDATE",
		batchUID).Scan(&issued, &total)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("券批次不存在")
		}
		return 0, err
	}
	if issued >= total {
		return issued, fmt.Errorf("券已发完")
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE ops_voucher_batch SET quota_issued = quota_issued + 1 WHERE batch_uid = ?",
		batchUID); err != nil {
		return issued, err
	}
	if err := tx.Commit(); err != nil {
		return issued, err
	}
	return issued + 1, nil
}

// ── 特性开关 ───────────────────────────────────────────────

// ToggleRow 特性开关。
//
// 注意 Value 是字符串不是布尔：开关有时不止开/关两态（灰度比例、
// A/B 分组、名单版本），用字符串存值，语义由开关名自己约定。
type ToggleRow struct {
	UID     int64  `json:"uid"`
	Key     string `json:"toggle_key"`
	Product string `json:"product_title"`
	Value   string `json:"toggle_value"`
	Remark  string `json:"remarks"`
	Updated string `json:"updated_at"`
}

// ListToggles 开关列表。
func (svc *SettingService) ListToggles(ctx context.Context, p Page) (ListResult[ToggleRow], error) {
	p.Normalize()
	res := ListResult[ToggleRow]{Items: make([]ToggleRow, 0), Page: p.Num, Size: p.Size}
	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM cfg_feature_toggle WHERE voided = 0").Scan(&res.Total); err != nil {
		return res, err
	}
	rows, err := svc.S.DB.QueryContext(ctx,
		"SELECT toggle_uid, toggle_key, product_title, toggle_value, remarks, updated_at "+
			"FROM cfg_feature_toggle WHERE voided = 0 ORDER BY toggle_key LIMIT ? OFFSET ?",
		p.Size, p.Offset())
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (ToggleRow, error) {
		var v ToggleRow
		var ut sql.NullTime
		err := r.Scan(&v.UID, &v.Key, &v.Product, &v.Value, &v.Remark, &ut)
		v.Updated = fmtTime(ut)
		return v, err
	})
	return res, err
}

// ── 代扣模板 ───────────────────────────────────────────────

// DebitRow 代扣模板。
type DebitRow struct {
	UID      int64  `json:"uid"`
	Title    string `json:"title"`
	Platform string `json:"platform_kind"`
	State    int    `json:"state"`
	Created  string `json:"created_at"`
}

// ListDebits 代扣模板列表。
func (svc *TradeService) ListDebits(ctx context.Context, p Page) (ListResult[DebitRow], error) {
	p.Normalize()
	res := ListResult[DebitRow]{Items: make([]DebitRow, 0), Page: p.Num, Size: p.Size}
	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM trd_debit_template WHERE voided = 0").Scan(&res.Total); err != nil {
		return res, err
	}
	rows, err := svc.S.DB.QueryContext(ctx,
		"SELECT template_uid, template_title, platform_kind, lifecycle_state, created_at "+
			"FROM trd_debit_template WHERE voided = 0 ORDER BY template_uid DESC LIMIT ? OFFSET ?",
		p.Size, p.Offset())
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (DebitRow, error) {
		var v DebitRow
		var ct sql.NullTime
		err := r.Scan(&v.UID, &v.Title, &v.Platform, &v.State, &ct)
		v.Created = fmtTime(ct)
		return v, err
	})
	return res, err
}

// ── 促销规则 ───────────────────────────────────────────────

// PromoRow 促销规则。
type PromoRow struct {
	UID       int64  `json:"uid"`
	Product   string `json:"product"`
	Merchant  string `json:"merchant_code"`
	PayModes  string `json:"pay_methods"`
	Priority  int    `json:"cond_priority"`
	State     int    `json:"state"`
	UpdatedAt string `json:"updated_at"`
}

// ListPromos 促销规则列表。
func (svc *TradeService) ListPromos(ctx context.Context, p Page) (ListResult[PromoRow], error) {
	p.Normalize()
	res := ListResult[PromoRow]{Items: make([]PromoRow, 0), Page: p.Num, Size: p.Size}
	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM trd_promo_rule WHERE voided = 0").Scan(&res.Total); err != nil {
		return res, err
	}
	rows, err := svc.S.DB.QueryContext(ctx,
		"SELECT promo_uid, product_title, merchant_code, pay_methods, cond_priority, lifecycle_state, updated_at "+
			"FROM trd_promo_rule WHERE voided = 0 ORDER BY cond_priority DESC, promo_uid DESC LIMIT ? OFFSET ?",
		p.Size, p.Offset())
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (PromoRow, error) {
		var v PromoRow
		var ut sql.NullTime
		err := r.Scan(&v.UID, &v.Product, &v.Merchant, &v.PayModes, &v.Priority, &v.State, &ut)
		v.UpdatedAt = fmtTime(ut)
		return v, err
	})
	return res, err
}

// ── 归因记录 ───────────────────────────────────────────────

// AttributionRow 归因记录。
//
// 归因记录只存「事实」（谁点的、落在哪、关联哪个订单），
// 不存「算出来的分成」：分润规则会变，历史记录必须保持当时的样子，
// 金额在查询时按当前规则重算，或另存结算快照表。
type AttributionRow struct {
	UID      int64  `json:"uid"`
	NodeUID  int64  `json:"node_uid"`
	Merchant int64  `json:"merchant_uid"`
	Source   string `json:"source_kind"`
	Landing  string `json:"landing_ref"`
	Created  string `json:"created_at"`
}

// ListAttribution 归因记录列表。
func (svc *ChannelService) ListAttribution(ctx context.Context, p Page) (ListResult[AttributionRow], error) {
	p.Normalize()
	res := ListResult[AttributionRow]{Items: make([]AttributionRow, 0), Page: p.Num, Size: p.Size}
	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM chn_attribution_record").Scan(&res.Total); err != nil {
		return res, err
	}
	rows, err := svc.S.DB.QueryContext(ctx,
		"SELECT record_uid, node_uid, merchant_uid, source_kind, landing_ref, created_at "+
			"FROM chn_attribution_record ORDER BY record_uid DESC LIMIT ? OFFSET ?", p.Size, p.Offset())
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (AttributionRow, error) {
		var v AttributionRow
		var ct sql.NullTime
		err := r.Scan(&v.UID, &v.NodeUID, &v.Merchant, &v.Source, &v.Landing, &ct)
		v.Created = fmtTime(ct)
		return v, err
	})
	return res, err
}

// ── 推广位 ─────────────────────────────────────────────────

// SlotRow 推广位。
type SlotRow struct {
	UID     int64  `json:"uid"`
	Title   string `json:"slot_title"`
	Product string `json:"product_title"`
	Kind    int    `json:"slot_kind"`
	State   int    `json:"lifecycle_state"`
}

// ListSlots 推广位列表。
func (svc *OperationService) ListSlots(ctx context.Context, p Page) (ListResult[SlotRow], error) {
	p.Normalize()
	res := ListResult[SlotRow]{Items: make([]SlotRow, 0), Page: p.Num, Size: p.Size}
	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM ops_promotion_slot WHERE voided = 0").Scan(&res.Total); err != nil {
		return res, err
	}
	rows, err := svc.S.DB.QueryContext(ctx,
		"SELECT slot_uid, slot_title, product_title, slot_kind, lifecycle_state "+
			"FROM ops_promotion_slot WHERE voided = 0 ORDER BY slot_uid DESC LIMIT ? OFFSET ?",
		p.Size, p.Offset())
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (SlotRow, error) {
		var v SlotRow
		err := r.Scan(&v.UID, &v.Title, &v.Product, &v.Kind, &v.State)
		return v, err
	})
	return res, err
}

// ── 绑定关系 ───────────────────────────────────────────────

// BindRow 定价策略与售卖卡的绑定。
//
// Ordinal 而不是 Weight：这里是**有序**关系，不是加权关系。
// 同一策略下多张卡按顺序展示，改顺序就是改运营意图；
// 用权重会让人误以为可以「各占一半」，那是没有意义的语义。
type BindRow struct {
	UID     int64 `json:"uid"`
	Policy  int64 `json:"policy_uid"`
	Card    int64 `json:"card_uid"`
	Ordinal int   `json:"ordinal"`
}

// ListBinds 绑定关系列表。
func (svc *CatalogService) ListBinds(ctx context.Context, p Page) (ListResult[BindRow], error) {
	p.Normalize()
	res := ListResult[BindRow]{Items: make([]BindRow, 0), Page: p.Num, Size: p.Size}
	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM cat_policy_card_bind").Scan(&res.Total); err != nil {
		return res, err
	}
	rows, err := svc.S.DB.QueryContext(ctx,
		"SELECT bind_uid, policy_uid, card_uid, ordinal FROM cat_policy_card_bind "+
			"ORDER BY policy_uid, ordinal LIMIT ? OFFSET ?", p.Size, p.Offset())
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (BindRow, error) {
		var v BindRow
		err := r.Scan(&v.UID, &v.Policy, &v.Card, &v.Ordinal)
		return v, err
	})
	return res, err
}
