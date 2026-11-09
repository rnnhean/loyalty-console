package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/example/loyalty-console/server/internal/domain"
	"github.com/example/loyalty-console/server/pkg/trace"
)

// ── 单表接口 handler ───────────────────────────────────────
//
// 统一约定：
//   - 成功 {"code":0, "items":[...], "total":N, "page":x, "page_size":y, "trace_id":"..."}
//   - 失败 {"code":500, "message":"查询失败", "trace_id":"..."}
// 前端拿到 trace_id 后可以在 Sentry 里关联，也可以用它在日志里检索。

func pageOf(c *gin.Context) domain.Page {
	n, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	s, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	return domain.Page{Num: n, Size: s}
}

func ok(c *gin.Context, res domain.ListResult[any]) {
	c.JSON(http.StatusOK, gin.H{
		"code": 0, "items": res.Items, "total": res.Total,
		"page": res.Page, "page_size": res.Size,
		"trace_id": trace.FromContext(c.Request.Context()),
	})
}

// fail 统一错误出口。
//
// 对外只回「查询失败」，不把 SQL / 连接串错误透出去 ——
// 公网部署下，详细的内部错误是最好的侦察情报。
// 详细信息进日志，靠 trace_id 关联。
func fail(c *gin.Context, err error) {
	gatewayLogger().ErrorContext(c.Request.Context(), "handler_error",
		"trace_id", trace.FromContext(c.Request.Context()),
		"path", c.FullPath(),
		"error", err.Error(),
	)
	c.JSON(http.StatusInternalServerError, gin.H{
		"code": 500, "message": "查询失败",
		"trace_id": trace.FromContext(c.Request.Context()),
	})
}

func gatewayLogger() *slog.Logger { return slog.Default() }

// 泛型适配：把 ListResult[T] 转成 ListResult[any] 交给 ok()
func anyList[T any](r domain.ListResult[T]) domain.ListResult[any] {
	return domain.ListResult[any]{Items: anySlice(r.Items), Total: r.Total, Page: r.Page, Size: r.Size}
}

func anySlice[T any](in []T) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func listSPU(svc *domain.CatalogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListSPU(c.Request.Context(), c.Query("keyword"), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}

func listCards(svc *domain.CatalogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListCards(c.Request.Context(), c.Query("keyword"), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}

func listPricing(svc *domain.CatalogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListPricing(c.Request.Context(), c.Query("product"), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}

func listSKU(svc *domain.CatalogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListSKU(c.Request.Context(), c.Query("product"), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}

func listTiers(svc *domain.BenefitService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListTiers(c.Request.Context(), c.Query("product"), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}

func listGroups(svc *domain.BenefitService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListGroups(c.Request.Context(), c.Query("product"), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}

// optionsHandler 把「返回产品选项」的函数包成 gin handler。
func optionsHandler(load func(ctx context.Context) ([]domain.ProductOption, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		rows, err := load(c.Request.Context())
		if err != nil {
			fail(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"code": 0, "items": rows,
			"trace_id": trace.FromContext(c.Request.Context()),
		})
	}
}

func listRules(svc *domain.BenefitService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListRules(c.Request.Context(), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}

func listTasks(svc *domain.BenefitService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListTasks(c.Request.Context(), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}

func listCheckouts(svc *domain.TradeService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListCheckouts(c.Request.Context(), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}

func listMerchants(svc *domain.ChannelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListMerchants(c.Request.Context(), c.Query("keyword"), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}

func listNodes(svc *domain.ChannelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListNodes(c.Request.Context(), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}

func listCampaigns(svc *domain.OperationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListCampaigns(c.Request.Context(), c.Query("product"), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}

func listDict(svc *domain.SettingService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListDict(c.Request.Context(), c.Query("kind"), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}

func listModules(svc *domain.SettingService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListModules(c.Request.Context(), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}

// ── 环境变量 ───────────────────────────────────────────────

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// envList 逗号分隔的列表。
func envList(k, def string) []string {
	raw := env(k, def)
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
