package notify

import (
	"fmt"
	"sync"
	"time"
)

// This file holds the state the Telegram command handler reads back:
// a mute window and a ring buffer of what was recently sent.

// recentCap is how many alerts /errors can look back over. Small on purpose —
// the buffer is a reminder of what just happened, not a log store. Anything
// older belongs in `docker logs`.
const recentCap = 25

// Record is one alert as it was delivered, kept for /errors.
type Record struct {
	Level Level
	Key   string
	Text  string
	At    time.Time
	Count int
	// Muted is true when the alert was recorded but suppressed rather than sent.
	Muted bool
}

var (
	ctrlMu     sync.RWMutex
	mutedUntil time.Time
	recent     []Record
)

// Mute suppresses P1 delivery until the deadline. P0 is deliberately NOT
// mutable: the point of muting is to stop the noise of a known, in-hand
// incident, and a mute that can hide the next unrelated outage is a trap.
// Alerts still land in the ring buffer while muted, so /errors shows what was
// swallowed.
func Mute(d time.Duration) time.Time {
	ctrlMu.Lock()
	defer ctrlMu.Unlock()
	mutedUntil = time.Now().Add(d)
	return mutedUntil
}

// Unmute lifts a mute early.
func Unmute() {
	ctrlMu.Lock()
	defer ctrlMu.Unlock()
	mutedUntil = time.Time{}
}

// MutedUntil returns the deadline and whether a mute is currently in force.
func MutedUntil() (time.Time, bool) {
	ctrlMu.RLock()
	defer ctrlMu.RUnlock()
	return mutedUntil, time.Now().Before(mutedUntil)
}

func isMuted() bool {
	_, ok := MutedUntil()
	return ok
}

// Recent returns up to n of the most recent alerts, newest first.
func Recent(n int) []Record {
	ctrlMu.RLock()
	defer ctrlMu.RUnlock()
	if n > len(recent) {
		n = len(recent)
	}
	out := make([]Record, 0, n)
	for i := len(recent) - 1; i >= len(recent)-n; i-- {
		out = append(out, recent[i])
	}
	return out
}

func remember(r Record) {
	ctrlMu.Lock()
	defer ctrlMu.Unlock()
	recent = append(recent, r)
	if len(recent) > recentCap {
		recent = recent[len(recent)-recentCap:]
	}
}

// Summary is the one-line state line /status prints about the alert pipeline
// itself — a reader needs to know whether silence means health or a mute.
func Summary() string {
	if active == nil {
		return "Cảnh báo: TẮT (thiếu token hoặc chat id)"
	}
	if until, ok := MutedUntil(); ok {
		return fmt.Sprintf("Cảnh báo: BẬT, đang mute P1 tới %s", until.Format("15:04:05 02/01"))
	}
	return "Cảnh báo: BẬT"
}
