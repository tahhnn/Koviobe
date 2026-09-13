package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/license"
	"gorm.io/gorm"
)

// ListLogs retrieves play logs and session reports for rooms owned by the authenticated user.
func ListLogs(c *gin.Context) {
	userID, _ := c.Get("user_id")

	var rooms []model.Room
	if err := db.DB.Where("host_id = ? AND status = 'finished'", userID.(uint)).Order("id DESC").Find(&rooms).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch logs history"})
		return
	}

	c.JSON(http.StatusOK, rooms)
}

// GetRoomLogs retrieves detailed score logs for a specific finished Room session
func GetRoomLogs(c *gin.Context) {
	userID, _ := c.Get("user_id")
	roomIDStr := c.Param("id")
	roomID, err := strconv.ParseUint(roomIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid room ID"})
		return
	}

	// Verify room belongs to this user (room.host_id)
	var room model.Room
	if err := db.DB.Preload("Quiz.Questions", func(db *gorm.DB) *gorm.DB {
		return db.Order("questions.order ASC, questions.id ASC")
	}).Where("id = ? AND host_id = ?", uint(roomID), userID.(uint)).First(&room).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Room session not found"})
		return
	}

	// Fetch all players and their final scores
	players := getRoomPlayers(room.ID, room.Status)

	// Fetch answer submissions
	var answers []model.AnswerLog
	db.DB.Where("room_id = ?", room.ID).Find(&answers)

	c.JSON(http.StatusOK, gin.H{
		"room":    room,
		"players": players,
		"answers": answers,
	})
}

// ExportRoomLogs returns detailed answer logs — Pro-only feature.
func ExportRoomLogs(c *gin.Context) {
	userID, _ := c.Get("user_id")
	uid := userID.(uint)

	ents, err := license.GetEntitlements(uid)
	if err != nil || (license.Enforcing() && !ents.AllowExportLogs) {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   "Exporting detailed logs requires Pro plan",
			"feature": "allow_export_logs",
			"plan_id": ents.PlanID,
		})
		return
	}

	roomIDStr := c.Param("id")
	roomID, parseErr := strconv.ParseUint(roomIDStr, 10, 32)
	if parseErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid room ID"})
		return
	}

	var room model.Room
	if err := db.DB.Where("id = ? AND host_id = ?", uint(roomID), uid).First(&room).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Room session not found"})
		return
	}

	var answers []model.AnswerLog
	db.DB.Where("room_id = ?", room.ID).Order("id ASC").Find(&answers)
	players := getRoomPlayers(room.ID, room.Status)

	c.JSON(http.StatusOK, gin.H{
		"room":    room,
		"players": players,
		"answers": answers,
		"format":  "json",
		"plan_id": ents.PlanID,
	})
}
