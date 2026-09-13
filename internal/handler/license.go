package handler

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/audit"
	"github.com/quizzzone/backend/internal/pkg/license"
)

// ListPlans returns all active pricing plans (public comparison table).
func ListPlans(c *gin.Context) {
	var plans []model.PricingPlan
	if err := db.DB.Where("is_active = ?", true).Order("sort_order ASC").Find(&plans).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load plans"})
		return
	}
	c.JSON(http.StatusOK, plans)
}

// GetMyLicense returns current plan, entitlements, and usage for the authenticated user.
func GetMyLicense(c *gin.Context) {
	userID, _ := c.Get("user_id")
	uid := userID.(uint)

	sub, ents, err := license.ResolveSubscription(uid)
	if err != nil {
		ents, err = license.GetEntitlements(uid)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to resolve entitlements"})
			return
		}
	}
	usage, _ := license.GetUsage(uid)

	c.JSON(http.StatusOK, gin.H{
		"subscription": sub,
		"entitlements": ents,
		"usage":        usage,
		// Whether the gates are actually armed. The admin console used to state
		// this from a hardcoded string, which went stale the moment enforcement
		// was switched on — and an admin reading "enforcement is off" while it is
		// on will misjudge what a deploy does to their hosts.
		"enforcement": license.Enforcing(),
	})
}

// AdminListPlans returns all pricing plans (including inactive) for admin management.
func AdminListPlans(c *gin.Context) {
	var plans []model.PricingPlan
	if err := db.DB.Order("sort_order ASC, id ASC").Find(&plans).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load plans"})
		return
	}
	c.JSON(http.StatusOK, plans)
}

// AdminListSubscriptions lists users with effective (resolved) license state.
// Admins are included but marked — license is a product entitlement, role is separate.
func AdminListSubscriptions(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	limit := 100
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	type row struct {
		UserID             uint       `json:"user_id"`
		Email              string     `json:"email"`
		Nickname           string     `json:"nickname"`
		Role               string     `json:"role"`
		PlanID             string     `json:"plan_id"`
		PlanName           string     `json:"plan_name"`
		Status             string     `json:"status"`
		StartsAt           time.Time  `json:"starts_at"`
		EndsAt             *time.Time `json:"ends_at,omitempty"`
		Lifetime           bool       `json:"lifetime"`
		MaxPlayers         int        `json:"max_players_per_room"`
		AllowPlayerPaced   bool       `json:"allow_player_paced"`
		MaxConcurrentRooms int        `json:"max_concurrent_rooms"`
	}

	var users []model.User
	tx := db.DB.Preload("Role").
		Joins("LEFT JOIN roles ON roles.id = users.role_id").
		Where("roles.name IN ?", []string{"host", "admin"}).
		Order("users.id DESC").
		Limit(limit)
	if q != "" {
		like := "%" + q + "%"
		tx = tx.Where("users.email ILIKE ? OR users.nickname ILIKE ?", like, like)
	}
	if err := tx.Find(&users).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list users"})
		return
	}

	out := make([]row, 0, len(users))
	for _, u := range users {
		r := row{
			UserID:   u.ID,
			Email:    u.Email,
			Nickname: u.Nickname,
			Role:     "host",
		}
		if u.Role != nil {
			r.Role = u.Role.Name
		}

		// Resolve applies expiry → Free; returns effective entitlements
		sub, ents, err := license.ResolveSubscription(u.ID)
		if err != nil || sub == nil {
			ents, _ = license.GetEntitlements(u.ID)
			r.PlanID = ents.PlanID
			r.PlanName = ents.PlanName
			r.Status = "active"
			r.Lifetime = true
			r.MaxPlayers = ents.MaxPlayersPerRoom
			r.AllowPlayerPaced = ents.AllowPlayerPaced
			r.MaxConcurrentRooms = ents.MaxConcurrentRooms
		} else {
			r.PlanID = ents.PlanID
			r.PlanName = ents.PlanName
			r.Status = sub.Status
			r.StartsAt = sub.StartsAt
			r.EndsAt = sub.EndsAt
			r.Lifetime = sub.EndsAt == nil
			r.MaxPlayers = ents.MaxPlayersPerRoom
			r.AllowPlayerPaced = ents.AllowPlayerPaced
			r.MaxConcurrentRooms = ents.MaxConcurrentRooms
		}
		out = append(out, r)
	}

	c.JSON(http.StatusOK, gin.H{"subscriptions": out, "total": len(out)})
}

// AdminUpdatePlan patches plan limits (e.g. max_players_per_room).
func AdminUpdatePlan(c *gin.Context) {
	planID := c.Param("id")
	var patch map[string]interface{}
	if err := c.ShouldBindJSON(&patch); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	plan, err := license.UpdatePlanLimits(planID, patch)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	userID, _ := c.Get("user_id")
	audit.Record(userID.(uint), "admin_update_plan", "plan_"+planID, c.ClientIP())
	c.JSON(http.StatusOK, plan)
}

// AdminAssignPlan assigns a plan to a user (admin only).
// Body:
//
//	{ "user_id": 1, "plan_id": "pro", "ends_at_days": 30 }  // timed Pro
//	{ "user_id": 1, "plan_id": "pro", "lifetime": true }    // Pro no expiry
//	{ "user_id": 1, "plan_id": "free" }                     // revoke to Free
func AdminAssignPlan(c *gin.Context) {
	var req struct {
		UserID      uint   `json:"user_id" binding:"required"`
		PlanID      string `json:"plan_id" binding:"required"`
		EndsAtDays  *int   `json:"ends_at_days"`
		Lifetime    bool   `json:"lifetime"`
		AmountVND   int    `json:"amount_vnd"`
		ExternalRef string `json:"external_ref"`
		Note        string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	opts := license.AssignOptions{}
	if req.PlanID == license.PlanFree {
		opts.EndsAtDays = nil
	} else if req.Lifetime {
		opts.EndsAtDays = nil
	} else if req.EndsAtDays != nil {
		opts.EndsAtDays = req.EndsAtDays
	} else {
		// Paid plan without duration or lifetime → default 30 days
		d := 30
		opts.EndsAtDays = &d
	}

	adminID, _ := c.Get("user_id")

	sub, err := license.AdminSetPlanWithContext(req.UserID, req.PlanID, opts, license.GrantContext{
		Source:      license.SourceAdminAssign,
		AmountVND:   req.AmountVND,
		ExternalRef: strings.TrimSpace(req.ExternalRef),
		ActorUserID: adminID.(uint),
		Note:        strings.TrimSpace(req.Note),
		IPAddress:   c.ClientIP(),
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	audit.Record(adminID.(uint), "admin_assign_plan", "user_"+strconv.FormatUint(uint64(req.UserID), 10)+"_"+req.PlanID, c.ClientIP())

	ents, _ := license.GetEntitlements(req.UserID)
	c.JSON(http.StatusOK, gin.H{
		"subscription": sub,
		"entitlements": ents,
	})
}
