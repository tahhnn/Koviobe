package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/cache"
	"github.com/redis/go-redis/v9"
)

// incrWithTTL increments a counter and guarantees it carries a TTL, in one round
// trip. Setting the expiry whenever the key has none (-1) also repairs a key that
// somehow lost its TTL, so a stuck counter self-heals rather than locking an IP
// out until someone deletes it by hand.
var incrWithTTL = redis.NewScript(`
local count = redis.call("INCR", KEYS[1])
if redis.call("TTL", KEYS[1]) < 0 then
  redis.call("EXPIRE", KEYS[1], ARGV[1])
end
return count
`)

// RateLimit enforces a per-IP rate limit via Redis.
// failClosed=true rejects requests when Redis is unavailable (auth endpoints).
func RateLimit(prefix string, maxAttempts int64, window time.Duration, failClosed bool) gin.HandlerFunc {
	return RateLimitKey(prefix, "", maxAttempts, window, failClosed)
}

// RateLimitKey is RateLimit with an explicit bucket identity. Pass an empty
// identity to key on the client IP; pass a user or token id to key on that
// instead, so callers who legitimately share one NAT address do not consume each
// other's budget.
func RateLimitKey(prefix, identity string, maxAttempts int64, window time.Duration, failClosed bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		bucket := identity
		if bucket == "" {
			bucket = c.ClientIP()
		}
		key := fmt.Sprintf("rate:%s:%s", prefix, bucket)
		ctx := context.Background()

		// INCR and EXPIRE must be atomic. Setting the TTL in a separate command,
		// and only when count == 1, meant a crash or a failed EXPIRE in that gap
		// left a key with no TTL — which never resets, so the IP stayed rate
		// limited permanently. The script also re-arms a TTL that went missing.
		res, err := incrWithTTL.Run(ctx, cache.RDB, []string{key}, int64(window.Seconds())).Result()
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

		count, ok := res.(int64)
		if !ok {
			log.Printf("[RateLimit] unexpected Redis reply type %T on %s", res, prefix)
			if failClosed {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Service temporarily unavailable. Please try again."})
				c.Abort()
				return
			}
			c.Next()
			return
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

// RedeemRateLimit: 8 activation-code attempts per authenticated user per minute,
// with a looser per-IP backstop (fail closed on both).
//
// Keyed on user_id rather than the client IP, for the same reason
// SubmitAnswerRateLimit keys on the player token: a school or an office
// activating a batch of accounts shares one NAT address, and an IP key would make
// those colleagues throttle each other out of an action each of them is entitled
// to perform exactly once.
//
// The per-user limit is the real guard — guessing a code requires burning attempts
// against an account that already exists. The IP backstop is deliberately loose
// (60/min): it only catches someone driving many accounts from one host, which the
// per-user limit alone would not see.
//
// Fail closed on both: the code is the only secret protecting a paid plan, so an
// unmetered brute force is worse than a redeem outage. The code space is ~5.3e17,
// so this is not what makes guessing infeasible — it caps the damage if a batch
// ever leaks in a predictable range.
func RedeemRateLimit() gin.HandlerFunc {
	perIP := RateLimit("redeem_ip", 60, time.Minute, true)
	return func(c *gin.Context) {
		perIP(c)
		if c.IsAborted() {
			return
		}
		userID, exists := c.Get("user_id")
		if !exists {
			// Unauthenticated: the route's auth middleware rejects it anyway.
			c.Next()
			return
		}
		uid, ok := userID.(uint)
		if !ok {
			c.Next()
			return
		}
		RateLimitKey("redeem_user", strconv.FormatUint(uint64(uid), 10), 8, time.Minute, true)(c)
	}
}

// PinLookupRateLimit: 30 PIN lookups per IP per minute.
func PinLookupRateLimit() gin.HandlerFunc {
	return RateLimit("pin", 30, time.Minute, false)
}

// SubmitAnswerRateLimit: 60 submissions per player per minute.
//
// Keyed on the player token rather than the client IP for two reasons: a whole
// classroom legitimately shares one NAT address, so an IP key would throttle
// real players, while a single valid 4-hour token is otherwise enough to hammer
// an endpoint that runs several queries, a SELECT FOR UPDATE and a realtime
// publish per call. Requests with no token fall through to the handler, which
// rejects them with 401 anyway.
func SubmitAnswerRateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.GetHeader("X-Player-Token")
		if token == "" {
			c.Next()
			return
		}

		// Hash the token: it is a bearer credential and must not become a Redis
		// key that shows up in logs, SCAN output or a memory dump.
		sum := sha256.Sum256([]byte(token))
		key := "rate:answer:" + hex.EncodeToString(sum[:16])
		ctx := context.Background()

		res, err := incrWithTTL.Run(ctx, cache.RDB, []string{key}, int64(60)).Result()
		if err != nil {
			// Fail open: Redis being down must not stop a live game.
			log.Printf("[RateLimit] Redis error on answer limiter: %v", err)
			c.Next()
			return
		}
		count, ok := res.(int64)
		if !ok || count <= 60 {
			c.Next()
			return
		}

		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":       "Too many answer submissions. Please slow down.",
			"retry_after": "1m",
		})
		c.Abort()
	}
}
