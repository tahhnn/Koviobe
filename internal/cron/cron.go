package cron

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"time"

	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/handler"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/license"
	"github.com/quizzzone/backend/internal/pkg/notify"
	"github.com/quizzzone/backend/internal/pkg/payment"
)

// StartCleanupWorker runs a background worker that cleans up abandoned rooms every hour.
func StartCleanupWorker() {
	log.Println("Starting abandoned rooms cleanup worker...")
	go func() {
		time.Sleep(10 * time.Second)
		cleanupAbandonedRooms()

		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for range ticker.C {
			cleanupAbandonedRooms()
		}
	}()
}

// StartLicenseExpiryWorker downgrades subscriptions the moment they lapse and
// closes any room the expired host still had open.
//
// Runs every 5 minutes rather than hourly: the window between "plan expired" and
// "host stops hosting" is time the customer is using capacity they no longer paid
// for, and an hour of that is long enough to run a whole event on.
//
// It is a no-op while LICENSE_ENFORCEMENT is off — SweepExpiredSubscriptions
// checks that itself, so turning enforcement off never ends anybody's game.
func StartLicenseExpiryWorker() {
	log.Println("Starting license expiry worker...")
	go func() {
		time.Sleep(20 * time.Second)
		sweepExpiredLicenses()

		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			sweepExpiredLicenses()
		}
	}()
}

// StartLicenseWarningWorker warns hosts before their term lapses.
//
// Hourly, not every five minutes like the expiry sweep: a warning is not
// time-critical to the minute, email.SendEmail is a synchronous SMTP dial, and an
// hourly tick bounds the blast radius if the "already warned" marker is ever
// wrong. WarnExpiringSubscriptions checks Enforcing() itself, so turning
// enforcement off never mails anybody about a cut that will not happen.
func StartLicenseWarningWorker() {
	log.Println("Starting license expiry warning worker...")
	go func() {
		time.Sleep(60 * time.Second)
		warnExpiringLicenses()

		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for range ticker.C {
			warnExpiringLicenses()
		}
	}()
}

func warnExpiringLicenses() {
	sent, err := license.WarnExpiringSubscriptions()
	if err != nil {
		log.Printf("[CRON ERROR] license expiry warnings failed: %v\n", err)
		notify.P1("cron_license_warn", "Gửi cảnh báo sắp hết hạn thất bại: %v — host sẽ bị cắt mà không được báo trước.", err)
		return
	}
	if len(sent) > 0 {
		log.Printf("[CRON] sent %d license expiry warning(s)", len(sent))
	}
}

// StartSettingsRefreshWorker re-reads runtime toggles into their in-process
// caches every minute.
//
// With one backend container this is belt-and-braces — the admin PUT updates
// that process's cache synchronously. It earns its keep in two cases: a second
// replica, where it is the only convergence mechanism (bounded at 60s), and a
// flag changed straight in psql during an incident.
func StartSettingsRefreshWorker() {
	log.Println("Starting settings refresh worker...")
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			license.RefreshEnforcement()
		}
	}()
}

func sweepExpiredLicenses() {
	grants, err := license.SweepExpiredSubscriptions()
	if err != nil {
		log.Printf("[CRON ERROR] license expiry sweep failed: %v\n", err)
		notify.P1("cron_license_sweep", "Quét hết hạn license thất bại: %v — host hết hạn vẫn giữ quyền.", err)
		return
	}
	if len(grants) == 0 {
		return
	}

	for _, g := range grants {
		log.Printf("[CRON] License expired: user %d (%s) %s -> free, %d open room(s)\n",
			g.UserID, g.Email, g.PreviousPlan, len(g.RoomIDs))

		for _, roomID := range g.RoomIDs {
			// Close through the handler's own finalize path so players get
			// game:ended and the rankings are archived, exactly as if the host
			// had pressed End Game.
			if err := handler.FinalizeRoomWithReason(roomID, handler.EndReasonLicenseExpired); err != nil {
				notify.P1("cron_close_expired_room", "Không đóng được phòng của host hết hạn (room=%d host=%d): %v", roomID, g.UserID, err)
				log.Printf("[CRON ERROR] could not close room %d of expired host %d: %v\n",
					roomID, g.UserID, err)
				continue
			}
			log.Printf("[CRON] Closed room %d (host %d license expired)\n", roomID, g.UserID)
		}
	}
}

func cleanupAbandonedRooms() {
	log.Println("[CRON] Checking for abandoned rooms...")

	threshold := time.Now().Add(-12 * time.Hour)

	var abandonedRooms []model.Room
	err := db.DB.Where("(status = 'waiting' OR status = 'active') AND updated_at < ?", threshold).Find(&abandonedRooms).Error
	if err != nil {
		log.Printf("[CRON ERROR] Failed to fetch active/waiting rooms: %v\n", err)
		notify.P1("cron_fetch_rooms", "Cron không đọc được danh sách phòng active/waiting: %v — phòng bỏ hoang sẽ không được dọn.", err)
		return
	}

	if len(abandonedRooms) == 0 {
		log.Println("[CRON] No abandoned rooms found.")
		return
	}

	for _, room := range abandonedRooms {
		log.Printf("[CRON] Cleaning up abandoned room ID %d...\n", room.ID)

		// Archive rankings before deleting players (same shape as EndGame)
		type Ranking struct {
			Nickname       string `json:"nickname"`
			Score          int    `json:"score"`
			CorrectAnswers int    `json:"correct_answers"`
		}
		var players []model.Player
		db.DB.Where("room_id = ?", room.ID).Find(&players)

		// One grouped count for the room, not one query per player — same
		// helper the host-initiated close uses.
		correctByPlayer := handler.CorrectAnswerCounts(room.ID)

		var rankings []Ranking
		for _, p := range players {
			rankings = append(rankings, Ranking{
				Nickname:       p.Nickname,
				Score:          p.Score,
				CorrectAnswers: int(correctByPlayer[p.ID]),
			})
		}
		sort.Slice(rankings, func(i, j int) bool {
			return rankings[i].Score > rankings[j].Score
		})

		rankingsJSON, err := json.Marshal(rankings)
		if err != nil {
			log.Printf("[CRON ERROR] Failed to marshal rankings for room %d, skipping: %v\n", room.ID, err)
			continue
		}
		var existing model.GameSession
		if db.DB.Where("room_id = ?", room.ID).First(&existing).Error != nil {
			session := model.GameSession{
				RoomID:      room.ID,
				HostID:      room.HostID,
				QuizID:      room.QuizID,
				Rankings:    string(rankingsJSON),
				PlayerCount: len(players),
				EndedAt:     time.Now(),
			}
			// The archive is the only surviving copy once the deletes below run,
			// so a failed archive must not be followed by a cleanup.
			if err := db.DB.Create(&session).Error; err != nil {
				log.Printf("[CRON ERROR] Failed to archive room %d, keeping players and answer logs: %v\n", room.ID, err)
				notify.P1("cron_archive_failed", "Lưu trữ phòng bỏ hoang thất bại (room=%d): %v — kết quả trận đấu chưa được archive.", room.ID, err)
				continue
			}
		}

		room.Status = "finished"
		room.CurrentQuestionID = nil
		room.QuestionActiveUntil = nil
		if err := db.DB.Save(&room).Error; err != nil {
			log.Printf("[CRON ERROR] Failed to close room %d: %v\n", room.ID, err)
			continue
		}

		if err := db.DB.Where("room_id = ?", room.ID).Delete(&model.Player{}).Error; err != nil {
			log.Printf("[CRON ERROR] Failed to delete players for room %d: %v\n", room.ID, err)
		}

		if err := db.DB.Where("room_id = ?", room.ID).Delete(&model.AnswerLog{}).Error; err != nil {
			log.Printf("[CRON ERROR] Failed to delete answer logs for room %d: %v\n", room.ID, err)
		}

		log.Printf("[CRON] Successfully archived and cleaned up abandoned room %d\n", room.ID)
	}
}

// StartAuditRetentionWorker deletes audit rows past AUDIT_RETENTION_DAYS once a
// day. admin_* and license_* rows are kept: they are the trail of privilege
// and money changes, and they are few.
func StartAuditRetentionWorker() {
	go func() {
		time.Sleep(2 * time.Minute)
		purgeOldAudit()
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			purgeOldAudit()
		}
	}()
}

func purgeOldAudit() {
	days := config.AppConfig.AuditRetentionDays
	if days <= 0 {
		return
	}
	res := db.DB.Exec(`DELETE FROM audit_logs
		WHERE created_at < NOW() - make_interval(days => ?)
		  AND action NOT LIKE 'admin\_%' AND action NOT LIKE 'license\_%'`, days)
	if res.Error != nil {
		log.Printf("[CRON] audit retention failed: %v", res.Error)
		return
	}
	if res.RowsAffected > 0 {
		log.Printf("[CRON] audit retention: deleted %d row(s) older than %d days.", res.RowsAffected, days)
	}
}

// StartPaymentExpiryWorker closes QR orders past their 30-minute TTL every five
// minutes. GetUserOrder already reports expiry the moment it happens; this
// makes the stored status agree for the admin list and for any transfer that
// arrives late (matched by code, then sent to review rather than granted).
func StartPaymentExpiryWorker() {
	log.Println("Starting payment order expiry worker...")
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			if n, err := payment.ExpireStale(); err != nil {
				log.Printf("[payment] expiry sweep failed: %v", err)
			} else if n > 0 {
				log.Printf("[payment] expired %d pending orders", n)
			}
		}
	}()
}

// StartPaymentReconcileWorker pulls SePay's transaction list hourly (48 h
// window) and records any incoming transfer whose webhook never arrived.
// Idle without SEPAY_API_TOKEN. A run that finds anything new is itself an
// alert: it means webhooks are being lost.
func StartPaymentReconcileWorker() {
	if !payment.ReconcileConfigured() {
		log.Println("Payment reconcile worker disabled (SEPAY_API_TOKEN or PAYMENT_BANK_ACCOUNT not set)")
		return
	}
	log.Println("Starting payment reconcile worker...")
	go func() {
		time.Sleep(2 * time.Minute)
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			RunPaymentReconcile(48 * time.Hour)
			<-ticker.C
		}
	}()
}

// RunPaymentReconcile performs one reconciliation over the given window.
func RunPaymentReconcile(window time.Duration) (payment.ReconcileResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := payment.Reconcile(ctx, time.Now().Add(-window), payment.AfterApplied)
	if err != nil {
		log.Printf("[payment] reconcile failed: %v", err)
		notify.P1("sepay-reconcile-failed", "Đối soát SePay thất bại: %v", err)
		return res, err
	}
	if res.New > 0 {
		notify.P1("sepay-reconcile-recovered",
			"Đối soát SePay vớt %d giao dịch webhook bị lỡ (%d đã kích hoạt, %d cần xử lý) — kiểm tra webhook SePay",
			res.New, res.Paid, res.NeedHuman)
	}
	log.Printf("[payment] reconcile: fetched=%d new=%d paid=%d review=%d skipped=%d",
		res.Fetched, res.New, res.Paid, res.NeedHuman, res.Skipped)
	return res, nil
}
