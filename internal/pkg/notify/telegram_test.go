package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type capture struct {
	mu   sync.Mutex
	msgs []string
	raw  []string
}

func (c *capture) bodies() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.raw...)
}

func (c *capture) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.msgs...)
}

func newServer(t *testing.T) (*capture, func()) {
	t.Helper()
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(body, &payload)
		c.mu.Lock()
		c.msgs = append(c.msgs, payload.Text)
		c.raw = append(c.raw, string(body))
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	old := apiBase
	apiBase = srv.URL
	reset()
	return c, func() {
		srv.Close()
		apiBase = old
		reset()
	}
}

// The whole point of the package: a repeated fault must arrive as one message
// with a count, not as one message per occurrence.
func TestIdenticalAlertsCoalesce(t *testing.T) {
	cap, cleanup := newServer(t)
	defer cleanup()

	Init(Config{BotToken: "tok", ChatID: "-100", Env: "test", FlushSeconds: 1, MaxPerMinute: 30})
	for i := 0; i < 50; i++ {
		P0("centrifugo_unreachable", "publish thất bại lần %d", i)
	}
	Shutdown()

	msgs := cap.all()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 coalesced message, got %d: %v", len(msgs), msgs)
	}
	if !strings.Contains(msgs[0], "×50") {
		t.Errorf("expected the count ×50 in the message, got: %s", msgs[0])
	}
	// The FIRST occurrence's text is the one kept — later ones are downstream noise.
	if !strings.Contains(msgs[0], "publish thất bại lần 0") {
		t.Errorf("expected the first occurrence's text, got: %s", msgs[0])
	}
}

func TestDistinctKeysStaySeparate(t *testing.T) {
	cap, cleanup := newServer(t)
	defer cleanup()

	Init(Config{BotToken: "tok", ChatID: "-100", Env: "test", FlushSeconds: 1, MaxPerMinute: 30})
	P0("a", "lỗi a")
	P1("b", "lỗi b")
	P1("b", "lỗi b lần 2")
	Shutdown()

	if got := len(cap.all()); got != 2 {
		t.Fatalf("expected 2 messages (one per key), got %d: %v", got, cap.all())
	}
}

// A disabled notifier must be a silent no-op, not a panic: dev and CI run
// without credentials.
func TestDisabledIsNoOp(t *testing.T) {
	_, cleanup := newServer(t)
	defer cleanup()

	Init(Config{BotToken: "", ChatID: ""})
	if Enabled() {
		t.Fatal("notifier should be disabled without a token")
	}
	P0("x", "không được gửi")
	Fatal("y", "cũng không")
	Raw("cũng không nốt")
	Shutdown()
}

// Escaping matters because parse_mode=HTML is set: an error string containing
// < or & would otherwise make Telegram reject the whole message.
func TestHTMLIsEscaped(t *testing.T) {
	cap, cleanup := newServer(t)
	defer cleanup()

	Init(Config{BotToken: "tok", ChatID: "-100", Env: "test", FlushSeconds: 1, MaxPerMinute: 30})
	P1("html", `lỗi: <script> & "trích dẫn"`)
	Shutdown()

	msgs := cap.all()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if strings.Contains(msgs[0], "<script>") {
		t.Errorf("raw <script> leaked into the payload: %s", msgs[0])
	}
	if !strings.Contains(msgs[0], "&lt;script&gt;") {
		t.Errorf("expected escaped markup, got: %s", msgs[0])
	}
}

func TestFatalSendsSynchronously(t *testing.T) {
	cap, cleanup := newServer(t)
	defer cleanup()

	Init(Config{BotToken: "tok", ChatID: "-100", Env: "test", FlushSeconds: 3600, MaxPerMinute: 30})
	Fatal("db_connect", "không kết nối được")
	// No Shutdown, no flush tick: Fatal must already have delivered, because
	// the real caller's next statement is log.Fatalf -> os.Exit(1).
	if got := len(cap.all()); got != 1 {
		t.Fatalf("Fatal must deliver before returning; got %d messages", got)
	}
}

// Queue overflow must be reported rather than swallowed — silence that looks
// like health is the worst outcome for an alerting path.
func TestOverloadIsReported(t *testing.T) {
	cap, cleanup := newServer(t)
	defer cleanup()

	Init(Config{BotToken: "tok", ChatID: "-100", Env: "test", FlushSeconds: 1, MaxPerMinute: 30})
	for i := 0; i < maxPendingKeys+25; i++ {
		P1(string(rune('a'+i%26))+time.Now().Format("")+itoa(i), "lỗi %d", i)
	}
	Shutdown()

	joined := strings.Join(cap.all(), "\n")
	if !strings.Contains(joined, "notify_overload") {
		t.Errorf("expected an overload report among %d messages", len(cap.all()))
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// The Bot API declares message_thread_id as Integer, and this is the one sender
// that posts JSON (the watchdog and the command bot use form encoding, which is
// untyped). A quoted value is rejected by Telegram, and the rejection surfaces
// only as a local log line — the alert just never arrives in the topic.
func TestThreadIDIsSentAsNumber(t *testing.T) {
	cap, cleanup := newServer(t)
	defer cleanup()
	Unmute()

	Init(Config{BotToken: "tok", ChatID: "-100", ThreadID: "4567", Env: "test", FlushSeconds: 1, MaxPerMinute: 30})
	P0("topic", "vào đúng topic")
	Shutdown()

	bodies := cap.bodies()
	if len(bodies) != 1 {
		t.Fatalf("expected 1 request, got %d", len(bodies))
	}
	if !strings.Contains(bodies[0], `"message_thread_id":4567`) {
		t.Errorf("thread id must be a JSON number, got: %s", bodies[0])
	}
	if strings.Contains(bodies[0], `"message_thread_id":"4567"`) {
		t.Errorf("thread id was quoted, Telegram will reject it: %s", bodies[0])
	}
}

// A typo must not silence alerting: fall back to the General topic.
func TestMalformedThreadIDFallsBackToGeneral(t *testing.T) {
	cap, cleanup := newServer(t)
	defer cleanup()
	Unmute()

	Init(Config{BotToken: "tok", ChatID: "-100", ThreadID: "không-phải-số", Env: "test", FlushSeconds: 1, MaxPerMinute: 30})
	P0("fallback", "vẫn phải gửi được")
	Shutdown()

	bodies := cap.bodies()
	if len(bodies) != 1 {
		t.Fatalf("a bad thread id must not stop delivery; got %d requests", len(bodies))
	}
	if strings.Contains(bodies[0], "message_thread_id") {
		t.Errorf("no thread id should be sent when it cannot be parsed: %s", bodies[0])
	}
}
