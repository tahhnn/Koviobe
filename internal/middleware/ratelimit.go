package middleware

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/cache"
)

// RateLimit enforces a per-IP (and optional suffix) rate limit via Redis.
// failClosed=true rejects requests when Redis is unavailable (auth endpoints).
func RateLimit(prefix string, maxAttempts int64, window time.Duration, failClosed bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()
		key := fmt.Sprintf("rate:%s:%s", prefix, ip)
		ctx := context.Background()

		count, err := cache.RDB.Incr(ctx, key).Result()
		if err != nil {
			log.Printf("[RateLimit] Redis error on %s: %v", prefix, err)
			if failClosed {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Service temporarily unavailable. Please try again."})
				c.Abort()
				return
			}
			c.Next()
			return
		}

		if count == 1 {
			cache.RDB.Expire(ctx, key, window)
		}

		if count > maxAttempts {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":       "Too many requests. Please wait before trying again.",
				"retry_after": window.String(),
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

// JoinRoomRateLimit: 10 join attempts per IP per minute (fail open if Redis down).
func JoinRoomRateLimit() gin.HandlerFunc {
	return RateLimit("join", 10, time.Minute, false)
}

// AuthRateLimit: 20 auth attempts per IP per minute (fail closed).
func AuthRateLimit() gin.HandlerFunc {
	return RateLimit("auth", 20, time.Minute, true)
}

// PinLookupRateLimit: 30 PIN lookups per IP per minute.
func PinLookupRateLimit() gin.HandlerFunc {
	return RateLimit("pin", 30, time.Minute, false)
}
