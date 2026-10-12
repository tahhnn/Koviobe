package email

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakeRedisQueue stands in for the Redis list.
type fakeRedisQueue struct {
	mu   sync.Mutex
	list [][]byte
	down bool
}

func (f *fakeRedisQueue) push(_ context.Context, p []byte, max int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return false, errors.New("connection refused")
	}
	if len(f.list) >= max {
		return false, nil
	}
	f.list = append(f.list, p)
	return true, nil
}

func (f *fakeRedisQueue) pop(_ context.Context, timeout time.Duration) ([]byte, error) {
	f.mu.Lock()
	if f.down {
		f.mu.Unlock()
		return nil, errors.New("connection refused")
	}
	if len(f.list) == 0 {
		f.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		return nil, redis.Nil
	}
	p := f.list[0]
	f.list = f.list[1:]
	f.mu.Unlock()
	return p, nil
}

// setupQueue resets the package state so each test starts its own workers.
func setupQueue(t *testing.T, f *fakeRedisQueue) chan job {
	t.Helper()
	sent := make(chan job, 100)
	oldPush, oldPop, oldSend, oldIncr := redisPush, redisPop, sendFn, recipientIncr
	redisPush, redisPop = f.push, f.pop
	sendFn = func(to, subject, html string) error {
		sent <- job{To: to, Subject: subject, HTML: html}
		return nil
	}
	recipientIncr = func(context.Context, string, time.Duration) (int64, error) { return 1, nil }
	startOnce = sync.Once{}
	t.Cleanup(func() {
		redisPush, redisPop, sendFn, recipientIncr = oldPush, oldPop, oldSend, oldIncr
	})
	return sent
}

func waitSent(t *testing.T, sent chan job) job {
	t.Helper()
	select {
	case j := <-sent:
		return j
	case <-time.After(3 * time.Second):
		t.Fatal("mail was not sent")
		return job{}
	}
}

func TestEnqueueGoesThroughRedis(t *testing.T) {
	f := &fakeRedisQueue{}
	sent := setupQueue(t, f)
	if err := Enqueue("a@example.com", "code", "<p>1</p>"); err != nil {
		t.Fatal(err)
	}
	if j := waitSent(t, sent); j.To != "a@example.com" || j.Subject != "code" {
		t.Fatalf("sent %+v", j)
	}
}

func TestEnqueueFallsBackToMemoryWhenRedisDown(t *testing.T) {
	f := &fakeRedisQueue{down: true}
	sent := setupQueue(t, f)
	if err := Enqueue("b@example.com", "code", "x"); err != nil {
		t.Fatalf("Redis outage must not drop the mail, got %v", err)
	}
	if j := waitSent(t, sent); j.To != "b@example.com" {
		t.Fatalf("sent %+v", j)
	}
}

func TestEnqueueRefusesWhenRedisQueueFull(t *testing.T) {
	f := &fakeRedisQueue{}
	setupQueue(t, f)
	// The bounded push script answers 0 once LLEN reaches the cap.
	redisPush = func(context.Context, []byte, int) (bool, error) { return false, nil }
	if err := Enqueue("c@example.com", "code", "x"); err != ErrQueueFull {
		t.Fatalf("want ErrQueueFull, got %v", err)
	}
}

func TestStaleJobsAreDropped(t *testing.T) {
	f := &fakeRedisQueue{}
	sent := setupQueue(t, f)
	f.list = append(f.list, []byte(`{"to":"old@example.com","subject":"old","queued":"2000-01-01T00:00:00Z"}`))
	if err := Enqueue("new@example.com", "new", "x"); err != nil {
		t.Fatal(err)
	}
	if j := waitSent(t, sent); j.To != "new@example.com" {
		t.Fatalf("stale mail was sent: %+v", j)
	}
}

func TestUncappedNeverTouchesRedis(t *testing.T) {
	f := &fakeRedisQueue{}
	sent := setupQueue(t, f)
	redisPush = func(context.Context, []byte, int) (bool, error) {
		t.Error("temporary password must not be written to Redis")
		return true, nil
	}
	if err := EnqueueUncapped("d@example.com", "pw", "secret"); err != nil {
		t.Fatal(err)
	}
	if j := waitSent(t, sent); j.To != "d@example.com" {
		t.Fatalf("sent %+v", j)
	}
}
