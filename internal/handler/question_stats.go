package handler

import (
	"encoding/json"
	"log"
	"strings"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
)

// How the room did on each question, for the results screen's second tab.
//
// It has to be captured inside finalizeRoom: answer_logs are deleted right
// after the archive is written, so nothing can count them once the room reads
// "finished". The question text and answer key are snapshotted too — the quiz
// can be edited (or deleted) after the game, and the results page must keep
// showing what was actually asked.
//
// The rate the client shows is Correct / TotalPlayers: a player who let the
// timer run out counts as not getting it right, the same as a wrong answer.
// The two numbers are kept apart so the client can still tell "nobody
// answered" from "everybody answered wrong".
type questionStat struct {
	Order         int    `json:"order"` // 1-based position in play order
	Content       string `json:"content"`
	Type          string `json:"type"`
	CorrectAnswer string `json:"correct_answer"` // raw key: option id, true/false, text, "x,y"
	CorrectText   string `json:"correct_text"`   // the option's text when the key is an option id
	Answered      int    `json:"answered"`
	Correct       int    `json:"correct"`
	TotalPlayers  int    `json:"total_players"`
}

// collectQuestionStats counts a room's answers per question. One grouped query
// over idx_answer_logs_room_id, run once per room at finalize. totalPlayers is
// the roster finalizeRoom already loaded, so the "did not answer" side of the
// rate costs nothing extra.
//
// Failures return nil and never abort the close: a missing tab is a cosmetic
// loss, a failed archive loses the game.
func collectQuestionStats(room model.Room, totalPlayers int) []questionStat {
	set, err := quizQuestions(room.QuizID)
	if err != nil {
		log.Printf("[collectQuestionStats] room %d: load questions: %v", room.ID, err)
		return nil
	}

	type row struct {
		QuestionID uint
		Answered   int
		Correct    int
	}
	var rows []row
	if err := db.DB.Model(&model.AnswerLog{}).
		Select("question_id, count(*) AS answered, count(*) FILTER (WHERE is_correct) AS correct").
		Where("room_id = ?", room.ID).
		Group("question_id").
		Scan(&rows).Error; err != nil {
		log.Printf("[collectQuestionStats] room %d: count answers: %v", room.ID, err)
		return nil
	}
	counts := make(map[uint]answerCount, len(rows))
	for _, r := range rows {
		counts[r.QuestionID] = answerCount{answered: r.Answered, correct: r.Correct}
	}

	return buildQuestionStats(set.list, counts, room.CurrentQuestionIndex,
		roomGameMode(room.ThemeConfig) == "player_paced", totalPlayers)
}

type answerCount struct{ answered, correct int }

// buildQuestionStats is the pure half of collectQuestionStats, split out so it
// can be tested without a database.
//
// Which questions belong in the list: a host-paced room only ever reached
// questions up to currentIndex — the host may have ended it early, and listing
// the unplayed rest at 0% would read as "everyone got these wrong". A solo room
// has no shared position, so every question is in play. Either way a question
// someone actually answered is always listed.
func buildQuestionStats(questions []model.Question, counts map[uint]answerCount,
	currentIndex int, playerPaced bool, totalPlayers int) []questionStat {
	stats := make([]questionStat, 0, len(questions))
	for i, q := range questions {
		c := counts[q.ID]
		if !playerPaced && i > currentIndex && c.answered == 0 {
			continue
		}
		stats = append(stats, questionStat{
			Order:         i + 1,
			Content:       q.Content,
			Type:          q.Type,
			CorrectAnswer: q.CorrectAnswer,
			CorrectText:   correctOptionText(q),
			Answered:      c.answered,
			Correct:       c.correct,
			TotalPlayers:  totalPlayers,
		})
	}
	return stats
}

// correctOptionText resolves an option-id answer key ("B") to the text players
// saw on that option. Empty when the key is not an option id (short_answer,
// pin_answer, poll) or the options cannot be read.
func correctOptionText(q model.Question) string {
	if q.Type != "multiple_choice" && q.Type != "true_false" {
		return ""
	}
	var opts []struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(q.Options), &opts) != nil {
		return ""
	}
	key := strings.TrimSpace(q.CorrectAnswer)
	for _, o := range opts {
		if strings.EqualFold(strings.TrimSpace(o.ID), key) {
			return o.Text
		}
	}
	return ""
}

// archivedQuestionStats returns the stats finalizeRoom stored, as raw JSON, or
// `[]` for a room archived before the column existed.
func archivedQuestionStats(roomID uint) json.RawMessage {
	var session model.GameSession
	if err := db.DB.Select("question_stats").Where("room_id = ?", roomID).First(&session).Error; err != nil {
		return json.RawMessage("[]")
	}
	return questionStatsJSON(session.QuestionStats)
}

// questionStatsJSON guards the stored text: anything that is not a JSON array
// (empty, a legacy row, damage) is served as `[]` rather than breaking the
// response.
func questionStatsJSON(stored string) json.RawMessage {
	s := strings.TrimSpace(stored)
	if !strings.HasPrefix(s, "[") || !json.Valid([]byte(s)) {
		return json.RawMessage("[]")
	}
	return json.RawMessage(s)
}

// liveQuestionStats is the uncached path's value: the archive's stats once the
// room is finished (archive written a moment after the status flips), `[]`
// while it is still running.
func liveQuestionStats(room model.Room) json.RawMessage {
	if room.Status != "finished" {
		return json.RawMessage("[]")
	}
	return archivedQuestionStats(room.ID)
}

// QuestionStatsJSON is collectQuestionStats encoded for game_sessions, for the
// cron sweep that archives abandoned rooms. "" when nothing could be counted.
func QuestionStatsJSON(room model.Room, totalPlayers int) string {
	stats := collectQuestionStats(room, totalPlayers)
	if stats == nil {
		return ""
	}
	b, _ := json.Marshal(stats)
	return string(b)
}
