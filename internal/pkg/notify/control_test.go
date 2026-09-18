package notify

import (
	"strings"
	"testing"
	"time"
)

// The invariant that makes /mute safe to offer at all: it silences P1 and
// never P0. A mute exists to quiet a known incident, not to blind the channel
// to the next unrelated one.
func TestMuteSuppressesP1ButNeverP0(t *testing.T) {
	cap, cleanup := newServer(t)
	defer cleanup()
	Unmute()

	Init(Config{BotToken: "tok", ChatID: "-100", Env: "test", FlushSeconds: 1, MaxPerMinute: 30})
	Mute(time.Hour)
	defer Unmute()

	P1("nen_bi_mute", "cảnh báo P1")
	P0("khong_duoc_mute", "cảnh báo P0")
	Shutdown()

	joined := strings.Join(cap.all(), "\n")
	if strings.Contains(joined, "nen_bi_mute") {
		t.Error("P1 was delivered while muted")
	}
	if !strings.Contains(joined, "khong_duoc_mute") {
		t.Error("P0 must ignore the mute")
	}
}

// A muted alert still has to be visible somewhere, or /errors would report
// calm during an incident somebody muted an hour ago.
func TestMutedAlertsStillRecorded(t *testing.T) {
	_, cleanup := newServer(t)
	defer cleanup()
	Unmute()

	Init(Config{BotToken: "tok", ChatID: "-100", Env: "test", FlushSeconds: 1, MaxPerMinute: 30})
	Mute(time.Hour)
	defer Unmute()

	P1("bi_nuot", "cảnh báo bị nuốt")
	Shutdown()

	recs := Recent(10)
	if len(recs) == 0 {
		t.Fatal("a muted alert must still reach the ring buffer")
	}
	found := false
	for _, r := range recs {
		if r.Key == "bi_nuot" {
			found = true
			if !r.Muted {
				t.Error("record should be flagged Muted")
			}
		}
	}
	if !found {
		t.Error("muted alert missing from Recent()")
	}
}

func TestRecentIsNewestFirstAndBounded(t *testing.T) {
	_, cleanup := newServer(t)
	defer cleanup()
	Unmute()

	Init(Config{BotToken: "tok", ChatID: "-100", Env: "test", FlushSeconds: 1, MaxPerMinute: 200})
	for i := 0; i < recentCap+10; i++ {
		P1("key"+itoa(i), "lỗi %d", i)
	}
	Shutdown()

	recs := Recent(1000)
	if len(recs) > recentCap {
		t.Fatalf("ring buffer grew past %d: got %d", recentCap, len(recs))
	}
	if len(recs) > 1 && recs[0].At.Before(recs[1].At) {
		t.Error("Recent() must return newest first")
	}
}

func TestUnmuteRestoresDelivery(t *testing.T) {
	cap, cleanup := newServer(t)
	defer cleanup()

	Init(Config{BotToken: "tok", ChatID: "-100", Env: "test", FlushSeconds: 1, MaxPerMinute: 30})
	Mute(time.Hour)
	Unmute()
	if _, muted := MutedUntil(); muted {
		t.Fatal("Unmute did not clear the mute")
	}
	P1("sau_unmute", "phải được gửi")
	Shutdown()

	if !strings.Contains(strings.Join(cap.all(), "\n"), "sau_unmute") {
		t.Error("P1 should be delivered again after /unmute")
	}
}
