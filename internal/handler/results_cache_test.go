package handler

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/model"
)

// legacyResults is the body GetRoomResults built before the cache, kept here
// so the cached bytes can be compared against it.
func legacyResults(room *model.Room, players []model.Player, stats json.RawMessage, callerNickname string) []byte {
	sort.SliceStable(players, func(i, j int) bool { return players[i].Score > players[j].Score })
	standings := make([]gin.H, 0, len(players))
	for i, p := range players {
		standings = append(standings, gin.H{
			"id":              p.ID,
			"nickname":        p.Nickname,
			"score":           p.Score,
			"correct_answers": p.CorrectAnswers,
			"rank":            i + 1,
			"you":             callerNickname != "" && p.Nickname == callerNickname,
		})
	}
	b, _ := json.Marshal(gin.H{
		"room_id":        room.ID,
		"status":         room.Status,
		"players":        standings,
		"ended_reason":   room.EndedReason,
		"theme_config":   room.ThemeConfig,
		"question_stats": stats,
	})
	return b
}

func sampleResults() (*model.Room, []model.Player) {
	room := &model.Room{Status: "finished", EndedReason: "", ThemeConfig: `{"bg":"<img src=x>&"}`}
	room.ID = 269
	players := []model.Player{
		{Nickname: "alice", Score: 3000, CorrectAnswers: 3},
		{Nickname: "bob <b>", Score: 5000, CorrectAnswers: 5},
		{Nickname: "chị Hà", Score: 3000, CorrectAnswers: 2},
		{Nickname: "dup", Score: 10, CorrectAnswers: 0},
		{Nickname: "dup", Score: 5, CorrectAnswers: 0},
	}
	for i := range players {
		players[i].ID = uint(i + 1)
	}
	return room, players
}

// sampleStats is stored the way archiveGameLogs stores it: json.Marshal output.
func sampleStats() json.RawMessage {
	b, _ := json.Marshal([]questionStat{
		{Order: 1, Content: "2 < 3 & \"x\"?", Type: "multiple_choice", CorrectAnswer: "A", CorrectText: "Đúng", Answered: 4, Correct: 3, TotalPlayers: 5},
		{Order: 2, Content: "Bình chọn", Type: "poll", CorrectAnswer: "A", Answered: 2, TotalPlayers: 5},
	})
	return b
}

func TestCachedResultsMatchTheLegacyResponseByteForByte(t *testing.T) {
	for _, stats := range []json.RawMessage{sampleStats(), json.RawMessage("[]")} {
		for _, caller := range []string{"", "alice", "bob <b>", "chị Hà", "dup", "nobody"} {
			room, players := sampleResults()
			want := legacyResults(room, append([]model.Player(nil), players...), stats, caller)
			e := &resultsEntry{}
			e.fill(room, append([]model.Player(nil), players...), stats)
			if got := e.render(caller); string(got) != string(want) {
				t.Fatalf("caller %q:\n got  %s\n want %s", caller, got, want)
			}
		}
	}
}

func TestCachedResultsServeEmptyStatsForALegacyArchive(t *testing.T) {
	for _, stored := range []string{"", "null", "{}", "[broken"} {
		room, players := sampleResults()
		e := &resultsEntry{}
		e.fill(room, players, json.RawMessage(stored))
		var out struct {
			QuestionStats []questionStat `json:"question_stats"`
		}
		if err := json.Unmarshal(e.render(""), &out); err != nil {
			t.Fatalf("stored %q: invalid response: %v", stored, err)
		}
		if out.QuestionStats == nil || len(out.QuestionStats) != 0 {
			t.Fatalf("stored %q: want [], got %#v", stored, out.QuestionStats)
		}
	}
}

func TestCachedResultsRenderDoesNotLeakTheYouFlagBetweenCallers(t *testing.T) {
	room, players := sampleResults()
	e := &resultsEntry{}
	e.fill(room, players, sampleStats())
	_ = e.render("alice")
	var out struct {
		Players []struct {
			You bool `json:"you"`
		} `json:"players"`
	}
	if err := json.Unmarshal(e.render(""), &out); err != nil {
		t.Fatal(err)
	}
	for i, p := range out.Players {
		if p.You {
			t.Fatalf("row %d flagged for the host after a player's render", i)
		}
	}
}
