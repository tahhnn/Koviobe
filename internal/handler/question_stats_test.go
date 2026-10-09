package handler

import (
	"testing"

	"github.com/quizzzone/backend/internal/model"
)

func statsQuestions() []model.Question {
	return []model.Question{
		{ID: 10, Content: "Q1", Type: "multiple_choice", CorrectAnswer: "b", Options: `[{"id":"A","text":"Hà Nội"},{"id":"B","text":"Huế"}]`},
		{ID: 11, Content: "Q2", Type: "true_false", CorrectAnswer: "A", Options: `[{"id":"A","text":"Đúng"},{"id":"B","text":"Sai"}]`},
		{ID: 12, Content: "Q3", Type: "short_answer", CorrectAnswer: "Mekong"},
		{ID: 13, Content: "Q4", Type: "poll", CorrectAnswer: "A", Options: `[{"id":"A","text":"x"}]`},
	}
}

func TestQuestionStatsHostPacedStopsAtTheLastQuestionReached(t *testing.T) {
	counts := map[uint]answerCount{10: {answered: 8, correct: 5}, 11: {answered: 6, correct: 6}}
	// The host ended the game on the second question (index 1).
	got := buildQuestionStats(statsQuestions(), counts, 1, false, 10)
	if len(got) != 2 {
		t.Fatalf("want the 2 questions played, got %d: %#v", len(got), got)
	}
	want := questionStat{Order: 1, Content: "Q1", Type: "multiple_choice", CorrectAnswer: "b", CorrectText: "Huế", Answered: 8, Correct: 5, TotalPlayers: 10}
	if got[0] != want {
		t.Fatalf("got  %#v\nwant %#v", got[0], want)
	}
	if got[1].CorrectText != "Đúng" || got[1].Order != 2 {
		t.Fatalf("true_false row: %#v", got[1])
	}
}

func TestQuestionStatsKeepsAnAnsweredQuestionPastTheIndex(t *testing.T) {
	counts := map[uint]answerCount{12: {answered: 1, correct: 0}}
	got := buildQuestionStats(statsQuestions(), counts, 0, false, 3)
	if len(got) != 2 || got[1].Order != 3 {
		t.Fatalf("want Q1 and the answered Q3, got %#v", got)
	}
}

func TestQuestionStatsSoloListsEveryQuestion(t *testing.T) {
	got := buildQuestionStats(statsQuestions(), nil, -1, true, 4)
	if len(got) != 4 {
		t.Fatalf("want all 4 questions, got %d", len(got))
	}
	for _, s := range got {
		if s.TotalPlayers != 4 || s.Answered != 0 {
			t.Fatalf("unanswered row: %#v", s)
		}
	}
	if got[2].CorrectText != "" || got[3].CorrectText != "" {
		t.Fatalf("short_answer and poll have no option text: %#v %#v", got[2], got[3])
	}
}

func TestQuestionStatsLobbyEndListsNothing(t *testing.T) {
	if got := buildQuestionStats(statsQuestions(), nil, -1, false, 5); len(got) != 0 {
		t.Fatalf("a game ended in the lobby played no question, got %#v", got)
	}
}
