package cron

import (
	"encoding/json"
	"log"
	"sort"
	"time"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/handler"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/license"
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

// StartLicenseExpiryWorker downgrades subscriptions the moment they lapse and
// closes any room the expired host still had open.
//
// Runs every 5 minutes rather than hourly: the window between "plan expired" and
// "host stops hosting" is time the customer is using capacity they no longer paid
// for, and an hour of that is long enough to run a whole event on.
//
// It is a no-op while LICENSE_ENFORCEMENT is off — SweepExpiredSubscriptions
// checks that itself, so turning enforcement off never ends anybody's game.
func StartLicenseExpiryWorker() {
	log.Println("Starting license expiry worker...")
	go func() {
		time.Sleep(20 * time.Second)
		sweepExpiredLicenses()

		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			sweepExpiredLicenses()
		}
	}()
}

func sweepExpiredLicenses() {
	grants, err := license.SweepExpiredSubscriptions()
	if err != nil {
		log.Printf("[CRON ERROR] license expiry sweep failed: %v\n", err)
		return
	}
	if len(grants) == 0 {
		return
	}

	for _, g := range grants {
		log.Printf("[CRON] License expired: user %d (%s) %s -> free, %d open room(s)\n",
			g.UserID, g.Email, g.PreviousPlan, len(g.RoomIDs))

		for _, roomID := range g.RoomIDs {
			// Close through the handler's own finalize path so players get
			// game:ended and the rankings are archived, exactly as if the host
			// had pressed End Game.
			if err := handler.FinalizeRoom(roomID); err != nil {
				log.Printf("[CRON ERROR] could not close room %d of expired host %d: %v\n",
					roomID, g.UserID, err)
				continue
			}
			log.Printf("[CRON] Closed room %d (host %d license expired)\n", roomID, g.UserID)
		}
	}
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
		log.Printf("[CRON] Cleaning up abandoned room ID %d...\n", room.ID)

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

		rankingsJSON, err := json.Marshal(rankings)
		if err != nil {
			log.Printf("[CRON ERROR] Failed to marshal rankings for room %d, skipping: %v\n", room.ID, err)
			continue
		}
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
			// The archive is the only surviving copy once the deletes below run,
			// so a failed archive must not be followed by a cleanup.
			if err := db.DB.Create(&session).Error; err != nil {
				log.Printf("[CRON ERROR] Failed to archive room %d, keeping players and answer logs: %v\n", room.ID, err)
				continue
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
