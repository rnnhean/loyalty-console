package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/errgroup"

	"github.com/example/loyalty-console/server/internal/domain"
	"github.com/example/loyalty-console/server/internal/observ"
	"github.com/example/loyalty-console/server/pkg/trace"
)

// ── BFF 聚合层 ─────────────────────────────────────────────
//
// 为什么要有这一层？
//
// 旧前端的每个页面有 iframe 里自己发 3~10 个请求，串行瀑布，
// 一个页面首屏要等四五轮 RTT。改成单一管理台后，前端只发 1 个请求，
// 由网关并发扇出到各领域服务，在服务端合并成 1 个响应。
//
// 这一层是 Go 后端相对 TS 后端最实在的差异点：
//   - Node 单线程：并发扇出本质是「同时发起 N 个 Promise」，DB 驱动和
//     JSON 反序列化仍在一个线程上排队，CPU 侧的合并成本无法并行；
//   - Go：N 个 goroutine 真正跑在多个 P 上，扫描/序列化可以并行，
//     且 ctx 取消会沿调用链一路传播（任一子任务失败，其余的立刻放弃）。
//
// 但扇出不是免费的，必须设两道闸：
//  1. errgroup.SetLimit —— 限制单个请求的并发度，否则一个页面聚合
//     20 个查询，10 个用户并发就能把连接池（MaxOpenConns）打满；
//  2. WithContext —— 任一子任务失败或父 ctx 超时，其余任务收到取消信号，
//     不会出现「主请求已经返回 504，后台还在跑 19 个查询」的浪费。

// fanTask 一个可并发的子任务。
type fanTask struct {
	Name string
	Run  func(context.Context) (any, error)
}

// Fanout 并发执行一组任务，合并成 map 返回。
//
// 语义选择：**部分失败不整体失败**。
// 页面首屏的各个区块互相独立，产品下拉框拿不到不应该让整页白屏，
// 所以这里记录各自错误、仍返回成功的那部分，由前端按 block 降级。
// 这与「写接口一失败就必须整体回滚」是相反的判断，原因是读聚合没有事务性。
func Fanout(ctx context.Context, route string, limit int, tasks ...fanTask) (map[string]any, map[string]string) {
	if len(tasks) == 0 {
		return map[string]any{}, nil
	}

	start := time.Now()
	out := make(map[string]any, len(tasks))
	var fails map[string]string

	// 每个任务写各自的槽位，map 本身不并发写 —— 避免给 map 加锁
	vals := make([]any, len(tasks))
	errs := make([]error, len(tasks))

	g, gctx := errgroup.WithContext(ctx)
	if limit > 0 {
		g.SetLimit(limit)
	}

	for i := range tasks {
		i := i
		t := tasks[i]
		g.Go(func() error {
			v, err := t.Run(gctx)
			if err != nil {
				errs[i] = err
				// 返回 nil：子任务失败不影响兄弟任务
				return nil
			}
			vals[i] = v
			return nil
		})
	}
	_ = g.Wait()

	for i, t := range tasks {
		if errs[i] != nil {
			if fails == nil {
				fails = make(map[string]string, 2)
			}
			fails[t.Name] = "数据加载失败"
			observ.DegradedTotal.WithLabelValues(route, t.Name).Inc()
			Logger.WarnContext(ctx, "bff_block_failed",
				"trace_id", trace.FromContext(ctx),
				"route", route,
				"block", t.Name,
				"error", errs[i].Error(),
			)
			continue
		}
		out[t.Name] = vals[i]
	}

	observ.FanoutDuration.WithLabelValues(route).Observe(time.Since(start).Seconds())
	observ.FanoutBlocks.WithLabelValues(route).Observe(float64(len(tasks)))
	return out, fails
}

// ── 聚合控制器 ─────────────────────────────────────────────

// Aggregator 持有各领域服务，负责把「一个页面要的东西」拼成一个响应。
//
// 注意它不持有 store —— 领域服务才碰数据源。
// 网关如果直接查库，路由层和 SQL 就耦合了，将来拆分服务得全改。
type Aggregator struct {
	Catalog   *domain.CatalogService
	Benefit   *domain.BenefitService
	Trade     *domain.TradeService
	Channel   *domain.ChannelService
	Operation *domain.OperationService
	Setting   *domain.SettingService
}

// NewAggregator 构造。
func NewAggregator(c *domain.CatalogService, b *domain.BenefitService,
	t *domain.TradeService, ch *domain.ChannelService,
	o *domain.OperationService, s *domain.SettingService) *Aggregator {
	return &Aggregator{Catalog: c, Benefit: b, Trade: t, Channel: ch, Operation: o, Setting: s}
}

// fanoutLimit 单个请求最多同时跑几个子查询。
//
// 取 6 而不是不限制：mysql 连接池按 MaxOpenConns=32 设计，
// 若每个请求扇出 20，16 个并发用户就能占满池子，后面的请求全部排队。
// 「聚合接口的并发度」必须与「连接池大小 / 预估并发」一起算。
const fanoutLimit = 6

// CatalogOverview GET /api/bff/catalog/overview
//
// 商品域首屏：产品下拉框 + SPU/卡券/定价三块概览。
func (a *Aggregator) CatalogOverview(c *gin.Context) {
	ctx := c.Request.Context()
	products, _ := a.Setting.ListProducts(ctx)

	blocks, fails := Fanout(ctx, "/api/bff/catalog/overview", fanoutLimit,
		fanTask{Name: "spu", Run: func(ctx context.Context) (any, error) {
			return a.Catalog.ListSPU(ctx, "", domain.Page{Num: 1, Size: 8})
		}},
		fanTask{Name: "cards", Run: func(ctx context.Context) (any, error) {
			return a.Catalog.ListCards(ctx, "", domain.Page{Num: 1, Size: 8})
		}},
		fanTask{Name: "pricing", Run: func(ctx context.Context) (any, error) {
			return a.Catalog.ListPricing(ctx, c.Query("product"), domain.Page{Num: 1, Size: 8})
		}},
		fanTask{Name: "sku", Run: func(ctx context.Context) (any, error) {
			return a.Catalog.ListSKU(ctx, c.Query("product"), domain.Page{Num: 1, Size: 8})
		}},
	)

	respondBlocks(c, products, blocks, fails)
}

// BenefitOverview GET /api/bff/benefit/overview
func (a *Aggregator) BenefitOverview(c *gin.Context) {
	ctx := c.Request.Context()
	product := c.Query("product")
	products, _ := a.Setting.ListProducts(ctx)

	blocks, fails := Fanout(ctx, "/api/bff/benefit/overview", fanoutLimit,
		fanTask{Name: "tiers", Run: func(ctx context.Context) (any, error) {
			return a.Benefit.ListTiers(ctx, product, domain.Page{Num: 1, Size: 20})
		}},
		fanTask{Name: "groups", Run: func(ctx context.Context) (any, error) {
			return a.Benefit.ListGroups(ctx, product, domain.Page{Num: 1, Size: 10})
		}},
		fanTask{Name: "rules", Run: func(ctx context.Context) (any, error) {
			return a.Benefit.ListRules(ctx, domain.Page{Num: 1, Size: 10})
		}},
		fanTask{Name: "tasks", Run: func(ctx context.Context) (any, error) {
			return a.Benefit.ListTasks(ctx, domain.Page{Num: 1, Size: 10})
		}},
	)
	respondBlocks(c, products, blocks, fails)
}

// ChannelOverview GET /api/bff/channel/overview
func (a *Aggregator) ChannelOverview(c *gin.Context) {
	ctx := c.Request.Context()
	products, _ := a.Setting.ListProducts(ctx)

	blocks, fails := Fanout(ctx, "/api/bff/channel/overview", fanoutLimit,
		fanTask{Name: "merchants", Run: func(ctx context.Context) (any, error) {
			return a.Channel.ListMerchants(ctx, "", domain.Page{Num: 1, Size: 10})
		}},
		fanTask{Name: "nodes", Run: func(ctx context.Context) (any, error) {
			return a.Channel.ListNodes(ctx, domain.Page{Num: 1, Size: 10})
		}},
	)
	respondBlocks(c, products, blocks, fails)
}

// OperationOverview GET /api/bff/operation/overview
func (a *Aggregator) OperationOverview(c *gin.Context) {
	ctx := c.Request.Context()
	products, _ := a.Setting.ListProducts(ctx)

	blocks, fails := Fanout(ctx, "/api/bff/operation/overview", fanoutLimit,
		fanTask{Name: "campaigns", Run: func(ctx context.Context) (any, error) {
			return a.Operation.ListCampaigns(ctx, c.Query("product"), domain.Page{Num: 1, Size: 10})
		}},
	)
	respondBlocks(c, products, blocks, fails)
}

// SettingOverview GET /api/bff/setting/overview
func (a *Aggregator) SettingOverview(c *gin.Context) {
	ctx := c.Request.Context()
	products, _ := a.Setting.ListProducts(ctx)

	blocks, fails := Fanout(ctx, "/api/bff/setting/overview", fanoutLimit,
		fanTask{Name: "dict", Run: func(ctx context.Context) (any, error) {
			return a.Setting.ListDict(ctx, c.Query("kind"), domain.Page{Num: 1, Size: 20})
		}},
		fanTask{Name: "modules", Run: func(ctx context.Context) (any, error) {
			return a.Setting.ListModules(ctx, domain.Page{Num: 1, Size: 10})
		}},
	)
	respondBlocks(c, products, blocks, fails)
}

// TradeOverview GET /api/bff/trade/overview
func (a *Aggregator) TradeOverview(c *gin.Context) {
	ctx := c.Request.Context()
	products, _ := a.Setting.ListProducts(ctx)

	blocks, fails := Fanout(ctx, "/api/bff/trade/overview", fanoutLimit,
		fanTask{Name: "checkouts", Run: func(ctx context.Context) (any, error) {
			return a.Trade.ListCheckouts(ctx, domain.Page{Num: 1, Size: 10})
		}},
	)
	respondBlocks(c, products, blocks, fails)
}

// respondBlocks 统一响应体。
//
// degraded 里列出失败区块，前端据此做局部降级提示，
// 而不是整个页面报错。
func respondBlocks(c *gin.Context, products any, blocks map[string]any, fails map[string]string) {
	ctx := c.Request.Context()
	body := gin.H{
		"code":     0,
		"products": products,
		"blocks":   blocks,
		"trace_id": trace.FromContext(ctx),
	}
	if len(fails) > 0 {
		body["degraded"] = fails
	}
	c.JSON(http.StatusOK, body)
}
