package handler

import (
	"context"
	"fmt"
	"time"

	"github.com/quizzzone/backend/internal/cache"
)

// "Has this player already answered this question?" — asked by every submit,
// answered from Redis.
//
// It used to be two SELECTs on answer_logs per submit (one before the
// transaction, one inside it), GORM First() adding an ORDER BY id that kept
// Postgres off the unique index: 0.48ms each and the costliest statement left
// in the submit path (measured 2026-09-24, 12000 calls in one 3x1000 run).
//
// The claim is an optimisation, never the guarantee. The guarantee is still
// the unique index idx_player_question: a duplicate that gets past Redis — the
// key evicted, Redis restarted, Redis down — fails the INSERT, which rolls the
// whole transaction back, score update included.
//
// Lifetime: long enough to outlive any game. A key outliving its room costs a
// few bytes and nothing else, since a finished room refuses submissions anyway.
const answerClaimTTL = 6 * time.Hour

func answerClaimKey(playerID, questionID uint) string {
	return fmt.Sprintf("quiz:answered:%d:%d", playerID, questionID)
}

// claimAnswer marks the pair as answered.
//
// claimed=false means Redis already had the mark: a repeat. ok=false means
// Redis could not answer at all, and the caller must fall back to the database
// check rather than guess in either direction.
func claimAnswer(ctx context.Context, playerID, questionID uint) (claimed, ok bool) {
	if cache.RDB == nil {
		return false, false
	}
	set, err := cache.RDB.SetNX(ctx, answerClaimKey(playerID, questionID), 1, answerClaimTTL).Result()
	if err != nil {
		return false, false
	}
	return set, true
}

// releaseAnswer undoes a claim whose submit did not record an answer — a late
// submit, a question that is not open, a failed write — so the player can send
// it again. Best effort: a release that fails leaves a stale mark, and the
// retry is then refused as a repeat. That is the same outcome as the answer
// having gone through, and it lasts only until the key expires.
func releaseAnswer(playerID, questionID uint) {
	if cache.RDB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = cache.RDB.Del(ctx, answerClaimKey(playerID, questionID)).Err()
}
