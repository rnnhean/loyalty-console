// Package domain —— 领域服务。
//
// 每个域对应一类业务实体，各自持有 store，不互相 import。
// 跨域的组合在网关的 BFF 层做（见 gateway/aggregate.go），
// 这是「组合优于继承」在服务划分上的体现：域之间靠接口组合，不靠继承层次。
//
// 统一的商品筛选约定：绝大多数页面顶部有「产品」下拉框，不选产品就没有数据。
// 产品参数统一走 `product` query 参数，未传则返回空列表而不是全量 ——
// 未传哨兵值时的默认语义是「无结果」，避免一次误调用把整库扫出来。
package domain

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/example/loyalty-console/server/internal/store"
)

// Page 统一分页参数。
type Page struct {
	Num  int `form:"page"`
	Size int `form:"page_size"`
}

// Normalize 归一化，防止前端传 page_size=100000 把内存打爆。
func (p *Page) Normalize() {
	if p.Num <= 0 {
		p.Num = 1
	}
	if p.Size <= 0 {
		p.Size = 20
	}
	if p.Size > 200 {
		p.Size = 200
	}
}

// Offset 计算 SQL OFFSET。
func (p Page) Offset() int { return (p.Num - 1) * p.Size }

// ListResult 统一列表返回。
type ListResult[T any] struct {
	Items []T   `json:"items"`
	Total int64 `json:"total"`
	Page  int   `json:"page"`
	Size  int   `json:"page_size"`
}

// scanRows 通用扫描辅助：把 *sql.Rows 转成切片并保证关闭。
func scanRows[T any](rows *sql.Rows, fn func(*sql.Rows) (T, error)) ([]T, error) {
	defer rows.Close()
	out := make([]T, 0, 16)
	for rows.Next() {
		v, err := fn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// fmtTime 统一时间格式（RFC3339），前端不用再处理时区。
func fmtTime(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return t.Time.Format(time.RFC3339)
}

// likeParam 构造 LIKE 参数，并转义通配符，防止用户输入 % 造成全表扫描。
func likeParam(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_")
	return "%" + r.Replace(s) + "%"
}

// ── 商品域 ─────────────────────────────────────────────────

// CatalogService 商品域服务。
type CatalogService struct{ S *store.Store }

// NewCatalogService 构造。
func NewCatalogService(s *store.Store) *CatalogService { return &CatalogService{S: s} }

// SPURow 商品标准单元。
type SPURow struct {
	UID       int64   `json:"uid"`
	Title     string  `json:"title"`
	Kind      int     `json:"kind"`
	Amount    float64 `json:"amount"`
	State     int     `json:"state"`
	CreatedAt string  `json:"created_at"`
}

// ListSPU 商品列表，支持标题模糊查询。
func (svc *CatalogService) ListSPU(ctx context.Context, keyword string, p Page) (ListResult[SPURow], error) {
	p.Normalize()
	res := ListResult[SPURow]{Page: p.Num, Size: p.Size}

	where, args := "voided = 0", []any{}
	if keyword != "" {
		where += " AND spu_title LIKE ?"
		args = append(args, likeParam(keyword))
	}

	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM cat_spu_archive WHERE "+where, args...).Scan(&res.Total); err != nil {
		return res, err
	}

	q := "SELECT spu_uid, spu_title, spu_kind, retail_amount, lifecycle_state, created_at " +
		"FROM cat_spu_archive WHERE " + where + " ORDER BY spu_uid DESC LIMIT ? OFFSET ?"
	rows, err := svc.S.DB.QueryContext(ctx, q, append(args, p.Size, p.Offset())...)
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (SPURow, error) {
		var v SPURow
		var ct sql.NullTime
		err := r.Scan(&v.UID, &v.Title, &v.Kind, &v.Amount, &v.State, &ct)
		v.CreatedAt = fmtTime(ct)
		return v, err
	})
	return res, err
}

// PricingRow 定价策略。
type PricingRow struct {
	UID       int64  `json:"uid"`
	Title     string `json:"title"`
	Product   string `json:"product"`
	Weight    int    `json:"weight"`
	Top       int    `json:"top"`
	Client    int    `json:"client"`
	State     int    `json:"state"`
	UpdatedAt string `json:"updated_at"`
}

// ListPricing 定价策略列表。product 为空时返回空集（与前端「不选产品无数据」一致）。
func (svc *CatalogService) ListPricing(ctx context.Context, product string, p Page) (ListResult[PricingRow], error) {
	p.Normalize()
	res := ListResult[PricingRow]{Items: make([]PricingRow, 0), Page: p.Num, Size: p.Size}
	if product == "" {
		return res, nil
	}

	where, args := "voided = 0 AND product_title = ?", []any{product}
	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM cat_pricing_policy WHERE "+where, args...).Scan(&res.Total); err != nil {
		return res, err
	}

	q := "SELECT policy_uid, policy_title, product_title, traffic_weight, priority_rank, " +
		"client_scope, lifecycle_state, updated_at FROM cat_pricing_policy WHERE " + where +
		" ORDER BY policy_uid DESC LIMIT ? OFFSET ?"
	rows, err := svc.S.DB.QueryContext(ctx, q, append(args, p.Size, p.Offset())...)
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (PricingRow, error) {
		var v PricingRow
		var ut sql.NullTime
		err := r.Scan(&v.UID, &v.Title, &v.Product, &v.Weight, &v.Top, &v.Client, &v.State, &ut)
		v.UpdatedAt = fmtTime(ut)
		return v, err
	})
	return res, err
}

// CardRow 售卖卡。
type CardRow struct {
	UID     int64  `json:"uid"`
	Title   string `json:"title"`
	Variant string `json:"variant"`
	Client  int    `json:"client"`
	State   int    `json:"state"`
	Created string `json:"created_at"`
}

// ListCards 售卖卡列表。
func (svc *CatalogService) ListCards(ctx context.Context, keyword string, p Page) (ListResult[CardRow], error) {
	p.Normalize()
	res := ListResult[CardRow]{Page: p.Num, Size: p.Size}
	where, args := "voided = 0", []any{}
	if keyword != "" {
		where += " AND card_title LIKE ?"
		args = append(args, likeParam(keyword))
	}
	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM cat_offer_card WHERE "+where, args...).Scan(&res.Total); err != nil {
		return res, err
	}
	q := "SELECT card_uid, card_title, card_variant, client_scope, lifecycle_state, created_at " +
		"FROM cat_offer_card WHERE " + where + " ORDER BY card_uid DESC LIMIT ? OFFSET ?"
	rows, err := svc.S.DB.QueryContext(ctx, q, append(args, p.Size, p.Offset())...)
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (CardRow, error) {
		var v CardRow
		var ct sql.NullTime
		err := r.Scan(&v.UID, &v.Title, &v.Variant, &v.Client, &v.State, &ct)
		v.Created = fmtTime(ct)
		return v, err
	})
	return res, err
}

// SKURow 单品。
type SKURow struct {
	UID     int64   `json:"uid"`
	Label   string  `json:"label"`
	Product string  `json:"product"`
	Amount  float64 `json:"amount"`
	Span    int     `json:"span"`
	Client  int     `json:"client"`
	State   int     `json:"state"`
}

// ListSKU 单品列表。
func (svc *CatalogService) ListSKU(ctx context.Context, product string, p Page) (ListResult[SKURow], error) {
	p.Normalize()
	res := ListResult[SKURow]{Page: p.Num, Size: p.Size}
	where, args := "voided = 0", []any{}
	if product != "" {
		where += " AND spu_title = ?"
		args = append(args, product)
	}
	if err := svc.S.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM cat_sku_inventory WHERE "+where, args...).Scan(&res.Total); err != nil {
		return res, err
	}
	q := "SELECT sku_uid, sku_label, spu_title, unit_amount, billable_span, client_scope, lifecycle_state " +
		"FROM cat_sku_inventory WHERE " + where + " ORDER BY sku_uid DESC LIMIT ? OFFSET ?"
	rows, err := svc.S.DB.QueryContext(ctx, q, append(args, p.Size, p.Offset())...)
	if err != nil {
		return res, err
	}
	res.Items, err = scanRows(rows, func(r *sql.Rows) (SKURow, error) {
		var v SKURow
		err := r.Scan(&v.UID, &v.Label, &v.Product, &v.Amount, &v.Span, &v.Client, &v.State)
		return v, err
	})
	return res, err
}
