package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/example/loyalty-console/server/internal/store"
)

// ── 权益域 ─────────────────────────────────────────────────

// BenefitService 权益域。
type BenefitService struct{ S *store.Store }

// NewBenefitService 构造。
func NewBenefitService(s *store.Store) *BenefitService { return &BenefitService{S: s} }

// TierRow 会员层级。
type TierRow struct {
	UID     int64  `json:"uid"`
	Label   string `json:"label"`
	Product string `json:"product"`
	Rank    int    `json:"rank"`
	State   int    `json:"state"`
	Online  int    `json:"online"`
	Updated string `json:"updated_at"`
}

// ListTiers 会员层级列表。
func (svc *BenefitService) ListTiers(ctx context.Context, product string, p Page) (ListResult[TierRow], error) {
	p.Normalize()
	res := ListResult[TierRow]{Page: p.Num, Size: p.Size}
	where, args := "voided = 0", []any{}
	if product != "" {
		where += " AND product_title = ?"
		args = append(args, product)
	}
	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM bft_tier_profile WHERE "+where, args...).Scan(&res.Total); err != nil {
		return res, err
	}
	q := "SELECT tier_uid, tier_label, product_title, tier_rank, lifecycle_state, online_state, updated_at " +
		"FROM bft_tier_profile WHERE " + where + " ORDER BY tier_rank ASC LIMIT ? OFFSET ?"
	rows, err := svc.S.DB.QueryContext(ctx, q, append(args, p.Size, p.Offset())...)
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (TierRow, error) {
		var v TierRow
		var ut sql.NullTime
		err := r.Scan(&v.UID, &v.Label, &v.Product, &v.Rank, &v.State, &v.Online, &ut)
		v.Updated = fmtTime(ut)
		return v, err
	})
	return res, err
}

// GroupRow 权益包。
type GroupRow struct {
	UID     int64  `json:"uid"`
	Title   string `json:"title"`
	Product string `json:"product"`
	Kind    int    `json:"kind"`
	State   int    `json:"state"`
}

// ListGroups 权益包列表。
func (svc *BenefitService) ListGroups(ctx context.Context, product string, p Page) (ListResult[GroupRow], error) {
	p.Normalize()
	res := ListResult[GroupRow]{Page: p.Num, Size: p.Size}
	where, args := "voided = 0", []any{}
	if product != "" {
		where += " AND product_title = ?"
		args = append(args, product)
	}
	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM bft_entitlement_group WHERE "+where, args...).Scan(&res.Total); err != nil {
		return res, err
	}
	q := "SELECT group_uid, group_title, product_title, group_kind, lifecycle_state " +
		"FROM bft_entitlement_group WHERE " + where + " ORDER BY group_uid DESC LIMIT ? OFFSET ?"
	rows, err := svc.S.DB.QueryContext(ctx, q, append(args, p.Size, p.Offset())...)
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (GroupRow, error) {
		var v GroupRow
		err := r.Scan(&v.UID, &v.Title, &v.Product, &v.Kind, &v.State)
		return v, err
	})
	return res, err
}

// RuleRow 权益规则。
type RuleRow struct {
	UID     int64   `json:"uid"`
	GroupID int64   `json:"group_uid"`
	Title   string  `json:"title"`
	Kind    int     `json:"kind"`
	Quota   float64 `json:"quota"`
	Unit    string  `json:"unit"`
	Cycle   int     `json:"cycle"`
	State   int     `json:"state"`
}

// ListRules 权益规则列表。
func (svc *BenefitService) ListRules(ctx context.Context, p Page) (ListResult[RuleRow], error) {
	p.Normalize()
	res := ListResult[RuleRow]{Page: p.Num, Size: p.Size}
	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM bft_entitlement_rule WHERE voided = 0").Scan(&res.Total); err != nil {
		return res, err
	}
	rows, err := svc.S.DB.QueryContext(ctx,
		"SELECT rule_uid, group_uid, rule_title, benefit_kind, quota_value, quota_unit, claim_cycle, lifecycle_state "+
			"FROM bft_entitlement_rule WHERE voided = 0 ORDER BY rule_uid DESC LIMIT ? OFFSET ?",
		p.Size, p.Offset())
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (RuleRow, error) {
		var v RuleRow
		err := r.Scan(&v.UID, &v.GroupID, &v.Title, &v.Kind, &v.Quota, &v.Unit, &v.Cycle, &v.State)
		return v, err
	})
	return res, err
}

// TaskRow 任务门槛。
type TaskRow struct {
	UID     int64  `json:"uid"`
	Title   string `json:"title"`
	Product string `json:"product"`
	Kind    int    `json:"kind"`
	Value   int    `json:"value"`
	State   int    `json:"state"`
}

// ListTasks 任务列表。
func (svc *BenefitService) ListTasks(ctx context.Context, p Page) (ListResult[TaskRow], error) {
	p.Normalize()
	res := ListResult[TaskRow]{Page: p.Num, Size: p.Size}
	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM bft_task_threshold WHERE voided = 0").Scan(&res.Total); err != nil {
		return res, err
	}
	rows, err := svc.S.DB.QueryContext(ctx,
		"SELECT task_uid, task_title, product_title, trigger_kind, threshold_value, lifecycle_state "+
			"FROM bft_task_threshold WHERE voided = 0 ORDER BY task_uid DESC LIMIT ? OFFSET ?",
		p.Size, p.Offset())
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (TaskRow, error) {
		var v TaskRow
		err := r.Scan(&v.UID, &v.Title, &v.Product, &v.Kind, &v.Value, &v.State)
		return v, err
	})
	return res, err
}

// ── 渠道域 ─────────────────────────────────────────────────

// ChannelService 渠道域。
type ChannelService struct{ S *store.Store }

// NewChannelService 构造。
func NewChannelService(s *store.Store) *ChannelService { return &ChannelService{S: s} }

// MerchantRow 商户档案。
type MerchantRow struct {
	UID     int64   `json:"uid"`
	Code    string  `json:"code"`
	Alias   string  `json:"alias"`
	Kind    int     `json:"kind"`
	Primary int     `json:"primary"`
	Settle  int     `json:"settle"`
	Cap     float64 `json:"cap"`
	State   int     `json:"state"`
}

// ListMerchants 商户列表，支持编号/别名模糊查询。
func (svc *ChannelService) ListMerchants(ctx context.Context, keyword string, p Page) (ListResult[MerchantRow], error) {
	p.Normalize()
	res := ListResult[MerchantRow]{Page: p.Num, Size: p.Size}
	where, args := "voided = 0", []any{}
	if keyword != "" {
		where += " AND (merchant_code LIKE ? OR merchant_alias LIKE ?)"
		kw := likeParam(keyword)
		args = append(args, kw, kw)
	}
	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM chn_merchant_profile WHERE "+where, args...).Scan(&res.Total); err != nil {
		return res, err
	}
	q := "SELECT merchant_uid, merchant_code, merchant_alias, merchant_kind, is_primary, " +
		"settle_kind, consume_cap, lifecycle_state FROM chn_merchant_profile WHERE " + where +
		" ORDER BY merchant_uid ASC LIMIT ? OFFSET ?"
	rows, err := svc.S.DB.QueryContext(ctx, q, append(args, p.Size, p.Offset())...)
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (MerchantRow, error) {
		var v MerchantRow
		err := r.Scan(&v.UID, &v.Code, &v.Alias, &v.Kind, &v.Primary, &v.Settle, &v.Cap, &v.State)
		return v, err
	})
	return res, err
}

// NodeRow 分销节点。
type NodeRow struct {
	UID   int64  `json:"uid"`
	Title string `json:"title"`
	Code  string `json:"code"`
	Kind  int    `json:"kind"`
	State int    `json:"state"`
}

// ListNodes 分销节点列表。
func (svc *ChannelService) ListNodes(ctx context.Context, p Page) (ListResult[NodeRow], error) {
	p.Normalize()
	res := ListResult[NodeRow]{Page: p.Num, Size: p.Size}
	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM chn_distribution_node WHERE voided = 0").Scan(&res.Total); err != nil {
		return res, err
	}
	rows, err := svc.S.DB.QueryContext(ctx,
		"SELECT node_uid, node_title, node_code, node_kind, lifecycle_state "+
			"FROM chn_distribution_node WHERE voided = 0 ORDER BY node_uid ASC LIMIT ? OFFSET ?",
		p.Size, p.Offset())
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (NodeRow, error) {
		var v NodeRow
		err := r.Scan(&v.UID, &v.Title, &v.Code, &v.Kind, &v.State)
		return v, err
	})
	return res, err
}

// ── 运营域 ─────────────────────────────────────────────────

// OperationService 运营域。
type OperationService struct{ S *store.Store }

// NewOperationService 构造。
func NewOperationService(s *store.Store) *OperationService { return &OperationService{S: s} }

// CampaignRow 投放计划。
type CampaignRow struct {
	UID     int64  `json:"uid"`
	Title   string `json:"title"`
	Product string `json:"product"`
	Kind    int    `json:"kind"`
	Start   string `json:"starts_at"`
	End     string `json:"ends_at"`
	State   int    `json:"state"`
}

// ListCampaigns 投放计划列表。
func (svc *OperationService) ListCampaigns(ctx context.Context, product string, p Page) (ListResult[CampaignRow], error) {
	p.Normalize()
	res := ListResult[CampaignRow]{Page: p.Num, Size: p.Size}
	where, args := "voided = 0", []any{}
	if product != "" {
		where += " AND product_title = ?"
		args = append(args, product)
	}
	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM ops_campaign_plan WHERE "+where, args...).Scan(&res.Total); err != nil {
		return res, err
	}
	q := "SELECT campaign_uid, campaign_title, product_title, campaign_kind, starts_at, ends_at, lifecycle_state " +
		"FROM ops_campaign_plan WHERE " + where + " ORDER BY campaign_uid DESC LIMIT ? OFFSET ?"
	rows, err := svc.S.DB.QueryContext(ctx, q, append(args, p.Size, p.Offset())...)
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (CampaignRow, error) {
		var v CampaignRow
		var st, et sql.NullTime
		err := r.Scan(&v.UID, &v.Title, &v.Product, &v.Kind, &st, &et, &v.State)
		v.Start, v.End = fmtTime(st), fmtTime(et)
		return v, err
	})
	return res, err
}

// ── 配置域 ─────────────────────────────────────────────────

// SettingService 配置域。
type SettingService struct{ S *store.Store }

// NewSettingService 构造。
func NewSettingService(s *store.Store) *SettingService { return &SettingService{S: s} }

// ProductRow 产品目录项。
type ProductRow struct {
	UID     int64  `json:"uid"`
	Code    string `json:"code"`
	Title   string `json:"title"`
	Display string `json:"display_title"`
	State   int    `json:"state"`
}

// ListProducts 产品下拉框数据源。
//
// 前端「产品」下拉框在几乎每个页面都有，是被访问最频繁的接口之一，
// 所以这份数据走 GetOrLoad 的两级缓存（本地 → Redis），避免每次点菜单都打 DB。
func (svc *SettingService) ListProducts(ctx context.Context) ([]ProductRow, error) {
	const q = `SELECT product_uid, product_code, product_title, display_title, lifecycle_state
	           FROM cfg_product_catalog WHERE voided = 0 ORDER BY ordinal ASC`

	raw, err := svc.S.GetOrLoad(ctx, "setting", "products", 5*time.Minute,
		func(ctx context.Context) ([]byte, error) {
			rows, err := svc.S.DB.QueryContext(ctx, q)
			if err != nil {
				return nil, err
			}
			list, err := scanRows(rows, func(r *sql.Rows) (ProductRow, error) {
				var v ProductRow
				err := r.Scan(&v.UID, &v.Code, &v.Title, &v.Display, &v.State)
				return v, err
			})
			if err != nil {
				return nil, err
			}
			return json.Marshal(list)
		})
	if err != nil {
		return nil, err
	}
	var list []ProductRow
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("products cache: decode: %w", err)
	}
	return list, nil
}

// DictRow 字典项。
type DictRow struct {
	UID   int64  `json:"uid"`
	Kind  string `json:"kind"`
	Code  string `json:"code"`
	Label string `json:"label"`
	Ord   int    `json:"ordinal"`
}

// ListDict 字典项列表。
func (svc *SettingService) ListDict(ctx context.Context, kind string, p Page) (ListResult[DictRow], error) {
	p.Normalize()
	res := ListResult[DictRow]{Page: p.Num, Size: p.Size}
	where, args := "voided = 0", []any{}
	if kind != "" {
		where += " AND dict_kind = ?"
		args = append(args, kind)
	}
	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM cfg_dictionary_entry WHERE "+where, args...).Scan(&res.Total); err != nil {
		return res, err
	}
	q := "SELECT entry_uid, dict_kind, entry_code, entry_label, ordinal " +
		"FROM cfg_dictionary_entry WHERE " + where + " ORDER BY dict_kind, ordinal LIMIT ? OFFSET ?"
	rows, err := svc.S.DB.QueryContext(ctx, q, append(args, p.Size, p.Offset())...)
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (DictRow, error) {
		var v DictRow
		err := r.Scan(&v.UID, &v.Kind, &v.Code, &v.Label, &v.Ord)
		return v, err
	})
	return res, err
}

// ModuleRow 集成模块。
type ModuleRow struct {
	UID     int64  `json:"uid"`
	Title   string `json:"title"`
	Product string `json:"product"`
	Kind    string `json:"kind"`
	State   int    `json:"state"`
}

// ListModules 集成模块列表。
func (svc *SettingService) ListModules(ctx context.Context, p Page) (ListResult[ModuleRow], error) {
	p.Normalize()
	res := ListResult[ModuleRow]{Page: p.Num, Size: p.Size}
	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM cfg_integration_module WHERE voided = 0").Scan(&res.Total); err != nil {
		return res, err
	}
	rows, err := svc.S.DB.QueryContext(ctx,
		"SELECT module_uid, module_title, product_title, module_kind, lifecycle_state "+
			"FROM cfg_integration_module WHERE voided = 0 ORDER BY module_uid ASC LIMIT ? OFFSET ?",
		p.Size, p.Offset())
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (ModuleRow, error) {
		var v ModuleRow
		err := r.Scan(&v.UID, &v.Title, &v.Product, &v.Kind, &v.State)
		return v, err
	})
	return res, err
}

// ── 交易域 ─────────────────────────────────────────────────

// TradeService 交易域。
type TradeService struct{ S *store.Store }

// NewTradeService 构造。
func NewTradeService(s *store.Store) *TradeService { return &TradeService{S: s} }

// CheckoutRow 结算配置。
type CheckoutRow struct {
	UID     int64  `json:"uid"`
	Title   string `json:"title"`
	Product string `json:"product"`
	Client  int    `json:"client"`
	State   int    `json:"state"`
}

// ListCheckouts 结算配置列表。
func (svc *TradeService) ListCheckouts(ctx context.Context, p Page) (ListResult[CheckoutRow], error) {
	p.Normalize()
	res := ListResult[CheckoutRow]{Page: p.Num, Size: p.Size}
	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM trd_checkout_profile WHERE voided = 0").Scan(&res.Total); err != nil {
		return res, err
	}
	rows, err := svc.S.DB.QueryContext(ctx,
		"SELECT profile_uid, profile_title, product_title, client_scope, lifecycle_state "+
			"FROM trd_checkout_profile WHERE voided = 0 ORDER BY profile_uid ASC LIMIT ? OFFSET ?",
		p.Size, p.Offset())
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (CheckoutRow, error) {
		var v CheckoutRow
		err := r.Scan(&v.UID, &v.Title, &v.Product, &v.Client, &v.State)
		return v, err
	})
	return res, err
}
