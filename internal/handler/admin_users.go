package handler

import (
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/kovio/backend/internal/db"
	"github.com/kovio/backend/internal/model"
	"github.com/kovio/backend/internal/pkg/audit"
	"github.com/kovio/backend/internal/pkg/license"
)

// AdminListUsers lists registered accounts (product Users + Admins) with license snapshot.
// RBAC still stores regular users as role name "host"; API/UI maps that to account type "user".
func AdminListUsers(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	limit := 100
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	type row struct {
		ID           uint   `json:"id"`
		Email        string `json:"email"`
		Nickname     string `json:"nickname"`
		Role         string `json:"role"`         // RBAC: host|admin
		AccountType  string `json:"account_type"` // product: user|admin
		PlanID       string `json:"plan_id"`
		PlanName     string `json:"plan_name"`
		IsActive     bool   `json:"is_active"`
		IsSeedAdmin  bool   `json:"is_seed_admin"`
		CreatedAt    string `json:"created_at"`
	}

	seedEmail := strings.ToLower(strings.TrimSpace(os.Getenv("SEED_ADMIN_EMAIL")))
	if seedEmail == "" {
		seedEmail = "admin@kovio.local"
	}

	var users []model.User
	tx := db.DB.Preload("Role").
		Joins("LEFT JOIN roles ON roles.id = users.role_id").
		Where("roles.name IN ?", []string{"host", "admin"}).
		Order("users.id ASC").
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
			ID:          u.ID,
			Email:       u.Email,
			Nickname:    u.Nickname,
			Role:        "host",
			AccountType: "user",
			IsActive:    u.IsActive,
			IsSeedAdmin: strings.EqualFold(u.Email, seedEmail),
			CreatedAt:   u.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		}
		if u.Role != nil {
			r.Role = u.Role.Name
			if strings.EqualFold(u.Role.Name, "admin") {
				r.AccountType = "admin"
			} else {
				r.AccountType = "user"
			}
		}
		ents, _ := license.GetEntitlements(u.ID)
		r.PlanID = ents.PlanID
		r.PlanName = ents.PlanName
		out = append(out, r)
	}

	c.JSON(http.StatusOK, gin.H{"users": out, "total": len(out)})
}

// AdminUpdateUserRole sets account type.
// Body: { "role": "user" | "admin" }  (also accepts legacy "host" for user)
func AdminUpdateUserRole(c *gin.Context) {
	idStr := c.Param("id")
	uid64, err := strconv.ParseUint(idStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}
	targetID := uint(uid64)

	var req struct {
		Role string `json:"role" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	requested := strings.ToLower(strings.TrimSpace(req.Role))
	// Product "user" maps to RBAC role "host"
	roleName := requested
	if requested == "user" {
		roleName = "host"
	}
	if roleName != "host" && roleName != "admin" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role must be user or admin"})
		return
	}

	actorID, _ := c.Get("user_id")
	actorUID := actorID.(uint)

	seedEmail := strings.ToLower(strings.TrimSpace(os.Getenv("SEED_ADMIN_EMAIL")))
	if seedEmail == "" {
		seedEmail = "admin@kovio.local"
	}

	var target model.User
	if err := db.DB.Preload("Role").First(&target, targetID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	// Never demote the fixed seeded admin account
	if strings.EqualFold(target.Email, seedEmail) && roleName != "admin" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot demote the seeded system admin account"})
		return
	}

	// Prevent self-demotion if you are the last admin
	if actorUID == targetID && roleName != "admin" {
		var adminCount int64
		db.DB.Model(&model.User{}).
			Joins("JOIN roles ON roles.id = users.role_id").
			Where("roles.name = ? AND users.deleted_at IS NULL", "admin").
			Count(&adminCount)
		if adminCount <= 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot demote the last admin"})
			return
		}
	}

	var role model.Role
	if err := db.DB.Where("name = ?", roleName).First(&role).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Role not found"})
		return
	}

	target.RoleID = &role.ID
	if err := db.DB.Save(&target).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update role"})
		return
	}

	audit.Record(actorUID, "admin_update_user_role", "user_"+strconv.FormatUint(uint64(targetID), 10)+"_"+roleName, c.ClientIP())

	accountType := "user"
	if roleName == "admin" {
		accountType = "admin"
	}
	c.JSON(http.StatusOK, gin.H{
		"id":           target.ID,
		"email":        target.Email,
		"nickname":     target.Nickname,
		"role":         roleName,
		"account_type": accountType,
	})
}

// AdminUpdateUserStatus activates or deactivates a user account.
// Body: { "is_active": true | false }
func AdminUpdateUserStatus(c *gin.Context) {
	idStr := c.Param("id")
	uid64, err := strconv.ParseUint(idStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}
	targetID := uint(uid64)

	var req struct {
		IsActive bool `json:"is_active" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	actorID, _ := c.Get("user_id")
	actorUID := actorID.(uint)

	seedEmail := strings.ToLower(strings.TrimSpace(os.Getenv("SEED_ADMIN_EMAIL")))
	if seedEmail == "" {
		seedEmail = "admin@kovio.local"
	}

	var target model.User
	if err := db.DB.First(&target, targetID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	// Never deactivate the fixed seeded admin account
	if strings.EqualFold(target.Email, seedEmail) && !req.IsActive {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot deactivate the seeded system admin account"})
		return
	}

	target.IsActive = req.IsActive
	if err := db.DB.Save(&target).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update user status"})
		return
	}

	audit.Record(actorUID, "admin_update_user_status", "user_"+strconv.FormatUint(uint64(targetID), 10)+"_active_"+strconv.FormatBool(req.IsActive), c.ClientIP())

	c.JSON(http.StatusOK, gin.H{
		"id":        target.ID,
		"email":     target.Email,
		"is_active": target.IsActive,
	})
}
