package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/cache"
	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/cron"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/handler"
	"github.com/quizzzone/backend/internal/middleware"
	"github.com/quizzzone/backend/internal/pkg/license"
	"github.com/quizzzone/backend/internal/pkg/notify"
	"github.com/quizzzone/backend/internal/realtime"
	"github.com/quizzzone/backend/internal/telegrambot"
)

func main() {
	log.Println("Starting quizzZone API Gateway...")

	// 1. Load Configurations
	config.LoadConfig()

	// 1b. Alerting, before any service that can fail fatally — db.InitPostgres
	// and cache.InitRedis call log.Fatalf, and a notifier started after them
	// would miss exactly the failures worth waking someone for. LoadConfig
	// itself is still uncovered (it can exit before this line); the external
	// watchdog reports that case as a container restart.
	notify.Init(notify.Config{
		BotToken:     config.AppConfig.TelegramBotToken,
		ChatID:       config.AppConfig.TelegramChatID,
		ThreadID:     config.AppConfig.TelegramThreadID,
		Env:          config.AppConfig.AppEnv,
		FlushSeconds: config.AppConfig.TelegramFlushSeconds,
		MaxPerMinute: config.AppConfig.TelegramMaxPerMinute,
	})
	defer notify.Shutdown()

	// 2. Initialize Services
	db.InitPostgres()
	db.AutoMigrate()
	// After AutoMigrate (the table must exist) and before the expiry worker,
	// whose first sweep fires 20s after boot and is a no-op or not depending on
	// this flag.
	license.InitEnforcement()
	cache.InitRedis()
	realtime.InitCentrifugo()
	cron.StartCleanupWorker()
	cron.StartLicenseExpiryWorker()
	cron.StartLicenseWarningWorker()
	cron.StartSettingsRefreshWorker()
	cron.StartUploadCleanupWorker()
	cron.StartDigestWorker()
	// Read-only command bot. Started after the services it reports on, so
	// /status never answers about a half-initialised process.
	telegrambot.Start(telegrambot.Config{
		BotToken:     config.AppConfig.TelegramBotToken,
		ChatID:       config.AppConfig.TelegramChatID,
		ThreadID:     config.AppConfig.TelegramThreadID,
		AdminUserIDs: parseAdminIDs(config.AppConfig.TelegramAdminIDs),
	})

	// 3. Setup Gin Engine
	// [L-2 FIX] Default to release mode to suppress verbose debug logs in production.
	// Override by setting GIN_MODE=debug in .env during local development.
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode)
	}
	// gin.Default() is Logger + Recovery. Build the same chain by hand so the
	// recovery step can also raise an alert — a panic is the one failure no
	// handler anticipated, and it was previously only visible in stdout.
	r := gin.New()
	r.Use(gin.Logger())
	r.Use(middleware.AlertRecovery())
	r.Use(middleware.AlertServerErrors())

	// gin.Default() trusts every proxy, so ClientIP() — and with it the per-IP rate
	// limits in middleware.RateLimit — comes straight from a client-supplied
	// X-Forwarded-For. Trust only the gateway. TRUSTED_PROXIES is a comma-separated
	// list of CIDRs; the default covers Docker's default bridge range. Keep it as
	// narrow as the deployment allows — every trusted range is a range that can
	// forge X-Forwarded-For.
	trusted := strings.Split(getEnvOrDefault("TRUSTED_PROXIES", "172.16.0.0/12"), ",")
	for i := range trusted {
		trusted[i] = strings.TrimSpace(trusted[i])
	}
	if err := r.SetTrustedProxies(trusted); err != nil {
		log.Fatalf("Invalid TRUSTED_PROXIES: %v", err)
	}

	// Enable CORS Middleware
	r.Use(CORSMiddleware())
	// Translates the "error" field of 4xx/5xx responses into the caller's
	// language. Placed before the routes so it wraps every handler's writer.
	r.Use(middleware.Localize())

	// Health Check. /health is the container-local liveness probe; /api/health
	// is the same check reached through the gateway's /api/ upstream, which is
	// the path that actually breaks — nginx resolves an upstream hostname once
	// at config load, so recreating the backend leaves the gateway proxying a
	// dead IP while the frontend (a variable proxy_pass, re-resolved) keeps
	// answering 200. Only a probe that traverses /api/ sees that.
	r.GET("/health", healthHandler)
	r.GET("/api/health", healthHandler)

	// 4. API Routes Setup
	api := r.Group("/api")
	{
		// Public Authentication Routes (rate limited)
		auth := api.Group("/auth")
		auth.Use(middleware.AuthRateLimit())
		{
			auth.POST("/login", handler.Login)
			auth.POST("/register", handler.Register)
			auth.POST("/verify-otp", handler.VerifyOTP)
			auth.POST("/refresh", handler.RefreshToken)
			auth.POST("/logout", handler.Logout)
		}

		// Public plan catalog
		api.GET("/license/plans", handler.ListPlans)

		// Realtime tokens — require host JWT or player JWT (no open public minting)
		api.GET("/realtime/token", handler.GetRealtimeToken)
		api.GET("/realtime/player-token", handler.GetPlayerRealtimeToken)

		// Public Player Room Joining & Actions
		api.POST("/rooms/join", middleware.JoinRoomRateLimit(), handler.JoinRoom)
		api.POST("/rooms/submit-answer", middleware.SubmitAnswerRateLimit(), handler.SubmitAnswer)
		api.GET("/rooms/public", handler.ListPublicRooms) // before /:id so "public" is not captured as id
		api.GET("/rooms/pin/:pin", middleware.PinLookupRateLimit(), handler.GetRoomByPin)
		api.GET("/rooms/:id", handler.GetRoom) // Auth handled inside handler (host JWT or player token)
		api.GET("/rooms/:id/questions/:index", handler.GetPlayerQuestion)
		api.GET("/rooms/:id/results", handler.GetRoomResults) // Auth handled inside handler (host JWT or player token)
		// Leaderboard slide between questions — top rows + the caller's own rank.
		// Auth handled inside handler (host JWT or player token).
		api.GET("/rooms/:id/standings", handler.GetRoomStandings)
		api.POST("/rooms/:id/leave", handler.LeaveRoom)

		// Private Routes (Requires Auth)
		private := api.Group("")
		private.Use(middleware.AuthMiddleware())
		{
			// Host Profile
			private.GET("/auth/profile", handler.GetProfile)
			private.POST("/auth/change-password", handler.ChangePassword)
			private.GET("/realtime/host-token", handler.GetRealtimeToken)

			// License / subscription — hosts can only view their own entitlements.
			// Plan changes are admin-only (no self-upgrade).
			private.GET("/license/me", handler.GetMyLicense)
			// Self-service activation: a host applies a prepaid code to their own
			// account. Rate limited because the code is the only secret.
			private.POST("/license/redeem", middleware.RedeemRateLimit(), handler.RedeemLicenseCode)

			adminLicense := private.Group("/admin/license")
			adminLicense.Use(middleware.RequireRole("admin"))
			{
				adminLicense.GET("/plans", handler.AdminListPlans)
				adminLicense.PUT("/plans/:id", handler.AdminUpdatePlan)
				adminLicense.GET("/subscriptions", handler.AdminListSubscriptions)
				adminLicense.POST("/assign", handler.AdminAssignPlan)
				adminLicense.POST("/codes", handler.AdminCreateLicenseCodes)
				adminLicense.GET("/codes", handler.AdminListLicenseCodes)
				adminLicense.POST("/codes/:code/revoke", handler.AdminRevokeLicenseCode)
				adminLicense.POST("/codes/:code/claw-back", handler.AdminClawBackLicenseCode)
				adminLicense.GET("/redemptions", handler.AdminListRedemptions)
				adminLicense.POST("/codes/send", handler.AdminSendLicenseCodes)
				adminLicense.GET("/history", handler.AdminListSubscriptionHistory)
				adminLicense.GET("/history.csv", handler.AdminExportSubscriptionHistory)
				adminLicense.GET("/enforcement", handler.AdminGetEnforcement)
				adminLicense.PUT("/enforcement", handler.AdminSetEnforcement)
			}

			adminUsers := private.Group("/admin/users")
			adminUsers.Use(middleware.RequireRole("admin"))
			{
				adminUsers.GET("", handler.AdminListUsers)
				adminUsers.PATCH("/:id/role", handler.AdminUpdateUserRole)
				adminUsers.PATCH("/:id/status", handler.AdminUpdateUserStatus)
			}

			// Quiz & Question Management (Host/Admin)
			quizzes := private.Group("/quizzes")
			quizzes.Use(middleware.RequireRole("host", "admin"))
			{
				quizzes.POST("", middleware.RequirePermission("quiz:create"), handler.CreateQuiz)
				quizzes.GET("", middleware.RequirePermission("quiz:read"), handler.ListQuizzes)
				quizzes.GET("/:id", middleware.RequirePermission("quiz:read"), handler.GetQuiz)
				quizzes.PUT("/:id", middleware.RequirePermission("quiz:update"), handler.UpdateQuiz)
				quizzes.DELETE("/:id", middleware.RequirePermission("quiz:delete"), handler.DeleteQuiz)
			}

			// Room Control Management (Host/Admin)
			rooms := private.Group("/rooms")
			rooms.Use(middleware.RequireRole("host", "admin"))
			{
				rooms.POST("", middleware.RequirePermission("room:control"), handler.CreateRoom)
				rooms.PATCH("/:id/privacy", middleware.RequirePermission("room:control"), handler.UpdateRoomPrivacy)
				rooms.POST("/:id/start", middleware.RequirePermission("room:control"), handler.StartGame)
				rooms.POST("/:id/next", middleware.RequirePermission("room:control"), handler.NextQuestion)
				rooms.POST("/:id/end-question", middleware.RequirePermission("room:control"), handler.EndQuestion)
				rooms.POST("/:id/explain", middleware.RequirePermission("room:control"), handler.ExplainQuestion)
				rooms.POST("/:id/leaderboard", middleware.RequirePermission("room:control"), handler.ShowLeaderboard)
				rooms.POST("/:id/end", middleware.RequirePermission("room:control"), handler.EndGame)
			}

			// Question Bank (Template packs) — save/remix questions into Quizzes
			templates := private.Group("/templates")
			templates.Use(middleware.RequireRole("host", "admin"))
			{
				templates.POST("", middleware.RequirePermission("quiz:create"), handler.CreateTemplate)
				templates.POST("/from-quiz/:quizId", middleware.RequirePermission("quiz:create"), handler.CreateTemplateFromQuiz)
				templates.GET("", middleware.RequirePermission("quiz:read"), handler.ListTemplates)
				templates.GET("/:id", middleware.RequirePermission("quiz:read"), handler.GetTemplate)
				templates.PUT("/:id", middleware.RequirePermission("quiz:update"), handler.UpdateTemplate)
				templates.POST("/:id/instantiate", middleware.RequirePermission("quiz:create"), handler.InstantiateTemplate)
				templates.POST("/:id/import", middleware.RequirePermission("quiz:update"), handler.ImportBankQuestions)
				templates.DELETE("/:id", middleware.RequirePermission("quiz:delete"), handler.DeleteTemplate)
			}

			// Play History & Reporting (Host/Admin)
			logs := private.Group("/logs")
			logs.Use(middleware.RequireRole("host", "admin"))
			{
				logs.GET("", middleware.RequirePermission("logs:read"), handler.ListLogs)
				logs.GET("/:id", middleware.RequirePermission("logs:read"), handler.GetRoomLogs)
				logs.GET("/:id/export", middleware.RequirePermission("logs:read"), handler.ExportRoomLogs)
			}
		}
	}

	// 5. Start Server
	port := config.AppConfig.Port
	log.Printf("quizzZone API Gateway running on port %s", port)
	// A start line is how the chat distinguishes a planned deploy from a crash
	// loop: one of these is a release, five in a minute is an incident.
	notify.P1("api_started", "API Gateway khởi động xong trên cổng %s (env=%s).", port, config.AppConfig.AppEnv)
	if err := r.Run(":" + port); err != nil {
		notify.Fatal("api_listen_failed", "Không mở được cổng %s: %v — API không phục vụ request nào.", port, err)
		log.Fatalf("Server failed to start: %v", err)
	}
}

// parseAdminIDs turns "123,456" into a lookup set. A malformed entry is logged
// and skipped rather than fatal: a typo here must not stop the API from
// booting, and the chat-id check is still in force.
func parseAdminIDs(raw string) map[int64]bool {
	out := map[int64]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			log.Printf("WARN: TELEGRAM_ADMIN_IDS contains a non-numeric entry %q — skipped", part)
			continue
		}
		out[id] = true
	}
	return out
}

// healthHandler reports readiness, not just liveness: a process that is up but
// cannot reach Postgres or Redis serves errors on every real request, and a
// 200 there would tell the watchdog everything is fine.
func healthHandler(c *gin.Context) {
	checks := gin.H{"api": "ok"}
	status := http.StatusOK

	if sqlDB, err := db.DB.DB(); err != nil || sqlDB.PingContext(c.Request.Context()) != nil {
		checks["postgres"] = "down"
		status = http.StatusServiceUnavailable
	} else {
		checks["postgres"] = "ok"
	}

	if cache.RDB == nil || cache.RDB.Ping(c.Request.Context()).Err() != nil {
		checks["redis"] = "down"
		status = http.StatusServiceUnavailable
	} else {
		checks["redis"] = "ok"
	}

	c.JSON(status, gin.H{"status": map[bool]string{true: "healthy", false: "degraded"}[status == http.StatusOK], "checks": checks})
}

// CORSMiddleware enforces a strict origin whitelist from CORS_ORIGINS env
// (comma-separated), with safe localhost defaults for local development.
func CORSMiddleware() gin.HandlerFunc {
	allowedOrigins := map[string]bool{
		"http://localhost:3000": true,
		"http://localhost:5173": true,
		"http://127.0.0.1:3000": true,
	}
	if extra := os.Getenv("CORS_ORIGINS"); extra != "" {
		for _, o := range strings.Split(extra, ",") {
			o = strings.TrimSpace(o)
			if o != "" {
				allowedOrigins[o] = true
			}
		}
	}

	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")

		if allowedOrigins[origin] {
			c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
			c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		}

		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With, X-Player-Token, X-Locale")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT, DELETE")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

func getEnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
