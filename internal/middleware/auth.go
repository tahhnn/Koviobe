package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
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

		// Role.Permissions must be preloaded — the authorization decision below reads
		// them off this row, and without the preload user.Role is nil and every
		// caller silently degrades to no role and no permissions.
		var user model.User
		if err := db.DB.Preload("Role.Permissions").First(&user, claims.UserID).Error; err != nil {
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
		// token expired; the DB is authoritative and is already in hand here.
		role := "player"
		var permissions []string
		if user.Role != nil {
			role = user.Role.Name
			for _, perm := range user.Role.Permissions {
				permissions = append(permissions, perm.Name)
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
			c.JSON(http.StatusForbidden, gin.H{"error": "Forbidden: missing permission '" + requiredPermission + "'"})
			c.Abort()
			return
		}

		c.Next()
	}
}
