package middleware

import (
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/pkg/notify"
)

// alertedKey marks a request whose failure has already been reported, so a
// panic is not also counted as an anonymous 5xx.
const alertedKey = "alert_reported"

// AlertRecovery replaces gin.Recovery(). Same contract — recover, log the
// stack, answer 500 — plus a P0 alert carrying the first frames of the stack.
// A panic is the one failure the code itself never anticipated, so it gets the
// highest level regardless of which endpoint it happened on.
func AlertRecovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			stack := string(debug.Stack())
			log.Printf("[PANIC] %s %s: %v\n%s", c.Request.Method, c.Request.URL.Path, rec, stack)

			// Coalesce by route pattern, not by URL: a panic in
			// GET /api/rooms/:id must not produce one alert per room.
			notify.P0("panic:"+routeOf(c),
				"PANIC ở %s %s\nclient=%s\n%v\n\n%s",
				c.Request.Method, routeOf(c), c.ClientIP(), rec, topFrames(stack, 12))

			c.Set(alertedKey, true)
			if !c.Writer.Written() {
				c.AbortWithStatusJSON(http.StatusInternalServerError,
					gin.H{"error": "Internal server error"})
			} else {
				c.Abort()
			}
		}()
		c.Next()
	}
}

// AlertServerErrors reports 5xx responses that no handler alerted on already.
// It is the backstop for failures nobody thought to instrument — a handler that
// returns 500 on an unexpected DB error still shows up in the feed.
//
// 4xx is deliberately NOT reported: a wrong PIN, an expired player token and a
// rate-limited join are normal traffic, and forwarding them would bury the
// signal. Brute force is tracked separately, by count, in internal/pkg/secmon.
func AlertServerErrors() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		status := c.Writer.Status()
		if status < 500 {
			return
		}
		if _, done := c.Get(alertedKey); done {
			return
		}
		// 503 from the rate limiter's fail-closed branch is a known, already
		// alerted condition (ratelimit_redis_error) and arrives in bulk.
		if status == http.StatusServiceUnavailable {
			return
		}

		detail := ""
		if len(c.Errors) > 0 {
			detail = "\n" + c.Errors.String()
		}
		notify.P0(fmt.Sprintf("http_%d:%s %s", status, c.Request.Method, routeOf(c)),
			"HTTP %d ở %s %s\nclient=%s%s",
			status, c.Request.Method, routeOf(c), c.ClientIP(), detail)
	}
}

// routeOf prefers the registered route pattern ("/api/rooms/:id") over the
// concrete path, which is what keeps the alert key stable across requests.
func routeOf(c *gin.Context) string {
	if p := c.FullPath(); p != "" {
		return p
	}
	return c.Request.URL.Path
}

// topFrames keeps the head of a stack trace. Telegram caps a message at 4096
// characters and the interesting frames are always at the top.
func topFrames(stack string, n int) string {
	lines := strings.Split(stack, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
