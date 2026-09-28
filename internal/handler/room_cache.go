package handler

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
)

// The parts of a room that SubmitAnswer needs before its transaction, cached
// in process.
//
// Every submit used to open with SELECT * FROM rooms for its room: one read
// per answer, a whole room's worth of them in the same second, for a row the
// transaction then locks and reads again anyway. Measured 2026-09-28, the
// submit path without its transaction still topped out near 1300 answers/s on
// CPU, so the reads that stay in front of it are worth removing.
//
// Only fields fixed when the room is created are kept here — nothing in the
// codebase writes quiz_id, host_id, pin_code or theme_config after CreateRoom
// — so there is nothing to invalidate. Status is deliberately absent: it
// changes, and the transaction reads it under FOR SHARE, which is the check
// that has always counted. The TTL only bounds memory for rooms long gone.
const roomMetaTTL = 10 * time.Minute

type roomMeta struct {
	ID          uint
	QuizID      uint
	HostID      uint
	PinCode     string
	PlayerPaced bool
}

type roomMetaEntry struct {
	// Held across the load, so the first submits of a room cost one query
	// between them rather than one each.
	mu     sync.Mutex
	meta   *roomMeta
	loaded time.Time
}

var roomMetaCache sync.Map // room id -> *roomMetaEntry

// submitRoomMeta returns the room's fixed fields. A room that does not exist
// is an error and is never cached.
func submitRoomMeta(roomID uint) (*roomMeta, error) {
	v, _ := roomMetaCache.LoadOrStore(roomID, &roomMetaEntry{})
	e := v.(*roomMetaEntry)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.meta != nil && time.Since(e.loaded) < roomMetaTTL {
		return e.meta, nil
	}
	var room model.Room
	if err := db.DB.Select("id", "quiz_id", "host_id", "pin_code", "theme_config").
		Take(&room, roomID).Error; err != nil {
		roomMetaCache.Delete(roomID)
		return nil, err
	}
	var config struct {
		GameMode string `json:"game_mode"`
	}
	json.Unmarshal([]byte(room.ThemeConfig), &config)
	e.meta = &roomMeta{
		ID:          room.ID,
		QuizID:      room.QuizID,
		HostID:      room.HostID,
		PinCode:     room.PinCode,
		PlayerPaced: config.GameMode == "player_paced",
	}
	e.loaded = time.Now()
	return e.meta, nil
}

// sweepRoomMeta drops entries past their TTL, so rooms that finished hours ago
// do not stay in memory for the life of the process.
func sweepRoomMeta() {
	roomMetaCache.Range(func(k, v any) bool {
		e := v.(*roomMetaEntry)
		e.mu.Lock()
		stale := e.meta != nil && time.Since(e.loaded) >= roomMetaTTL
		e.mu.Unlock()
		if stale {
			roomMetaCache.Delete(k)
		}
		return true
	})
}

func init() {
	go func() {
		for range time.Tick(roomMetaTTL) {
			sweepRoomMeta()
		}
	}()
}
