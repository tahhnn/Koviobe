package email

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/quizzzone/backend/internal/cache"
	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/pkg/notify"
	"github.com/redis/go-redis/v9"
)

// ErrQueueFull means the outgoing queue is saturated; the mail was dropped.
var ErrQueueFull = errors.New("email queue is full")

// Enqueue hands a user-triggered mail (signup code, password reset, invite) to
// the background sender and returns at once. The queue lives in Redis.
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
//
// It also jumps the queue and never touches Redis: a flood of signup codes
// must not delay it, and a plaintext password has no business in a Redis
// snapshot on disk.
func EnqueueUncapped(to, subject, htmlBody string) error {
	StartWorkers()
	select {
	case priority <- job{To: to, Subject: subject, HTML: htmlBody, Queued: time.Now()}:
		return nil
	default:
		notify.P1("email_queue_full", "Hàng đợi email ưu tiên đầy (%d) — mật khẩu tạm thời không gửi được.", cap(priority))
		return ErrQueueFull
	}
}

// enqueue puts a capped mail on the Redis list, so a backend restart or
// redeploy does not silently lose every code that was waiting to go out. If
// Redis cannot take it, the in-process channel does — same limits, just not
// durable.
func enqueue(to, subject, htmlBody string) error {
	StartWorkers()
	j := job{To: to, Subject: subject, HTML: htmlBody, Queued: time.Now()}
	payload, err := json.Marshal(j)
	if err != nil {
		return err
	}
	pushed, err := redisPush(context.Background(), payload, queueSize)
	if err == nil {
		if !pushed {
			notify.P1("email_queue_full", "Hàng đợi email đầy (%d) — mail người dùng đang bị bỏ. Có thể đang bị spam form đăng ký/quên mật khẩu/mời.", queueSize)
			return ErrQueueFull
		}
		return nil
	}
	log.Printf("[EMAIL] Redis queue unavailable, using in-memory queue: %v", err)
	select {
	case fallback <- j:
		return nil
	default:
		notify.P1("email_queue_full", "Hàng đợi email (dự phòng trong RAM) đầy (%d) và Redis lỗi: %v", cap(fallback), err)
		return ErrQueueFull
	}
}

type job struct {
	To      string    `json:"to"`
	Subject string    `json:"subject"`
	HTML    string    `json:"html"`
	Queued  time.Time `json:"queued"`
}

const (
	sendWorkers = 2
	redisQueue  = "mail_queue"
	// A queued mail older than this is dropped instead of sent: the longest-
	// lived thing we mail (a reset link) is dead after 30 minutes, and a code
	// arriving after it expired only confuses.
	maxJobAge   = 30 * time.Minute
	popTimeout  = 2 * time.Second
	redisErrNap = time.Second
)

var (
	startOnce sync.Once
	queueSize int
	priority  chan job
	fallback  chan job
	sendFn    = SendEmail // swappable in tests
	bucket    *tokenBucket
)

// Bounded push: refuse rather than grow without limit under a flood.
var boundedPush = redis.NewScript(`
if redis.call("LLEN", KEYS[1]) >= tonumber(ARGV[2]) then return 0 end
redis.call("RPUSH", KEYS[1], ARGV[1])
return 1
`)

// Swappable in tests.
var (
	redisPush = func(ctx context.Context, payload []byte, max int) (bool, error) {
		if cache.RDB == nil {
			return false, errors.New("redis not initialised")
		}
		n, err := boundedPush.Run(ctx, cache.RDB, []string{redisQueue}, string(payload), max).Int()
		return n == 1, err
	}
	redisPop = func(ctx context.Context, timeout time.Duration) ([]byte, error) {
		if cache.RDB == nil {
			return nil, errors.New("redis not initialised")
		}
		res, err := cache.RDB.BLPop(ctx, timeout, redisQueue).Result()
		if err != nil {
			return nil, err
		}
		return []byte(res[1]), nil
	}
)

// StartWorkers starts the senders. Called at boot so mail left in Redis by the
// previous process goes out without waiting for a new request; also called
// lazily by every enqueue, so it is safe to call more than once.
func StartWorkers() {
	startOnce.Do(func() {
		size, perMin := 500, 30
		if cfg := config.AppConfig; cfg != nil {
			if cfg.EmailQueueSize > 0 {
				size = cfg.EmailQueueSize
			}
			if cfg.EmailMaxPerMinute > 0 {
				perMin = cfg.EmailMaxPerMinute
			}
		}
		queueSize = size
		priority = make(chan job, 100)
		fallback = make(chan job, size)
		// A short burst goes out immediately (a few people signing up
		// together); past that, the steady rate applies.
		bucket = newTokenBucket(perMin, max(3, perMin/6))
		for i := 0; i < sendWorkers; i++ {
			go worker()
		}
	})
}

func worker() {
	for {
		j, ok := nextJob()
		if !ok {
			continue
		}
		if time.Since(j.Queued) > maxJobAge {
			log.Printf("[EMAIL] dropped stale mail %q queued %s ago", j.Subject, time.Since(j.Queued).Round(time.Second))
			continue
		}
		bucket.wait()
		_ = sendFn(j.To, j.Subject, j.HTML)
	}
}

// nextJob prefers, in order: the priority lane, the in-memory fallback, then
// Redis (blocking up to popTimeout, so the first two are rechecked often).
func nextJob() (job, bool) {
	select {
	case j := <-priority:
		return j, true
	default:
	}
	select {
	case j := <-fallback:
		return j, true
	default:
	}
	payload, err := redisPop(context.Background(), popTimeout)
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			// Redis down: keep serving the in-process lanes without spinning.
			select {
			case j := <-priority:
				return j, true
			case j := <-fallback:
				return j, true
			case <-time.After(redisErrNap):
			}
		}
		return job{}, false
	}
	var j job
	if err := json.Unmarshal(payload, &j); err != nil {
		log.Printf("[EMAIL] dropped undecodable queue entry: %v", err)
		return job{}, false
	}
	return j, true
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

// SetSendFuncForTest replaces the SMTP sender and returns a restore func.
// Only for tests in other packages that need to see what would be mailed.
func SetSendFuncForTest(fn func(to, subject, html string) error) func() {
	old := sendFn
	sendFn = fn
	return func() { sendFn = old }
}
