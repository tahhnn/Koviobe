package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/cache"
	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/audit"
	"github.com/quizzzone/backend/internal/pkg/email"
	"github.com/quizzzone/backend/internal/pkg/jwt"
	"github.com/quizzzone/backend/internal/pkg/notify"
	"golang.org/x/crypto/bcrypt"
)

const (
	resetTokenTTL = 30 * time.Minute
	resetCooldown = 60 * time.Second
	// 32 random bytes: guessing a live token is not a rate-limiting problem,
	// so there is no attempt counter (the 6-digit code needed one).
	resetTokenBytes = 32
)

// resetSentMessage is returned for every well-formed forgot-password request,
// whether or not the account exists, so the endpoint cannot be used to find
// out which emails are registered.
const resetSentMessage = "If an account exists for this email, a password reset link has been sent."

const resetInvalidMessage = "This reset link is invalid or has expired"

type ForgotPasswordRequest struct {
	Email string `json:"email" binding:"required,email"`
}

type ResetPasswordRequest struct {
	Token       string `json:"token" binding:"required,max=128"`
	NewPassword string `json:"new_password" binding:"required,min=8,max=72"`
}

type CheckResetTokenRequest struct {
	Token string `json:"token" binding:"required,max=128"`
}

// Only the SHA-256 of a token is used as a key, so listing the keys does not
// hand out working links. (The queued mail holds the link until it is sent.)
func resetTokenKey(hash string) string     { return "pwreset_tok:" + hash }
func resetUserKey(userID uint) string      { return fmt.Sprintf("pwreset_user:%d", userID) }
func resetCooldownKey(email string) string { return "pwreset_cooldown:" + email }

func hashResetToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// revokeResetToken kills the user's outstanding reset link, if any. Called on
// a new request (only the latest link works) and on any password change.
func revokeResetToken(ctx context.Context, userID uint) {
	if old, err := cache.RDB.GetDel(ctx, resetUserKey(userID)).Result(); err == nil && old != "" {
		cache.RDB.Del(ctx, resetTokenKey(old))
	}
}

// ForgotPassword emails a one-time reset link to an existing, active account.
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

	appURL := config.AppConfig.PublicAppURL
	if appURL == "" {
		// Without it there is nowhere to point the link. Same answer to the
		// caller as any other request; the operator gets told.
		log.Printf("[ForgotPassword] PUBLIC_APP_URL is not set; reset link not sent")
		notify.P1("reset_no_app_url", "PUBLIC_APP_URL chưa đặt — không gửi được link đặt lại mật khẩu.")
		c.JSON(http.StatusOK, gin.H{"message": resetSentMessage})
		return
	}

	raw := make([]byte, resetTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process request"})
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash := hashResetToken(token)

	revokeResetToken(ctx, user.ID)
	pipe := cache.RDB.TxPipeline()
	pipe.Set(ctx, resetTokenKey(hash), strconv.FormatUint(uint64(user.ID), 10), resetTokenTTL)
	pipe.Set(ctx, resetUserKey(user.ID), hash, resetTokenTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process request"})
		return
	}

	// Fragment, not query string: browsers never send it to the server, so the
	// token stays out of nginx access logs and Referer headers.
	link := appURL + "/reset-password#token=" + token
	htmlBody := fmt.Sprintf(`
		<div style="font-family: sans-serif; padding: 20px; border: 1px solid #eee; border-radius: 10px; max-width: 500px;">
			<h2 style="color: #4f46e5; text-align: center;">quizzZone — Đặt lại mật khẩu</h2>
			<p>Xin chào,</p>
			<p>Chúng tôi nhận được yêu cầu đặt lại mật khẩu cho tài khoản quizzZone dùng email này. Bấm nút dưới đây để chọn mật khẩu mới:</p>
			<p style="text-align: center; margin: 24px 0;">
				<a href="%[1]s" style="background: #e85d4c; color: #ffffff; padding: 12px 24px; border-radius: 8px; text-decoration: none; font-weight: bold; display: inline-block;">Đặt lại mật khẩu</a>
			</p>
			<p style="font-size: 13px; color: #4b5563;">Nếu nút không bấm được, mở liên kết này:<br><a href="%[1]s" style="color: #4f46e5; word-break: break-all;">%[1]s</a></p>
			<p style="font-size: 12px; color: #6b7280; margin-top: 20px;">
				Liên kết dùng được một lần và hết hạn sau 30 phút. Nếu bạn không yêu cầu đặt lại mật khẩu, hãy bỏ qua email này — mật khẩu của bạn không thay đổi.
			</p>
		</div>
	`, html.EscapeString(link))

	// Queued and capped per recipient: the cooldown alone still let one
	// address receive 60 reset mails an hour.
	_ = email.Enqueue(req.Email, "Đặt lại mật khẩu quizzZone", htmlBody)

	audit.Record(user.ID, "password_reset_request", fmt.Sprintf("user_%d", user.ID), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"message": resetSentMessage})
}

// lookupResetToken returns the user a live token belongs to, without
// consuming it.
func lookupResetToken(ctx context.Context, token string) (uint, bool) {
	s, err := cache.RDB.Get(ctx, resetTokenKey(hashResetToken(strings.TrimSpace(token)))).Result()
	if err != nil {
		return 0, false
	}
	id, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return uint(id), true
}

// CheckResetToken lets the reset page say "this link has expired" before the
// user types a new password. It does not consume the token: mail scanners
// that prefetch links only load the page, and the page only calls this.
func CheckResetToken(c *gin.Context) {
	var req CheckResetTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": resetInvalidMessage})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, ok := lookupResetToken(ctx, req.Token); !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": resetInvalidMessage})
		return
	}
	c.JSON(http.StatusOK, gin.H{"valid": true})
}

// ResetPassword consumes the token from the emailed link and sets the new
// password.
func ResetPassword(c *gin.Context) {
	var req ResetPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Hash first, before the token is consumed: bcrypt takes ~100ms and the
	// token must not be burned by a request that then fails on it.
	hashed, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to hash new password"})
		return
	}

	// GETDEL makes the token single-use even under concurrent submits: only
	// one request gets the value back.
	hash := hashResetToken(strings.TrimSpace(req.Token))
	s, err := cache.RDB.GetDel(ctx, resetTokenKey(hash)).Result()
	if err != nil {
		audit.Record(0, "password_reset_bad_token", "", c.ClientIP())
		c.JSON(http.StatusBadRequest, gin.H{"error": resetInvalidMessage})
		return
	}
	id, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": resetInvalidMessage})
		return
	}
	userID := uint(id)
	cache.RDB.Del(ctx, resetUserKey(userID))

	var user model.User
	if err := db.DB.First(&user, userID).Error; err != nil || !user.IsActive {
		c.JSON(http.StatusBadRequest, gin.H{"error": resetInvalidMessage})
		return
	}

	if err := db.DB.Model(&model.User{}).Where("id = ?", user.ID).Update("password", string(hashed)).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update password"})
		return
	}

	// Same reasoning as ChangePassword: a reset is often triggered because
	// someone else may hold a session, so old refresh tokens must die.
	if err := jwt.RevokeAllRefreshTokens(user.ID); err != nil {
		log.Printf("[ResetPassword] failed to revoke refresh tokens for user %d: %v", user.ID, err)
		notify.P1("revoke_tokens_failed", "Reset mật khẩu nhưng không thu hồi được refresh token (user=%d): %v — phiên cũ vẫn dùng được.", user.ID, err)
	}

	audit.Record(user.ID, "password_reset", fmt.Sprintf("user_%d", user.ID), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"message": "Password has been reset. You can now sign in with your new password."})
}
