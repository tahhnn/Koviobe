package model

import (
	"time"

	"gorm.io/gorm"
)

// PricingPlan defines subscription tiers and their entitlements.
type PricingPlan struct {
	ID                  string `gorm:"primaryKey;size:50" json:"id"` // free | pro
	Name                string `gorm:"size:100;not null" json:"name"`
	Description         string `gorm:"type:text" json:"description"`
	PriceMonthlyVND     int    `gorm:"default:0" json:"price_monthly_vnd"`
	MaxPlayersPerRoom   int    `gorm:"not null" json:"max_players_per_room"`
	MaxQuizzes          int    `gorm:"not null" json:"max_quizzes"`          // -1 = unlimited
	MaxTemplates        int    `gorm:"not null" json:"max_templates"`        // -1 = unlimited
	MaxConcurrentRooms  int    `gorm:"not null" json:"max_concurrent_rooms"` // waiting+active
	MaxQuestionsPerQuiz int    `gorm:"not null;default:50" json:"max_questions_per_quiz"`
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

// LicenseCode is a prepaid activation code. An admin mints a batch, sells them
// offline (bank transfer, reseller, event voucher), and the buyer redeems the
// code themselves — no payment gateway in the loop.
//
// Redeeming applies PlanID to the redeemer's subscription for DurationDays
// (0 = lifetime), reusing the same path as an admin assignment.
type LicenseCode struct {
	// Code is the primary key and is what the buyer types. Stored uppercase,
	// generated from a Crockford-style alphabet with I/L/O/U removed so it
	// survives being read over the phone and retyped.
	Code   string `gorm:"primaryKey;size:32" json:"code"`
	PlanID string `gorm:"size:50;index;not null" json:"plan_id"`
	// DurationDays: 0 = lifetime (no expiry on the resulting subscription).
	//
	// Deliberately carries NO `default:` tag. GORM omits a zero-valued field from
	// the INSERT when the column declares a default, so `default:30` silently
	// turned every lifetime code (duration_days = 0) into a 30-day one.
	DurationDays int `gorm:"not null" json:"duration_days"`
	// MaxUses lets one code cover a classroom or a reseller batch. UsedCount is
	// only ever incremented inside the redeem transaction, under a row lock.
	MaxUses   int `gorm:"not null;default:1" json:"max_uses"`
	UsedCount int `gorm:"not null;default:0" json:"used_count"`
	// ExpiresAt is the shelf life of the code itself, not of the plan it grants.
	// nil = never goes stale.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// RevokedAt blocks further redemptions without deleting the audit trail.
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	// Batch groups codes minted in one call so an admin can find or revoke them together.
	Batch string `gorm:"size:100;index" json:"batch,omitempty"`
	Note  string `gorm:"size:255" json:"note,omitempty"`
	// AmountVND and ExternalRef tie a code to the money that paid for it. Payment
	// happens off-platform (bank transfer, reseller), so these are what makes a
	// code reconcilable against a statement afterwards — without them a sold code
	// is indistinguishable from one an admin minted by mistake.
	AmountVND   int            `gorm:"default:0" json:"amount_vnd"`
	ExternalRef string         `gorm:"size:120;index" json:"external_ref,omitempty"`
	DeliveredTo string         `gorm:"size:255" json:"delivered_to,omitempty"`
	DeliveredAt *time.Time     `json:"delivered_at,omitempty"`
	CreatedBy   uint           `gorm:"index" json:"created_by"`
	Plan        *PricingPlan   `gorm:"foreignKey:PlanID" json:"plan,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`
}

// LicenseRedemption records one successful use of a LicenseCode.
//
// The unique index on (code, user_id) is the idempotency guard: a user who
// double-submits the same code gets one redemption, not two, even if both
// requests pass the MaxUses check at the same instant.
type LicenseRedemption struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Code      string    `gorm:"size:32;not null;uniqueIndex:idx_redemption_code_user" json:"code"`
	UserID    uint      `gorm:"not null;index;uniqueIndex:idx_redemption_code_user" json:"user_id"`
	PlanID    string    `gorm:"size:50;not null" json:"plan_id"`
	IPAddress string    `gorm:"size:64" json:"ip_address,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// SubscriptionEvent is the append-only history behind a Subscription row.
//
// Subscription itself is one row per user, overwritten on every grant, so it
// answers "what does this user have now" and nothing else. Reconciling a month
// of off-platform payments, or answering "who did we give Pro to in September
// and why", needs the row that overwriting destroys — this is that row.
//
// Never updated after insert. A correction is a new event, not an edit.
type SubscriptionEvent struct {
	ID     uint   `gorm:"primaryKey" json:"id"`
	UserID uint   `gorm:"index;not null" json:"user_id"`
	Email  string `gorm:"size:255;index" json:"email,omitempty"`
	// Action: grant | renew | downgrade | expire.
	Action string `gorm:"size:32;index;not null" json:"action"`
	// Source: admin_assign | code_redeem | expiry_auto | seed.
	Source string `gorm:"size:32;index;not null" json:"source"`
	// SourceRef is the activation code for code_redeem, empty otherwise.
	SourceRef      string     `gorm:"size:64;index" json:"source_ref,omitempty"`
	PreviousPlanID string     `gorm:"size:50" json:"previous_plan_id,omitempty"`
	PlanID         string     `gorm:"size:50;index;not null" json:"plan_id"`
	StartsAt       time.Time  `json:"starts_at"`
	EndsAt         *time.Time `json:"ends_at,omitempty"`
	// AmountVND is what the customer actually paid, carried from the code or
	// entered by the admin — not the plan list price, which changes over time and
	// would silently rewrite history.
	AmountVND int `gorm:"default:0" json:"amount_vnd"`
	// ExternalRef is the bank transfer id / invoice number from the intermediary.
	ExternalRef string `gorm:"size:120;index" json:"external_ref,omitempty"`
	// ActorUserID is the admin who performed it; equal to UserID for a self-serve
	// redeem, and 0 for anything the system did on its own.
	ActorUserID uint      `gorm:"index" json:"actor_user_id"`
	Note        string    `gorm:"size:255" json:"note,omitempty"`
	IPAddress   string    `gorm:"size:64" json:"ip_address,omitempty"`
	CreatedAt   time.Time `gorm:"index" json:"created_at"`
}
