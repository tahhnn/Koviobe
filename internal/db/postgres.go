package db

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/notify"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

func InitPostgres() {
	cfg := config.AppConfig
	// connect_timeout bounds the dial; statement_timeout bounds every query the
	// pool runs, so a lock wait or a runaway scan cannot pin a request goroutine
	// forever. Both are server-side backstops — they do not replace per-request
	// context deadlines, they survive their absence.
	dsn := fmt.Sprintf(
		"host=%s user=%s password=%s dbname=%s port=%s sslmode=%s TimeZone=%s connect_timeout=%d statement_timeout=%d",
		cfg.DBHost, cfg.DBUser, cfg.DBPassword, cfg.DBName, cfg.DBPort, cfg.DBSSLMode,
		cfg.DBTimeZone, cfg.DBConnectTimeoutSec, cfg.DBStatementTimeoutMs,
	)

	logMode := logger.Warn
	if cfg.AppEnv == "development" || os.Getenv("GIN_MODE") == "debug" {
		logMode = logger.Info
	}

	var err error
	DB, err = gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logMode),
	})
	if err != nil {
		notify.Fatal("db_connect", "Không kết nối được PostgreSQL (%s:%s/%s): %v — container sẽ exit(1) và restart loop.",
			cfg.DBHost, cfg.DBPort, cfg.DBName, err)
		log.Fatalf("Failed to connect to PostgreSQL: %v", err)
	}

	// Without these the pool is unbounded on open connections and holds only 2
	// idle ones: under load the API both exhausts Postgres max_connections and
	// churns through reconnects at the same time.
	sqlDB, err := DB.DB()
	if err != nil {
		notify.Fatal("db_pool", "Không lấy được sql.DB để cấu hình pool: %v — container sẽ exit(1).", err)
		log.Fatalf("Failed to access underlying sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(cfg.DBMaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.DBMaxIdleConns)
	sqlDB.SetConnMaxLifetime(time.Duration(cfg.DBConnMaxLifetimeMin) * time.Minute)
	sqlDB.SetConnMaxIdleTime(time.Duration(cfg.DBConnMaxIdleMin) * time.Minute)

	log.Printf("PostgreSQL connection established successfully (max_open=%d max_idle=%d statement_timeout=%dms).",
		cfg.DBMaxOpenConns, cfg.DBMaxIdleConns, cfg.DBStatementTimeoutMs)
}

func AutoMigrate() {
	if DB == nil {
		log.Fatal("Database client is nil. Call InitPostgres first.")
	}

	// Auto migrate tables
	err := DB.AutoMigrate(
		&model.Permission{},
		&model.Role{},
		&model.User{},
		&model.Quiz{},
		&model.Question{},
		&model.Template{},
		&model.Room{},
		&model.Player{},
		&model.AnswerLog{},
		&model.GameSession{},
		&model.AuditLog{},
		&model.PricingPlan{},
		&model.Subscription{},
		&model.LicenseCode{},
		&model.LicenseRedemption{},
		&model.SubscriptionEvent{},
		&model.SystemSetting{},
	)
	if err != nil {
		notify.Fatal("db_migrate", "AutoMigrate thất bại: %v — schema có thể đang dở dang, container sẽ exit(1).", err)
		log.Fatalf("Auto migration failed: %v", err)
	}

	log.Println("Database auto-migration completed.")

	// Seed default data
	seedDefaultData()
}

func seedDefaultData() {
	// 1. Seed Permissions
	permissionsList := []model.Permission{
		{Name: "quiz:create", Description: "Allow creating quizzes"},
		{Name: "quiz:read", Description: "Allow reading quizzes"},
		{Name: "quiz:update", Description: "Allow updating quizzes"},
		{Name: "quiz:delete", Description: "Allow deleting quizzes"},
		{Name: "room:control", Description: "Allow controlling room sessions (start, next, end)"},
		{Name: "logs:read", Description: "Allow reading finished game reports and score logs"},
	}

	for i, perm := range permissionsList {
		var count int64
		DB.Model(&model.Permission{}).Where("name = ?", perm.Name).Count(&count)
		if count == 0 {
			if err := DB.Create(&permissionsList[i]).Error; err != nil {
				log.Printf("Failed to seed permission %s: %v", perm.Name, err)
			}
		}
	}
	log.Println("Permissions seeded.")

	// Fetch all permissions for seeding roles
	var allPerms []model.Permission
	DB.Find(&allPerms)

	// 2. Seed Roles
	var hostRole model.Role
	var adminRole model.Role

	// Seed Host Role
	var hostRoleCount int64
	DB.Model(&model.Role{}).Where("name = ?", "host").Count(&hostRoleCount)
	if hostRoleCount == 0 {
		hostRole = model.Role{
			Name:        "host",
			Description: "Quiz hosts who create quizzes and adjust room games",
			Permissions: allPerms,
		}
		if err := DB.Create(&hostRole).Error; err != nil {
			log.Printf("Failed to seed host role: %v", err)
		}
		log.Println("Host role seeded.")
	} else {
		DB.Preload("Permissions").Where("name = ?", "host").First(&hostRole)
	}

	// Seed Admin Role
	var adminRoleCount int64
	DB.Model(&model.Role{}).Where("name = ?", "admin").Count(&adminRoleCount)
	if adminRoleCount == 0 {
		adminRole = model.Role{
			Name:        "admin",
			Description: "Global administrators with full system access",
			Permissions: allPerms,
		}
		if err := DB.Create(&adminRole).Error; err != nil {
			log.Printf("Failed to seed admin role: %v", err)
		}
		log.Println("Admin role seeded.")
	} else {
		DB.Preload("Permissions").Where("name = ?", "admin").First(&adminRole)
	}

	seedPricingPlans()
	backfillFreeSubscriptions()
	seedFixedAdmin(adminRole)
	promoteAdminEmails(adminRole)

	// Optional extra host for local demos — only when SEED_DEV_USER=true
	if strings.EqualFold(os.Getenv("SEED_DEV_USER"), "true") {
		seedPassword := os.Getenv("SEED_DEV_PASSWORD")
		if seedPassword == "" {
			log.Println("SEED_DEV_USER=true but SEED_DEV_PASSWORD is empty — skipping seed user.")
			return
		}
		var seedUser model.User
		if err := DB.Where("email = ?", "user@email.com").First(&seedUser).Error; err != nil {
			hashedPassword, err := bcrypt.GenerateFromPassword([]byte(seedPassword), bcrypt.DefaultCost)
			if err != nil {
				log.Printf("Failed to hash seed password: %v", err)
				return
			}
			hostUser := model.User{
				RoleID:   &hostRole.ID,
				Email:    "user@email.com",
				Password: string(hashedPassword),
				Nickname: "Default Host",
			}
			if err := DB.Create(&hostUser).Error; err != nil {
				log.Printf("Failed to seed default user: %v", err)
				return
			}
			log.Println("Seeded default Host user (user@email.com) — DEV ONLY.")
			_ = ensureUserFreeSub(hostUser.ID)
		} else {
			seedUser.RoleID = &hostRole.ID
			if err := DB.Save(&seedUser).Error; err != nil {
				log.Printf("Failed to update seed user role: %v", err)
			}
			_ = ensureUserFreeSub(seedUser.ID)
		}
	}
}

// seedFixedAdmin creates/updates the optional platform bootstrap admin (idempotent).
// Both SEED_ADMIN_EMAIL and SEED_ADMIN_PASSWORD are required to enable bootstrap.
// Set SEED_ADMIN_RESET_PASSWORD=true to force-reset password on every boot.
func seedFixedAdmin(adminRole model.Role) {
	if adminRole.ID == 0 {
		return
	}
	email := strings.ToLower(strings.TrimSpace(os.Getenv("SEED_ADMIN_EMAIL")))
	password := os.Getenv("SEED_ADMIN_PASSWORD")
	if email == "" && password == "" {
		log.Println("Admin bootstrap disabled; SEED_ADMIN_EMAIL and SEED_ADMIN_PASSWORD are not set.")
		return
	}
	if email == "" || password == "" {
		log.Println("Admin bootstrap skipped; both SEED_ADMIN_EMAIL and SEED_ADMIN_PASSWORD are required.")
		return
	}
	resetPassword := strings.EqualFold(os.Getenv("SEED_ADMIN_RESET_PASSWORD"), "true")

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("Failed to hash admin seed password: %v", err)
		return
	}

	var user model.User
	err = DB.Where("email = ?", email).First(&user).Error
	if err != nil {
		user = model.User{
			RoleID:   &adminRole.ID,
			Email:    email,
			Password: string(hashedPassword),
			Nickname: "System Admin",
		}
		if err := DB.Create(&user).Error; err != nil {
			log.Printf("Failed to seed fixed admin %s: %v", email, err)
			return
		}
		_ = ensureUserFreeSub(user.ID)
		log.Printf("Seeded fixed admin account: %s", email)
		return
	}

	changed := false
	if user.RoleID == nil || *user.RoleID != adminRole.ID {
		user.RoleID = &adminRole.ID
		changed = true
	}
	if resetPassword {
		user.Password = string(hashedPassword)
		changed = true
	}
	if user.Nickname == "" {
		user.Nickname = "System Admin"
		changed = true
	}
	if changed {
		if err := DB.Save(&user).Error; err != nil {
			log.Printf("Failed to update fixed admin %s: %v", email, err)
			return
		}
		log.Printf("Updated fixed admin account: %s", email)
	} else {
		log.Printf("Fixed admin account ready: %s", email)
	}
	_ = ensureUserFreeSub(user.ID)
}

// promoteAdminEmails elevates emails listed in ADMIN_EMAILS (comma-separated) to admin role.
func promoteAdminEmails(adminRole model.Role) {
	if adminRole.ID == 0 {
		return
	}
	raw := strings.TrimSpace(os.Getenv("ADMIN_EMAILS"))
	if raw == "" {
		return
	}
	for _, part := range strings.Split(raw, ",") {
		email := strings.ToLower(strings.TrimSpace(part))
		if email == "" {
			continue
		}
		var user model.User
		if err := DB.Where("email = ?", email).First(&user).Error; err != nil {
			log.Printf("ADMIN_EMAILS: user %s not found — create the account first, then restart API.", email)
			continue
		}
		if user.RoleID != nil && *user.RoleID == adminRole.ID {
			continue
		}
		user.RoleID = &adminRole.ID
		if err := DB.Save(&user).Error; err != nil {
			log.Printf("ADMIN_EMAILS: failed to promote %s: %v", email, err)
			continue
		}
		log.Printf("Promoted %s to admin role.", email)
	}
}

func seedPricingPlans() {
	plans := []model.PricingPlan{
		{
			ID:                   "free",
			Name:                 "Chưa kích hoạt",
			Description:          "Tài khoản chưa mua dịch vụ. Liên hệ quản trị viên để được cấp gói.",
			PriceMonthlyVND:      0,
			MaxPlayersPerRoom:    0,
			MaxQuizzes:           0,
			MaxTemplates:         0,
			MaxConcurrentRooms:   0,
			MaxQuestionsPerQuiz:  0,
			AllowPlayerPaced:     false,
			AllowCustomBranding:  false,
			AllowExportLogs:      false,
			AllowPrioritySupport: false,
			AllowRemoveWatermark: false,
			IsActive:             true,
			SortOrder:            1,
		},
		{
			ID:                   "pro",
			Name:                 "Pro",
			Description:          "Dành cho sự kiện / doanh nghiệp. Tới 2000 người/phòng, không watermark, player-paced, export logs.",
			PriceMonthlyVND:      199000,
			MaxPlayersPerRoom:    2000,
			MaxQuizzes:           -1, // unlimited
			MaxTemplates:         -1,
			MaxConcurrentRooms:   10,
			MaxQuestionsPerQuiz:  100,
			AllowPlayerPaced:     true,
			AllowCustomBranding:  true,
			AllowExportLogs:      true,
			AllowPrioritySupport: true,
			AllowRemoveWatermark: true,
			IsActive:             true,
			SortOrder:            2,
		},
	}

	for _, p := range plans {
		var existing model.PricingPlan
		if err := DB.Where("id = ?", p.ID).First(&existing).Error; err != nil {
			if err := DB.Create(&p).Error; err != nil {
				log.Printf("Failed to seed plan %s: %v", p.ID, err)
			} else {
				log.Printf("Seeded pricing plan: %s", p.ID)
			}
		}
		// Do not overwrite admin-tuned limits on every boot — only create if missing
	}
}

func ensureUserFreeSub(userID uint) error {
	var count int64
	DB.Model(&model.Subscription{}).Where("user_id = ?", userID).Count(&count)
	if count > 0 {
		return nil
	}
	return DB.Create(&model.Subscription{
		UserID:   userID,
		PlanID:   "free",
		Status:   "active",
		StartsAt: time.Now(),
	}).Error
}

// backfillFreeSubscriptions gives every user without a subscription the free
// plan. It runs as a single set-based statement: the previous version loaded the
// entire users table into memory and issued one or two queries per row on every
// single boot, so startup cost and memory grew with the user count forever even
// though this is a one-time migration.
func backfillFreeSubscriptions() {
	res := DB.Exec(`
		INSERT INTO subscriptions (user_id, plan_id, status, starts_at, created_at, updated_at)
		SELECT u.id, 'free', 'active', NOW(), NOW(), NOW()
		FROM users u
		WHERE u.deleted_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM subscriptions s WHERE s.user_id = u.id)
	`)
	if res.Error != nil {
		log.Printf("Failed to backfill free subscriptions: %v", res.Error)
		return
	}
	if res.RowsAffected > 0 {
		log.Printf("Backfilled free subscriptions for %d user(s).", res.RowsAffected)
	}
}
