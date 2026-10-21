package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/pkg/audit"
	"github.com/quizzzone/backend/internal/pkg/settings"
	"github.com/quizzzone/backend/internal/pkg/theme"
)

var defaultThemeKeys = []string{
	settings.KeyThemeDefaultEnabled,
	settings.KeyThemeDefaultBgHostURL,
	settings.KeyThemeDefaultBgPlayerURL,
	settings.KeyThemeDefaultLogoURL,
	settings.KeyThemeDefaultBgOverlay,
	settings.KeyThemeDefaultBgOverlayColor,
}

// loadDefaultTheme reads the admin's default room theme.
//
// Never written is a valid state and means "off": a deployment that has not
// opened the console keeps every room looking exactly as it did before.
func loadDefaultTheme() (theme.Defaults, error) {
	rows, err := settings.GetStrings(defaultThemeKeys...)
	if err != nil {
		return theme.Defaults{}, err
	}
	enabled, _ := strconv.ParseBool(rows[settings.KeyThemeDefaultEnabled])
	overlay, _ := strconv.ParseFloat(rows[settings.KeyThemeDefaultBgOverlay], 64)
	return theme.SanitizeDefaults(theme.Defaults{
		Enabled:        enabled,
		BgHostURL:      rows[settings.KeyThemeDefaultBgHostURL],
		BgPlayerURL:    rows[settings.KeyThemeDefaultBgPlayerURL],
		LogoURL:        rows[settings.KeyThemeDefaultLogoURL],
		BgOverlay:      overlay,
		BgOverlayColor: rows[settings.KeyThemeDefaultBgOverlayColor],
	}), nil
}

func defaultThemeState() (gin.H, error) {
	d, err := loadDefaultTheme()
	if err != nil {
		return nil, err
	}
	out := gin.H{"theme": d}
	if row, err := settings.Get(settings.KeyThemeDefaultEnabled); err == nil {
		out["updated_at"] = row.UpdatedAt.Format(time.RFC3339)
		out["updated_by"] = row.UpdatedBy
	}
	return out, nil
}

// AdminGetDefaultTheme reports the room theme applied to quizzes that set none.
func AdminGetDefaultTheme(c *gin.Context) {
	state, err := defaultThemeState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load default theme"})
		return
	}
	c.JSON(http.StatusOK, state)
}

// AdminSetDefaultTheme replaces the default room theme.
//
// Applies to rooms created from now on. A room copies its theme when it is
// created, so a game already running keeps the look it started with rather
// than changing under the audience.
func AdminSetDefaultTheme(c *gin.Context) {
	var req theme.Defaults
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	clean := theme.SanitizeDefaults(req)
	// A URL the sanitizer dropped is refused rather than silently cleared: the
	// admin would otherwise save, see a success, and find the image gone.
	for _, pair := range [][2]string{
		{req.BgHostURL, clean.BgHostURL},
		{req.BgPlayerURL, clean.BgPlayerURL},
		{req.LogoURL, clean.LogoURL},
	} {
		if pair[0] != "" && pair[1] == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Images must be files uploaded to this site"})
			return
		}
	}

	adminID := c.GetUint("user_id")
	if err := settings.SetStrings(map[string]string{
		settings.KeyThemeDefaultEnabled:        strconv.FormatBool(clean.Enabled),
		settings.KeyThemeDefaultBgHostURL:      clean.BgHostURL,
		settings.KeyThemeDefaultBgPlayerURL:    clean.BgPlayerURL,
		settings.KeyThemeDefaultLogoURL:        clean.LogoURL,
		settings.KeyThemeDefaultBgOverlay:      strconv.FormatFloat(clean.BgOverlay, 'f', 2, 64),
		settings.KeyThemeDefaultBgOverlayColor: clean.BgOverlayColor,
	}, adminID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save default theme"})
		return
	}

	audit.Log(c, adminID, "admin_set_default_theme", "theme.default", map[string]any{
		"is_active": clean.Enabled,
		"theme":     clean,
	})

	state, err := defaultThemeState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Saved, but failed to reload default theme"})
		return
	}
	c.JSON(http.StatusOK, state)
}
