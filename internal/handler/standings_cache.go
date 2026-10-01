package handler

import (
	"sort"
	"sync"
	"time"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
)

// A short-lived ranked snapshot of a live room, shared by every player who
// opens the leaderboard slide in the same second.
//
// The slide makes the whole room ask at once, and each request used to cost
// three queries — top N, a COUNT of the room, and a COUNT of the players ahead
// of the caller. The last one is O(room) per caller, so O(room²) per slide.
// Measured 2026-09-24 at 3x1000 players this was the largest DB cost left, p95
// 6.6-7.4s. One ordered read of the room per second replaces all of it.
//
// A snapshot up to a second old is fine for everyone else's rows: in classic
// mode the board opens after the reveal, when the question's scores are final,
// and solo mode refreshes the board every 2s anyway. The caller's own row is
// NOT taken from the snapshot — see roomStandings.
const standingsSnapshotTTL = 1 * time.Second

// Entries for rooms nobody is asking about any more are dropped after this.
const standingsSnapshotIdle = 1 * time.Minute

type standingsEntry struct {
	mu     sync.Mutex
	rows   []StandingRow // score DESC, id ASC, Rank filled in
	loaded time.Time
}

var standingsCache sync.Map // room id -> *standingsEntry

// roomSnapshot returns the room's ranked rows, read at most once per TTL.
// The returned slice is shared — read it, never modify it.
func roomSnapshot(roomID uint) ([]StandingRow, error) {
	v, _ := standingsCache.LoadOrStore(roomID, &standingsEntry{})
	e := v.(*standingsEntry)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rows != nil && time.Since(e.loaded) < standingsSnapshotTTL {
		return e.rows, nil
	}
	rows := []StandingRow{}
	if err := db.DB.Model(&model.Player{}).
		Select("id, nickname, score").
		Where("room_id = ?", roomID).
		Order("score DESC, id ASC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].Rank = i + 1
	}
	e.rows, e.loaded = rows, time.Now()
	sweepStandingsCache(roomID)
	return rows, nil
}

// sweepStandingsCache drops idle rooms. It runs on a load, which happens at most
// once a second per live room, over a map holding only rooms read in the last
// minute — so it stays cheap. Another entry's loaded field is read under its
// own lock; TryLock skips one that is mid-load rather than waiting on it.
func sweepStandingsCache(current uint) {
	standingsCache.Range(func(k, v any) bool {
		if k.(uint) == current {
			return true
		}
		e := v.(*standingsEntry)
		if !e.mu.TryLock() {
			return true
		}
		idle := time.Since(e.loaded) > standingsSnapshotIdle
		e.mu.Unlock()
		if idle {
			standingsCache.Delete(k)
		}
		return true
	})
}

// rankAgainst places a row with the given score and id into a snapshot: the
// number of snapshot rows strictly ahead of it, plus one. The caller's own
// stale row, if present, never counts — scores only go up, so an older copy of
// the same player sorts at or behind the live one.
func rankAgainst(rows []StandingRow, score int, id uint) int {
	ahead := sort.Search(len(rows), func(i int) bool {
		r := rows[i]
		return !(r.Score > score || (r.Score == score && r.ID < id))
	})
	return ahead + 1
}
