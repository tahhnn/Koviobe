package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/audit"
	"github.com/quizzzone/backend/internal/pkg/license"
	"github.com/quizzzone/backend/internal/pkg/theme"
)

// loadUserTheme reads a host's room theme.
//
// No row is a valid state and means "none": a host who never opened the
// settings tab keeps every room looking exactly as it did before.
func loadUserTheme(uid uint) (theme.Defaults, *time.Time, error) {
	var row model.UserTheme
	err := db.DB.Where("user_id = ?", uid).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return theme.Defaults{}, nil, nil
	}
	if err != nil {
		return theme.Defaults{}, nil, err
	}
	var d theme.Defaults
	_ = json.Unmarshal([]byte(row.Config), &d)
	return theme.SanitizeDefaults(d), &row.UpdatedAt, nil
}

// roomThemeFor is the host theme a new room of uid's should get, with the plan
// applied: images uploaded on a plan the host has since left are dropped, the
// built-in looks are kept.
func roomThemeFor(uid uint, ents license.Entitlements, enforcing bool) (theme.Defaults, error) {
	d, _, err := loadUserTheme(uid)
	if err != nil {
		return theme.Defaults{}, err
	}
	if enforcing && !ents.AllowCustomBranding {
		d = d.WithoutCustom()
	}
	return d, nil
}

func myThemeResponse(d theme.Defaults, updatedAt *time.Time) gin.H {
	out := gin.H{"theme": d}
	if updatedAt != nil {
		out["updated_at"] = updatedAt.Format(time.RFC3339)
	}
	return out
}

// GetMyTheme returns the caller's room theme.
func GetMyTheme(c *gin.Context) {
	d, updatedAt, err := loadUserTheme(c.GetUint("user_id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load room theme"})
		return
	}
	c.JSON(http.StatusOK, myThemeResponse(d, updatedAt))
}

// vetUserTheme sanitizes a submitted room theme and applies the plan gate.
//
// It returns the theme to store, or a non-zero status and the body to send.
// Separate from the handler, and taking enforcing and the entitlement lookup
// as arguments, so the gate can be tested without a database or package state
// — the same reason vetThemeConfig takes them.
func vetUserTheme(req theme.Defaults, enforcing bool, entitlements func() (license.Entitlements, error)) (theme.Defaults, int, gin.H) {
	clean := theme.SanitizeDefaults(req)
	// A URL the sanitizer dropped is refused rather than silently cleared: the
	// host would otherwise save, see a success, and find the image gone.
	for _, pair := range [][2]string{
		{req.BgHostURL, clean.BgHostURL},
		{req.BgPlayerURL, clean.BgPlayerURL},
		{req.LogoURL, clean.LogoURL},
	} {
		if pair[0] != "" && pair[1] == "" {
			return theme.Defaults{}, http.StatusBadRequest, gin.H{"error": "Images must be files uploaded to this site"}
		}
	}

	// The built-in looks are open to every plan; the host's own images are
	// custom branding, gated the same way as on a quiz.
	keys := clean.CustomKeys()
	if len(keys) == 0 || !enforcing {
		return clean, 0, nil
	}
	ents, err := entitlements()
	if err != nil {
		return theme.Defaults{}, http.StatusInternalServerError, gin.H{"error": "Failed to resolve license"}
	}
	if !ents.AllowCustomBranding {
		return theme.Defaults{}, http.StatusForbidden, gin.H{
			"error":   "Custom branding requires Pro plan",
			"plan_id": ents.PlanID,
			"feature": "allow_custom_branding",
			"fields":  keys,
		}
	}
	return clean, 0, nil
}

// SetMyTheme replaces the caller's room theme.
//
// Applies to rooms created from now on. A room copies its theme when it is
// created, so a game already running keeps the look it started with rather
// than changing under the audience.
func SetMyTheme(c *gin.Context) {
	var req theme.Defaults
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	uid := c.GetUint("user_id")

	clean, status, body := vetUserTheme(req, license.Enforcing(), func() (license.Entitlements, error) {
		return license.GetEntitlements(uid)
	})
	if status != 0 {
		c.JSON(status, body)
		return
	}

	raw, err := json.Marshal(clean)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save room theme"})
		return
	}
	row := model.UserTheme{UserID: uid, Config: string(raw), UpdatedAt: time.Now()}
	if err := db.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"config", "updated_at"}),
	}).Create(&row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save room theme"})
		return
	}

	audit.Log(c, uid, "update_room_theme", "user_theme", map[string]any{"theme": clean})

	c.JSON(http.StatusOK, myThemeResponse(clean, &row.UpdatedAt))
}
