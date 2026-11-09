package domain

import (
	"context"
	"database/sql"
)

// 产品下拉框选项。
//
// 为什么不让所有页面共用一个全局产品列表：
// 不同业务表里的 product 字段取值体系根本不是一套 ——
// 会员层级里是「插件平台-xxx」，定价策略里是商品线名称，
// 投放计划里又是另一套。共用一个列表的结果就是「选了什么都没数据」。
//
// 正确的做法是每个筛选页返回**自己表里真实出现过的取值**，
// 这样选了就一定有数据。表名在这里是编译期常量，不存在注入面。

// ProductOption 产品选项。
type ProductOption struct {
	Title string `json:"title"`
	Count int64  `json:"count"`
}

func distinctProducts(ctx context.Context, db *sql.DB, table string) ([]ProductOption, error) {
	//nolint:gosec // table 由包内常量传入，非用户输入
	q := "SELECT product_title, COUNT(*) c FROM " + table +
		" WHERE voided = 0 AND product_title <> '' GROUP BY product_title ORDER BY c DESC, product_title"
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	return scanRows(rows, func(r *sql.Rows) (ProductOption, error) {
		var v ProductOption
		err := r.Scan(&v.Title, &v.Count)
		return v, err
	})
}

// PricingProducts 定价策略页的产品选项。
func (svc *CatalogService) PricingProducts(ctx context.Context) ([]ProductOption, error) {
	return distinctProducts(ctx, svc.S.DB, "cat_pricing_policy")
}

// TierProducts 会员层级页的产品选项。
func (svc *BenefitService) TierProducts(ctx context.Context) ([]ProductOption, error) {
	return distinctProducts(ctx, svc.S.DB, "bft_tier_profile")
}

// GroupProducts 权益包页的产品选项。
func (svc *BenefitService) GroupProducts(ctx context.Context) ([]ProductOption, error) {
	return distinctProducts(ctx, svc.S.DB, "bft_entitlement_group")
}

// CampaignProducts 投放计划页的产品选项。
func (svc *OperationService) CampaignProducts(ctx context.Context) ([]ProductOption, error) {
	return distinctProducts(ctx, svc.S.DB, "ops_campaign_plan")
}
