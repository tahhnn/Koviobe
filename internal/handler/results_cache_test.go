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
func legacyResults(room *model.Room, players []model.Player, callerNickname string) []byte {
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
		"room_id":      room.ID,
		"status":       room.Status,
		"players":      standings,
		"ended_reason": room.EndedReason,
		"theme_config": room.ThemeConfig,
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

func TestCachedResultsMatchTheLegacyResponseByteForByte(t *testing.T) {
	for _, caller := range []string{"", "alice", "bob <b>", "chị Hà", "dup", "nobody"} {
		room, players := sampleResults()
		want := legacyResults(room, append([]model.Player(nil), players...), caller)
		e := &resultsEntry{}
		e.fill(room, append([]model.Player(nil), players...))
		if got := e.render(caller); string(got) != string(want) {
			t.Fatalf("caller %q:\n got  %s\n want %s", caller, got, want)
		}
	}
}

func TestCachedResultsRenderDoesNotLeakTheYouFlagBetweenCallers(t *testing.T) {
	room, players := sampleResults()
	e := &resultsEntry{}
	e.fill(room, players)
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
