// Package notify pushes operational alerts to a Telegram chat.
//
// Design notes, because the obvious implementation breaks in production:
//
//   - It is NOT a log sink. Nothing hooks the stdlib logger; every alert is an
//     explicit call at a site that was classified as worth waking someone for.
//     Hooking log.Print would (a) forward the ~60 seeding/startup lines on every
//     boot and (b) risk recursion the moment this package logs its own failure.
//
//   - Alerts are COALESCED by key. A Centrifugo outage produces one failed
//     publish per question per room; sending those one-for-one would hit
//     Telegram's per-chat flood limit within seconds and get the bot throttled.
//     Identical keys inside a flush window collapse into a single message
//     carrying a ×N count.
//
//   - Sending is asynchronous and lossy under pressure. A blocked Telegram API
//     must never add latency to a game request, so the queue is buffered and
//     drops (with a counter) when full.
//
//   - Fatal() is the one synchronous path: log.Fatalf calls os.Exit(1), which
//     runs no defers and drains no channels, so a queued alert about the crash
//     would die with the process.
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Level orders alerts by how urgently a human must look.
type Level int

const (
	// LevelP1 — data loss, money, or a security control that stopped working.
	// The app still serves traffic.
	LevelP1 Level = iota
	// LevelP0 — the app is down, crash-looping, or a live room is frozen.
	LevelP0
)

func (p *pending) at() time.Time { return p.lastAt }

func (l Level) tag() string {
	if l == LevelP0 {
		return "🔴 P0"
	}
	return "🟠 P1"
}

const (
	// Telegram rejects messages over 4096 chars.
	maxMessageLen = 4000
	// Beyond this many distinct pending keys the app is in a state no alert
	// feed can usefully describe; extras are counted and dropped.
	maxPendingKeys = 60
)

type alert struct {
	level Level
	key   string
	text  string
	at    time.Time
}

type pending struct {
	level   Level
	key     string
	text    string
	count   int
	firstAt time.Time
	lastAt  time.Time
}

type notifier struct {
	token     string
	chatID    string
	threadID  int64
	env       string
	host      string
	flushEach time.Duration
	perMin    int

	queue chan alert
	done  chan struct{}
	wg    sync.WaitGroup

	mu       sync.Mutex
	dropped  int // queue full
	overflow int // too many distinct keys
}

var (
	initOnce sync.Once
	active   *notifier
)

// Config carries the Telegram settings. Passing it in (rather than reading env
// here) keeps every environment lookup in internal/config, as the rest of the
// codebase does.
type Config struct {
	BotToken string
	ChatID   string
	// ThreadID targets a topic inside a forum-style group. Empty for a plain chat.
	ThreadID string
	Env      string
	// FlushSeconds is how long identical alerts are allowed to accumulate before
	// the batch is sent. Larger = fewer, denser messages.
	FlushSeconds int
	// MaxPerMinute caps outbound messages. Telegram throttles a bot at roughly
	// 20 messages/minute to one group; staying under that keeps alerts flowing
	// during an incident instead of getting the bot rate limited exactly when it
	// matters.
	MaxPerMinute int
}

// Init wires up the notifier. Calling it with an empty token or chat ID leaves
// the package inert — every Pn call becomes a no-op — so development and CI
// need no Telegram credentials.
func Init(cfg Config) {
	initOnce.Do(func() {
		if strings.TrimSpace(cfg.BotToken) == "" || strings.TrimSpace(cfg.ChatID) == "" {
			log.Println("[notify] Telegram alerts disabled: TELEGRAM_BOT_TOKEN or TELEGRAM_CHAT_ID not set.")
			return
		}
		if cfg.FlushSeconds <= 0 {
			cfg.FlushSeconds = 10
		}
		if cfg.MaxPerMinute <= 0 {
			cfg.MaxPerMinute = 15
		}
		hostname, _ := os.Hostname()

		n := &notifier{
			token:     strings.TrimSpace(cfg.BotToken),
			chatID:    strings.TrimSpace(cfg.ChatID),
			threadID:  parseThreadID(cfg.ThreadID),
			env:       cfg.Env,
			host:      hostname,
			flushEach: time.Duration(cfg.FlushSeconds) * time.Second,
			perMin:    cfg.MaxPerMinute,
			queue:     make(chan alert, 512),
			done:      make(chan struct{}),
		}
		active = n
		n.wg.Add(1)
		go n.run()
		topic := "General"
		if n.threadID != 0 {
			topic = fmt.Sprintf("topic %d", n.threadID)
		}
		log.Printf("[notify] Telegram alerts enabled (chat=%s → %s, flush=%s cap=%d/min).",
			maskChatID(n.chatID), topic, n.flushEach, n.perMin)
	})
}

// reset clears the package singleton. Tests only — production calls Init once.
func reset() {
	if active != nil {
		active = nil
	}
	initOnce = sync.Once{}
}

// Enabled reports whether alerts will actually be delivered.
func Enabled() bool { return active != nil }

// P0 queues a critical alert. key is the coalescing key: pick something stable
// across repetitions of the SAME fault and distinct between different faults —
// "centrifugo_publish", not fmt.Sprintf("room_%d_failed", id), or every room
// gets its own message during an outage.
func P0(key, format string, args ...interface{}) { enqueue(LevelP0, key, format, args...) }

// P1 queues an important-but-not-paging alert.
func P1(key, format string, args ...interface{}) { enqueue(LevelP1, key, format, args...) }

func enqueue(level Level, key, format string, args ...interface{}) {
	n := active
	if n == nil {
		return
	}
	a := alert{level: level, key: key, text: fmt.Sprintf(format, args...), at: time.Now()}
	select {
	case n.queue <- a:
	default:
		n.mu.Lock()
		n.dropped++
		n.mu.Unlock()
	}
}

// Fatal sends synchronously and returns once Telegram has answered (or the
// timeout expires). Use it immediately before log.Fatalf — an async alert would
// be lost to os.Exit.
func Fatal(key, format string, args ...interface{}) {
	n := active
	if n == nil {
		return
	}
	p := &pending{
		level:   LevelP0,
		key:     key,
		text:    fmt.Sprintf(format, args...),
		count:   1,
		firstAt: time.Now(),
		lastAt:  time.Now(),
	}
	remember(Record{Level: p.level, Key: p.key, Text: p.text, At: p.at(), Count: 1})
	n.deliver(n.render(p))
}

// Raw sends a pre-formatted message (the daily digest). It bypasses coalescing
// but not the outbound rate limit's courtesy pause.
func Raw(text string) {
	n := active
	if n == nil {
		return
	}
	n.deliver(text)
}

// Shutdown drains whatever is pending. main defers it so a graceful stop does
// not swallow the alert that explains why the process is stopping.
func Shutdown() {
	n := active
	if n == nil {
		return
	}
	close(n.done)
	n.wg.Wait()
}

func (n *notifier) run() {
	defer n.wg.Done()

	batch := make(map[string]*pending)
	flush := time.NewTicker(n.flushEach)
	defer flush.Stop()
	// Token bucket for the outbound cap, refilled once a minute.
	budget := n.perMin
	refill := time.NewTicker(time.Minute)
	defer refill.Stop()

	for {
		select {
		case a := <-n.queue:
			n.absorb(batch, a)
		case <-refill.C:
			budget = n.perMin
		case <-flush.C:
			budget = n.flush(batch, budget)
		case <-n.done:
			// Drain anything still queued, then send it all regardless of budget:
			// on the way out, completeness beats politeness.
			for {
				select {
				case a := <-n.queue:
					n.absorb(batch, a)
					continue
				default:
				}
				break
			}
			n.flush(batch, len(batch)+1)
			return
		}
	}
}

func (n *notifier) absorb(batch map[string]*pending, a alert) {
	id := fmt.Sprintf("%d|%s", a.level, a.key)
	if p, ok := batch[id]; ok {
		p.count++
		p.lastAt = a.at
		// Keep the FIRST message body. During a cascade the first failure names
		// the original cause; later ones are usually downstream noise.
		return
	}
	if len(batch) >= maxPendingKeys {
		n.mu.Lock()
		n.overflow++
		n.mu.Unlock()
		return
	}
	batch[id] = &pending{
		level: a.level, key: a.key, text: a.text,
		count: 1, firstAt: a.at, lastAt: a.at,
	}
}

// flush sends up to budget messages, P0 first. Returns the remaining budget.
// Anything not sent stays in the batch and keeps accumulating its count, so a
// throttled alert arrives late but with an accurate total.
func (n *notifier) flush(batch map[string]*pending, budget int) int {
	if len(batch) == 0 {
		n.reportDrops(&budget)
		return budget
	}
	ids := make([]string, 0, len(batch))
	for id := range batch {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := batch[ids[i]], batch[ids[j]]
		if a.level != b.level {
			return a.level > b.level // P0 before P1
		}
		return a.firstAt.Before(b.firstAt)
	})
	muted := isMuted()
	for _, id := range ids {
		if budget <= 0 {
			break
		}
		p := batch[id]
		// P0 ignores the mute. Muting exists to quiet a known incident, not to
		// blind the channel to the next unrelated one.
		suppress := muted && p.level == LevelP1
		remember(Record{Level: p.level, Key: p.key, Text: p.text, At: p.lastAt, Count: p.count, Muted: suppress})
		if !suppress {
			n.deliver(n.render(p))
			budget--
		}
		delete(batch, id)
	}
	n.reportDrops(&budget)
	return budget
}

// reportDrops tells the chat when the notifier itself shed load — silence that
// looks like health would be worse than the extra message.
func (n *notifier) reportDrops(budget *int) {
	n.mu.Lock()
	dropped, overflow := n.dropped, n.overflow
	n.dropped, n.overflow = 0, 0
	n.mu.Unlock()

	if dropped == 0 && overflow == 0 || *budget <= 0 {
		return
	}
	n.deliver(n.render(&pending{
		level: LevelP1,
		key:   "notify_overload",
		text: fmt.Sprintf("Alert pipeline shed load: %d dropped (queue full), %d dropped (too many distinct faults). Check container logs directly — the feed is incomplete.",
			dropped, overflow),
		count:   1,
		firstAt: time.Now(),
		lastAt:  time.Now(),
	}))
	*budget--
}

func (n *notifier) render(p *pending) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>%s</b>", p.level.tag(), html.EscapeString(p.key))
	if p.count > 1 {
		fmt.Fprintf(&b, " ×%d", p.count)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "<code>%s</code>\n", html.EscapeString(truncate(p.text, maxMessageLen-400)))
	fmt.Fprintf(&b, "\n<i>%s · %s · %s</i>",
		html.EscapeString(n.env), html.EscapeString(n.host), p.firstAt.Format("15:04:05 02/01"))
	if p.count > 1 {
		fmt.Fprintf(&b, " <i>→ %s</i>", p.lastAt.Format("15:04:05"))
	}
	return b.String()
}

var httpClient = &http.Client{Timeout: 8 * time.Second}

// apiBase is a variable, not a constant, only so tests can point the notifier
// at a local server instead of Telegram.
var apiBase = "https://api.telegram.org"

func (n *notifier) deliver(text string) {
	body := map[string]interface{}{
		"chat_id":                  n.chatID,
		"text":                     truncate(text, maxMessageLen),
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	// Must be a NUMBER, not a string: this path posts JSON (the watchdog and the
	// command bot use form encoding, which is untyped), and the Bot API declares
	// message_thread_id as Integer. A quoted value is rejected, and the rejection
	// only shows up as a local [notify] log line — the alert simply never arrives.
	if n.threadID != 0 {
		body["message_thread_id"] = n.threadID
	}
	payload, err := json.Marshal(body)
	if err != nil {
		log.Printf("[notify] could not marshal Telegram payload: %v", err)
		return
	}
	url := fmt.Sprintf("%s/bot%s/sendMessage", apiBase, n.token)
	resp, err := httpClient.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		// Deliberately only a local log: a failed alert must not recurse into
		// the alert path.
		log.Printf("[notify] Telegram send failed: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf := make([]byte, 512)
		nRead, _ := resp.Body.Read(buf)
		log.Printf("[notify] Telegram returned %d: %s", resp.StatusCode, strings.TrimSpace(string(buf[:nRead])))
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(cắt bớt)"
}

// parseThreadID tolerates a blank or malformed value by falling back to "no
// topic". Posting to the group's General topic is a visible, fixable mistake;
// refusing to start the notifier over a typo would silence alerting entirely.
func parseThreadID(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		log.Printf("[notify] TELEGRAM_THREAD_ID=%q is not a number — gửi vào topic General.", raw)
		return 0
	}
	return id
}

func maskChatID(id string) string {
	if len(id) <= 4 {
		return "****"
	}
	return strings.Repeat("*", len(id)-4) + id[len(id)-4:]
}
