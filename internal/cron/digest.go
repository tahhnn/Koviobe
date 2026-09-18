package cron

import (
	"fmt"
	"html"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/notify"
)

// StartDigestWorker sends one summary message a day.
//
// Everything in here is P2: an admin changed a plan, someone redeemed a code,
// N games were played. None of it needs a human within the hour, and pushing
// each event live would drown the P0/P1 alerts that do. Batching is what keeps
// the channel worth reading.
func StartDigestWorker() {
	hour := config.AppConfig.TelegramDigestHour
	if hour < 0 || hour > 23 {
		log.Println("Daily Telegram digest disabled (TELEGRAM_DIGEST_HOUR out of range).")
		return
	}
	if !notify.Enabled() {
		return
	}
	log.Printf("Starting daily Telegram digest worker (%02d:00 local)...", hour)

	go func() {
		for {
			time.Sleep(time.Until(nextRun(hour)))
			sendDailyDigest()
		}
	}()
}

// nextRun is the next occurrence of hour:00 in the server's local zone. The
// container's TZ decides what "8am" means — set TZ in compose if the default
// UTC is not what the team expects.
func nextRun(hour int) time.Time {
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

func sendDailyDigest() {
	notify.Raw(BuildDigest())
	log.Println("[CRON] Daily Telegram digest sent.")
}

// BuildDigest renders the summary without sending it, so the /digest command
// can answer in the chat it was typed in rather than firing the scheduled
// broadcast a second time.
func BuildDigest() string {
	since := time.Now().Add(-24 * time.Hour)
	var b strings.Builder
	b.WriteString("📊 <b>quizzZone — 24 giờ qua</b>\n")
	fmt.Fprintf(&b, "<i>%s → %s</i>\n\n",
		since.Format("15:04 02/01"), time.Now().Format("15:04 02/01"))

	// --- Hoạt động ---
	var newUsers, newQuizzes, newRooms, finishedGames, playersPlayed int64
	var openRooms int64
	db.DB.Model(&model.User{}).Where("created_at >= ?", since).Count(&newUsers)
	db.DB.Model(&model.Quiz{}).Where("created_at >= ?", since).Count(&newQuizzes)
	db.DB.Model(&model.Room{}).Where("created_at >= ?", since).Count(&newRooms)
	db.DB.Model(&model.GameSession{}).Where("created_at >= ?", since).Count(&finishedGames)
	db.DB.Model(&model.GameSession{}).Where("created_at >= ?", since).
		Select("COALESCE(SUM(player_count), 0)").Scan(&playersPlayed)
	db.DB.Model(&model.Room{}).Where("status IN ?", []string{"waiting", "active"}).Count(&openRooms)

	b.WriteString("<b>Hoạt động</b>\n")
	fmt.Fprintf(&b, "• Trận hoàn tất: <b>%d</b> (tổng %d lượt chơi)\n", finishedGames, playersPlayed)
	fmt.Fprintf(&b, "• Phòng tạo mới: %d · đang mở lúc này: %d\n", newRooms, openRooms)
	fmt.Fprintf(&b, "• Quiz mới: %d · Người dùng mới: %d\n\n", newQuizzes, newUsers)

	// --- Hành động admin & license: the money and privilege trail. Every row
	// here is one a human should be able to recognise as their own. ---
	type row struct {
		Action string
		N      int64
	}
	var actions []row
	db.DB.Model(&model.AuditLog{}).
		Select("action, COUNT(*) AS n").
		Where("created_at >= ?", since).
		Group("action").Order("n DESC").Scan(&actions)

	admin, security := []row{}, []row{}
	for _, a := range actions {
		switch {
		case strings.HasPrefix(a.Action, "admin_"), strings.HasPrefix(a.Action, "license_"):
			admin = append(admin, a)
		case strings.HasPrefix(a.Action, "login_failed"):
			security = append(security, a)
		}
	}
	if len(admin) > 0 {
		b.WriteString("<b>Admin &amp; license</b>\n")
		for _, a := range admin {
			fmt.Fprintf(&b, "• %s: %d\n", html.EscapeString(a.Action), a.N)
		}
		b.WriteString("\n")
	}

	// --- Bảo mật ---
	var failTotal int64
	for _, s := range security {
		failTotal += s.N
	}
	b.WriteString("<b>Bảo mật</b>\n")
	if failTotal == 0 {
		b.WriteString("• Không có lần đăng nhập thất bại nào.\n")
	} else {
		fmt.Fprintf(&b, "• Đăng nhập thất bại: <b>%d</b>\n", failTotal)
		type ipRow struct {
			IPAddress string
			N         int64
		}
		var ips []ipRow
		db.DB.Model(&model.AuditLog{}).
			Select("ip_address, COUNT(*) AS n").
			Where("created_at >= ? AND action LIKE ?", since, "login_failed%").
			Group("ip_address").Order("n DESC").Limit(5).Scan(&ips)
		sort.SliceStable(ips, func(i, j int) bool { return ips[i].N > ips[j].N })
		for _, r := range ips {
			fmt.Fprintf(&b, "   ↳ <code>%s</code> × %d\n", html.EscapeString(r.IPAddress), r.N)
		}
	}

	// --- Sức khoẻ hệ thống ---
	b.WriteString("\n<b>Hệ thống</b>\n")
	if sqlDB, err := db.DB.DB(); err == nil {
		st := sqlDB.Stats()
		fmt.Fprintf(&b, "• DB pool: %d/%d đang dùng, %d chờ\n", st.InUse, st.MaxOpenConnections, st.WaitCount)
	}
	fmt.Fprintf(&b, "• License enforcement: %s\n", onOff(config.AppConfig.LicenseEnforcement))

	return b.String()
}

func onOff(b bool) string {
	if b {
		return "BẬT"
	}
	return "TẮT"
}
