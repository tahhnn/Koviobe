package handler

import (
	"log"
	"math"
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

	// Days left, computed here so every surface that shows a term agrees on the
	// rounding. nil means no expiry.
	var daysRemaining *int
	if sub != nil && sub.EndsAt != nil {
		d := int(math.Ceil(time.Until(*sub.EndsAt).Hours() / 24))
		daysRemaining = &d
	}

	c.JSON(http.StatusOK, gin.H{
		"subscription":   sub,
		"entitlements":   ents,
		"usage":          usage,
		"days_remaining": daysRemaining,
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

// AdminListSubscriptions lists users with the license state stored against them.
// Admins are included but marked — license is a product entitlement, role is separate.
//
// It reports the STORED plan, not the resolved entitlement. GetEntitlements
// returns plan_id "open" for every host while enforcement is off, and the two
// admin consoles branched on that string: /admin/users disabled "revoke Pro" for
// everybody, /admin/license enabled it for everybody. What an admin manages here
// is the row in `subscriptions`, so that is what this endpoint hands them.
//
// It also no longer resolves per user. ResolveSubscription downgrades an expired
// plan as a side effect, so opening a list page wrote to a hundred subscriptions
// and stamped the ledger at the moment an admin happened to look rather than the
// moment the plan lapsed. The 5-minute sweep (cron.StartLicenseExpiryWorker) is
// the system of record for expiry; this page only reports it via `expired`.
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
		Expired            bool       `json:"expired"`
		DaysRemaining      *int       `json:"days_remaining,omitempty"`
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

	subs := subscriptionsByUser(users)

	now := time.Now()
	out := make([]row, 0, len(users))
	for _, u := range users {
		r := row{
			UserID:   u.ID,
			Email:    u.Email,
			Nickname: u.Nickname,
			Role:     "host",
			// No subscription row yet (registration backfills one, but a DB
			// restored from before that did not). Free is the honest answer, and
			// its limits are zero by design.
			PlanID:   license.PlanFree,
			PlanName: "",
			Status:   "active",
			Lifetime: true,
		}
		if u.Role != nil {
			r.Role = u.Role.Name
		}

		if sub := subs[u.ID]; sub != nil {
			r.PlanID = sub.PlanID
			r.Status = sub.Status
			r.StartsAt = sub.StartsAt
			r.EndsAt = sub.EndsAt
			r.Lifetime = sub.EndsAt == nil
			if sub.Plan != nil {
				r.PlanName = sub.Plan.Name
				r.MaxPlayers = sub.Plan.MaxPlayersPerRoom
				r.AllowPlayerPaced = sub.Plan.AllowPlayerPaced
				r.MaxConcurrentRooms = sub.Plan.MaxConcurrentRooms
			}
			if sub.EndsAt != nil {
				r.Expired = sub.EndsAt.Before(now) && sub.PlanID != license.PlanFree
				d := int(math.Ceil(sub.EndsAt.Sub(now).Hours() / 24))
				r.DaysRemaining = &d
			}
		}
		out = append(out, r)
	}

	c.JSON(http.StatusOK, gin.H{
		"subscriptions": out,
		"total":         len(out),
		// So the console can label the "open" mode honestly instead of implying
		// these stored plans are currently blocking anything.
		"enforcement": license.Enforcing(),
	})
}

// subscriptionsByUser loads every user's subscription in one query, keyed by
// user id. One query for the page instead of two to four per row: the old loop
// called ResolveSubscription per user, which is what made an admin list page
// scale with the number of accounts.
func subscriptionsByUser(users []model.User) map[uint]*model.Subscription {
	if len(users) == 0 {
		return map[uint]*model.Subscription{}
	}
	ids := make([]uint, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.ID)
	}
	var subs []model.Subscription
	if err := db.DB.Preload("Plan").Where("user_id IN ?", ids).Find(&subs).Error; err != nil {
		log.Printf("[AdminList] failed to load subscriptions: %v", err)
		return map[uint]*model.Subscription{}
	}
	byUser := make(map[uint]*model.Subscription, len(subs))
	for i := range subs {
		byUser[subs[i].UserID] = &subs[i]
	}
	return byUser
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
		// CloseRooms ends the user's live games as part of a downgrade. Honored
		// only when plan_id is "free"; false by default so every existing caller
		// behaves exactly as before.
		CloseRooms bool `json:"close_rooms"`
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
		// "Unspecified" meant 30 days here and "lifetime" one layer down in
		// setPlanTx, with LICENSING.md documenting the second. Rather than pick a
		// winner — aligning to lifetime would silently turn every caller that
		// forgot the field into a perpetual licence — make the ambiguous request
		// unrepresentable.
		c.JSON(http.StatusBadRequest, gin.H{"error": "Specify ends_at_days or set lifetime"})
		return
	}

	adminID, _ := c.Get("user_id")

	// Collected before the write: once the plan is gone the rooms are still
	// running, but this is the last moment the "before" state is knowable.
	var openRooms []uint
	if req.PlanID == license.PlanFree && req.CloseRooms {
		openRooms, _ = license.OpenRoomIDs(req.UserID)
	}

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

	// Closed here rather than inside the license package: archiving a room
	// publishes to Centrifugo and schedules cleanup, neither of which belongs in
	// a database transaction, and license must not import handler.
	closed := 0
	for _, id := range openRooms {
		if err := FinalizeRoomWithReason(id, EndReasonLicenseRevoked); err != nil {
			log.Printf("[AdminAssignPlan] could not close room %d: %v", id, err)
			continue
		}
		closed++
	}

	ents, _ := license.GetEntitlements(req.UserID)
	c.JSON(http.StatusOK, gin.H{
		"subscription": sub,
		"entitlements": ents,
		"rooms_closed": closed,
	})
}
