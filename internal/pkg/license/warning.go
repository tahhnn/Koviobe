package license

import (
	"fmt"
	"html"
	"log"
	"math"
	"strings"
	"time"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/email"
)

// warnThresholds are the days-before-expiry marks a host is warned at.
//
// Walked in descending order: a term two days out is caught by the 3-day pass,
// marked at 3, then caught again by the 1-day pass. A worker outage skips the
// threshold it missed rather than sending a stale warning late.
var warnThresholds = []int{7, 3, 1}

// warnBatchLimit caps one pass. email.SendEmail is a synchronous SMTP dial, so
// N hosts is N serial round trips — better to carry the tail to the next hour
// than to hold the worker open for minutes.
const warnBatchLimit = 200

// ExpiryWarning is one host told their term is about to end.
type ExpiryWarning struct {
	UserID   uint
	Email    string
	Nickname string
	PlanID   string
	EndsAt   time.Time
	DaysLeft int
}

// WarnExpiringSubscriptions emails every host whose paid term is inside a
// warning window and has not been warned at that threshold yet.
//
// No-op while enforcement is off: warning a host about a cut that will not
// happen is a false alarm, and the first false alarm costs us every later real
// one.
func WarnExpiringSubscriptions() ([]ExpiryWarning, error) {
	if !Enforcing() {
		return nil, nil
	}

	now := time.Now()
	sent := make([]ExpiryWarning, 0)

	for _, threshold := range warnThresholds {
		if len(sent) >= warnBatchLimit {
			log.Printf("[LICENSE] expiry warning batch limit reached (%d), remainder deferred to the next pass", warnBatchLimit)
			break
		}

		var due []model.Subscription
		err := db.DB.
			Where("ends_at IS NOT NULL AND ends_at > ? AND ends_at <= ?", now, now.AddDate(0, 0, threshold)).
			Where("plan_id <> ?", PlanFree).
			Where("status = ?", "active").
			// Not yet warned for this exact term, or warned at a wider threshold.
			Where("warned_for_ends_at IS NULL OR warned_for_ends_at <> ends_at OR warned_threshold_days > ?", threshold).
			Limit(warnBatchLimit - len(sent)).
			Find(&due).Error
		if err != nil {
			return sent, err
		}

		for _, sub := range due {
			var u model.User
			if err := db.DB.First(&u, sub.UserID).Error; err != nil || strings.TrimSpace(u.Email) == "" {
				continue
			}

			w := ExpiryWarning{
				UserID:   sub.UserID,
				Email:    u.Email,
				Nickname: u.Nickname,
				PlanID:   sub.PlanID,
				EndsAt:   *sub.EndsAt,
				DaysLeft: int(math.Ceil(sub.EndsAt.Sub(now).Hours() / 24)),
			}

			// Mark BEFORE sending — the opposite of DeliverCodes, on purpose.
			// There, an unmarked-but-sent code makes an admin stop chasing a
			// customer who got nothing. Here, an unmarked row re-sends the same
			// warning on every pass, which is the exact bug the marker exists to
			// prevent. A failed send costs one missed warning; a failed mark
			// costs a mailbox full of duplicates.
			if err := db.DB.Model(&model.Subscription{}).
				Where("id = ?", sub.ID).
				UpdateColumns(map[string]interface{}{
					"warned_threshold_days": threshold,
					"warned_for_ends_at":    sub.EndsAt,
				}).Error; err != nil {
				log.Printf("[LICENSE] could not mark expiry warning for user %d: %v", sub.UserID, err)
				continue
			}

			if err := email.SendEmail(u.Email, "Gói quizzZone của bạn sắp hết hạn", expiryWarningHTML(w)); err != nil {
				log.Printf("[LICENSE] expiry warning to %s failed: %v", u.Email, err)
				continue
			}
			sent = append(sent, w)
		}
	}

	return sent, nil
}

// expiryWarningHTML renders the host-facing mail. Every interpolated value is
// escaped — the nickname is user input.
func expiryWarningHTML(w ExpiryWarning) string {
	greeting := "Xin chào,"
	if strings.TrimSpace(w.Nickname) != "" {
		greeting = "Xin chào " + html.EscapeString(strings.TrimSpace(w.Nickname)) + ","
	}

	when := w.EndsAt.Format("02/01/2006")
	days := fmt.Sprintf("%d ngày", w.DaysLeft)
	if w.DaysLeft <= 1 {
		days = "chưa tới 1 ngày"
	}

	return fmt.Sprintf(`<div style="font-family:system-ui,-apple-system,'Segoe UI',sans-serif;max-width:560px;margin:0 auto;color:#111">
	<h2 style="margin:0 0 4px">Gói quizzZone sắp hết hạn</h2>
	<p style="color:#555;margin:0 0 18px">%s gói <strong>%s</strong> của bạn còn %s, hết hạn ngày <strong>%s</strong>.</p>
	<p style="color:#444;font-size:14px;margin:0 0 18px">
		Khi hết hạn, tài khoản trở về trạng thái chưa kích hoạt: bạn sẽ không tạo được
		quiz hay phòng chơi mới, và phòng đang mở sẽ được kết thúc.
	</p>
	<h3 style="margin:22px 0 8px;font-size:15px">Gia hạn thế nào</h3>
	<ol style="color:#444;font-size:14px;padding-left:18px;margin:0 0 18px">
		<li>Liên hệ quản trị viên để lấy mã kích hoạt mới.</li>
		<li>Vào <strong>Cài đặt tài khoản</strong>, mục <strong>Mã kích hoạt</strong>.</li>
		<li>Dán mã rồi bấm <strong>Kích hoạt</strong>.</li>
	</ol>
	<p style="color:#777;font-size:12px;margin:0">Nếu bạn vừa gia hạn rồi thì bỏ qua email này.</p>
</div>`, greeting, html.EscapeString(w.PlanID), html.EscapeString(days), html.EscapeString(when))
}
