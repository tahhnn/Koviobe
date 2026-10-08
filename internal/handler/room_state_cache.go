package handler

import (
	"sync"
	"time"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
)

// A whole room row, shared for a moment by the players polling it.
//
// GetRoom's player branch and GetPlayerQuestion each opened with SELECT * FROM
// rooms for the caller's room. pg_stat_statements since 2026-09-24: 1.9M calls,
// the largest total time of any statement — a 1000-player room polling every
// 2.5s is ~400 reads a second of one row. With the snapshot it is at most one
// read per room per TTL.
//
// Unlike room_cache.go this holds the fields that change (status, current
// question, deadline), so every writer in this package calls
// invalidateRoomState after it commits: a player fetching the question that
// NextQuestion just pushed must not be told it is not active yet. Writers
// outside the package (the abandoned-room cron, the licence sweep) only ever
// finish a room, and for them the TTL is the bound: a player learns the room
// closed at most one TTL after the game:ended push already told them.
//
// One backend replica is assumed. A second replica would not see the other's
// invalidations and could serve a room up to one TTL stale; move this to Redis
// before scaling out.
const roomStateTTL = 500 * time.Millisecond

// Entries for rooms nobody polls any more are dropped after this.
const roomStateIdle = 1 * time.Minute

type roomStateEntry struct {
	// Held across the load, so a room's worth of polls arriving together cost
	// one query between them.
	mu     sync.Mutex
	room   *model.Room
	loaded time.Time
}

var roomStateCache sync.Map // room id -> *roomStateEntry

// roomState returns a copy of the room row, read at most once per TTL. The copy
// is the caller's own: modify it freely. gorm.ErrRecordNotFound is returned
// as-is and never cached.
func roomState(roomID uint) (model.Room, error) {
	v, _ := roomStateCache.LoadOrStore(roomID, &roomStateEntry{})
	e := v.(*roomStateEntry)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.room != nil && time.Since(e.loaded) < roomStateTTL {
		return *e.room, nil
	}
	var room model.Room
	if err := db.DB.Take(&room, roomID).Error; err != nil {
		roomStateCache.Delete(roomID)
		return model.Room{}, err
	}
	e.room, e.loaded = &room, time.Now()
	sweepRoomState(roomID)
	return room, nil
}

// invalidateRoomState drops the snapshot. Deleting the map entry rather than
// clearing it is what makes this race-free, as in invalidateQuizQuestions: a
// load still in flight writes into the detached entry, and the next reader
// starts a fresh one that reads the committed row.
func invalidateRoomState(roomID uint) {
	roomStateCache.Delete(roomID)
}

// sweepRoomState drops idle rooms. Same shape as sweepStandingsCache: it runs
// on a load, at most once per TTL per live room.
func sweepRoomState(current uint) {
	roomStateCache.Range(func(k, v any) bool {
		if k.(uint) == current {
			return true
		}
		e := v.(*roomStateEntry)
		if !e.mu.TryLock() {
			return true
		}
		idle := time.Since(e.loaded) > roomStateIdle
		e.mu.Unlock()
		if idle {
			roomStateCache.Delete(k)
		}
		return true
	})
}
