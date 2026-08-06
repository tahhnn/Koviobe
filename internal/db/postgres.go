package db

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/model"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

func InitPostgres() {
	cfg := config.AppConfig
	dsn := fmt.Sprintf("host=%s user=%s password=%s dbname=%s port=%s sslmode=%s TimeZone=Asia/Ho_Chi_Minh",
		cfg.DBHost, cfg.DBUser, cfg.DBPassword, cfg.DBName, cfg.DBPort, cfg.DBSSLMode)

	logMode := logger.Warn
	if cfg.AppEnv == "development" || os.Getenv("GIN_MODE") == "debug" {
		logMode = logger.Info
	}

	var err error
	DB, err = gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logMode),
	})
	if err != nil {
		log.Fatalf("Failed to connect to PostgreSQL: %v", err)
	}

	log.Println("PostgreSQL connection established successfully.")
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
	)
	if err != nil {
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

// seedFixedAdmin creates/updates the platform admin account (idempotent).
// Defaults: admin@quizzzone.local / QuizzZoneAdmin!2026 (override via SEED_ADMIN_EMAIL / SEED_ADMIN_PASSWORD).
// Set SEED_ADMIN_RESET_PASSWORD=true to force-reset password on every boot.
func seedFixedAdmin(adminRole model.Role) {
	if adminRole.ID == 0 {
		return
	}
	email := strings.ToLower(strings.TrimSpace(os.Getenv("SEED_ADMIN_EMAIL")))
	if email == "" {
		email = "admin@quizzzone.local"
	}
	password := os.Getenv("SEED_ADMIN_PASSWORD")
	if password == "" {
		password = "QuizzZoneAdmin!2026"
		log.Printf("SEED_ADMIN_PASSWORD unset — using default for %s (change in production).", email)
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
			Name:                 "Free",
			Description:          "Dành cho host cá nhân / lớp học nhỏ. Giới hạn 20 người/phòng.",
			PriceMonthlyVND:      0,
			MaxPlayersPerRoom:    20,
			MaxQuizzes:           20,
			MaxTemplates:         10,
			MaxConcurrentRooms:   1,
			MaxQuestionsPerQuiz:  30,
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
			Description:          "Dành cho sự kiện / doanh nghiệp. Tới 200 người/phòng, không watermark, player-paced, export logs.",
			PriceMonthlyVND:      199000,
			MaxPlayersPerRoom:    200,
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

func backfillFreeSubscriptions() {
	var users []model.User
	DB.Find(&users)
	for _, u := range users {
		_ = ensureUserFreeSub(u.ID)
	}
}
