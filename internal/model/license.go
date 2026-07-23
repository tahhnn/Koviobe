package model

import (
	"time"

	"gorm.io/gorm"
)

// PricingPlan defines subscription tiers and their entitlements.
type PricingPlan struct {
	ID                   string         `gorm:"primaryKey;size:50" json:"id"` // free | pro
	Name                 string         `gorm:"size:100;not null" json:"name"`
	Description          string         `gorm:"type:text" json:"description"`
	PriceMonthlyVND      int            `gorm:"default:0" json:"price_monthly_vnd"`
	MaxPlayersPerRoom    int            `gorm:"not null" json:"max_players_per_room"`
	MaxQuizzes           int            `gorm:"not null" json:"max_quizzes"`            // -1 = unlimited
	MaxTemplates         int            `gorm:"not null" json:"max_templates"`          // -1 = unlimited
	MaxConcurrentRooms   int            `gorm:"not null" json:"max_concurrent_rooms"`   // waiting+active
	MaxQuestionsPerQuiz  int            `gorm:"not null;default:50" json:"max_questions_per_quiz"`
	// Feature flags (Pro benefits)
	AllowPlayerPaced     bool           `gorm:"default:false" json:"allow_player_paced"`
	AllowCustomBranding  bool           `gorm:"default:false" json:"allow_custom_branding"`
	AllowExportLogs      bool           `gorm:"default:false" json:"allow_export_logs"`
	AllowPrioritySupport bool           `gorm:"default:false" json:"allow_priority_support"`
	AllowRemoveWatermark bool           `gorm:"default:false" json:"allow_remove_watermark"`
	IsActive             bool           `gorm:"default:true" json:"is_active"`
	SortOrder            int            `gorm:"default:0" json:"sort_order"`
	CreatedAt            time.Time      `json:"created_at"`
	UpdatedAt            time.Time      `json:"updated_at"`
	DeletedAt            gorm.DeletedAt `gorm:"index" json:"-"`
}

// Subscription binds a host user to a pricing plan.
type Subscription struct {
	ID        uint           `gorm:"primaryKey" json:"id"`
	UserID    uint           `gorm:"uniqueIndex;not null" json:"user_id"`
	PlanID    string         `gorm:"size:50;index;not null" json:"plan_id"`
	Status    string         `gorm:"size:50;default:'active'" json:"status"` // active, canceled, expired
	StartsAt  time.Time      `json:"starts_at"`
	EndsAt    *time.Time     `json:"ends_at,omitempty"` // nil = no expiry
	Plan      *PricingPlan   `gorm:"foreignKey:PlanID" json:"plan,omitempty"`
	User      *User          `gorm:"foreignKey:UserID" json:"-"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}
