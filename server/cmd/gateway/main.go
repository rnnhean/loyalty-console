// Command gateway —— 管理台 API 网关。
//
// 部署形态（公网）：
//
//	浏览器 ──TLS──> Nginx ──HTTP(内网)──> gateway:8080 ──> MySQL / Redis
//	                              └──/metrics──> Prometheus（只在内网网段可达）
//
// 启动参数全部来自环境变量，不读配置文件：
// 容器化部署下配置即环境，且避免「配置文件被打进镜像带走凭据」。
package main

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/example/loyalty-console/server/internal/auth"
	"github.com/example/loyalty-console/server/internal/blob"
	"github.com/example/loyalty-console/server/internal/config"
	"github.com/example/loyalty-console/server/internal/domain"
	"github.com/example/loyalty-console/server/internal/exporter"
	"github.com/example/loyalty-console/server/internal/gateway"
	"github.com/example/loyalty-console/server/internal/observ"
	"github.com/example/loyalty-console/server/internal/store"
	"github.com/example/loyalty-console/server/pkg/trace"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "健康检查模式：请求一次 /healthz 后退出")
	flag.Parse()
	if *healthcheck {
		os.Exit(doHealthcheck())
	}

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gateway exited: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// ★ 配置加载必须在 logger 之前：MC_LOG_LEVEL 也要能从环境文件里读到。
	// 环境变量优先于文件，文件优先于下面的默认值。
	envFile, err := config.Load(os.Getenv("MC_ENV_FILE"))
	if err != nil {
		return err
	}

	logger := observ.NewLogger(env("MC_LOG_LEVEL", "info"))
	gateway.Logger = logger
	slog.SetDefault(logger)

	// 启动即声明「这次跑的是哪个环境、哪份配置」，排查配置错乱时第一眼看这个。
	attrs := []any{slog.String("env", string(config.Current()))}
	if envFile != "" {
		attrs = append(attrs, slog.String("config_file", envFile))
	} else {
		attrs = append(attrs, slog.String("config_file", "none，仅用环境变量与默认值"))
	}
	logger.Info("config_loaded", attrs...)

	// 根 ctx：SIGINT/SIGTERM 时取消，所有后台 goroutine 与在途请求一起收尾
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ── 数据源 ──
	st, err := store.Open(ctx, store.Config{
		MySQLDSN:      env("MC_MYSQL_DSN", "root:root123@tcp(127.0.0.1:13306)/mc_core?charset=utf8mb4&parseTime=true&loc=Local"),
		RedisAddr:     env("MC_REDIS_ADDR", "127.0.0.1:16379"),
		RedisPassword: os.Getenv("MC_REDIS_PASSWORD"),
		RedisDB:       envInt("MC_REDIS_DB", 0),
		MaxOpenConn:   envInt("MC_MYSQL_MAX_OPEN", 32),
		MaxIdleConn:   envInt("MC_MYSQL_MAX_IDLE", 8),
		ConnMaxLife:   time.Duration(envInt("MC_MYSQL_CONN_LIFE_MIN", 30)) * time.Minute,
	})
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer st.Close()

	// ── 领域服务 ──
	catalog := domain.NewCatalogService(st)
	benefit := domain.NewBenefitService(st)
	trade := domain.NewTradeService(st)
	channel := domain.NewChannelService(st)
	operation := domain.NewOperationService(st)
	setting := domain.NewSettingService(st)
	agg := gateway.NewAggregator(catalog, benefit, trade, channel, operation, setting)

	// ── 中间件句柄 ──
	// 单独一层：任何一个中间件不可用都不阻塞启动（见 startInfra 的注释）。
	inf := startInfra(ctx, logger, st)
	defer inf.closeInfra()

	// ── 对象存储 + 导出服务 ──
	var exportSvc *exporter.Service
	if blobStore, err := blob.New(ctx, blob.Config{
		Endpoint:       env("MC_BLOB_ENDPOINT", "127.0.0.1:19000"),
		AccessKey:      env("MC_BLOB_ACCESS_KEY", "mcadmin"),
		SecretKey:      env("MC_BLOB_SECRET_KEY", "mcadmin123"),
		UseSSL:         env("MC_BLOB_USE_SSL", "0") == "1",
		PublicEndpoint: env("MC_BLOB_PUBLIC_ENDPOINT", "127.0.0.1:19000"),
		ExportBucket:   env("MC_BLOB_EXPORT_BUCKET", "console-export"),
		TmpBucket:      env("MC_BLOB_TMP_BUCKET", "console-tmp"),
	}); err != nil {
		// 对象存储不可用只是「导出功能不可用」，不影响其他接口。
		logger.Warn("blob_unavailable", "err", err.Error(), "hint", "导出功能将返回 503")
		inf.state.set("blob", false, err.Error())
	} else {
		exportSvc = exporter.NewService(blobStore, logger, 2)
		inf.state.set("blob", true, "")
	}

	// ── 后台消费者 ──
	startAuditConsumer(ctx, inf, trade, logger)
	startExpiryConsumer(ctx, inf, benefit, logger)

	// ── 鉴权 ──
	secret := os.Getenv("MC_TOKEN_SECRET")
	if secret == "" {
		// 公网部署绝不允许用默认密钥：那等于没鉴权。
		// 开发环境显式设 MC_ALLOW_DEV_SECRET=1 才放行。
		if os.Getenv("MC_ALLOW_DEV_SECRET") != "1" {
			return errors.New("MC_TOKEN_SECRET 未设置（开发环境可设 MC_ALLOW_DEV_SECRET=1 使用临时密钥）")
		}
		buf := make([]byte, 32)
		if _, err := cryptorand.Read(buf); err != nil {
			return err
		}
		secret = string(buf)
		logger.Warn("using_ephemeral_token_secret", "hint", "重启后所有 token 失效")
	}
	issuer := auth.NewIssuer([]byte(secret), time.Duration(envInt("MC_TOKEN_TTL_MIN", 720))*time.Minute)

	// ── 限流 ──
	// 两档：普通接口宽松，登录接口严格（防口令爆破）。
	apiLimiter := gateway.NewRateLimiter(envFloat("MC_RATE_RPS", 30), envInt("MC_RATE_BURST", 60))
	defer apiLimiter.Close()
	loginLimiter := gateway.NewRateLimiter(envFloat("MC_LOGIN_RPS", 2), 5)
	defer loginLimiter.Close()
	telemetryLimiter := gateway.NewRateLimiter(envFloat("MC_TELEMETRY_RPS", 5), 20)
	defer telemetryLimiter.Close()
	// 第三档：按业务 ID 分桶，防「多个来源一起刷同一条数据」。
	// 配额比 apiLimiter 松——它统计的是**所有来源对该 ID 的合计**，
	// 天然比单来源计数高一个数量级；但必须远低于「让数据库扛不住」的量。
	bizLimiter := gateway.NewRateLimiter(envFloat("MC_BIZ_RPS", 10), envInt("MC_BIZ_BURST", 20))
	defer bizLimiter.Close()

	// ── 路由 ──
	r := gin.New()
	r.Use(gateway.Trace(), gateway.Recover(), gateway.Metrics(),
		gateway.CORS(envList("MC_CORS_ORIGINS",
			"http://localhost:5173,http://127.0.0.1:5173")))
	gateway.Mount(r)

	// 登录：不鉴权，但限流最严
	r.POST("/api/auth/login", gateway.RateLimit(loginLimiter), loginHandler(issuer))

	// 前端监控上报：匿名可达，所以单独一档限流 + 白名单校验
	r.POST("/api/telemetry/beacon", gateway.RateLimit(telemetryLimiter), gateway.Telemetry())

	// 受保护路由
	api := r.Group("/api",
		gateway.RateLimit(apiLimiter),
		gateway.Auth(func(token string) (gateway.Operator, error) {
			id, err := issuer.Verify(token)
			if err != nil {
				return gateway.Operator{}, err
			}
			return gateway.Operator{ID: id.ID, Role: id.Role}, nil
		}),
		gateway.Timeout(time.Duration(envInt("MC_REQUEST_TIMEOUT_SEC", 6))*time.Second),
	)

	api.GET("/auth/me", func(c *gin.Context) {
		op, _ := gateway.OperatorFrom(c)
		c.JSON(http.StatusOK, gin.H{"code": 0, "id": op.ID, "role": op.Role})
	})

	// 聚合接口（首屏一次拿全）
	api.GET("/bff/catalog/overview", agg.CatalogOverview)
	api.GET("/bff/benefit/overview", agg.BenefitOverview)
	api.GET("/bff/trade/overview", agg.TradeOverview)
	api.GET("/bff/channel/overview", agg.ChannelOverview)
	api.GET("/bff/operation/overview", agg.OperationOverview)
	api.GET("/bff/setting/overview", agg.SettingOverview)

	// 单表接口（表格分页/筛选时用）
	api.GET("/catalog/spu", listSPU(catalog))
	api.GET("/catalog/cards", listCards(catalog))
	api.GET("/catalog/pricing", listPricing(catalog))
	api.GET("/catalog/sku", listSKU(catalog))
	api.GET("/benefit/tiers", listTiers(benefit))
	api.GET("/benefit/groups", listGroups(benefit))
	api.GET("/benefit/rules", listRules(benefit))
	api.GET("/benefit/tasks", listTasks(benefit))
	api.GET("/trade/checkouts", listCheckouts(trade))
	api.GET("/channel/merchants", listMerchants(channel))
	api.GET("/channel/nodes", listNodes(channel))
	api.GET("/operation/campaigns", listCampaigns(operation))
	api.GET("/setting/products", func(c *gin.Context) {
		rows, err := setting.ListProducts(c.Request.Context())
		if err != nil {
			fail(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "items": rows, "trace_id": trace.FromContext(c.Request.Context())})
	})
	api.GET("/setting/dict", listDict(setting))
	api.GET("/setting/modules", listModules(setting))

	// 各筛选页自己的产品下拉选项（取该表真实出现过的产品值）
	api.GET("/catalog/pricing-products", optionsHandler(catalog.PricingProducts))
	api.GET("/benefit/tier-products", optionsHandler(benefit.TierProducts))
	api.GET("/benefit/group-products", optionsHandler(benefit.GroupProducts))
	api.GET("/operation/campaign-products", optionsHandler(operation.CampaignProducts))

	// ── 新增模块的单表接口 ──
	api.GET("/trade/audit", auditHandler(trade))
	api.GET("/trade/debits", listDebits(trade))
	api.GET("/trade/promos", listPromos(trade))
	api.GET("/operation/vouchers", listVouchers(operation))
	api.GET("/operation/slots", listSlots(operation))
	api.GET("/channel/attribution", listAttribution(channel))
	api.GET("/setting/toggles", listToggles(setting))
	api.GET("/catalog/binds", listBinds(catalog))

	// ── 基础设施能力接口 ──
	//
	// 导出走「提交 + 轮询」：大数据量下不能占着请求连接。
	api.POST("/export/jobs", exportHandler(exportSvc, catalog, benefit))
	api.GET("/export/jobs/:id",
		gateway.RateLimitByKey(bizLimiter, gateway.KeyFromParam("id")),
		exportStatusHandler(exportSvc))

	// 延迟任务：把「权益到期回收」交给 RabbitMQ 的 TTL + DLX
	//
	// 挂两层限流：apiLimiter 管「谁在刷」，bizLimiter 管「刷的是哪条规则」。
	// 后者尤其重要——这两个接口都会对同一 rule_uid 做 DB 更新 + 加锁，
	// 是该表最热的行；不加这层的话，N 个 IP 各刷几次就能把它钉死。
	api.POST("/benefit/expiry-task",
		gateway.RateLimitByKey(bizLimiter, gateway.KeyFromJSON("rule_uid")),
		requireRabbit(inf, expireTaskHandler(inf.rabbit)))

	// 锁 + 幂等的组合演示：同一规则并发处置
	api.POST("/benefit/expire-rule",
		gateway.RateLimitByKey(bizLimiter, gateway.KeyFromJSON("rule_uid")),
		requireRedis(inf, lockDemoHandler(inf, benefit)))

	// 中间件健康视图（不暴露密码与连接串，只报可达性）
	api.GET("/infra/health", infraHealthHandler(inf.state))

	// ── HTTP 服务 ──
	addr := env("MC_ADDR", ":8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		// 公网部署必须限制请求头大小，防止超大 header 打内存
		MaxHeaderBytes: 1 << 20,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("gateway_started", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("gateway_shutting_down")
	}

	// 优雅退出：停止接收新连接，给在途请求最多 10s 收尾
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

// loginHandler 登录。
//
// 本工程只做演示，账号口令来自环境变量。真实系统应查用户表 + bcrypt 校验
// + 登录失败计数 + 图形验证码。这里保留结构：签发 → 回传 token，
// 前端把 token 放 Authorization 头。
func loginHandler(issuer *auth.Issuer) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Username string `json:"username" binding:"required"`
			Password string `json:"password" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "用户名或密码格式不正确"})
			return
		}
		user := env("MC_ADMIN_USER", "admin")
		pass := env("MC_ADMIN_PASS", "")
		if pass == "" || !auth.CheckPassword(req.Username, user) || !auth.CheckPassword(req.Password, pass) {
			// 不区分「用户不存在」与「密码错误」，避免账号枚举
			c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "用户名或密码错误"})
			return
		}
		tok, err := issuer.Issue(req.Username, "admin")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "登录失败"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "token": tok})
	}
}
