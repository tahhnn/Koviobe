package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/pkg/captcha"
)

// CaptchaConfig hands the browser the public Turnstile site key. Served at
// runtime rather than baked into the frontend build, so turning the captcha on
// or rotating the key is an .env change plus a backend restart. An empty key
// tells the frontend to render no widget.
func CaptchaConfig(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"site_key": captcha.SiteKey()})
}
