package handler

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/cache"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/audit"
	"github.com/quizzzone/backend/internal/pkg/email"
	"github.com/quizzzone/backend/internal/pkg/jwt"
	"github.com/quizzzone/backend/internal/pkg/notify"
	"golang.org/x/crypto/bcrypt"
)

const (
	resetCodeTTL     = 10 * time.Minute
	resetCooldown    = 60 * time.Second
	resetMaxAttempts = 5
	// The attempt counter outlives any single code and is NOT cleared when a
	// new code is requested. Clearing it (as Register does) would let an
	// attacker guess 5 times, request a fresh code, and repeat — 5 guesses per
	// cooldown window against a 6-digit space. Tied to the hour instead, the
	// ceiling is 5 guesses per email per hour.
	resetAttemptsTTL = time.Hour
)

// resetSentMessage is returned for every well-formed forgot-password request,
// whether or not the account exists, so the endpoint cannot be used to find
// out which emails are registered.
const resetSentMessage = "If an account exists for this email, a password reset code has been sent."

type ForgotPasswordRequest struct {
	Email string `json:"email" binding:"required,email"`
}

type ResetPasswordRequest struct {
	Email       string `json:"email" binding:"required,email"`
	OTP         string `json:"otp" binding:"required"`
	NewPassword string `json:"new_password" binding:"required,min=8,max=72"`
}

func resetCodeKey(email string) string     { return "pwreset:" + email }
func resetAttemptsKey(email string) string { return "pwreset_attempts:" + email }
func resetCooldownKey(email string) string { return "pwreset_cooldown:" + email }

// ForgotPassword emails a 6-digit reset code to an existing, active account.
func ForgotPassword(c *gin.Context) {
	var req ForgotPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The cooldown is set for unknown emails too; skipping it there would make
	// "second request is throttled" an existence oracle.
	ok, err := cache.RDB.SetNX(ctx, resetCooldownKey(req.Email), "1", resetCooldown).Result()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process request"})
		return
	}
	if !ok {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Please wait a minute before requesting another code"})
		return
	}

	var user model.User
	if err := db.DB.Where("email = ?", req.Email).First(&user).Error; err != nil || !user.IsActive {
		audit.Record(0, "password_reset_request_no_user", req.Email, c.ClientIP())
		c.JSON(http.StatusOK, gin.H{"message": resetSentMessage})
		return
	}

	n, err := rand.Int(rand.Reader, big.NewInt(900000))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate OTP"})
		return
	}
	otp := fmt.Sprintf("%06d", n.Int64()+100000)

	if err := cache.RDB.Set(ctx, resetCodeKey(req.Email), otp, resetCodeTTL).Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save verification code"})
		return
	}

	htmlBody := fmt.Sprintf(`
		<div style="font-family: sans-serif; padding: 20px; border: 1px solid #eee; border-radius: 10px; max-width: 500px;">
			<h2 style="color: #4f46e5; text-align: center;">🔑 QUIZBATTLE Password Reset</h2>
			<p>Hello,</p>
			<p>We received a request to reset the password for your account. Enter this code to choose a new password:</p>
			<div style="background: #f3f4f6; padding: 15px; border-radius: 8px; text-align: center; font-size: 24px; font-weight: bold; letter-spacing: 4px; color: #1f2937; margin: 20px 0;">
				%s
			</div>
			<p style="font-size: 12px; color: #6b7280; margin-top: 20px; text-align: center;">
				This code is valid for 10 minutes. If you did not request a password reset, ignore this email — your password stays unchanged.
			</p>
		</div>
	`, otp)

	go func() {
		_ = email.SendEmail(req.Email, "🔑 QUIZBATTLE: Password Reset Code", htmlBody)
	}()

	audit.Record(user.ID, "password_reset_request", fmt.Sprintf("user_%d", user.ID), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"message": resetSentMessage})
}

// ResetPassword checks the emailed code and sets the new password.
func ResetPassword(c *gin.Context) {
	var req ResetPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.OTP = strings.TrimSpace(req.OTP)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	codeKey := resetCodeKey(req.Email)
	attemptsKey := resetAttemptsKey(req.Email)

	var attempts int
	if s, err := cache.RDB.Get(ctx, attemptsKey).Result(); err == nil {
		fmt.Sscanf(s, "%d", &attempts)
	}
	if attempts >= resetMaxAttempts {
		cache.RDB.Del(ctx, codeKey)
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many failed attempts. Please try again in an hour."})
		return
	}

	saved, err := cache.RDB.Get(ctx, codeKey).Result()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Verification code has expired or is invalid"})
		return
	}

	match := len(saved) == len(req.OTP) &&
		subtle.ConstantTimeCompare([]byte(saved), []byte(req.OTP)) == 1
	if !match {
		cache.RDB.Incr(ctx, attemptsKey)
		cache.RDB.Expire(ctx, attemptsKey, resetAttemptsTTL)
		audit.Record(0, "password_reset_bad_code", req.Email, c.ClientIP())
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid verification code"})
		return
	}

	var user model.User
	if err := db.DB.Where("email = ?", req.Email).First(&user).Error; err != nil || !user.IsActive {
		cache.RDB.Del(ctx, codeKey)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Verification code has expired or is invalid"})
		return
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to hash new password"})
		return
	}

	// Single-use: delete before writing, so a replay racing this request finds
	// no code. Del returning 0 means another request already consumed it.
	if n, err := cache.RDB.Del(ctx, codeKey).Result(); err != nil || n == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Verification code has expired or is invalid"})
		return
	}

	if err := db.DB.Model(&model.User{}).Where("id = ?", user.ID).Update("password", string(hashed)).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update password"})
		return
	}
	cache.RDB.Del(ctx, attemptsKey)

	// Same reasoning as ChangePassword: a reset is often triggered because
	// someone else may hold a session, so old refresh tokens must die.
	if err := jwt.RevokeAllRefreshTokens(user.ID); err != nil {
		log.Printf("[ResetPassword] failed to revoke refresh tokens for user %d: %v", user.ID, err)
		notify.P1("revoke_tokens_failed", "Reset mật khẩu nhưng không thu hồi được refresh token (user=%d): %v — phiên cũ vẫn dùng được.", user.ID, err)
	}

	audit.Record(user.ID, "password_reset", fmt.Sprintf("user_%d", user.ID), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"message": "Password has been reset. You can now sign in with your new password."})
}
