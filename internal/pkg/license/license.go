package license

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/kovio/backend/internal/db"
	"github.com/kovio/backend/internal/model"
)

const (
	PlanFree = "free"
	PlanPro  = "pro"
)

// EnforcementEnabled controls whether Free/Pro limits actually block API actions.
//
// LICENSE DEFERRED (2026-07): Product licensing (Free vs Pro gates) will be
// developed later. Keep this false so hosts are not blocked by plan limits
// (quiz count, questions/quiz, concurrent rooms, player cap, solo mode,
// branding, export logs, templates). Plan catalog + admin assign APIs remain
// for future work; re-enable by setting this to true.
//
// See: LICENSE_DEFERRED.md in this package.
const EnforcementEnabled = false

// Entitlements is the resolved limit set for a host.
type Entitlements struct {
	PlanID               string `json:"plan_id"`
	PlanName             string `json:"plan_name"`
	MaxPlayersPerRoom    int    `json:"max_players_per_room"`
	MaxQuizzes           int    `json:"max_quizzes"`
	MaxTemplates         int    `json:"max_templates"`
	MaxConcurrentRooms   int    `json:"max_concurrent_rooms"`
	MaxQuestionsPerQuiz  int    `json:"max_questions_per_quiz"`
	AllowPlayerPaced     bool   `json:"allow_player_paced"`
	AllowCustomBranding  bool   `json:"allow_custom_branding"`
	AllowExportLogs      bool   `json:"allow_export_logs"`
	AllowPrioritySupport bool   `json:"allow_priority_support"`
	AllowRemoveWatermark bool   `json:"allow_remove_watermark"`
}

// Usage mirrors current resource consumption for a host.
type Usage struct {
	Quizzes         int64 `json:"quizzes"`
	Templates       int64 `json:"templates"`
	ConcurrentRooms int64 `json:"concurrent_rooms"`
}

// AssignOptions controls how AdminSetPlan writes subscription dates.
type AssignOptions struct {
	// EndsAtDays: nil = lifetime (no expiry). Pointer to N > 0 = expires in N days.
	// Free plan always clears EndsAt regardless of this field.
	EndsAtDays *int
}

// openEntitlements unlocks all commercial gates while EnforcementEnabled is false.
func openEntitlements() Entitlements {
	return Entitlements{
		PlanID:               "open",
		PlanName:             "Open (license deferred)",
		MaxPlayersPerRoom:    1000,
		MaxQuizzes:           -1,
		MaxTemplates:         -1,
		MaxConcurrentRooms:   -1,
		MaxQuestionsPerQuiz:  -1,
		AllowPlayerPaced:     true,
		AllowCustomBranding:  true,
		AllowExportLogs:      true,
		AllowPrioritySupport: true,
		AllowRemoveWatermark: true,
	}
}

func defaultFreeEntitlements() Entitlements {
	return Entitlements{
		PlanID:              PlanFree,
		PlanName:            "Free",
		MaxPlayersPerRoom:   20,
		MaxQuizzes:          20,
		MaxTemplates:        10,
		MaxConcurrentRooms:  1,
		MaxQuestionsPerQuiz: 30,
		AllowPlayerPaced:    false,
		AllowCustomBranding: false,
		AllowExportLogs:     false,
	}
}

func entitlementsFromPlan(p *model.PricingPlan) Entitlements {
	if p == nil {
		return defaultFreeEntitlements()
	}
	return Entitlements{
		PlanID:               p.ID,
		PlanName:             p.Name,
		MaxPlayersPerRoom:    p.MaxPlayersPerRoom,
		MaxQuizzes:           p.MaxQuizzes,
		MaxTemplates:         p.MaxTemplates,
		MaxConcurrentRooms:   p.MaxConcurrentRooms,
		MaxQuestionsPerQuiz:  p.MaxQuestionsPerQuiz,
		AllowPlayerPaced:     p.AllowPlayerPaced,
		AllowCustomBranding:  p.AllowCustomBranding,
		AllowExportLogs:      p.AllowExportLogs,
		AllowPrioritySupport: p.AllowPrioritySupport,
		AllowRemoveWatermark: p.AllowRemoveWatermark,
	}
}

// EnsureFreeSubscription creates a free subscription if the user has none.
func EnsureFreeSubscription(userID uint) error {
	var count int64
	if err := db.DB.Model(&model.Subscription{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	sub := model.Subscription{
		UserID:   userID,
		PlanID:   PlanFree,
		Status:   "active",
		StartsAt: time.Now(),
		EndsAt:   nil,
	}
	return db.DB.Create(&sub).Error
}

// DowngradeToFree rewrites the user's single subscription row to Free (no expiry).
func DowngradeToFree(userID uint) error {
	var free model.PricingPlan
	if err := db.DB.Where("id = ?", PlanFree).First(&free).Error; err != nil {
		return errors.New("free plan missing")
	}
	now := time.Now()
	var sub model.Subscription
	err := db.DB.Where("user_id = ?", userID).First(&sub).Error
	if err != nil {
		return EnsureFreeSubscription(userID)
	}
	sub.PlanID = PlanFree
	sub.Status = "active"
	sub.StartsAt = now
	sub.EndsAt = nil
	return db.DB.Save(&sub).Error
}

func isExpired(sub *model.Subscription) bool {
	return sub.EndsAt != nil && time.Now().After(*sub.EndsAt)
}

// GetEntitlements resolves the active plan limits for a host.
// Expired paid plans are downgraded in-place to Free (one subscription row per user).
// When EnforcementEnabled is false, all hosts receive open entitlements (no Pro blocks).
func GetEntitlements(userID uint) (Entitlements, error) {
	if !EnforcementEnabled {
		return openEntitlements(), nil
	}

	var sub model.Subscription
	err := db.DB.Preload("Plan").Where("user_id = ?", userID).First(&sub).Error
	if err != nil {
		_ = EnsureFreeSubscription(userID)
		err = db.DB.Preload("Plan").Where("user_id = ?", userID).First(&sub).Error
		if err != nil {
			return defaultFreeEntitlements(), nil
		}
	}

	if isExpired(&sub) || sub.Status == "expired" || sub.Status == "canceled" {
		_ = DowngradeToFree(userID)
		err = db.DB.Preload("Plan").Where("user_id = ?", userID).First(&sub).Error
		if err != nil {
			return defaultFreeEntitlements(), nil
		}
	}

	if sub.Status != "active" {
		_ = DowngradeToFree(userID)
		return defaultFreeEntitlements(), nil
	}

	return entitlementsFromPlan(sub.Plan), nil
}

// GetUsage returns current usage counters for a host.
func GetUsage(userID uint) (Usage, error) {
	var u Usage
	db.DB.Model(&model.Quiz{}).Where("host_id = ?", userID).Count(&u.Quizzes)
	db.DB.Model(&model.Template{}).Where("host_id = ?", userID).Count(&u.Templates)
	db.DB.Model(&model.Room{}).Where("host_id = ? AND status IN ?", userID, []string{"waiting", "active"}).Count(&u.ConcurrentRooms)
	return u, nil
}

// ResolveSubscription returns the subscription after applying expiry downgrade, plus entitlements.
func ResolveSubscription(userID uint) (*model.Subscription, Entitlements, error) {
	ents, err := GetEntitlements(userID)
	if err != nil {
		return nil, ents, err
	}
	var sub model.Subscription
	if err := db.DB.Preload("Plan").Where("user_id = ?", userID).First(&sub).Error; err != nil {
		return nil, ents, err
	}
	return &sub, ents, nil
}

// AdminSetPlan assigns a commercial plan to a host (admin only).
// Rules:
//   - plan must exist and be active
//   - Free always has EndsAt = nil (lifetime free tier)
//   - Paid plans: EndsAtDays nil → lifetime; EndsAtDays N → expires in N days
func AdminSetPlan(userID uint, planID string, opts AssignOptions) (*model.Subscription, error) {
	var plan model.PricingPlan
	if err := db.DB.Where("id = ? AND is_active = ?", planID, true).First(&plan).Error; err != nil {
		return nil, errors.New("plan not found or inactive")
	}

	var user model.User
	if err := db.DB.First(&user, userID).Error; err != nil {
		return nil, errors.New("user not found")
	}

	now := time.Now()
	var endsAt *time.Time
	if planID == PlanFree {
		endsAt = nil
	} else if opts.EndsAtDays != nil {
		if *opts.EndsAtDays <= 0 {
			return nil, errors.New("ends_at_days must be > 0 (omit for lifetime)")
		}
		e := now.AddDate(0, 0, *opts.EndsAtDays)
		endsAt = &e
	}
	// else paid + EndsAtDays nil → lifetime license

	var sub model.Subscription
	err := db.DB.Where("user_id = ?", userID).First(&sub).Error
	if err != nil {
		sub = model.Subscription{
			UserID:   userID,
			PlanID:   planID,
			Status:   "active",
			StartsAt: now,
			EndsAt:   endsAt,
		}
		if createErr := db.DB.Create(&sub).Error; createErr != nil {
			return nil, createErr
		}
	} else {
		sub.PlanID = planID
		sub.Status = "active"
		sub.StartsAt = now
		sub.EndsAt = endsAt
		if saveErr := db.DB.Save(&sub).Error; saveErr != nil {
			return nil, saveErr
		}
	}
	_ = db.DB.Preload("Plan").First(&sub, sub.ID)
	return &sub, nil
}

// UpdatePlanLimits lets admins tune plan numbers (e.g. max players).
func UpdatePlanLimits(planID string, patch map[string]interface{}) (*model.PricingPlan, error) {
	var plan model.PricingPlan
	if err := db.DB.Where("id = ?", planID).First(&plan).Error; err != nil {
		return nil, errors.New("plan not found")
	}

	allowed := map[string]bool{
		"max_players_per_room":   true,
		"max_quizzes":            true,
		"max_templates":          true,
		"max_concurrent_rooms":   true,
		"max_questions_per_quiz": true,
		"allow_player_paced":     true,
		"allow_custom_branding":  true,
		"allow_export_logs":      true,
		"allow_priority_support": true,
		"allow_remove_watermark": true,
		"price_monthly_vnd":      true,
		"description":            true,
		"name":                   true,
		"is_active":              true,
	}

	clean := map[string]interface{}{}
	for k, v := range patch {
		if allowed[k] {
			clean[k] = v
		}
	}
	if len(clean) == 0 {
		return &plan, nil
	}
	if err := db.DB.Model(&plan).Updates(clean).Error; err != nil {
		return nil, err
	}
	_ = db.DB.First(&plan, "id = ?", planID)
	return &plan, nil
}

// ThemeRequestsPlayerPaced checks theme_config JSON for player_paced mode.
func ThemeRequestsPlayerPaced(themeConfig string) bool {
	if themeConfig == "" {
		return false
	}
	var cfg struct {
		GameMode string `json:"game_mode"`
	}
	if err := json.Unmarshal([]byte(themeConfig), &cfg); err != nil {
		return false
	}
	return cfg.GameMode == "player_paced"
}

// IsUnlimited reports whether a limit value means unlimited (-1).
func IsUnlimited(n int) bool {
	return n < 0
}
