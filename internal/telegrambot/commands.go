package telegrambot

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/quizzzone/backend/internal/cache"
	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/cron"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/notify"
)

// spec describes one command in the single place that defines them.
//
// Name, menu text, help text and handler live in the same struct on purpose.
// When they were a list plus a switch, nothing stopped a command from being
// advertised in the Telegram menu months after its handler was deleted; here
// an entry without a handler does not compile into anything callable, and an
// entry with no menu line cannot exist.
type spec struct {
	Name string // không có dấu "/"
	Args string // gợi ý tham số cho /help, để trống nếu không có
	// Menu is the short line Telegram shows in the command menu. The Bot API
	// caps it at 256 characters and renders it on one line, so it stays terse.
	Menu string
	Run  func(args []string, startedAt time.Time) string
}

// Populated in init(), not as a var literal: the /help entry's handler calls
// cmdHelp, which reads this same list, and Go's package-level initialisation
// analysis treats that as a cycle even though the closure only runs later.
var commands []spec

func init() {
	commands = []spec{
		{"status", "", "Sức khoẻ DB, Redis, Centrifugo, uptime, phòng đang mở",
			func(_ []string, startedAt time.Time) string { return cmdStatus(startedAt) }},
		{"rooms", "", "Danh sách phòng đang waiting/active",
			func(_ []string, _ time.Time) string { return cmdRooms() }},
		{"errors", "[n]", "n cảnh báo gần nhất (mặc định 10)",
			func(args []string, _ time.Time) string { return cmdErrors(args) }},
		{"digest", "", "Gửi tổng kết 24h ngay, không chờ 08:00",
			func(_ []string, _ time.Time) string { return cron.BuildDigest() }},
		{"mute", "<2h>", "Tạm tắt cảnh báo P1 (P0 vẫn luôn gửi)",
			func(args []string, _ time.Time) string { return cmdMute(args) }},
		{"unmute", "", "Bật lại cảnh báo P1",
			func(_ []string, _ time.Time) string { notify.Unmute(); return "🔔 Đã bật lại cảnh báo P1." }},
		{"help", "", "Danh sách lệnh",
			func(_ []string, _ time.Time) string { return cmdHelp() }},
	}

	byName = make(map[string]spec, len(commands)+1)
	for _, c := range commands {
		byName[c.Name] = c
	}
	// /start is what Telegram sends on first contact; treat it as /help but
	// keep it out of the menu, where it would just be a duplicate entry.
	byName["start"] = byName["help"]
}

var byName map[string]spec

func dispatch(cmd string, args []string, startedAt time.Time) string {
	c, ok := byName[strings.TrimPrefix(cmd, "/")]
	if !ok {
		return "Lệnh không hợp lệ. Gõ /help để xem danh sách."
	}
	return c.Run(args, startedAt)
}

func cmdHelp() string {
	var b strings.Builder
	b.WriteString("<b>quizzZone — lệnh khả dụng</b>\n\n")
	for _, c := range commands {
		if c.Args != "" {
			fmt.Fprintf(&b, "/%s %s — %s\n", c.Name, html.EscapeString(c.Args), c.Menu)
		} else {
			fmt.Fprintf(&b, "/%s — %s\n", c.Name, c.Menu)
		}
	}
	b.WriteString("\n<i>Tất cả đều chỉ đọc. Bot không restart, không sửa, không xoá bất cứ thứ gì — token Telegram bị lộ thì kẻ tấn công cũng chỉ xem được metric.</i>\n")
	b.WriteString("\n<i>Trạng thái container và cảnh báo 502 đến từ watchdog chạy trên host, không phải bot này — bot sống trong chính container backend nên không tự thấy mình chết được.</i>")
	return b.String()
}

func cmdStatus(startedAt time.Time) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var b strings.Builder
	b.WriteString("<b>quizzZone — trạng thái</b>\n\n")

	// Postgres
	if sqlDB, err := db.DB.DB(); err != nil || sqlDB.PingContext(ctx) != nil {
		b.WriteString("🔴 Postgres: KHÔNG kết nối được\n")
	} else {
		st := sqlDB.Stats()
		fmt.Fprintf(&b, "🟢 Postgres: ok (pool %d/%d dùng, %d idle, %d lượt chờ)\n",
			st.InUse, st.MaxOpenConnections, st.Idle, st.WaitCount)
	}

	// Redis
	if cache.RDB == nil || cache.RDB.Ping(ctx).Err() != nil {
		b.WriteString("🔴 Redis: KHÔNG kết nối được (rate limiter đang fail-open)\n")
	} else {
		b.WriteString("🟢 Redis: ok\n")
	}

	// Centrifugo. Checked over HTTP rather than by publishing, because a probe
	// publish would land in a real channel that real clients are subscribed to.
	if code, err := probe(ctx, centrifugoHealthURL()); err != nil {
		fmt.Fprintf(&b, "🔴 Centrifugo: %v\n", err)
	} else if code != http.StatusOK {
		fmt.Fprintf(&b, "🔴 Centrifugo: HTTP %d\n", code)
	} else {
		b.WriteString("🟢 Centrifugo: ok\n")
	}

	// Activity right now
	var waiting, active, players int64
	db.DB.Model(&model.Room{}).Where("status = ?", "waiting").Count(&waiting)
	db.DB.Model(&model.Room{}).Where("status = ?", "active").Count(&active)
	db.DB.Model(&model.Player{}).
		Where("room_id IN (?)", db.DB.Model(&model.Room{}).
			Select("id").Where("status IN ?", []string{"waiting", "active"})).
		Count(&players)

	fmt.Fprintf(&b, "\n<b>Đang chạy</b>\n• Phòng: %d chờ, %d đang chơi\n• Người chơi trong phòng: %d\n",
		waiting, active, players)

	fmt.Fprintf(&b, "\n<b>Tiến trình</b>\n• Uptime: %s (khởi động %s)\n",
		humanDuration(time.Since(startedAt)), startedAt.Format("15:04 02/01"))
	fmt.Fprintf(&b, "• Môi trường: %s · License enforcement: %s\n",
		html.EscapeString(config.AppConfig.AppEnv), onOff(config.AppConfig.LicenseEnforcement))
	fmt.Fprintf(&b, "• %s\n", html.EscapeString(notify.Summary()))

	return b.String()
}

func cmdRooms() string {
	var rooms []model.Room
	if err := db.DB.Where("status IN ?", []string{"waiting", "active"}).
		Order("created_at DESC").Limit(20).Find(&rooms).Error; err != nil {
		return "🔴 Không đọc được danh sách phòng: " + html.EscapeString(err.Error())
	}
	if len(rooms) == 0 {
		return "Không có phòng nào đang mở."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "<b>%d phòng đang mở</b>\n\n", len(rooms))
	for _, r := range rooms {
		var n int64
		db.DB.Model(&model.Player{}).Where("room_id = ?", r.ID).Count(&n)
		icon := "⏳"
		if r.Status == "active" {
			icon = "▶️"
		}
		fmt.Fprintf(&b, "%s <code>%s</code> · room %d · %d người · câu %d · mở %s\n",
			icon, html.EscapeString(r.PinCode), r.ID, n, r.CurrentQuestionIndex+1,
			humanDuration(time.Since(r.CreatedAt)))
	}
	if len(rooms) == 20 {
		b.WriteString("\n<i>(chỉ hiện 20 phòng mới nhất)</i>")
	}
	return b.String()
}

func cmdErrors(args []string) string {
	n := 10
	if len(args) > 0 {
		if parsed, err := strconv.Atoi(args[0]); err == nil && parsed > 0 {
			n = parsed
		}
	}
	recs := notify.Recent(n)
	if len(recs) == 0 {
		return "Chưa có cảnh báo nào kể từ lần khởi động gần nhất.\n\n<i>Bộ đệm nằm trong RAM — restart là mất. Lịch sử đầy đủ ở <code>docker logs quizzzone-backend</code>.</i>"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "<b>%d cảnh báo gần nhất</b>\n\n", len(recs))
	for _, r := range recs {
		tag := "🟠"
		if r.Level == notify.LevelP0 {
			tag = "🔴"
		}
		fmt.Fprintf(&b, "%s <b>%s</b>", tag, html.EscapeString(r.Key))
		if r.Count > 1 {
			fmt.Fprintf(&b, " ×%d", r.Count)
		}
		if r.Muted {
			b.WriteString(" <i>(đã bị mute)</i>")
		}
		fmt.Fprintf(&b, " · %s\n<code>%s</code>\n\n",
			r.At.Format("15:04:05 02/01"), html.EscapeString(trim(r.Text, 220)))
	}
	return b.String()
}

func cmdMute(args []string) string {
	if len(args) == 0 {
		return "Cú pháp: <code>/mute 30m</code> hoặc <code>/mute 2h</code> (tối đa 24h)."
	}
	d, err := time.ParseDuration(args[0])
	if err != nil || d <= 0 {
		return "Không hiểu khoảng thời gian " + html.EscapeString(args[0]) + ". Ví dụ hợp lệ: 30m, 2h, 90m."
	}
	// A mute that outlives the shift that set it is how an incident goes
	// unnoticed for a day; 24h is the hard ceiling.
	if d > 24*time.Hour {
		d = 24 * time.Hour
	}
	until := notify.Mute(d)
	return fmt.Sprintf("🔇 Đã tắt cảnh báo <b>P1</b> tới <b>%s</b>.\n\n<i>P0 vẫn gửi bình thường — mute chỉ để dập tiếng ồn của sự cố đang xử lý, không phải để bịt mắt kênh trước sự cố tiếp theo. Bật lại sớm bằng /unmute.</i>",
		until.Format("15:04:05 02/01"))
}

// centrifugoHealthURL derives the health endpoint from the configured API URL
// ("http://centrifugo:8000/api" -> "http://centrifugo:8000/health").
func centrifugoHealthURL() string {
	base := strings.TrimSuffix(strings.TrimRight(config.AppConfig.CentrifugoAPIURL, "/"), "/api")
	return base + "/health"
}

func probe(ctx context.Context, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dp", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dg%dp", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%d ngày %dg", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func trim(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func onOff(b bool) string {
	if b {
		return "BẬT"
	}
	return "TẮT"
}
