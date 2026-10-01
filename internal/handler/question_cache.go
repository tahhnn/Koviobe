package handler

import (
	"sync"
	"time"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
)

// A quiz's questions, cached in process for the player hot paths.
//
// Every player request used to re-read them: GetPlayerQuestion loaded the whole
// list to find one index, GetRoom's player branch loaded it again plus a
// COUNT, and SubmitAnswer loaded the answered question and then the list.
// Measured 2026-09-24 under 1000-3000 players, that was ~65k reads of each
// shape in a few minutes for rows that do not change while a game runs.
//
// Freshness: every writer of the questions table (UpdateQuiz, DeleteQuiz,
// ImportBankQuestions) calls invalidateQuizQuestions after it commits. The TTL
// is only the safety net for a writer added later that forgets to.
//
// The returned set is shared between requests — read it, never modify it.
const questionCacheTTL = 30 * time.Second

type questionSet struct {
	list  []model.Question
	index map[uint]int // question id -> position in list
}

// byID returns the question and its position, or ok=false when the id is not
// part of this quiz.
func (s *questionSet) byID(id uint) (model.Question, int, bool) {
	i, ok := s.index[id]
	if !ok {
		return model.Question{}, -1, false
	}
	return s.list[i], i, true
}

type questionCacheEntry struct {
	// Held across the load, so a cold cache hit by a whole room at once costs
	// one query rather than one per player.
	mu     sync.Mutex
	set    *questionSet
	loaded time.Time
}

var questionCache sync.Map // quiz id -> *questionCacheEntry

// quizQuestions returns the quiz's questions in play order.
func quizQuestions(quizID uint) (*questionSet, error) {
	v, _ := questionCache.LoadOrStore(quizID, &questionCacheEntry{})
	e := v.(*questionCacheEntry)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.set != nil && time.Since(e.loaded) < questionCacheTTL {
		return e.set, nil
	}
	var qs []model.Question
	if err := db.DB.Where("quiz_id = ?", quizID).
		Order("questions.order ASC, questions.id ASC").Find(&qs).Error; err != nil {
		return nil, err
	}
	s := &questionSet{list: qs, index: make(map[uint]int, len(qs))}
	for i, q := range qs {
		s.index[q.ID] = i
	}
	e.set, e.loaded = s, time.Now()
	return s, nil
}

// invalidateQuizQuestions drops the cached set. Dropping the map entry rather
// than clearing it is what makes this race-free: a load still in flight writes
// into the detached entry, and the next reader starts a fresh one.
func invalidateQuizQuestions(quizID uint) {
	questionCache.Delete(quizID)
}
