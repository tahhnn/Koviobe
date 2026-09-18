package handler

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/audit"
	"github.com/quizzzone/backend/internal/pkg/license"
	"github.com/quizzzone/backend/internal/pkg/notify"
	"github.com/quizzzone/backend/internal/pkg/secmon"
)

// redeemUserErrors are the redeem failures that describe what the buyer did
// wrong. Anything outside this set is an internal fault and must surface as a
// generic 500 — the code table is not something to narrate to an anonymous
// caller probing it.
var redeemUserErrors = []error{
	license.ErrCodeNotFound,
	license.ErrCodeRevoked,
	license.ErrCodeExpired,
	license.ErrCodeExhausted,
	license.ErrCodeAlreadyUsed,
	license.ErrCodePlanInactive,
}

func isRedeemUserError(err error) bool {
	for _, e := range redeemUserErrors {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// RedeemLicenseCode lets a signed-in host activate their own account with a
// prepaid code. Rate limited; see middleware.RedeemRateLimit.
func RedeemLicenseCode(c *gin.Context) {
	userID, _ := c.Get("user_id")
	uid := userID.(uint)

	var req struct {
		Code string `json:"code" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Activation code is required"})
		return
	}

	sub, ents, err := license.RedeemCode(uid, req.Code, c.ClientIP())
	if err != nil {
		if isRedeemUserError(err) {
			audit.Record(uid, "license_redeem_failed", license.NormalizeCode(req.Code), c.ClientIP())
			secmon.RedeemFailed(c.ClientIP(), uid)
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not activate the code, please try again later"})
		return
	}

	audit.Record(uid, "license_redeem", license.NormalizeCode(req.Code), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{
		"subscription": sub,
		"entitlements": ents,
		"message":      "Kích hoạt thành công",
	})
}

// AdminCreateLicenseCodes mints a batch of activation codes.
//
// Body: { "plan_id": "pro", "count": 10, "duration_days": 30,
//
//	"max_uses": 1, "expires_in_days": 90, "batch": "thang9", "note": "..." }
//
// duration_days 0 = the redeemed subscription never expires.
// expires_in_days is the shelf life of the code itself; omit for no shelf life.
func AdminCreateLicenseCodes(c *gin.Context) {
	adminID, _ := c.Get("user_id")

	var req struct {
		PlanID        string `json:"plan_id" binding:"required"`
		Count         int    `json:"count" binding:"required,min=1,max=500"`
		DurationDays  int    `json:"duration_days"`
		MaxUses       int    `json:"max_uses"`
		ExpiresInDays int    `json:"expires_in_days"`
		Batch         string `json:"batch"`
		Note          string `json:"note"`
		AmountVND     int    `json:"amount_vnd"`
		ExternalRef   string `json:"external_ref"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	opts := license.GenerateOptions{
		PlanID:       req.PlanID,
		Count:        req.Count,
		DurationDays: req.DurationDays,
		MaxUses:      req.MaxUses,
		Batch:        strings.TrimSpace(req.Batch),
		Note:         strings.TrimSpace(req.Note),
		AmountVND:    req.AmountVND,
		ExternalRef:  strings.TrimSpace(req.ExternalRef),
		CreatedBy:    adminID.(uint),
	}
	if req.ExpiresInDays > 0 {
		e := time.Now().AddDate(0, 0, req.ExpiresInDays)
		opts.ExpiresAt = &e
	}

	codes, err := license.GenerateCodes(opts)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	audit.Record(adminID.(uint), "admin_create_license_codes",
		req.PlanID+"_x"+strconv.Itoa(len(codes)), c.ClientIP())
	c.JSON(http.StatusCreated, gin.H{"codes": codes, "total": len(codes)})
}

// AdminListLicenseCodes lists codes with their redemption state.
// Filters: ?batch=, ?plan_id=, ?status=available|used|revoked, ?q= (code substring).
func AdminListLicenseCodes(c *gin.Context) {
	limit := 100
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	tx := db.DB.Model(&model.LicenseCode{}).Order("created_at DESC").Limit(limit)
	if v := strings.TrimSpace(c.Query("batch")); v != "" {
		tx = tx.Where("batch = ?", v)
	}
	if v := strings.TrimSpace(c.Query("plan_id")); v != "" {
		tx = tx.Where("plan_id = ?", v)
	}
	if v := strings.TrimSpace(c.Query("q")); v != "" {
		tx = tx.Where("code ILIKE ?", "%"+strings.ToUpper(v)+"%")
	}
	switch c.Query("status") {
	case "available":
		tx = tx.Where("revoked_at IS NULL AND used_count < max_uses").
			Where("expires_at IS NULL OR expires_at > ?", time.Now())
	case "used":
		tx = tx.Where("used_count >= max_uses")
	case "revoked":
		tx = tx.Where("revoked_at IS NOT NULL")
	}

	var codes []model.LicenseCode
	if err := tx.Find(&codes).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list codes"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"codes": codes, "total": len(codes)})
}

// AdminRevokeLicenseCode blocks further redemptions of a code. Redemptions that
// already happened stand — revoking does not claw back a granted subscription.
func AdminRevokeLicenseCode(c *gin.Context) {
	adminID, _ := c.Get("user_id")

	code, err := license.RevokeCode(c.Param("code"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	audit.Record(adminID.(uint), "admin_revoke_license_code", code.Code, c.ClientIP())
	c.JSON(http.StatusOK, code)
}

// AdminClawBackLicenseCode revokes a code AND takes the plan back from the users
// it granted, ending any game they still have running.
//
// A separate endpoint rather than a flag on /revoke: that one's non-destructive
// meaning is documented in the handler, in LICENSING.md and in the console's own
// copy, and a URL whose destructiveness depends on a body field is a URL nobody
// can reason about from the audit log. Claw-back includes the revoke.
//
// Users who moved on to a different grant are left alone — see
// license.shouldClawBack for the rule. Every redeemer is reported either way.
func AdminClawBackLicenseCode(c *gin.Context) {
	adminID, _ := c.Get("user_id")

	var req struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&req) // body is optional

	code, results, err := license.ClawBackCode(c.Param("code"), license.GrantContext{
		ActorUserID: adminID.(uint),
		Note:        strings.TrimSpace(req.Note),
		IPAddress:   c.ClientIP(),
	})
	if err != nil {
		if errors.Is(err, license.ErrCodeNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "License code not found"})
			return
		}
		log.Printf("[AdminClawBack] code %s failed: %v", c.Param("code"), err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to claw back the code"})
		return
	}

	// Rooms are closed here, outside every transaction: publishing to Centrifugo
	// and scheduling cleanup cannot sit inside a database transaction, and the
	// license package must not import this one.
	revoked, skipped, failed, closed := 0, 0, 0, 0
	for _, r := range results {
		switch r.Outcome {
		case license.OutcomeRevoked:
			revoked++
		case license.OutcomeFailed:
			failed++
		default:
			skipped++
		}
		for _, id := range r.RoomIDs {
			if err := FinalizeRoomWithReason(id, EndReasonLicenseRevoked); err != nil {
				log.Printf("[AdminClawBack] could not close room %d: %v", id, err)
				notify.P1("clawback_close_room",
					"Không đóng được phòng %d khi thu hồi mã %s: %v", id, code.Code, err)
				continue
			}
			closed++
		}
	}

	audit.Record(adminID.(uint), "admin_clawback_license_code",
		fmt.Sprintf("%s_users%d_rooms%d", code.Code, revoked, closed), c.ClientIP())

	c.JSON(http.StatusOK, gin.H{
		"code":         code.Code,
		"revoked_at":   code.RevokedAt,
		"total":        len(results),
		"revoked":      revoked,
		"skipped":      skipped,
		"failed":       failed,
		"rooms_closed": closed,
		"results":      results,
	})
}

// AdminListRedemptions shows who redeemed what, newest first.
func AdminListRedemptions(c *gin.Context) {
	limit := 100
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	type row struct {
		ID        uint      `json:"id"`
		Code      string    `json:"code"`
		UserID    uint      `json:"user_id"`
		Email     string    `json:"email"`
		PlanID    string    `json:"plan_id"`
		IPAddress string    `json:"ip_address,omitempty"`
		CreatedAt time.Time `json:"created_at"`
	}

	var out []row
	tx := db.DB.Model(&model.LicenseRedemption{}).
		Select("license_redemptions.id, license_redemptions.code, license_redemptions.user_id, users.email, license_redemptions.plan_id, license_redemptions.ip_address, license_redemptions.created_at").
		Joins("LEFT JOIN users ON users.id = license_redemptions.user_id").
		Order("license_redemptions.created_at DESC").
		Limit(limit)
	if v := strings.TrimSpace(c.Query("code")); v != "" {
		tx = tx.Where("license_redemptions.code = ?", license.NormalizeCode(v))
	}
	if err := tx.Scan(&out).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list redemptions"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"redemptions": out, "total": len(out)})
}
