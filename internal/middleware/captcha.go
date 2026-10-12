package middleware

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/pkg/captcha"
)

// CaptchaHeader carries the Turnstile token. A header rather than a body field
// so the check can run before the handler binds its JSON.
const CaptchaHeader = "X-Captcha-Token"

// RequireCaptcha rejects the request unless it carries a Turnstile token solved
// for action. A no-op while TURNSTILE_SECRET_KEY is unset.
func RequireCaptcha(action string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := captcha.Verify(c.Request.Context(), c.GetHeader(CaptchaHeader), action, c.ClientIP()); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "CAPTCHA_FAILED"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// MailRequestRateLimit caps, per IP and per hour, the requests that make us
// send mail to a typed address. AuthRateLimit (20/min) is sized for logins and
// token refreshes; left alone it allows 1200 signup mails an hour from one
// host. Fails closed: a Redis outage should not become an open mail relay.
func MailRequestRateLimit() gin.HandlerFunc {
	return RateLimit("mail_ip", int64(capacityLimit(func() int {
		return config.AppConfig.MailRequestsPerIPHour
	}, 10)), time.Hour, true)
}
