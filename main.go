package main

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/HenZenKuriRIP/k2pay/config"
	"github.com/HenZenKuriRIP/k2pay/internal/handler"
	"github.com/HenZenKuriRIP/k2pay/internal/middleware"
	"github.com/HenZenKuriRIP/k2pay/internal/model"
	"github.com/HenZenKuriRIP/k2pay/internal/payment"
	"github.com/HenZenKuriRIP/k2pay/internal/service"
	"github.com/HenZenKuriRIP/k2pay/internal/util"
	"github.com/HenZenKuriRIP/k2pay/web"

	"github.com/gin-gonic/gin"
)

// 版本信息（在编译时通过 -ldflags 注入）
var (
	Version   = "dev"
	BuildDate = "unknown"
)

func main() {
	// 加载配置
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// 初始化数据库（使用配置的连接池参数）
	dbConfig := model.DBConfig{
		MaxOpenConns:    cfg.Database.MaxOpenConns,
		MaxIdleConns:    cfg.Database.MaxIdleConns,
		ConnMaxLifetime: time.Duration(cfg.Database.ConnMaxLifetime) * time.Minute,
		ConnMaxIdleTime: 10 * time.Minute,
	}
	if err := model.InitDBWithConfig(cfg.Database.DSN(), dbConfig); err != nil {
		log.Fatalf("Failed to init database: %v", err)
	}

	// 初始化服务
	initServices(cfg)

	// 官方支付默认配置键
	payment.EnsureDefaultConfigs()

	// 订单轮询令牌密钥（与 JWT 同源，部署时务必修改 jwt.secret）
	util.InitOrderPollSecret(cfg.JWT.Secret)

	// 设置Gin模式
	gin.SetMode(gin.ReleaseMode)

	// 创建路由
	r := gin.Default()

	// 加载模板和静态文件 (根据构建模式自动选择嵌入或文件系统)
	funcMap := template.FuncMap{
		"safeHTML": func(s string) template.HTML { return template.HTML(s) },
	}
	if err := web.LoadTemplates(r, funcMap); err != nil {
		log.Fatalf("Failed to load templates: %v", err)
	}
	if err := web.SetupStatic(r, cfg.Storage.DataDir); err != nil {
		log.Fatalf("Failed to setup static files: %v", err)
	}

	// 打印运行模式
	if web.IsEmbedded() {
		log.Println("Running in RELEASE mode (embedded resources)")
	} else {
		log.Println("Running in DEV mode (filesystem resources)")
	}

	// 注册路由
	registerRoutes(r, cfg)

	// 启动后台服务
	startBackgroundServices(cfg)

	// 启动服务器
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	log.Printf("K2Pay v%s (built %s) starting on %s", Version, BuildDate, addr)

	// 优雅关闭
	go func() {
		if err := r.Run(addr); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// 等待中断信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")
	service.GetBlockchainService().Stop()
	log.Println("Server exited")
}

// initServices 初始化服务
func initServices(cfg *config.Config) {
	// 初始化安全配置
	middleware.InitRateLimiters(
		cfg.Security.RateLimitAPI,
		cfg.Security.RateLimitAPIBurst,
		cfg.Security.RateLimitLogin,
		cfg.Security.RateLimitBurst,
	)
	middleware.SetIPBlacklistCacheTTL(cfg.Security.IPBlacklistCacheTTL)

	// 初始化区块链服务（使用配置的钱包缓存TTL）
	service.GetBlockchainService().Init(cfg)
	service.GetBlockchainService().SetWalletCacheTTL(cfg.Order.WalletCacheTTL)
	// 动态链（管理端新增）纳入 IsValidChain 校验
	util.SetDynamicChainValidator(func(chain string) bool {
		return service.GetBlockchainService().IsValidChainDynamic(chain)
	})

	// 初始化汇率服务
	rateService := service.GetRateService()
	rateService.SetCacheSeconds(cfg.Rate.CacheSeconds)
}

// registerRoutes 注册路由
func registerRoutes(r *gin.Engine, cfg *config.Config) {
	// CORS（使用配置的域名白名单）
	r.Use(middleware.CORSWithConfig(cfg.Security.CORSAllowOrigins))

	// 创建处理器
	epayHandler := handler.NewEpayHandler()
	adminHandler := handler.NewAdminHandler(cfg)
	merchantHandler := handler.NewMerchantHandler(cfg)
	cashierHandler := handler.NewCashierHandler()
	channelHandler := handler.NewChannelHandler()
	rateHandler := handler.NewRateHandler()

	// ============ 支付开放 API（易支付协议语义，REST 路径） ============
	paymentAPI := r.Group("/api/pay")
	paymentAPI.Use(middleware.IPBlacklistCheck())
	paymentAPI.Use(middleware.RateLimit())
	paymentAPI.Use(middleware.APILogger())
	{
		// 表单跳转下单（原 submit）
		paymentAPI.Any("/submit", epayHandler.Submit)
		// JSON 下单（原 mapi）
		paymentAPI.Any("/create", epayHandler.MAPISubmit)
		// 商户信息 / 查单 / 列表 / 结算 / 退款（原 api.php?act=）
		// 支持: /api/pay/merchant|order|orders|settle|refund 或 ?act=
		paymentAPI.Any("/merchant", epayHandler.API)
		paymentAPI.Any("/order", epayHandler.API)
		paymentAPI.Any("/orders", epayHandler.API)
		paymentAPI.Any("/settle", epayHandler.API)
		paymentAPI.Any("/refund", epayHandler.API)
		paymentAPI.Any("", epayHandler.API) // ?act= 兼容

		// 收银台轮询
		paymentAPI.GET("/check", epayHandler.CheckOrder)
		// 支付方式列表
		paymentAPI.GET("/types", epayHandler.GetPaymentTypes)
		// 选择支付方式
		paymentAPI.POST("/:trade_no/select-type", epayHandler.SelectPaymentType)
	}

	// ============ 收银台 ============
	r.GET("/cashier/:trade_no", cashierHandler.ShowCashier)
	r.GET("/api/cashier/order/:trade_no", cashierHandler.GetOrderInfo)

	// ============ 上游/官方通道回调 ============
	r.GET("/api/channel/notify/epay", channelHandler.EpayNotify)
	r.POST("/api/channel/notify/epay", channelHandler.EpayNotify)
	officialPayHandler := handler.NewOfficialPayHandler()
	r.POST("/api/channel/notify/alipay", officialPayHandler.AlipayNotify)
	r.POST("/api/channel/notify/wechat", officialPayHandler.WechatNotify)

	// ============ Telegram Webhook ============
	telegramHandler := handler.NewTelegramHandler()
	r.POST("/telegram/webhook", telegramHandler.HandleWebhook)

	// ============ 管理后台 ============
	// 管理后台页面
	r.GET("/admin", func(c *gin.Context) {
		c.HTML(http.StatusOK, "admin.html", nil)
	})

	// 登录 (无需认证，严格限流防撞库)
	r.POST("/admin/api/login", middleware.LoginRateLimit(), adminHandler.Login)

	// 需要认证的管理API
	adminAPI := r.Group("/admin/api")
	adminAPI.Use(middleware.AdminAuth(cfg))
	{
		// 仪表盘
		adminAPI.GET("/dashboard", adminHandler.Dashboard)
		adminAPI.GET("/dashboard/trend", adminHandler.DashboardTrend)
		adminAPI.GET("/dashboard/top", adminHandler.DashboardTop)
		adminAPI.GET("/dashboard/system", adminHandler.GetSystemMetrics)

		// 订单管理
		adminAPI.GET("/orders", adminHandler.ListOrders)
		adminAPI.GET("/orders/export", adminHandler.ExportOrders)
		adminAPI.GET("/orders/:trade_no", adminHandler.GetOrder)
		adminAPI.POST("/orders/:trade_no/paid", adminHandler.MarkOrderPaid)
		adminAPI.POST("/orders/:trade_no/notify", adminHandler.RetryNotify)
		adminAPI.POST("/orders/test", adminHandler.CreateTestOrder)
		adminAPI.POST("/orders/clean", adminHandler.CleanInvalidOrders)

		// 商户管理
		adminAPI.GET("/merchants", adminHandler.ListMerchants)
		adminAPI.POST("/merchants", adminHandler.CreateMerchant)
		adminAPI.PUT("/merchants/:id", adminHandler.UpdateMerchant)
		adminAPI.GET("/merchants/:id/key", adminHandler.GetMerchantKey)
		adminAPI.POST("/merchants/:id/reset-key", adminHandler.ResetMerchantKey)
		adminAPI.POST("/merchants/:id/balance", adminHandler.AdjustMerchantBalance)

		// 钱包管理
		adminAPI.GET("/wallets", adminHandler.ListWallets)
		adminAPI.POST("/wallets", adminHandler.CreateWallet)
		adminAPI.PUT("/wallets/:id", adminHandler.UpdateWallet)
		adminAPI.DELETE("/wallets/:id", adminHandler.DeleteWallet)
		adminAPI.POST("/upload/qrcode", adminHandler.UploadQRCode)

		// 汇率管理
		adminAPI.GET("/exchange-rates", rateHandler.ListExchangeRates)
		adminAPI.PUT("/exchange-rates/:id", rateHandler.UpdateExchangeRate)
		adminAPI.POST("/exchange-rates/refresh", rateHandler.RefreshAutoRates)
		adminAPI.GET("/exchange-rates/float", rateHandler.GetFloatSettings)
		adminAPI.POST("/exchange-rates/float", rateHandler.UpdateFloatSettings)

		// 系统配置
		adminAPI.GET("/configs", adminHandler.GetConfigs)
		adminAPI.POST("/configs", adminHandler.UpdateConfigs)

		// 官方支付宝/微信支付配置
		adminAPI.GET("/official-pay", officialPayHandler.GetOfficialPayConfig)
		adminAPI.POST("/official-pay", officialPayHandler.UpdateOfficialPayConfig)
		adminAPI.POST("/official-pay/test-alipay", officialPayHandler.TestAlipayConfig)
		adminAPI.POST("/official-pay/test-wechat", officialPayHandler.TestWechatConfig)

		// 汇率
		adminAPI.GET("/rate", adminHandler.GetRate)
		adminAPI.POST("/rate/refresh", adminHandler.RefreshRate)

		// 交易日志
		adminAPI.GET("/transactions", adminHandler.GetTransactionLogs)

		// API调用日志
		adminAPI.GET("/api-logs", adminHandler.GetAPILogs)
		adminAPI.POST("/api-logs/clean", adminHandler.CleanAPILogs)

		// IP黑名单管理
		adminAPI.GET("/ip-blacklist", adminHandler.ListIPBlacklist)
		adminAPI.POST("/ip-blacklist", adminHandler.AddIPBlacklist)
		adminAPI.DELETE("/ip-blacklist/:id", adminHandler.RemoveIPBlacklist)
		adminAPI.POST("/ip-blacklist/block", adminHandler.BlockIPFromAPILog)

		// 修改密码
		adminAPI.POST("/password", adminHandler.ChangePassword)

		// 测试机器人通知
		adminAPI.POST("/test-bot", func(c *gin.Context) {
			if err := service.GetBotService().SendTestMessage(); err != nil {
				c.JSON(http.StatusOK, gin.H{"code": -1, "msg": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"code": 1, "msg": "已发送测试消息"})
		})

		// 测试Telegram Bot连接
		adminAPI.POST("/telegram/test", adminHandler.TestTelegramBot)

		// 查询Telegram Webhook状态
		adminAPI.GET("/telegram/webhook-info", func(c *gin.Context) {
			info, err := service.GetTelegramService().GetWebhookInfo()
			if err != nil {
				c.JSON(http.StatusOK, gin.H{"code": -1, "msg": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{
				"code": 1,
				"data": gin.H{
					"mode":    service.GetTelegramService().GetMode(),
					"enabled": service.GetTelegramService().IsEnabled(),
					"webhook": info,
				},
			})
		})

		// 链监控管理
		adminAPI.GET("/chains", adminHandler.GetChainStatus)
		adminAPI.POST("/chains", adminHandler.CreateChain)
		adminAPI.PUT("/chains/:chain", adminHandler.UpdateChainConfig)
		adminAPI.DELETE("/chains/:chain", adminHandler.DeleteChain)
		adminAPI.POST("/chains/:chain/enable", adminHandler.EnableChain)
		adminAPI.POST("/chains/:chain/disable", adminHandler.DisableChain)
		adminAPI.POST("/chains/batch", adminHandler.BatchUpdateChains)
		adminAPI.GET("/chains/health", adminHandler.CheckAllChainsHealth)
		adminAPI.GET("/chains/:chain/health", adminHandler.CheckChainHealth)

		// 提现管理
		adminAPI.GET("/withdrawals", adminHandler.ListWithdrawals)
		adminAPI.POST("/withdrawals/:id/approve", adminHandler.ApproveWithdrawal)
		adminAPI.POST("/withdrawals/:id/reject", adminHandler.RejectWithdrawal)
		adminAPI.POST("/withdrawals/:id/complete", adminHandler.CompleteWithdrawal)

		// 提现地址审核
		adminAPI.GET("/withdraw-addresses", adminHandler.ListWithdrawAddresses)
		adminAPI.POST("/withdraw-addresses/:id/approve", adminHandler.ApproveWithdrawAddress)
		adminAPI.POST("/withdraw-addresses/:id/reject", adminHandler.RejectWithdrawAddress)

		// APP版本管理
		adminAPI.GET("/app-versions", adminHandler.ListAppVersions)
		adminAPI.POST("/app-versions", adminHandler.UploadAppVersion)
		adminAPI.PUT("/app-versions/:id", adminHandler.UpdateAppVersion)
		adminAPI.DELETE("/app-versions/:id", adminHandler.DeleteAppVersion)
	}

	// ============ 商户后台 ============
	// 商户后台页面
	r.GET("/merchant", func(c *gin.Context) {
		c.HTML(http.StatusOK, "merchant.html", nil)
	})

	// 商户登录 (无需认证，严格限流防撞库)
	r.POST("/merchant/api/login", middleware.LoginRateLimit(), merchantHandler.Login)

	// 需要认证的商户API
	merchantAPI := r.Group("/merchant/api")
	merchantAPI.Use(middleware.MerchantAuth(cfg))
	{
		// 仪表盘
		merchantAPI.GET("/dashboard", merchantHandler.Dashboard)
		merchantAPI.GET("/dashboard/trend", merchantHandler.DashboardTrend)

		// 个人信息
		merchantAPI.GET("/profile", merchantHandler.GetProfile)
		merchantAPI.PUT("/profile", merchantHandler.UpdateProfile)
		merchantAPI.POST("/password", merchantHandler.ChangePassword)

		// API密钥
		merchantAPI.GET("/key", merchantHandler.GetKey)
		merchantAPI.POST("/key/reset", merchantHandler.ResetKey)

		// 订单管理
		merchantAPI.GET("/orders", merchantHandler.ListOrders)
		merchantAPI.GET("/orders/:trade_no", merchantHandler.GetOrder)
		merchantAPI.POST("/orders/:trade_no/confirm", merchantHandler.ConfirmPayment)
		merchantAPI.POST("/orders/:trade_no/cancel", merchantHandler.CancelOrder)
		merchantAPI.POST("/orders/test", merchantHandler.CreateTestOrder)

		// 钱包管理
		merchantAPI.GET("/wallets", merchantHandler.ListWallets)
		merchantAPI.POST("/wallets", merchantHandler.CreateWallet)
		merchantAPI.PUT("/wallets/:id", merchantHandler.UpdateWallet)
		merchantAPI.DELETE("/wallets/:id", merchantHandler.DeleteWallet)
		merchantAPI.POST("/upload/qrcode", merchantHandler.UploadQRCode)

		// 链状态 (只读)
		merchantAPI.GET("/chains", merchantHandler.GetChainStatus)

		// 提现管理
		merchantAPI.GET("/balance", merchantHandler.GetBalance)
		merchantAPI.GET("/recharge-addresses", merchantHandler.GetRechargeAddresses)
		merchantAPI.GET("/withdrawals", merchantHandler.ListWithdrawals)
		merchantAPI.POST("/withdrawals", merchantHandler.CreateWithdrawal)

		// 提现地址管理
		merchantAPI.GET("/withdraw-addresses", merchantHandler.ListWithdrawAddresses)
		merchantAPI.POST("/withdraw-addresses", merchantHandler.CreateWithdrawAddress)
		merchantAPI.PUT("/withdraw-addresses/:id", merchantHandler.UpdateWithdrawAddress)
		merchantAPI.DELETE("/withdraw-addresses/:id", merchantHandler.DeleteWithdrawAddress)

		// 钱包模式设置
		merchantAPI.GET("/wallet-mode", merchantHandler.GetWalletMode)
		merchantAPI.PUT("/wallet-mode", merchantHandler.UpdateWalletMode)

		// Telegram Bot 信息
		merchantAPI.GET("/telegram-bot", merchantHandler.GetTelegramBotInfo)

		// 通知设置
		merchantAPI.GET("/notify-settings", merchantHandler.GetNotifySettings)
		merchantAPI.PUT("/notify-settings", merchantHandler.UpdateNotifySettings)

		// 监控/服务配置（App 下载信息等）
		merchantAPI.GET("/monitor-config", merchantHandler.GetMonitorConfig)
	}

	// 健康检查 - 简单版本（用于负载均衡器）
	r.GET("/health", func(c *gin.Context) {
		// 快速检查数据库连接
		if err := model.CheckDBHealth(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "unhealthy",
				"error":  "database connection failed",
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
		})
	})

	// 健康检查 - 详细版本（用于监控系统）
	r.GET("/health/detail", func(c *gin.Context) {
		health := gin.H{
			"status":    "ok",
			"timestamp": time.Now().Format(time.RFC3339),
			"version":   Version,
		}

		// 检查数据库
		dbStatus := "ok"
		if err := model.CheckDBHealth(); err != nil {
			dbStatus = "error: " + err.Error()
			health["status"] = "degraded"
		}
		health["database"] = gin.H{
			"status": dbStatus,
			"stats":  model.GetDBStats(),
		}

		// 检查区块链服务
		blockchainStatus := service.GetBlockchainService().GetListenerStatus()
		enabledChains := 0
		runningChains := 0
		for _, status := range blockchainStatus {
			if s, ok := status.(map[string]interface{}); ok {
				if enabled, _ := s["enabled"].(bool); enabled {
					enabledChains++
				}
				if running, _ := s["running"].(bool); running {
					runningChains++
				}
			}
		}
		health["blockchain"] = gin.H{
			"enabled_chains": enabledChains,
			"running_chains": runningChains,
		}

		// 返回状态码
		statusCode := http.StatusOK
		if health["status"] == "degraded" {
			statusCode = http.StatusOK // 降级但仍可用
		} else if health["status"] == "unhealthy" {
			statusCode = http.StatusServiceUnavailable
		}

		c.JSON(statusCode, health)
	})

	// ============ APP 公开接口 ============
	r.GET("/api/app/version", adminHandler.GetLatestAppVersion)
	r.GET("/api/app/download", adminHandler.DownloadApp)
}

// startBackgroundServices 启动后台服务
func startBackgroundServices(cfg *config.Config) {
	// 启动区块链监控
	service.GetBlockchainService().Start()

	// 启动订单过期处理
	service.GetOrderService().StartExpireWorker()

	// 启动通知重试
	service.GetNotifyService().StartNotifyWorker()

	// 启动汇率自动更新（根据配置决定是否启用）
	if cfg.Rate.AutoUpdateEnabled {
		rateUpdater := service.NewRateUpdater()
		rateUpdater.Start()
		log.Printf("汇率自动更新已启用，更新间隔: %d 分钟", cfg.Rate.UpdateInterval)
	} else {
		log.Println("汇率自动更新已禁用")
	}

	// 启动每日报告
	service.GetBotService().StartDailyReportWorker()

	// 启动Telegram通知服务 - 从数据库加载配置
	var configs []model.SystemConfig
	model.GetDB().Where(`"key" IN (?)`, []string{
		"telegram_enabled",
		"telegram_bot_token",
		"telegram_mode",
		"telegram_webhook_url",
		"telegram_webhook_secret",
	}).Find(&configs)

	// 解析配置
	configMap := make(map[string]string)
	for _, cfg := range configs {
		configMap[cfg.Key] = cfg.Value
	}

	// 获取配置值
	enabled := configMap["telegram_enabled"] == "1"
	botToken := configMap["telegram_bot_token"]
	mode := configMap["telegram_mode"]
	if mode == "" {
		mode = "polling" // 默认轮询模式
	}

	// Webhook URL：如果配置中有值则使用，否则根据服务器配置自动生成
	webhookURL := configMap["telegram_webhook_url"]
	if webhookURL == "" && mode == "webhook" {
		// 自动生成 webhook URL
		protocol := "https"
		if cfg.Server.Host == "localhost" || cfg.Server.Host == "127.0.0.1" {
			protocol = "http"
		}
		host := cfg.Server.Host
		if cfg.Server.Port != 80 && cfg.Server.Port != 443 {
			webhookURL = fmt.Sprintf("%s://%s:%d/telegram/webhook", protocol, host, cfg.Server.Port)
		} else {
			webhookURL = fmt.Sprintf("%s://%s/telegram/webhook", protocol, host)
		}
		log.Printf("[Telegram] 自动生成 Webhook URL: %s", webhookURL)
	}

	// 获取 webhook secret
	webhookSecret := configMap["telegram_webhook_secret"]

	// 更新 Telegram 服务配置
	if enabled && botToken != "" {
		service.GetTelegramService().UpdateFullConfig(enabled, botToken, mode, webhookURL, webhookSecret)
	}
	service.GetTelegramService().Start()

	log.Println("Background services started")
}
