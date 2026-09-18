// Package secmon counts security-relevant events and alerts once a count
// crosses a threshold inside a window.
//
// It exists because the events it watches are individually normal. One failed
// login is a typo; sixty from one address in ten minutes is an attack. Alerting
// per event would be pure noise, so the alert fires on the crossing — exactly
// once per window — and the window then runs out silently.
package secmon

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/quizzzone/backend/internal/cache"
	"github.com/quizzzone/backend/internal/pkg/notify"
)

// Thresholds are per source IP per window.
const (
	loginFailWindow    = 10 * time.Minute
	loginFailThreshold = 25

	redeemFailWindow    = 30 * time.Minute
	redeemFailThreshold = 10
)

// LoginFailed records one failed login. email is included in the alert because
// a spread of many emails from one IP is credential stuffing, while one email
// repeated is a targeted guess — the response differs.
func LoginFailed(ip, email string) {
	if crossed("loginfail", ip, loginFailThreshold, loginFailWindow) {
		notify.P1("bruteforce_login",
			"Đăng nhập sai %d lần trong %s từ IP %s (lần cuối: %s) — nghi brute force / credential stuffing.",
			loginFailThreshold, loginFailWindow, ip, email)
	}
}

// RedeemFailed records one rejected license code. A run of these is somebody
// enumerating the code space, and the code IS the only secret protecting a paid
// plan.
func RedeemFailed(ip string, userID uint) {
	if crossed("redeemfail", ip, redeemFailThreshold, redeemFailWindow) {
		notify.P1("bruteforce_redeem",
			"Nhập sai mã license %d lần trong %s từ IP %s (user=%d) — nghi đang dò mã.",
			redeemFailThreshold, redeemFailWindow, ip, userID)
	}
}

// crossed increments the counter and reports true only on the exact hit that
// reaches the threshold. Using == rather than >= is what makes the alert fire
// once instead of on every subsequent attempt in the window.
func crossed(bucket, ip string, threshold int64, window time.Duration) bool {
	if cache.RDB == nil || !notify.Enabled() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	key := fmt.Sprintf("secmon:%s:%s", bucket, ip)
	n, err := cache.RDB.Incr(ctx, key).Result()
	if err != nil {
		log.Printf("[secmon] Redis INCR failed on %s: %v", key, err)
		return false
	}
	if n == 1 {
		// Best effort: a missing TTL only means the window is longer than
		// intended, which under-alerts rather than spamming.
		if err := cache.RDB.Expire(ctx, key, window).Err(); err != nil {
			log.Printf("[secmon] Redis EXPIRE failed on %s: %v", key, err)
		}
	}
	return n == threshold
}
