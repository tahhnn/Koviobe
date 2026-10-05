package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/pkg/jwt"
)

// AuthMiddleware authenticates requests using Bearer JWT tokens.
func AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authorization header is required"})
			c.Abort()
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if !(len(parts) == 2 && parts[0] == "Bearer") {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authorization header must be Bearer token"})
			c.Abort()
			return
		}

		tokenString := parts[1]
		claims, err := jwt.VerifyToken(tokenString)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired token"})
			c.Abort()
			return
		}

		user, ok := loadPrincipal(claims.UserID)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "User not found"})
			c.Abort()
			return
		}
		if !user.IsActive {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Account is deactivated"})
			c.Abort()
			return
		}

		// Role and permissions come from the freshly loaded user row, not from the
		// token claims. Reading them from the claims kept a demoted or
		// permission-stripped user at their old level until the 15-minute access
		// token expired; the DB is authoritative and is read on every request.
		role := "player"
		var permissions []string
		if user.RoleName != nil {
			role = *user.RoleName
			if user.Perms != "" {
				permissions = strings.Split(user.Perms, ",")
			}
		}

		// Set variables to Gin Context
		c.Set("user_id", user.ID)
		c.Set("email", user.Email)
		c.Set("role", role)
		c.Set("permissions", permissions)

		c.Next()
	}
}

// principal is what AuthMiddleware needs to know about the caller.
type principal struct {
	ID       uint
	Email    string
	IsActive bool
	RoleName *string // nil: no role, or the role was soft-deleted
	Perms    string  // comma-joined permission names; names never contain a comma
}

// loadPrincipal reads the user, their role and its permissions in one query.
//
// Preload("Role.Permissions") did the same in four (users, roles,
// role_permissions, permissions) on every authenticated request — ~35k calls
// of each since 2026-09-24. The joins keep the Preload's soft-delete rules:
// a deleted user is not found, and a deleted role reads as no role.
func loadPrincipal(userID uint) (principal, bool) {
	var p principal
	res := db.DB.Raw(`
		SELECT u.id, u.email, u.is_active, r.name AS role_name,
		       COALESCE(string_agg(p.name, ',' ORDER BY p.id), '') AS perms
		FROM users u
		LEFT JOIN roles r ON r.id = u.role_id AND r.deleted_at IS NULL
		LEFT JOIN role_permissions rp ON rp.role_id = r.id
		LEFT JOIN permissions p ON p.id = rp.permission_id
		WHERE u.id = ? AND u.deleted_at IS NULL
		GROUP BY u.id, r.name`, userID).Scan(&p)
	if res.Error != nil || res.RowsAffected == 0 {
		return principal{}, false
	}
	return p, true
}

// RequireRole checks if the user has one of the allowed roles.
func RequireRole(allowedRoles ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		roleVal, exists := c.Get("role")
		if !exists {
			c.JSON(http.StatusForbidden, gin.H{"error": "Role not found in context"})
			c.Abort()
			return
		}

		role := roleVal.(string)
		isAllowed := false
		for _, r := range allowedRoles {
			if strings.EqualFold(r, role) {
				isAllowed = true
				break
			}
		}

		if !isAllowed {
			c.JSON(http.StatusForbidden, gin.H{"error": "Forbidden: insufficient role permissions"})
			c.Abort()
			return
		}

		c.Next()
	}
}

// RequirePermission checks if the user has the required permission in their claims.
// Admin role always passes.
func RequirePermission(requiredPermission string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if roleVal, exists := c.Get("role"); exists {
			if role, ok := roleVal.(string); ok && strings.EqualFold(role, "admin") {
				c.Next()
				return
			}
		}

		permsVal, exists := c.Get("permissions")
		if !exists {
			c.JSON(http.StatusForbidden, gin.H{"error": "Permissions not found in context"})
			c.Abort()
			return
		}

		permissions, ok := permsVal.([]string)
		if !ok {
			c.JSON(http.StatusForbidden, gin.H{"error": "Invalid permissions format in context"})
			c.Abort()
			return
		}

		hasPermission := false
		for _, p := range permissions {
			if p == requiredPermission {
				hasPermission = true
				break
			}
		}

		if !hasPermission {
			c.JSON(http.StatusForbidden, gin.H{"error": "Forbidden: missing permission", "permission": requiredPermission})
			c.Abort()
			return
		}

		c.Next()
	}
}
