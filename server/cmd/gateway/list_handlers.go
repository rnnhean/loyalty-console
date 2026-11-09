package main

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/example/loyalty-console/server/internal/domain"
)

// ── 中间件不可用时的守卫 ───────────────────────────────────
//
// 为什么要显式返回 503 而不是让 handler 内部 panic：
// 「中间件没起来」和「业务报错」在运维眼里是两类问题，
// 状态码必须能区分。503 会触发前端的「功能暂不可用」提示，
// 500 会触发 Sentry 告警 —— 前者是预期内的降级，后者才是 bug。

func requireRabbit(in *infra, h gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if in.rabbit == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"code": 503, "message": "延迟任务服务暂不可用",
			})
			return
		}
		h(c)
	}
}

func requireRedis(in *infra, h gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if in.locker == nil || in.guard == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"code": 503, "message": "协调服务暂不可用",
			})
			return
		}
		h(c)
	}
}

// ── 新表的列表接口 ──

func listDebits(svc *domain.TradeService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListDebits(c.Request.Context(), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}

func listPromos(svc *domain.TradeService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListPromos(c.Request.Context(), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}

func listVouchers(svc *domain.OperationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListVouchers(c.Request.Context(), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}

func listSlots(svc *domain.OperationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListSlots(c.Request.Context(), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}

func listAttribution(svc *domain.ChannelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListAttribution(c.Request.Context(), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}

func listToggles(svc *domain.SettingService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListToggles(c.Request.Context(), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}

func listBinds(svc *domain.CatalogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		res, err := svc.ListBinds(c.Request.Context(), pageOf(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, anyList(res))
	}
}
