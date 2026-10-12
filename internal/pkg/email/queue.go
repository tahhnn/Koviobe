package email

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/pkg/notify"
)

// ErrQueueFull means the outgoing queue is saturated; the mail was dropped.
var ErrQueueFull = errors.New("email queue is full")

// Enqueue hands a user-triggered mail (signup code, password reset, invite) to
// the background sender and returns at once.
//
// It replaces `go SendEmail(...)` at those call sites. That pattern opened one
// goroutine and one SMTP connection per request with nothing in between, so a
// burst of signups — real or scripted — became a burst of parallel SMTP
// sessions: the provider throttles or blocks the account, real users' codes
// stop arriving, and the domain's reputation (and so everyone's inbox
// placement) pays for it. Here the burst waits in a bounded queue and leaves at
// a fixed rate, and one address can only receive so much.
//
// Mail an admin or a payment triggers (license codes, receipts, expiry
// warnings) keeps calling SendEmail directly: low volume, and the caller needs
// the error.
func Enqueue(to, subject, htmlBody string) error {
	if !allowRecipient(context.Background(), to) {
		log.Printf("[EMAIL] recipient cap reached, dropped %q", subject)
		return ErrRecipientLimited
	}
	return enqueue(to, subject, htmlBody)
}

// EnqueueUncapped is Enqueue without the per-recipient cap, for mail only the
// recipient can trigger (the temporary password sent after they entered the
// signup code). Dropping that one would leave a new account with no password.
func EnqueueUncapped(to, subject, htmlBody string) error {
	return enqueue(to, subject, htmlBody)
}

func enqueue(to, subject, htmlBody string) error {
	startOnce.Do(startWorkers)
	select {
	case queue <- job{to: to, subject: subject, html: htmlBody}:
		return nil
	default:
		notify.P1("email_queue_full", "Hàng đợi email đầy (%d) — mail người dùng đang bị bỏ. Có thể đang bị spam form đăng ký/quên mật khẩu/mời.", cap(queue))
		return ErrQueueFull
	}
}

type job struct {
	to, subject, html string
}

const sendWorkers = 2

var (
	startOnce sync.Once
	queue     chan job
	sendFn    = SendEmail // swappable in tests
)

func startWorkers() {
	size, perMin := 500, 30
	if cfg := config.AppConfig; cfg != nil {
		if cfg.EmailQueueSize > 0 {
			size = cfg.EmailQueueSize
		}
		if cfg.EmailMaxPerMinute > 0 {
			perMin = cfg.EmailMaxPerMinute
		}
	}
	queue = make(chan job, size)
	// A short burst goes out immediately (a few people signing up together);
	// past that, the steady rate applies.
	bucket := newTokenBucket(perMin, max(3, perMin/6))
	for i := 0; i < sendWorkers; i++ {
		go func() {
			for j := range queue {
				bucket.wait()
				_ = sendFn(j.to, j.subject, j.html)
			}
		}()
	}
}

// tokenBucket paces sends: capacity tokens, refilled at perMinute.
type tokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	capacity float64
	perSec   float64
	last     time.Time
}

func newTokenBucket(perMinute, capacity int) *tokenBucket {
	return &tokenBucket{
		tokens:   float64(capacity),
		capacity: float64(capacity),
		perSec:   float64(perMinute) / 60,
		last:     time.Now(),
	}
}

// take consumes a token if one is available, else reports how long to wait.
func (b *tokenBucket) take(now time.Time) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens += now.Sub(b.last).Seconds() * b.perSec
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / b.perSec * float64(time.Second))
}

func (b *tokenBucket) wait() {
	for {
		ok, d := b.take(time.Now())
		if ok {
			return
		}
		time.Sleep(d)
	}
}
