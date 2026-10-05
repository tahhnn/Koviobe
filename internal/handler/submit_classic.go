package handler

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// classicAnswer is one host-paced submission, already scored.
type classicAnswer struct {
	RoomID         uint
	PlayerID       uint
	QuestionID     uint
	SelectedOption string
	IsCorrect      bool
	PointsEarned   int
	ResponseTimeMs int
}

type classicAnswerResult struct {
	Nickname string
	Score    int // after this answer
}

// recordClassicAnswer writes a host-paced answer and its points in a single
// statement, inside the caller's transaction (which holds the room FOR SHARE).
//
// It replaces SELECT players ... FOR UPDATE, an UPDATE of the whole player row
// (correct answers only) and the INSERT: 3–4 statements per submit, 2–3 round
// trips more than this. During a burst the pool (DB_MAX_OPEN_CONNS) is always
// full, so time a connection spends waiting on round trips is throughput lost
// (see kovio queue notes, 2026-09-28: Postgres CPU is not the limit).
//
// The player lock is not needed for correctness here:
//   - a repeat is stopped by idx_player_question: a concurrent duplicate waits
//     on the first INSERT and then fails the unique check, so points are added
//     once — the same guarantee the lock-then-insert order relied on;
//   - points are added with score = score + n, which takes the row lock for
//     the UPDATE itself and re-reads the committed score, so two answers can
//     never overwrite each other's points;
//   - host-paced scoring reads nothing from the player row.
//
// The INSERT only happens for a player who is in the token's room. The outer
// SELECT reads the statement's snapshot, so for a wrong answer (no UPDATE) it
// reports the score as of this statement — what the response always showed.
func recordClassicAnswer(tx *gorm.DB, a classicAnswer) (classicAnswerResult, error) {
	var row struct {
		Nickname string
		Score    int
		Inserted bool
	}
	res := tx.Raw(`
		WITH ins AS (
			INSERT INTO answer_logs
				(room_id, player_id, question_id, selected_option, is_correct, points_earned, response_time_ms, created_at)
			SELECT @room, p.id, @question, @option, @correct, @points, @rt, @now
			FROM players p
			WHERE p.id = @player AND p.room_id = @room
			RETURNING player_id
		), upd AS (
			UPDATE players SET score = score + @points, updated_at = @now
			WHERE @points > 0 AND id = (SELECT player_id FROM ins)
			RETURNING score
		)
		SELECT p.nickname,
		       COALESCE((SELECT score FROM upd), p.score) AS score,
		       EXISTS (SELECT 1 FROM ins) AS inserted
		FROM players p
		WHERE p.id = @player`,
		map[string]interface{}{
			"room":     a.RoomID,
			"player":   a.PlayerID,
			"question": a.QuestionID,
			"option":   a.SelectedOption,
			"correct":  a.IsCorrect,
			"points":   a.PointsEarned,
			"rt":       a.ResponseTimeMs,
			"now":      time.Now(),
		}).Scan(&row)
	if res.Error != nil {
		if isUniqueViolation(res.Error) {
			return classicAnswerResult{}, errors.New("already_answered")
		}
		return classicAnswerResult{}, res.Error
	}
	if res.RowsAffected == 0 {
		return classicAnswerResult{}, fmt.Errorf("player_not_found")
	}
	if !row.Inserted {
		// The player exists but in another room than the token's.
		return classicAnswerResult{}, fmt.Errorf("token_room_mismatch")
	}
	return classicAnswerResult{Nickname: row.Nickname, Score: row.Score}, nil
}

// isUniqueViolation matches Postgres's duplicate-key error text, as the
// submit path always has.
func isUniqueViolation(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "duplicate") || strings.Contains(msg, "unique")
}
