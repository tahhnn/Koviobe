package handler

import (
	"bytes"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/quizzzone/backend/internal/model"
)

// The final results of a finished room, encoded once and served to everyone.
//
// Every player lands on the results screen in the same second the game ends,
// and each request used to unmarshal the archived ranking, build one map per
// player and marshal the whole roster again — ~10ms of CPU and 130KB for a
// 1500-player room, so the room's own end-of-game burst cost ~15 CPU-seconds.
// Measured 2026-09-25 at 1500 players: p50 5.2s, p95 9s.
//
// A finished room's archive never changes, so the roster is encoded once. The
// only per-caller difference is the "you" flag, and that is spliced in by
// swapping the caller's pre-encoded rows — the response bytes are identical to
// what the handler built before.
//
// Only archived rankings are cached. In the window where a room already reads
// "finished" but its archive is not written yet (see getRoomPlayers), the
// handler keeps building the response from the live rows every time.
const resultsCacheIdle = 10 * time.Minute

type resultsEntry struct {
	mu     sync.Mutex
	built  bool
	hostID uint
	head   []byte   // `{"ended_reason":…,"players":[`
	tail   []byte   // `],"question_stats":…,"room_id":…,"status":…,"theme_config":…}`
	rows   [][]byte // each row with "you":false
	youRow [][]byte // the same row with "you":true
	byNick map[string][]int
	used   time.Time
}

var resultsCache sync.Map // room id -> *resultsEntry

// resultRow keeps the key order encoding/json gives the gin.H it replaces
// (keys sorted), so the wire format does not change.
type resultRow struct {
	CorrectAnswers int    `json:"correct_answers"`
	ID             uint   `json:"id"`
	Nickname       string `json:"nickname"`
	Rank           int    `json:"rank"`
	Score          int    `json:"score"`
	You            bool   `json:"you"`
}

// cachedResults returns the built entry for a room, or nil when none is ready.
func cachedResults(roomID uint) *resultsEntry {
	v, ok := resultsCache.Load(roomID)
	if !ok {
		return nil
	}
	e := v.(*resultsEntry)
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.built {
		return nil
	}
	e.used = time.Now()
	return e
}

// buildResults encodes a finished room's archived results once. It returns nil
// when the archive is not readable yet, in which case nothing is cached.
// Concurrent callers for the same room wait on one build instead of each
// repeating it.
func buildResults(room *model.Room) *resultsEntry {
	v, _ := resultsCache.LoadOrStore(room.ID, &resultsEntry{})
	e := v.(*resultsEntry)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.built {
		e.used = time.Now()
		return e
	}

	players := archivedPlayers(room.ID)
	if len(players) == 0 {
		resultsCache.Delete(room.ID)
		return nil
	}
	e.fill(room, players, archivedQuestionStats(room.ID))
	e.built, e.used = true, time.Now()
	sweepResultsCache(room.ID)
	return e
}

// fill encodes a room, its ranked players and its per-question stats (already
// JSON, as archived) into the entry.
func (e *resultsEntry) fill(room *model.Room, players []model.Player, questionStats json.RawMessage) {
	sort.SliceStable(players, func(i, j int) bool {
		return players[i].Score > players[j].Score
	})

	e.rows = make([][]byte, len(players))
	e.youRow = make([][]byte, len(players))
	e.byNick = make(map[string][]int, len(players))
	for i, p := range players {
		r := resultRow{CorrectAnswers: p.CorrectAnswers, ID: p.ID, Nickname: p.Nickname, Rank: i + 1, Score: p.Score}
		e.rows[i], _ = json.Marshal(r)
		r.You = true
		e.youRow[i], _ = json.Marshal(r)
		e.byNick[p.Nickname] = append(e.byNick[p.Nickname], i)
	}

	endedReason, _ := json.Marshal(room.EndedReason)
	roomID, _ := json.Marshal(room.ID)
	status, _ := json.Marshal(room.Status)
	theme, _ := json.Marshal(room.ThemeConfig)
	e.head = append(append([]byte(`{"ended_reason":`), endedReason...), []byte(`,"players":[`)...)
	e.tail = bytes.Join([][]byte{[]byte(`],"question_stats":`), questionStatsJSON(string(questionStats)),
		[]byte(`,"room_id":`), roomID, []byte(`,"status":`), status,
		[]byte(`,"theme_config":`), theme, []byte(`}`)}, nil)
	e.hostID = room.HostID
}

// render writes the response for one caller; nickname "" (the host) flags no row.
func (e *resultsEntry) render(nickname string) []byte {
	mine := map[int]bool{}
	if nickname != "" {
		for _, i := range e.byNick[nickname] {
			mine[i] = true
		}
	}
	size := len(e.head) + len(e.tail) + len(e.rows)
	for _, r := range e.rows {
		size += len(r)
	}
	out := make([]byte, 0, size)
	out = append(out, e.head...)
	for i, r := range e.rows {
		if i > 0 {
			out = append(out, ',')
		}
		if mine[i] {
			out = append(out, e.youRow[i]...)
		} else {
			out = append(out, r...)
		}
	}
	return append(out, e.tail...)
}

// sweepResultsCache drops rooms nobody has asked about for a while. It runs on
// a build, which happens once per finished room, so it stays cheap.
func sweepResultsCache(current uint) {
	resultsCache.Range(func(k, v any) bool {
		if k.(uint) == current {
			return true
		}
		e := v.(*resultsEntry)
		if !e.mu.TryLock() {
			return true
		}
		idle := e.built && time.Since(e.used) > resultsCacheIdle
		e.mu.Unlock()
		if idle {
			resultsCache.Delete(k)
		}
		return true
	})
}
