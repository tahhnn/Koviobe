package handler

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/cache"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/audit"
	"github.com/quizzzone/backend/internal/pkg/email"
	"github.com/quizzzone/backend/internal/pkg/jwt"
	"github.com/quizzzone/backend/internal/pkg/license"
	"github.com/quizzzone/backend/internal/realtime"
	"golang.org/x/crypto/bcrypt"
)

type LoginRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

type RegisterRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Nickname string `json:"nickname" binding:"required,min=1,max=100"`
}

type VerifyOTPRequest struct {
	Email string `json:"email" binding:"required,email"`
	OTP   string `json:"otp" binding:"required"`
}

type ChangePasswordRequest struct {
	OldPassword string `json:"old_password" binding:"required"`
	NewPassword string `json:"new_password" binding:"required,min=8"`
}

// Login authenticates a user and returns a JWT token containing role and permissions
func Login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	req.Email = strings.ToLower(req.Email)

	var user model.User
	if err := db.DB.Preload("Role.Permissions").Where("email = ?", req.Email).First(&user).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid email or password"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.Password)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid email or password"})
		return
	}

	if !user.IsActive {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Account is deactivated"})
		return
	}

	// Extract permission names
	var permissions []string
	roleName := "player"
	if user.Role != nil {
		roleName = user.Role.Name
		for _, perm := range user.Role.Permissions {
			permissions = append(permissions, perm.Name)
		}
	}

	token, err := jwt.GenerateToken(user.ID, user.Email, roleName, permissions)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}

	refreshToken, err := jwt.GenerateRefreshToken(user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate refresh token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token":         token,
		"refresh_token": refreshToken,
		"user": gin.H{
			"id":          user.ID,
			"email":       user.Email,
			"role":        roleName,
			"permissions": permissions,
			"nickname":    user.Nickname,
		},
	})
}

// Register generates a 6-digit OTP and saves it to Redis. It does not create the user account yet.
func Register(c *gin.Context) {
	var req RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	req.Email = strings.ToLower(req.Email)

	// Check if user already exists
	var count int64
	db.DB.Model(&model.User{}).Where("email = ?", req.Email).Count(&count)
	if count > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Email is already registered"})
		return
	}

	// Generate 6-digit OTP
	n, err := rand.Int(rand.Reader, big.NewInt(900000))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate OTP"})
		return
	}
	otp := fmt.Sprintf("%06d", n.Int64()+100000)

	// Save OTP + Nickname as JSON to Redis
	type RedisOTPData struct {
		OTP      string `json:"otp"`
		Nickname string `json:"nickname"`
	}
	otpData := RedisOTPData{
		OTP:      otp,
		Nickname: req.Nickname,
	}
	otpBytes, err := json.Marshal(otpData)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process data"})
		return
	}

	// Save OTP to Redis with a 10-minute TTL
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	otpKey := fmt.Sprintf("otp:%s", req.Email)
	err = cache.RDB.Set(ctx, otpKey, string(otpBytes), 10*time.Minute).Err()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save verification code"})
		return
	}

	// Reset attempts limit on new registration
	attemptsKey := fmt.Sprintf("otp_attempts:%s", req.Email)
	cache.RDB.Del(ctx, attemptsKey)

	// Send OTP email
	htmlBody := fmt.Sprintf(`
		<div style="font-family: sans-serif; padding: 20px; border: 1px solid #eee; border-radius: 10px; max-width: 500px;">
			<h2 style="color: #4f46e5; text-align: center;">⚔️ QUIZBATTLE Verification</h2>
			<p>Hello,</p>
			<p>Thank you for registering. Here is your verification code to complete the process:</p>
			<div style="background: #f3f4f6; padding: 15px; border-radius: 8px; text-align: center; font-size: 24px; font-weight: bold; letter-spacing: 4px; color: #1f2937; margin: 20px 0;">
				%s
			</div>
			<p style="font-size: 12px; color: #6b7280; margin-top: 20px; text-align: center;">
				This code is valid for 10 minutes. If you did not request this code, you can safely ignore this email.
			</p>
		</div>
	`, otp)
	
	// We call email.SendEmail and log the output. We don't block registration on email failure to prevent API issues during temporary SMTP outages.
	go func() {
		_ = email.SendEmail(req.Email, "⚔️ QUIZBATTLE: Verify Your Account", htmlBody)
	}()

	c.JSON(http.StatusOK, gin.H{
		"message": "Verification code has been sent to your email.",
	})
}

// VerifyOTP verifies the OTP. If valid, creates the registered user and signs them in immediately.
func VerifyOTP(c *gin.Context) {
	var req VerifyOTPRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	req.Email = strings.ToLower(req.Email)

	// Fetch OTP from Redis
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Anti-Brute-Force: check maximum failed attempts (max 5)
	attemptsKey := fmt.Sprintf("otp_attempts:%s", req.Email)
	attemptsStr, err := cache.RDB.Get(ctx, attemptsKey).Result()
	var attempts int
	if err == nil {
		fmt.Sscanf(attemptsStr, "%d", &attempts)
	}

	if attempts >= 5 {
		// Invalidate OTP on too many failed attempts
		otpKey := fmt.Sprintf("otp:%s", req.Email)
		cache.RDB.Del(ctx, otpKey)
		cache.RDB.Del(ctx, attemptsKey)
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many failed verification attempts. Your verification code has been invalidated. Please register again."})
		return
	}

	otpKey := fmt.Sprintf("otp:%s", req.Email)
	savedOTPJSON, err := cache.RDB.Get(ctx, otpKey).Result()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Verification code has expired or is invalid"})
		return
	}

	type RedisOTPData struct {
		OTP      string `json:"otp"`
		Nickname string `json:"nickname"`
	}
	var otpData RedisOTPData
	if err := json.Unmarshal([]byte(savedOTPJSON), &otpData); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process verification data"})
		return
	}

      // Soften OTP compare length mismatch without panic
	otpMatch := len(otpData.OTP) == len(req.OTP) &&
		subtle.ConstantTimeCompare([]byte(otpData.OTP), []byte(req.OTP)) == 1
	if !otpMatch {
		// Increment attempts
		cache.RDB.Incr(ctx, attemptsKey)
		cache.RDB.Expire(ctx, attemptsKey, 10*time.Minute)
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid verification code. Attempts remaining: %d", 5-(attempts+1))})
		return
	}

	// OTP is valid, check if user exists one more time
	var count int64
	db.DB.Model(&model.User{}).Where("email = ?", req.Email).Count(&count)
	if count > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Email is already registered"})
		return
	}

	// Fetch default system "host" role
	var hostRole model.Role
	if err := db.DB.Where("name = ?", "host").First(&hostRole).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to locate default user role"})
		return
	}

	// Auto-generate a password and hash it
	generatedPassword := generateRandomPassword()
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(generatedPassword), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process password"})
		return
	}

	newUser := model.User{
		RoleID:   &hostRole.ID,
		Email:    req.Email,
		Password: string(hashedPassword),
		Nickname: otpData.Nickname, // [FIXED] Use Nickname from register payload instead of hardcoded "Host"
	}

	if err := db.DB.Create(&newUser).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create user"})
		return
	}

	_ = license.EnsureFreeSubscription(newUser.ID)

	// Delete OTP and attempts from Redis
	cache.RDB.Del(ctx, otpKey)
	cache.RDB.Del(ctx, attemptsKey)

	// Record Audit Log
	audit.Record(newUser.ID, "verify_otp", fmt.Sprintf("user_%d", newUser.ID), c.ClientIP())

	// Send temporary password email
	pwdBody := fmt.Sprintf(`
		<div style="font-family: sans-serif; padding: 20px; border: 1px solid #eee; border-radius: 10px; max-width: 500px;">
			<h2 style="color: #10b981; text-align: center;">🎉 Verification Successful!</h2>
			<p>Hello,</p>
			<p>Your account has been verified and successfully created.</p>
			<p>Here is your temporary login password:</p>
			<div style="background: #f3f4f6; padding: 15px; border-radius: 8px; text-align: center; font-size: 20px; font-weight: bold; color: #047857; font-family: monospace; margin: 20px 0;">
				%s
			</div>
			<p style="font-weight: bold; color: #dc2626; margin-top: 15px;">
				⚠️ Note: Please change this password in your Settings as soon as you log in!
			</p>
		</div>
	`, generatedPassword)
	
	go func() {
		_ = email.SendEmail(req.Email, "🎉 QUIZBATTLE: Your Temporary Password", pwdBody)
	}()

	// Preload role permissions for JWT generation
	if err := db.DB.Preload("Role.Permissions").First(&newUser, newUser.ID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to build user session"})
		return
	}

	var permissions []string
	roleName := "host"
	if newUser.Role != nil {
		roleName = newUser.Role.Name
		for _, perm := range newUser.Role.Permissions {
			permissions = append(permissions, perm.Name)
		}
	}

	token, err := jwt.GenerateToken(newUser.ID, newUser.Email, roleName, permissions)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}

	refreshToken, err := jwt.GenerateRefreshToken(newUser.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate refresh token"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"message": "Account verified and created successfully. Your temporary password has been sent to your email.",
		"token":   token,
		"refresh_token": refreshToken,
		"user": gin.H{
			"id":          newUser.ID,
			"email":       newUser.Email,
			"role":        roleName,
			"permissions": permissions,
			"nickname":    newUser.Nickname,
		},
	})
}

// ChangePassword allows an authenticated user to change their password
func ChangePassword(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	var req ChangePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var user model.User
	if err := db.DB.First(&user, userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	// Verify old password
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.OldPassword)); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Incorrect current password"})
		return
	}

	// Hash new password
	newHashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to hash new password"})
		return
	}

	user.Password = string(newHashedPassword)
	if err := db.DB.Save(&user).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update password"})
		return
	}

	// Record Audit Log
	audit.Record(user.ID, "change_password", fmt.Sprintf("user_%d", user.ID), c.ClientIP())

	c.JSON(http.StatusOK, gin.H{"message": "Password changed successfully."})
}

// GetProfile returns the current authenticated user's profile
func GetProfile(c *gin.Context) {
	userID, _ := c.Get("user_id")

	var user model.User
	if err := db.DB.Preload("Role.Permissions").First(&user, userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	c.JSON(http.StatusOK, user)
}

// GetRealtimeToken generates a Centrifugo connection token scoped to a room channel.
// Host: requires AuthMiddleware + room_id query (must own the room).
// Player: requires X-Player-Token; channel is derived from the joined room.
func GetRealtimeToken(c *gin.Context) {
	var clientID string
	var channel string

	if userIDVal, exists := c.Get("user_id"); exists {
		roomIDStr := c.Query("room_id")
		if roomIDStr == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "room_id query parameter is required"})
			return
		}
		roomID, err := strconv.ParseUint(roomIDStr, 10, 32)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid room_id"})
			return
		}
		var room model.Room
		if err := db.DB.Where("id = ? AND host_id = ?", uint(roomID), userIDVal.(uint)).First(&room).Error; err != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "Room not found or access denied"})
			return
		}
		clientID = fmt.Sprintf("user_%v", userIDVal)
		channel = realtime.RoomChannel(room.PinCode)
	} else {
		playerTokenStr := c.GetHeader("X-Player-Token")
		if playerTokenStr == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "X-Player-Token header is required for anonymous realtime token"})
			return
		}
		claims, err := jwt.VerifyPlayerToken(playerTokenStr)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired player token"})
			return
		}
		var room model.Room
		if err := db.DB.First(&room, claims.RoomID).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Room not found"})
			return
		}
		clientID = fmt.Sprintf("player_%d_%s", claims.PlayerID, claims.Nickname)
		channel = realtime.RoomChannel(room.PinCode)
	}

	token, err := realtime.Client.GenerateConnectionToken(clientID, 3600, channel)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate connection token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token":     token,
		"client_id": clientID,
		"channel":   channel,
	})
}

// GetPlayerRealtimeToken generates a Centrifugo connection token for players.
func GetPlayerRealtimeToken(c *gin.Context) {
	playerTokenStr := c.GetHeader("X-Player-Token")
	if playerTokenStr == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "X-Player-Token header is required"})
		return
	}

	claims, err := jwt.VerifyPlayerToken(playerTokenStr)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired player token"})
		return
	}

	var room model.Room
	if err := db.DB.First(&room, claims.RoomID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Room not found"})
		return
	}

	clientID := fmt.Sprintf("player_%d_%s", claims.PlayerID, claims.Nickname)
	channel := realtime.RoomChannel(room.PinCode)
	token, err := realtime.Client.GenerateConnectionToken(clientID, 3600, channel)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate connection token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token":     token,
		"client_id": clientID,
		"channel":   channel,
	})
}

// generateRandomPassword creates a cryptographically secure random password.
// [C-1 FIX] Uses crypto/rand (CSPRNG) instead of math/rand (PRNG) to prevent
// password prediction attacks based on timestamp seeding.
func generateRandomPassword() string {
	digits := "0123456789"
	specials := "@#$%"
	letters := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	allChars := letters + digits + specials

	cryptoRandChar := func(charset string) byte {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		return charset[n.Int64()]
	}

	// Guarantee at least one character from each required set
	pwd := make([]byte, 10)
	pwd[0] = cryptoRandChar(letters)
	pwd[1] = cryptoRandChar(letters)
	pwd[2] = cryptoRandChar(digits)
	pwd[3] = cryptoRandChar(digits)
	pwd[4] = cryptoRandChar(specials)
	for i := 5; i < 10; i++ {
		pwd[i] = cryptoRandChar(allChars)
	}

	// Shuffle using crypto/rand for Fisher-Yates
	for i := len(pwd) - 1; i > 0; i-- {
		j, _ := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		pwd[i], pwd[j.Int64()] = pwd[j.Int64()], pwd[i]
	}

	return string(pwd)
}

type RefreshTokenRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

// RefreshToken receives a refresh token, revokes it, and issues a new pair.
func RefreshToken(c *gin.Context) {
	var req RefreshTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	claims, err := jwt.VerifyRefreshToken(req.RefreshToken)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired refresh token"})
		return
	}

	// Rotate: revoke the presented refresh token immediately
	jwt.RevokeRefreshToken(req.RefreshToken)

	var user model.User
	if err := db.DB.Preload("Role.Permissions").First(&user, claims.UserID).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "User not found"})
		return
	}

	if !user.IsActive {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Account is deactivated"})
		return
	}

	var permissions []string
	roleName := "player"
	if user.Role != nil {
		roleName = user.Role.Name
		for _, perm := range user.Role.Permissions {
			permissions = append(permissions, perm.Name)
		}
	}

	newAccessToken, err := jwt.GenerateToken(user.ID, user.Email, roleName, permissions)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate access token"})
		return
	}

	newRefreshToken, err := jwt.GenerateRefreshToken(user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate refresh token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token":         newAccessToken,
		"refresh_token": newRefreshToken,
	})
}

// Logout revokes the refresh token so it cannot be reused.
func Logout(c *gin.Context) {
	var req RefreshTokenRequest
	_ = c.ShouldBindJSON(&req)
	if req.RefreshToken != "" {
		jwt.RevokeRefreshToken(req.RefreshToken)
	}
	c.JSON(http.StatusOK, gin.H{"message": "Logged out"})
}
