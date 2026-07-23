package cron

import (
	"encoding/json"
	"log"
	"sort"
	"time"

	"github.com/kovio/backend/internal/db"
	"github.com/kovio/backend/internal/model"
)

// StartCleanupWorker runs a background worker that cleans up abandoned rooms every hour.
func StartCleanupWorker() {
	log.Println("Starting abandoned rooms cleanup worker...")
	go func() {
		time.Sleep(10 * time.Second)
		cleanupAbandonedRooms()

		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for range ticker.C {
			cleanupAbandonedRooms()
		}
	}()
}

func cleanupAbandonedRooms() {
	log.Println("[CRON] Checking for abandoned rooms...")

	threshold := time.Now().Add(-12 * time.Hour)

	var abandonedRooms []model.Room
	err := db.DB.Where("(status = 'waiting' OR status = 'active') AND updated_at < ?", threshold).Find(&abandonedRooms).Error
	if err != nil {
		log.Printf("[CRON ERROR] Failed to fetch active/waiting rooms: %v\n", err)
		return
	}

	if len(abandonedRooms) == 0 {
		log.Println("[CRON] No abandoned rooms found.")
		return
	}

	for _, room := range abandonedRooms {
		log.Printf("[CRON] Cleaning up abandoned room ID %d (PIN: %s)...\n", room.ID, room.PinCode)

		// Archive rankings before deleting players (same shape as EndGame)
		type Ranking struct {
			Nickname       string `json:"nickname"`
			Score          int    `json:"score"`
			CorrectAnswers int    `json:"correct_answers"`
		}
		var players []model.Player
		db.DB.Where("room_id = ?", room.ID).Find(&players)

		var rankings []Ranking
		for _, p := range players {
			var correctCount int64
			db.DB.Model(&model.AnswerLog{}).Where("player_id = ? AND is_correct = ?", p.ID, true).Count(&correctCount)
			rankings = append(rankings, Ranking{
				Nickname:       p.Nickname,
				Score:          p.Score,
				CorrectAnswers: int(correctCount),
			})
		}
		sort.Slice(rankings, func(i, j int) bool {
			return rankings[i].Score > rankings[j].Score
		})

		rankingsJSON, _ := json.Marshal(rankings)
		var existing model.GameSession
		if db.DB.Where("room_id = ?", room.ID).First(&existing).Error != nil {
			session := model.GameSession{
				RoomID:      room.ID,
				HostID:      room.HostID,
				QuizID:      room.QuizID,
				Rankings:    string(rankingsJSON),
				PlayerCount: len(players),
				EndedAt:     time.Now(),
			}
			if err := db.DB.Create(&session).Error; err != nil {
				log.Printf("[CRON ERROR] Failed to archive room %d: %v\n", room.ID, err)
			}
		}

		room.Status = "finished"
		room.CurrentQuestionID = nil
		room.QuestionActiveUntil = nil
		if err := db.DB.Save(&room).Error; err != nil {
			log.Printf("[CRON ERROR] Failed to close room %d: %v\n", room.ID, err)
			continue
		}

		if err := db.DB.Where("room_id = ?", room.ID).Delete(&model.Player{}).Error; err != nil {
			log.Printf("[CRON ERROR] Failed to delete players for room %d: %v\n", room.ID, err)
		}

		if err := db.DB.Where("room_id = ?", room.ID).Delete(&model.AnswerLog{}).Error; err != nil {
			log.Printf("[CRON ERROR] Failed to delete answer logs for room %d: %v\n", room.ID, err)
		}

		log.Printf("[CRON] Successfully archived and cleaned up abandoned room %d\n", room.ID)
	}
}
