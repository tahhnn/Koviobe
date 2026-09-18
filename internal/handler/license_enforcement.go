package handler

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/pkg/audit"
	"github.com/quizzzone/backend/internal/pkg/license"
	"github.com/quizzzone/backend/internal/pkg/notify"
	"github.com/quizzzone/backend/internal/pkg/settings"
)

// enforcementState is what both handlers return, so the console never has to
// reconcile two shapes of the same fact.
func enforcementState() gin.H {
	source := "database"
	if license.EnvPinned() {
		source = "env"
	}
	out := gin.H{
		"enforcement": license.Enforcing(),
		"env_pinned":  license.EnvPinned(),
		"source":      source,
		"readiness":   license.CheckEnforcementReadiness(),
	}
	if row, err := settings.Get(settings.KeyLicenseEnforcement); err == nil {
		out["updated_at"] = row.UpdatedAt.Format(time.RFC3339)
		out["updated_by"] = row.UpdatedBy
	}
	return out
}

// AdminGetEnforcement reports the license kill switch and whether it is safe to
// throw it.
func AdminGetEnforcement(c *gin.Context) {
	c.JSON(http.StatusOK, enforcementState())
}

// AdminSetEnforcement flips the kill switch at runtime — no container restart.
//
// Turning it OFF never needs a readiness check: unlocking every host cannot lock
// anyone out. Only the ON direction can, so only that one is gated.
func AdminSetEnforcement(c *gin.Context) {
	var req struct {
		Enabled bool `json:"enabled"`
		Force   bool `json:"force"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	adminID, _ := c.Get("user_id")

	if license.EnvPinned() {
		c.JSON(http.StatusConflict, gin.H{
			"error":      license.ErrEnforcementPinned.Error(),
			"env_pinned": true,
		})
		return
	}

	if req.Enabled {
		if rd := license.CheckEnforcementReadiness(); !rd.Ready && !req.Force {
			c.JSON(http.StatusConflict, gin.H{
				"error":     "License enforcement readiness check failed",
				"readiness": rd,
			})
			return
		}
	}

	if err := license.SetEnforcement(req.Enabled, adminID.(uint)); err != nil {
		if errors.Is(err, license.ErrEnforcementPinned) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "env_pinned": true})
			return
		}
		log.Printf("[LICENSE] failed to persist enforcement=%t: %v", req.Enabled, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save enforcement setting"})
		return
	}

	target := "license_enforcement_off"
	if req.Enabled {
		target = "license_enforcement_on"
	}
	if req.Force {
		target += "_forced"
	}
	audit.Record(adminID.(uint), "admin_set_license_enforcement", target, c.ClientIP())

	// Page on the OFF transition only. Disabling every commercial gate is a
	// control that stopped working; turning it on is a deliberate, intended
	// change, and paging for it would train the team to ignore the channel.
	if req.Enabled {
		log.Printf("[LICENSE] enforcement turned ON by admin %d (forced=%t)", adminID.(uint), req.Force)
	} else {
		notify.P1("license_enforcement_off",
			"Admin %d đã TẮT license enforcement từ %s — mọi host được mở khoá hoàn toàn.",
			adminID.(uint), c.ClientIP())
	}

	c.JSON(http.StatusOK, enforcementState())
}
