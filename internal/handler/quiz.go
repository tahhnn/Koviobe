package handler

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/audit"
	"github.com/quizzzone/backend/internal/pkg/jwt"
	"github.com/quizzzone/backend/internal/pkg/license"
	"github.com/quizzzone/backend/internal/realtime"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type CreateQuizReq struct {
	Title       string        `json:"title" binding:"required"`
	Description string        `json:"description"`
	ThemeConfig string        `json:"theme_config"` // Receives custom theme settings
	Questions   []QuestionReq `json:"questions"`
}

type QuestionReq struct {
	ID            uint        `json:"id"`
	Content       string      `json:"content" binding:"required"`
	// [M-5 FIX] Restrict type to known values; duration 5-120s; points 0-2000
	Type          string      `json:"type" binding:"required,oneof=multiple_choice true_false short_answer pin_answer poll"`
	Options       interface{} `json:"options"`
	CorrectAnswer string      `json:"correct_answer" binding:"required"`
	Duration      int         `json:"duration" binding:"min=5,max=120"`
	Points        int         `json:"points" binding:"min=0,max=2000"`
	Order         int         `json:"order"`
}

type JoinRoomReq struct {
	PinCode  string `json:"pin_code" binding:"required,len=6,numeric"`
	Nickname string `json:"nickname" binding:"required,min=1,max=20"`
}

type UpdateRoomPrivacyReq struct {
	IsPrivate bool `json:"is_private"`
}

type SubmitAnswerReq struct {
	QuestionID     uint   `json:"question_id" binding:"required"`
	SelectedOption string `json:"selected_option"`   // Empty allowed on timeout (0 points)
	ResponseTimeMs int    `json:"response_time_ms"` // Kept for client logging only; server ignores for scoring
}

// CRUD Quizzes
func CreateQuiz(c *gin.Context) {
	userID, _ := c.Get("user_id")
	uid := userID.(uint)

	ents, err := license.GetEntitlements(uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to resolve license"})
		return
	}

	var quizCount int64
	db.DB.Model(&model.Quiz{}).Where("host_id = ?", uid).Count(&quizCount)
	if license.EnforcementEnabled && !license.IsUnlimited(ents.MaxQuizzes) && int(quizCount) >= ents.MaxQuizzes {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   fmt.Sprintf("Quiz limit reached (%d). Upgrade to Pro for unlimited quizzes.", ents.MaxQuizzes),
			"plan_id": ents.PlanID,
			"limit":   ents.MaxQuizzes,
			"usage":   quizCount,
		})
		return
	}

	var req CreateQuizReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if license.EnforcementEnabled && !license.IsUnlimited(ents.MaxQuestionsPerQuiz) && len(req.Questions) > ents.MaxQuestionsPerQuiz {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   fmt.Sprintf("Too many questions (max %d on %s plan)", ents.MaxQuestionsPerQuiz, ents.PlanName),
			"plan_id": ents.PlanID,
		})
		return
	}

	if license.EnforcementEnabled && license.ThemeRequestsPlayerPaced(req.ThemeConfig) && !ents.AllowPlayerPaced {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   "Player-paced mode requires Pro plan",
			"plan_id": ents.PlanID,
			"feature": "allow_player_paced",
		})
		return
	}

	if license.EnforcementEnabled && !ents.AllowCustomBranding && req.ThemeConfig != "" && req.ThemeConfig != "{}" {
		// Free may still store basic theme; only block advanced branding keys if present
		var theme map[string]interface{}
		if json.Unmarshal([]byte(req.ThemeConfig), &theme) == nil {
			if _, ok := theme["logo_url"]; ok && !ents.AllowCustomBranding {
				c.JSON(http.StatusForbidden, gin.H{
					"error":   "Custom branding (logo) requires Pro plan",
					"feature": "allow_custom_branding",
				})
				return
			}
			if _, ok := theme["remove_watermark"]; ok && !ents.AllowRemoveWatermark {
				c.JSON(http.StatusForbidden, gin.H{
					"error":   "Removing watermark requires Pro plan",
					"feature": "allow_remove_watermark",
				})
				return
			}
		}
	}

	tx := db.DB.Begin()

	quiz := model.Quiz{
		HostID:      userID.(uint),
		Title:       req.Title,
		Description: req.Description,
		ThemeConfig: req.ThemeConfig,
	}

	if err := tx.Create(&quiz).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create quiz"})
		return
	}

	for idx, q := range req.Questions {
		optsJSON, err := json.Marshal(q.Options)
		if err != nil {
			tx.Rollback()
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid options format for question %d", idx)})
			return
		}

		question := model.Question{
			QuizID:        quiz.ID,
			Content:       q.Content,
			Type:          q.Type,
			Options:       string(optsJSON),
			CorrectAnswer: q.CorrectAnswer,
			Duration:      q.Duration,
			Points:        q.Points,
			Order:         q.Order,
		}

		if err := tx.Create(&question).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create questions"})
			return
		}
	}

	tx.Commit()
	if err := tx.Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to commit quiz"})
		return
	}

	// Record Audit Log
	audit.Record(userID.(uint), "create_quiz", fmt.Sprintf("quiz_%d", quiz.ID), c.ClientIP())

	c.JSON(http.StatusCreated, quiz)
}

func ListQuizzes(c *gin.Context) {
	userID, _ := c.Get("user_id")

	type quizListItem struct {
		ID            uint      `json:"id"`
		HostID        uint      `json:"host_id"`
		Title         string    `json:"title"`
		Description   string    `json:"description"`
		ThemeConfig   string    `json:"theme_config"`
		CreatedAt     time.Time `json:"created_at"`
		UpdatedAt     time.Time `json:"updated_at"`
		QuestionCount int64     `json:"question_count"`
	}

	var quizzes []model.Quiz
	if err := db.DB.Where("host_id = ?", userID.(uint)).Order("updated_at DESC").Find(&quizzes).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch quizzes"})
		return
	}

	counts := map[uint]int64{}
	if len(quizzes) > 0 {
		ids := make([]uint, len(quizzes))
		for i, q := range quizzes {
			ids[i] = q.ID
		}
		type countRow struct {
			QuizID uint  `gorm:"column:quiz_id"`
			Cnt    int64 `gorm:"column:cnt"`
		}
		var rows []countRow
		_ = db.DB.Model(&model.Question{}).
			Select("quiz_id, COUNT(*) AS cnt").
			Where("quiz_id IN ?", ids).
			Group("quiz_id").
			Scan(&rows).Error
		for _, r := range rows {
			counts[r.QuizID] = r.Cnt
		}
	}

	// Always JSON [] (never null) for an empty list
	out := make([]quizListItem, 0, len(quizzes))
	for _, q := range quizzes {
		out = append(out, quizListItem{
			ID:            q.ID,
			HostID:        q.HostID,
			Title:         q.Title,
			Description:   q.Description,
			ThemeConfig:   q.ThemeConfig,
			CreatedAt:     q.CreatedAt,
			UpdatedAt:     q.UpdatedAt,
			QuestionCount: counts[q.ID],
		})
	}

	c.JSON(http.StatusOK, out)
}

func GetQuiz(c *gin.Context) {
	userID, _ := c.Get("user_id")
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid quiz ID"})
		return
	}

	var quiz model.Quiz
	if err := db.DB.Preload("Questions", func(db *gorm.DB) *gorm.DB {
		return db.Order("questions.order ASC, questions.id ASC")
	}).Where("id = ? AND host_id = ?", uint(id), userID.(uint)).First(&quiz).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Quiz not found"})
		return
	}

	c.JSON(http.StatusOK, quiz)
}

// Room & Game Control Handlers
func CreateRoom(c *gin.Context) {
	userID, _ := c.Get("user_id")
	uid := userID.(uint)

	ents, err := license.GetEntitlements(uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to resolve license"})
		return
	}

	var concurrent int64
	db.DB.Model(&model.Room{}).Where("host_id = ? AND status IN ?", uid, []string{"waiting", "active"}).Count(&concurrent)
	if license.EnforcementEnabled && !license.IsUnlimited(ents.MaxConcurrentRooms) && int(concurrent) >= ents.MaxConcurrentRooms {
		// Free plan often leaves stale waiting lobbies — close them so host can start a new room.
		db.DB.Model(&model.Room{}).
			Where("host_id = ? AND status = ?", uid, "waiting").
			Updates(map[string]interface{}{
				"status":                 "finished",
				"current_question_id":    nil,
				"current_question_index": -1,
			})
		db.DB.Model(&model.Room{}).Where("host_id = ? AND status IN ?", uid, []string{"waiting", "active"}).Count(&concurrent)
		if int(concurrent) >= ents.MaxConcurrentRooms {
			c.JSON(http.StatusForbidden, gin.H{
				"error":   fmt.Sprintf("Concurrent room limit reached (%d). End an active room or upgrade to Pro.", ents.MaxConcurrentRooms),
				"plan_id": ents.PlanID,
				"limit":   ents.MaxConcurrentRooms,
			})
			return
		}
	}

	quizIDStr := c.Query("quiz_id")
	if quizIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "quiz_id query param is required"})
		return
	}

	quizID, err := strconv.ParseUint(quizIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid quiz_id"})
		return
	}

	// Verify quiz exists and belongs to host
	var quiz model.Quiz
	if err := db.DB.Preload("Questions").Where("id = ? AND host_id = ?", uint(quizID), uid).First(&quiz).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Quiz not found"})
		return
	}

	if len(quiz.Questions) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Quiz has no questions"})
		return
	}
	if license.EnforcementEnabled && !license.IsUnlimited(ents.MaxQuestionsPerQuiz) && len(quiz.Questions) > ents.MaxQuestionsPerQuiz {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   fmt.Sprintf("Quiz has %d questions (max %d on %s plan). Remove some questions or upgrade.", len(quiz.Questions), ents.MaxQuestionsPerQuiz, ents.PlanName),
			"plan_id": ents.PlanID,
			"limit":   ents.MaxQuestionsPerQuiz,
			"count":   len(quiz.Questions),
		})
		return
	}

	if license.EnforcementEnabled && license.ThemeRequestsPlayerPaced(quiz.ThemeConfig) && !ents.AllowPlayerPaced {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   "This quiz uses player-paced mode which requires Pro plan",
			"feature": "allow_player_paced",
		})
		return
	}

	// [L-3 FIX] Use crypto/rand instead of deprecated rand.Seed + math/rand for PIN generation
	var pinCode string
	for {
		n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
		pinCode = fmt.Sprintf("%06d", n.Int64())
		var count int64
		db.DB.Model(&model.Room{}).Where("pin_code = ? AND status != 'finished'", pinCode).Count(&count)
		if count == 0 {
			break
		}
	}

	isPrivate := true
	if v := c.Query("is_private"); v == "false" || v == "0" {
		isPrivate = false
	} else if v == "true" || v == "1" {
		isPrivate = true
	}

	room := model.Room{
		PinCode:              pinCode,
		QuizID:               quiz.ID,
		HostID:               uid,
		Status:               "waiting",
		IsPrivate:            isPrivate,
		ThemeConfig:          quiz.ThemeConfig, // Inherit theme config directly from Quiz
		CurrentQuestionIndex: -1,
	}

	if err := db.DB.Create(&room).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create room"})
		return
	}

	// Record Audit Log
	audit.Record(uid, "create_room", fmt.Sprintf("room_%d", room.ID), c.ClientIP())

	c.JSON(http.StatusCreated, gin.H{
		"id":                     room.ID,
		"pin_code":               room.PinCode,
		"quiz_id":                room.QuizID,
		"host_id":                room.HostID,
		"status":                 room.Status,
		"is_private":             room.IsPrivate,
		"theme_config":           room.ThemeConfig,
		"current_question_index": room.CurrentQuestionIndex,
		"max_players":            ents.MaxPlayersPerRoom,
		"plan_id":                ents.PlanID,
	})
}

// UpdateRoomPrivacy toggles whether a waiting room appears on the open lobby.
func UpdateRoomPrivacy(c *gin.Context) {
	userID, _ := c.Get("user_id")
	roomIDStr := c.Param("id")
	roomID, err := strconv.ParseUint(roomIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid room ID"})
		return
	}

	var req UpdateRoomPrivacyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var room model.Room
	if err := db.DB.Where("id = ? AND host_id = ?", uint(roomID), userID.(uint)).First(&room).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Room not found"})
		return
	}
	if room.Status != "waiting" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Privacy can only be changed while the room is waiting"})
		return
	}

	room.IsPrivate = req.IsPrivate
	if err := db.DB.Model(&room).Update("is_private", req.IsPrivate).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update privacy"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"id":         room.ID,
		"is_private": room.IsPrivate,
	})
}

// ListPublicRooms returns waiting rooms that are not private (open lobby).
func ListPublicRooms(c *gin.Context) {
	type publicRoom struct {
		ID          uint   `json:"id"`
		PinCode     string `json:"pin_code"`
		Status      string `json:"status"`
		QuizTitle   string `json:"quiz_title"`
		PlayerCount int64  `json:"player_count"`
		MaxPlayers  int    `json:"max_players"`
	}

	var rooms []model.Room
	if err := db.DB.Preload("Quiz").Where("status = ? AND is_private = ?", "waiting", false).
		Order("created_at DESC").Limit(30).Find(&rooms).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list rooms"})
		return
	}

	out := make([]publicRoom, 0, len(rooms))
	for _, room := range rooms {
		var playerCount int64
		db.DB.Model(&model.Player{}).Where("room_id = ?", room.ID).Count(&playerCount)
		ents, _ := license.GetEntitlements(room.HostID)
		title := room.Quiz.Title
		if title == "" {
			title = "Open quiz"
		}
		out = append(out, publicRoom{
			ID:          room.ID,
			PinCode:     room.PinCode,
			Status:      room.Status,
			QuizTitle:   title,
			PlayerCount: playerCount,
			MaxPlayers:  ents.MaxPlayersPerRoom,
		})
	}

	c.JSON(http.StatusOK, gin.H{"rooms": out})
}

// JoinRoom lets an anonymous Player join a Room.
func JoinRoom(c *gin.Context) {
	var req JoinRoomReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var room model.Room
	if err := db.DB.Where("pin_code = ? AND status = ?", req.PinCode, "waiting").First(&room).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Room not found or invalid PIN"})
		return
	}

	var playerCount int64
	db.DB.Model(&model.Player{}).Where("room_id = ?", room.ID).Count(&playerCount)

	ents, _ := license.GetEntitlements(room.HostID)
	maxPlayers := ents.MaxPlayersPerRoom
	if maxPlayers <= 0 {
		maxPlayers = 20
	}
	if license.EnforcementEnabled && int(playerCount) >= maxPlayers {
		c.JSON(http.StatusForbidden, gin.H{
			"error":       fmt.Sprintf("Room is full (max %d players on host's %s plan)", maxPlayers, ents.PlanName),
			"max_players": maxPlayers,
			"plan_id":     ents.PlanID,
		})
		return
	}

	// Check if nickname already taken in this room
	var existingPlayer model.Player
	err := db.DB.Where("room_id = ? AND nickname = ?", room.ID, req.Nickname).First(&existingPlayer).Error
	if err == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Nickname is already taken in this room"})
		return
	}

	player := model.Player{
		RoomID:      room.ID,
		Nickname:    req.Nickname,
		Score:       0,
		IsConnected: true,
	}

	if err := db.DB.Create(&player).Error; err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Nickname is already taken in this room"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to join room"})
		return
	}

	clientID := fmt.Sprintf("player_%d_%s", player.ID, player.Nickname)
	channel := realtime.RoomChannel(room.PinCode)
	centrifugoToken, err := realtime.Client.GenerateConnectionToken(clientID, 3600, channel)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate realtime token"})
		return
	}

	// [C-3 FIX] Issue a signed player JWT so the server can verify identity
	// on answer submissions — replacing the insecure X-Player-ID header.
	playerToken, err := jwt.GeneratePlayerToken(player.ID, room.ID, player.Nickname)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate player token"})
		return
	}

	if err := realtime.Client.Publish(channel, "player:joined", gin.H{
		"player_id": player.ID,
		"nickname":  player.Nickname,
	}); err != nil {
		log.Printf("[JoinRoom] realtime publish failed room=%d pin=%s: %v", room.ID, room.PinCode, err)
	}

	c.JSON(http.StatusOK, gin.H{
		"player_id":      player.ID,
		"nickname":       player.Nickname,
		"room_id":        room.ID,
		"theme_config":   room.ThemeConfig,
		"player_token":   playerToken,
		"centrifugo_tok": centrifugoToken,
		"centrifugo_cli": clientID,
	})
}

// StartGame changes the status of a room to active
func StartGame(c *gin.Context) {
	userID, _ := c.Get("user_id")
	roomIDStr := c.Param("id")
	roomID, _ := strconv.ParseUint(roomIDStr, 10, 32)

	var room model.Room
	if err := db.DB.Where("id = ? AND host_id = ?", uint(roomID), userID.(uint)).First(&room).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Room not found"})
		return
	}

	if room.Status != "waiting" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Room can only be started from waiting status"})
		return
	}

	room.Status = "active"
	room.CurrentQuestionIndex = -1
	room.CurrentQuestionID = nil
	db.DB.Save(&room)

	// Parse ThemeConfig to check game mode
	var themeCfg struct {
		GameMode string `json:"game_mode"`
	}
	json.Unmarshal([]byte(room.ThemeConfig), &themeCfg)
	isPlayerPaced := themeCfg.GameMode == "player_paced"

	if isPlayerPaced {
		// Assign first question but do NOT start the timer yet — timer starts when
		// the player actually loads the question (avoids desync vs client clock).
		var firstQuestion model.Question
		err := db.DB.Where("quiz_id = ?", room.QuizID).Order("questions.order ASC, questions.id ASC").First(&firstQuestion).Error
		if err == nil {
			db.DB.Model(&model.Player{}).Where("room_id = ?", room.ID).UpdateColumns(map[string]interface{}{
				"current_question_id":   firstQuestion.ID,
				"question_active_until": nil,
			})
		}
	}

	realtime.Client.Publish(fmt.Sprintf("rooms:%s", room.PinCode), "game:started", gin.H{
		"room_id": room.ID,
	})

	// Record Audit Log
	audit.Record(userID.(uint), "start_game", fmt.Sprintf("room_%d", room.ID), c.ClientIP())

	c.JSON(http.StatusOK, gin.H{"message": "Game started"})
}

// NextQuestion triggers next question display for players
func NextQuestion(c *gin.Context) {
	userID, _ := c.Get("user_id")
	roomIDStr := c.Param("id")
	roomID, _ := strconv.ParseUint(roomIDStr, 10, 32)

	var room model.Room
	if err := db.DB.Where("id = ? AND host_id = ?", uint(roomID), userID.(uint)).First(&room).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Room not found"})
		return
	}
	if room.Status != "active" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Room is not active"})
		return
	}
	if roomGameMode(room.ThemeConfig) == "player_paced" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Host question controls are not available in solo mode"})
		return
	}

	var quiz model.Quiz
	db.DB.Preload("Questions", func(db *gorm.DB) *gorm.DB {
		return db.Order("questions.order ASC, questions.id ASC")
	}).First(&quiz, room.QuizID)

	nextIndex := room.CurrentQuestionIndex + 1
	if nextIndex >= len(quiz.Questions) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No more questions. Use EndGame to close room."})
		return
	}

	activeQuestion := quiz.Questions[nextIndex]
	activeUntil := time.Now().Add(time.Duration(activeQuestion.Duration) * time.Second)

	room.CurrentQuestionIndex = nextIndex
	room.CurrentQuestionID = &activeQuestion.ID
	room.QuestionActiveUntil = &activeUntil
	db.DB.Save(&room)

	options := sanitizePlayerOptions(activeQuestion.Options)

	realtime.Client.Publish(fmt.Sprintf("rooms:%s", room.PinCode), "question:active", gin.H{
		"question_id":  activeQuestion.ID,
		"content":       activeQuestion.Content,
		"type":          activeQuestion.Type,
		"options":       options,
		"duration":      activeQuestion.Duration,
		"active_until": activeUntil.Format(time.RFC3339),
		"index":         nextIndex,
		"total":         len(quiz.Questions),
	})

	c.JSON(http.StatusOK, gin.H{
		"message":        "Question activated",
		"question_index": nextIndex,
		"duration":       activeQuestion.Duration,
		"active_until":   activeUntil.Format(time.RFC3339),
	})
}

// parsePinCoords parses "x,y" percentage coordinates (0-100).
func parsePinCoords(raw string) (x, y float64, ok bool) {
	parts := strings.Split(strings.TrimSpace(raw), ",")
	if len(parts) != 2 {
		return 0, 0, false
	}
	x, errX := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	y, errY := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if errX != nil || errY != nil {
		return 0, 0, false
	}
	return x, y, true
}

// sanitizePlayerOptions strips answer-key fields so players cannot cheat from the payload.
func sanitizePlayerOptions(raw string) interface{} {
	var opts []map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &opts); err != nil {
		var any interface{}
		_ = json.Unmarshal([]byte(raw), &any)
		return any
	}
	for i := range opts {
		delete(opts[i], "isCorrect")
		delete(opts[i], "is_correct")
	}
	return opts
}

func roomGameMode(themeConfig string) string {
	var cfg struct {
		GameMode string `json:"game_mode"`
	}
	_ = json.Unmarshal([]byte(themeConfig), &cfg)
	return cfg.GameMode
}

// evaluateAnswer scores a submission. pin_answer uses % distance tolerance (default 8%).
// poll never awards correctness. short_answer / true_false / multiple_choice use EqualFold.
func evaluateAnswer(question model.Question, selected string) bool {
	selected = strings.TrimSpace(selected)
	correct := strings.TrimSpace(question.CorrectAnswer)
	if selected == "" {
		return false
	}
	switch question.Type {
	case "poll":
		// Opinion collection only — never scored as correct/incorrect.
		return false
	case "pin_answer":
		cx, cy, okC := parsePinCoords(correct)
		sx, sy, okS := parsePinCoords(selected)
		if !okC || !okS {
			return false
		}
		const radiusPct = 8.0
		dx := cx - sx
		dy := cy - sy
		return math.Sqrt(dx*dx+dy*dy) <= radiusPct
	case "short_answer":
		return strings.EqualFold(correct, selected)
	default:
		// multiple_choice, true_false: compare option id (A/B/…)
		return strings.EqualFold(correct, selected)
	}
}

// SubmitAnswer receives answer from client and calculates scores.
// [C-3 FIX] Authentication now uses a signed player JWT (X-Player-Token header)
// instead of the forged-able X-Player-ID plain integer header.
func SubmitAnswer(c *gin.Context) {
	// Validate signed player token from header
	playerTokenStr := c.GetHeader("X-Player-Token")
	if playerTokenStr == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "X-Player-Token header is required"})
		return
	}

	playerClaims, err := jwt.VerifyPlayerToken(playerTokenStr)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired player token"})
		return
	}

	var player model.Player
	if err := db.DB.First(&player, playerClaims.PlayerID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Player not found"})
		return
	}

	// Extra check: ensure token's room matches the player's actual room
	if player.RoomID != playerClaims.RoomID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Token room mismatch"})
		return
	}

	var room model.Room
	db.DB.First(&room, player.RoomID)

	if room.Status != "active" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Room is not active"})
		return
	}

	var req SubmitAnswerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Parse ThemeConfig to check game mode
	var config struct {
		GameMode string `json:"game_mode"`
	}
	json.Unmarshal([]byte(room.ThemeConfig), &config)
	isPlayerPaced := config.GameMode == "player_paced"

	var question model.Question
	if err := db.DB.First(&question, req.QuestionID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Question not found"})
		return
	}

	// Security check: ensure question belongs to room's quiz
	if question.QuizID != room.QuizID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Question does not belong to this quiz room"})
		return
	}

	var existingLog model.AnswerLog
	existingErr := db.DB.Where("player_id = ? AND question_id = ?", player.ID, req.QuestionID).First(&existingLog).Error
	if existingErr == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "You have already answered this question"})
		return
	}

	isCorrect := evaluateAnswer(question, req.SelectedOption)
	pointsEarned := 0
	var responseTimeMs int
	finalScore := player.Score

	err = db.DB.Transaction(func(tx *gorm.DB) error {
		// Lock player row to prevent concurrent double-submit
		var lockedPlayer model.Player
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&lockedPlayer, player.ID).Error; err != nil {
			return err
		}

		var dup model.AnswerLog
		if err := tx.Where("player_id = ? AND question_id = ?", lockedPlayer.ID, req.QuestionID).First(&dup).Error; err == nil {
			return fmt.Errorf("already_answered")
		}

		if isPlayerPaced {
			if lockedPlayer.CurrentQuestionID == nil || *lockedPlayer.CurrentQuestionID != req.QuestionID {
				return fmt.Errorf("question_not_active")
			}
			// Lazy-start timer if player submits before fetch finished starting it
			if lockedPlayer.QuestionActiveUntil == nil {
				until := time.Now().Add(time.Duration(question.Duration) * time.Second)
				lockedPlayer.QuestionActiveUntil = &until
			}
			// 2s grace for client/server clock skew
			if time.Now().After(lockedPlayer.QuestionActiveUntil.Add(2 * time.Second)) {
				return fmt.Errorf("time_exceeded")
			}

			questionStartedAt := lockedPlayer.QuestionActiveUntil.Add(-time.Duration(question.Duration) * time.Second)
			responseTimeMs = int(time.Since(questionStartedAt).Milliseconds())
			if responseTimeMs < 0 {
				responseTimeMs = 0
			}

			durationMs := float64(question.Duration * 1000)
			if durationMs <= 0 {
				durationMs = 30000
			}
			ratio := 1.0 - (float64(responseTimeMs) / durationMs)
			if ratio < 0 {
				ratio = 0
			} else if ratio > 1 {
				ratio = 1
			}

			if isCorrect {
				pointsEarned = int(float64(question.Points) * (0.5 + 0.5*ratio))
				lockedPlayer.Score += pointsEarned
			}

			var questions []model.Question
			if err := tx.Where("quiz_id = ?", room.QuizID).Order("questions.order ASC, questions.id ASC").Find(&questions).Error; err != nil {
				return err
			}

			nextIdx := -1
			for idx, q := range questions {
				if q.ID == req.QuestionID {
					nextIdx = idx + 1
					break
				}
			}
			if nextIdx > 0 && nextIdx < len(questions) {
				nextQ := questions[nextIdx]
				// Assign next question but delay timer until player loads it
				lockedPlayer.CurrentQuestionID = &nextQ.ID
				lockedPlayer.QuestionActiveUntil = nil
			} else {
				lockedPlayer.CurrentQuestionID = nil
				lockedPlayer.QuestionActiveUntil = nil
			}
			if err := tx.Save(&lockedPlayer).Error; err != nil {
				return err
			}
			finalScore = lockedPlayer.Score
		} else {
			var lockedRoom model.Room
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&lockedRoom, room.ID).Error; err != nil {
				return err
			}
			if lockedRoom.CurrentQuestionID == nil || *lockedRoom.CurrentQuestionID != req.QuestionID {
				return fmt.Errorf("question_not_active")
			}
			// 2s grace for client/server clock skew
			if lockedRoom.QuestionActiveUntil == nil || time.Now().UTC().After(lockedRoom.QuestionActiveUntil.UTC().Add(2*time.Second)) {
				return fmt.Errorf("time_exceeded")
			}

			activeSince := lockedRoom.QuestionActiveUntil.UTC().Add(-time.Duration(question.Duration) * time.Second)
			responseTimeMs = int(time.Now().UTC().Sub(activeSince).Milliseconds())

			if isCorrect {
				timeLeft := lockedRoom.QuestionActiveUntil.UTC().Sub(time.Now().UTC())
				totalDuration := time.Duration(question.Duration) * time.Second
				ratio := float64(timeLeft) / float64(totalDuration)
				if ratio < 0 {
					ratio = 0
				} else if ratio > 1 {
					ratio = 1
				}
				pointsEarned = int(float64(question.Points) * (0.5 + 0.5*ratio))
				lockedPlayer.Score += pointsEarned
				if err := tx.Save(&lockedPlayer).Error; err != nil {
					return err
				}
			}
			finalScore = lockedPlayer.Score
		}

		logEntry := model.AnswerLog{
			RoomID:         room.ID,
			PlayerID:       lockedPlayer.ID,
			QuestionID:     req.QuestionID,
			SelectedOption: req.SelectedOption,
			IsCorrect:      isCorrect,
			PointsEarned:   pointsEarned,
			ResponseTimeMs: responseTimeMs,
		}
		if err := tx.Create(&logEntry).Error; err != nil {
			if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
				return fmt.Errorf("already_answered")
			}
			return err
		}
		return nil
	})

	if err != nil {
		switch err.Error() {
		case "already_answered":
			c.JSON(http.StatusBadRequest, gin.H{"error": "You have already answered this question"})
		case "question_not_active":
			c.JSON(http.StatusBadRequest, gin.H{"error": "Question is not currently active"})
		case "time_exceeded":
			c.JSON(http.StatusBadRequest, gin.H{"error": "Time limit exceeded for this question"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to submit answer"})
		}
		return
	}

	answeredPayload := gin.H{
		"player_id":     player.ID,
		"nickname":      player.Nickname,
		"score":         finalScore,
		"points_earned": pointsEarned,
		"question_type": question.Type,
	}
	if question.Type != "poll" {
		answeredPayload["is_correct"] = isCorrect
	}
	_ = realtime.Client.Publish(realtime.RoomChannel(room.PinCode), "player:answered", answeredPayload)

	resp := gin.H{
		"is_correct":    isCorrect,
		"points_earned": pointsEarned,
		"score":         finalScore,
		"question_type": question.Type,
	}
	if question.Type == "poll" {
		resp["is_correct"] = false
		resp["recorded"] = true
	}
	c.JSON(http.StatusOK, resp)
}

// pollOptionStats tallies AnswerLog rows for a poll question into a per-option
// vote count + percentage breakdown, ordered the same as the question's options.
func pollOptionStats(questionID uint, optionsJSON string) ([]gin.H, int) {
	var opts []struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal([]byte(optionsJSON), &opts)

	type voteCount struct {
		SelectedOption string
		Count          int
	}
	var counts []voteCount
	db.DB.Model(&model.AnswerLog{}).
		Select("selected_option, count(*) as count").
		Where("question_id = ?", questionID).
		Group("selected_option").
		Scan(&counts)

	countByOption := make(map[string]int, len(counts))
	total := 0
	for _, c := range counts {
		countByOption[c.SelectedOption] = c.Count
		total += c.Count
	}

	stats := make([]gin.H, 0, len(opts))
	for _, o := range opts {
		count := countByOption[o.ID]
		percentage := 0
		if total > 0 {
			percentage = int(math.Round(float64(count) / float64(total) * 100))
		}
		stats = append(stats, gin.H{
			"option_id":  o.ID,
			"text":       o.Text,
			"count":      count,
			"percentage": percentage,
		})
	}
	return stats, total
}

// EndQuestion closes the current question, reveals correct answer, and broadcasts leaderboard
func EndQuestion(c *gin.Context) {
	userID, _ := c.Get("user_id")
	roomIDStr := c.Param("id")
	roomID, _ := strconv.ParseUint(roomIDStr, 10, 32)

	var room model.Room
	if err := db.DB.Where("id = ? AND host_id = ?", uint(roomID), userID.(uint)).First(&room).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Room not found"})
		return
	}
	if room.Status != "active" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Room is not active"})
		return
	}
	if roomGameMode(room.ThemeConfig) == "player_paced" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Host question controls are not available in solo mode"})
		return
	}

	if room.CurrentQuestionID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No active question"})
		return
	}

	var question model.Question
	db.DB.First(&question, *room.CurrentQuestionID)

	type PlayerScore struct {
		ID       uint   `json:"id"`
		Nickname string `json:"nickname"`
		Score    int    `json:"score"`
	}
	var leaderboard []PlayerScore
	db.DB.Model(&model.Player{}).
		Select("id, nickname, score").
		Where("room_id = ?", room.ID).
		Order("score DESC").
		Limit(5).
		Scan(&leaderboard)

	endedPayload := gin.H{
		"question_id": question.ID,
		"type":        question.Type,
		"leaderboard": leaderboard,
	}
	if question.Type != "poll" {
		endedPayload["correct_answer"] = question.CorrectAnswer
	} else {
		optionStats, totalVotes := pollOptionStats(question.ID, question.Options)
		endedPayload["option_stats"] = optionStats
		endedPayload["total_votes"] = totalVotes
	}
	realtime.Client.Publish(fmt.Sprintf("rooms:%s", room.PinCode), "question:ended", endedPayload)

	room.QuestionActiveUntil = nil
	db.DB.Save(&room)

	resp := gin.H{
		"message":     "Question ended",
		"type":        question.Type,
		"leaderboard": leaderboard,
	}
	if question.Type != "poll" {
		resp["correct_answer"] = question.CorrectAnswer
	} else {
		resp["option_stats"] = endedPayload["option_stats"]
		resp["total_votes"] = endedPayload["total_votes"]
	}
	c.JSON(http.StatusOK, resp)
}

// EndGame finishes the room session
func EndGame(c *gin.Context) {
	userID, _ := c.Get("user_id")
	roomIDStr := c.Param("id")
	roomID, _ := strconv.ParseUint(roomIDStr, 10, 32)

	var room model.Room
	if err := db.DB.Where("id = ? AND host_id = ?", uint(roomID), userID.(uint)).First(&room).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Room not found"})
		return
	}

	room.Status = "finished"
	room.CurrentQuestionID = nil
	db.DB.Save(&room)

	// Record Audit Log
	audit.Record(userID.(uint), "end_game", fmt.Sprintf("room_%d", room.ID), c.ClientIP())

	// Collect final rankings BEFORE cleanup
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

	// Archive to game_sessions (permanent record)
	archiveGameLogs(room, rankings)

	// Broadcast result to all connected clients
	realtime.Client.Publish(fmt.Sprintf("rooms:%s", room.PinCode), "game:ended", gin.H{
		"room_id":  room.ID,
		"rankings": rankings,
	})

	// Cleanup: delete transient player records and answer logs for this room.
	// Rankings are safely persisted in game_sessions, so this data is no longer needed.
	// Running in a goroutine to not block the response.
	go func(roomID uint) {
		if err := db.DB.Where("room_id = ?", roomID).Delete(&model.Player{}).Error; err != nil {
			fmt.Printf("[CLEANUP] Warning: failed to delete players for room %d: %v\n", roomID, err)
		}
		if err := db.DB.Where("room_id = ?", roomID).Delete(&model.AnswerLog{}).Error; err != nil {
			fmt.Printf("[CLEANUP] Warning: failed to delete answer logs for room %d: %v\n", roomID, err)
		}
		fmt.Printf("[CLEANUP] Removed players and answer logs for finished room %d\n", roomID)
	}(room.ID)

	c.JSON(http.StatusOK, gin.H{
		"message":  "Game ended and room closed",
		"rankings": rankings,
	})
}

// archiveGameLogs records the final state of a completed game.
// [C-4 FIX] Now writes to the dedicated game_sessions table instead of
// polluting answer_logs with synthetic "GAME_ENDED" records that corrupted analytics.
func archiveGameLogs(room model.Room, rankings interface{}) {
	rankingsJSON, _ := json.Marshal(rankings)

	// Count actual players in the room
	var playerCount int64
	db.DB.Model(&model.Player{}).Where("room_id = ?", room.ID).Count(&playerCount)

	session := model.GameSession{
		RoomID:      room.ID,
		HostID:      room.HostID,
		QuizID:      room.QuizID,
		Rankings:    string(rankingsJSON),
		PlayerCount: int(playerCount),
		EndedAt:     time.Now(),
	}

	if err := db.DB.Create(&session).Error; err != nil {
		fmt.Printf("[AUDIT LOG] Warning: failed to archive game session for room %d: %v\n", room.ID, err)
	}

	fmt.Printf("[AUDIT LOG] Game Room %d Ended. Rankings archived to game_sessions: %s\n", room.ID, string(rankingsJSON))
}

// UpdateQuiz updates the quiz details and replaces its questions
func UpdateQuiz(c *gin.Context) {
	userID, _ := c.Get("user_id")
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid quiz ID"})
		return
	}

	var req CreateQuizReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var quiz model.Quiz
	if err := db.DB.Where("id = ? AND host_id = ?", uint(id), userID.(uint)).First(&quiz).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Quiz not found"})
		return
	}

	ents, err := license.GetEntitlements(userID.(uint))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to resolve license"})
		return
	}
	if license.EnforcementEnabled && !license.IsUnlimited(ents.MaxQuestionsPerQuiz) && len(req.Questions) > ents.MaxQuestionsPerQuiz {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   fmt.Sprintf("Too many questions (max %d on %s plan)", ents.MaxQuestionsPerQuiz, ents.PlanName),
			"plan_id": ents.PlanID,
		})
		return
	}

	tx := db.DB.Begin()

	// Update quiz fields
	quiz.Title = req.Title
	quiz.Description = req.Description
	quiz.ThemeConfig = req.ThemeConfig
	if err := tx.Save(&quiz).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update quiz"})
		return
	}

	// Sync questions:
	// 1. Get existing questions for this quiz
	var existingQuestions []model.Question
	if err := tx.Where("quiz_id = ?", quiz.ID).Find(&existingQuestions).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch existing questions"})
		return
	}

	existingMap := make(map[uint]*model.Question)
	for i := range existingQuestions {
		existingMap[existingQuestions[i].ID] = &existingQuestions[i]
	}

	// Track which question IDs are kept
	keptIDs := make(map[uint]bool)

	// Update or Create questions
	for idx, q := range req.Questions {
		optsJSON, err := json.Marshal(q.Options)
		if err != nil {
			tx.Rollback()
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid options format for question %d", idx)})
			return
		}

		if q.ID > 0 && existingMap[q.ID] != nil {
			// Update existing question
			question := existingMap[q.ID]
			question.Content = q.Content
			question.Type = q.Type
			question.Options = string(optsJSON)
			question.CorrectAnswer = q.CorrectAnswer
			question.Duration = q.Duration
			question.Points = q.Points
			question.Order = q.Order

			if err := tx.Save(question).Error; err != nil {
				tx.Rollback()
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update question"})
				return
			}
			keptIDs[q.ID] = true
		} else {
			// Create new question
			question := model.Question{
				QuizID:        quiz.ID,
				Content:       q.Content,
				Type:          q.Type,
				Options:       string(optsJSON),
				CorrectAnswer: q.CorrectAnswer,
				Duration:      q.Duration,
				Points:        q.Points,
				Order:         q.Order,
			}

			if err := tx.Create(&question).Error; err != nil {
				tx.Rollback()
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create question"})
				return
			}
		}
	}

	// Delete questions that were not kept
	for _, eq := range existingQuestions {
		if !keptIDs[eq.ID] {
			if err := tx.Delete(&eq).Error; err != nil {
				tx.Rollback()
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete old question"})
				return
			}
		}
	}

	tx.Commit()

	// Record Audit Log
	audit.Record(userID.(uint), "update_quiz", fmt.Sprintf("quiz_%d", quiz.ID), c.ClientIP())

	// Return full updated quiz preloaded with new questions
	db.DB.Preload("Questions").First(&quiz, quiz.ID)
	c.JSON(http.StatusOK, quiz)
}

// DeleteQuiz deletes a quiz and cascade deletes its questions
func DeleteQuiz(c *gin.Context) {
	userID, _ := c.Get("user_id")
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid quiz ID"})
		return
	}

	var quiz model.Quiz
	if err := db.DB.Where("id = ? AND host_id = ?", uint(id), userID.(uint)).First(&quiz).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Quiz not found"})
		return
	}

	// GORM cascade delete via Cascade constraint defined on Questions
	if err := db.DB.Delete(&quiz).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete quiz"})
		return
	}

	// Record Audit Log
	audit.Record(userID.(uint), "delete_quiz", fmt.Sprintf("quiz_%d", id), c.ClientIP())

	c.JSON(http.StatusOK, gin.H{"message": "Quiz deleted successfully"})
}

// GetRoom retrieves room info.
// [H-3 FIX] Access control:
//   - Host (Bearer JWT): full room info, only if they own the room.
//   - Player (X-Player-Token): limited info (theme, status, players), no quiz/host details.
//   - No token: 401 Unauthorized.
func GetRoom(c *gin.Context) {
	roomIDStr := c.Param("id")
	roomID, err := strconv.ParseUint(roomIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid room ID"})
		return
	}

	// Determine caller identity
	authHeader := c.GetHeader("Authorization")
	playerTokenStr := c.GetHeader("X-Player-Token")

	// Prefer player token when present so play-page calls (often same browser as host)
	// are not mis-routed to the host response shape (missing question_count).
	if playerTokenStr != "" {
		// --- Player path: requires valid player JWT, returns limited info ---
		pClaims, err := jwt.VerifyPlayerToken(playerTokenStr)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired player token"})
			return
		}
		// [H-3 FIX] Player can only query the room they joined
		if pClaims.RoomID != uint(roomID) {
			c.JSON(http.StatusForbidden, gin.H{"error": "Token room mismatch"})
			return
		}

		var room model.Room
		if err := db.DB.First(&room, uint(roomID)).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Room not found"})
			return
		}

		players := getRoomPlayers(room.ID, room.Status)

		var currentPlayer model.Player
		var activeQuestionInfo interface{}
		db.DB.Where("id = ?", pClaims.PlayerID).First(&currentPlayer)

		var config struct {
			GameMode string `json:"game_mode"`
		}
		json.Unmarshal([]byte(room.ThemeConfig), &config)
		isPlayerPaced := config.GameMode == "player_paced"

		if isPlayerPaced && currentPlayer.CurrentQuestionID != nil {
			var question model.Question
			if db.DB.First(&question, *currentPlayer.CurrentQuestionID).Error == nil {
				// Start per-player timer on first view of the question
				if currentPlayer.QuestionActiveUntil == nil {
					until := time.Now().Add(time.Duration(question.Duration) * time.Second)
					currentPlayer.QuestionActiveUntil = &until
					db.DB.Model(&currentPlayer).Update("question_active_until", until)
				}
				qIndex := 0
				var allQs []model.Question
				if db.DB.Where("quiz_id = ?", room.QuizID).Order("questions.order ASC, questions.id ASC").Find(&allQs).Error == nil {
					for i, q := range allQs {
						if q.ID == question.ID {
							qIndex = i
							break
						}
					}
				}
				activeInfo := gin.H{
					"id":       question.ID,
					"content":  question.Content,
					"type":     question.Type,
					"options":  sanitizePlayerOptions(question.Options),
					"duration": question.Duration,
					"index":    qIndex,
					"total":    len(allQs),
				}
				if currentPlayer.QuestionActiveUntil != nil {
					activeInfo["active_until"] = currentPlayer.QuestionActiveUntil.Format(time.RFC3339)
				}
				activeQuestionInfo = activeInfo
			}
		}

		// Return limited info: no quiz details, no host_id, no pin_code
		var qCount int64
		db.DB.Model(&model.Question{}).Where("quiz_id = ?", room.QuizID).Count(&qCount)
		entsPlayer, _ := license.GetEntitlements(room.HostID)

		c.JSON(http.StatusOK, gin.H{
			"id":                     room.ID,
			"status":                 room.Status,
			"theme_config":           room.ThemeConfig,
			"players":                players,
			"current_question":       activeQuestionInfo,
			"question_count":         qCount,
			"current_question_index": room.CurrentQuestionIndex,
			"max_players":            entsPlayer.MaxPlayersPerRoom,
			"player_count":           len(players),
			"plan_name":              entsPlayer.PlanName,
		})
		return
	}

	if authHeader != "" {
		// --- Host path: requires valid Bearer JWT and room ownership ---
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || parts[0] != "Bearer" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid Authorization header"})
			return
		}
		hostClaims, err := jwt.VerifyToken(parts[1])
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired token"})
			return
		}

		var room model.Room
		if err := db.DB.Preload("Quiz.Questions", func(db *gorm.DB) *gorm.DB {
			return db.Order("questions.order ASC, questions.id ASC")
		}).First(&room, uint(roomID)).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Room not found"})
			return
		}
		// [H-3 FIX] Only the room's own host can see full details
		if room.HostID != hostClaims.UserID {
			c.JSON(http.StatusForbidden, gin.H{"error": "You do not own this room"})
			return
		}

		players := getRoomPlayers(room.ID, room.Status)
		ents, _ := license.GetEntitlements(room.HostID)
		qCount := 0
		if room.Quiz.ID != 0 {
			qCount = len(room.Quiz.Questions)
		}
		c.JSON(http.StatusOK, gin.H{
			"room":           room,
			"players":        players,
			"max_players":    ents.MaxPlayersPerRoom,
			"player_count":   len(players),
			"plan_id":        ents.PlanID,
			"plan_name":      ents.PlanName,
			"question_count": qCount,
		})
		return
	}

	c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required: provide Authorization or X-Player-Token header"})
}

// GetPlayerQuestion returns a question by index without the correct answer.
// Requires X-Player-Token bound to this room.
func GetPlayerQuestion(c *gin.Context) {
	roomIDStr := c.Param("id")
	roomID, err := strconv.ParseUint(roomIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid room ID"})
		return
	}
	indexStr := c.Param("index")
	index, err := strconv.Atoi(indexStr)
	if err != nil || index < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid question index"})
		return
	}

	playerTokenStr := c.GetHeader("X-Player-Token")
	if playerTokenStr == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "X-Player-Token header is required"})
		return
	}
	pClaims, err := jwt.VerifyPlayerToken(playerTokenStr)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired player token"})
		return
	}
	if pClaims.RoomID != uint(roomID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Token room mismatch"})
		return
	}

	var room model.Room
	if err := db.DB.First(&room, uint(roomID)).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Room not found"})
		return
	}
	if room.Status == "finished" {
		c.JSON(http.StatusGone, gin.H{"error": "ROOM_FINISHED"})
		return
	}

	var questions []model.Question
	if err := db.DB.Where("quiz_id = ?", room.QuizID).Order("questions.order ASC, questions.id ASC").Find(&questions).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load questions"})
		return
	}
	if index >= len(questions) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Question not found"})
		return
	}

	q := questions[index]

	// Player-paced: start the timer when the player loads their current question
	isPlayerPaced := roomGameMode(room.ThemeConfig) == "player_paced"
	if isPlayerPaced {
		var pl model.Player
		if err := db.DB.First(&pl, pClaims.PlayerID).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Player not found"})
			return
		}
		if pl.CurrentQuestionID == nil {
			c.JSON(http.StatusGone, gin.H{"error": "NO_MORE_QUESTIONS"})
			return
		}
		if *pl.CurrentQuestionID != q.ID {
			c.JSON(http.StatusForbidden, gin.H{"error": "Question is not currently active"})
			return
		}
		if pl.QuestionActiveUntil == nil {
			until := time.Now().Add(time.Duration(q.Duration) * time.Second)
			db.DB.Model(&pl).Update("question_active_until", until)
		}
	} else {
		if room.Status != "active" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Room is not active"})
			return
		}
		if room.CurrentQuestionIndex < 0 || index != room.CurrentQuestionIndex {
			c.JSON(http.StatusForbidden, gin.H{"error": "Question is not currently active"})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"id":       q.ID,
		"quiz_id":  q.QuizID,
		"content":  q.Content,
		"type":     q.Type,
		"options":  sanitizePlayerOptions(q.Options),
		"duration": q.Duration,
		"order":    q.Order,
		"index":    index,
		"total":    len(questions),
	})
}

// LeaveRoom removes a player from a room when they disconnect.
// [H-4 FIX] Previously accepted player_id from an unauthenticated query param,
// allowing anyone to kick any player. Now requires a valid X-Player-Token so
// only the player themselves can remove themselves from the room.
func LeaveRoom(c *gin.Context) {
	roomIDStr := c.Param("id")
	roomID, err := strconv.ParseUint(roomIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid room ID"})
		return
	}

	// [H-4 FIX] Verify player identity via signed JWT — don't trust query param
	playerTokenStr := c.GetHeader("X-Player-Token")
	if playerTokenStr == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "X-Player-Token header is required"})
		return
	}
	pClaims, err := jwt.VerifyPlayerToken(playerTokenStr)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired player token"})
		return
	}
	// [H-4 FIX] Ensure token's room matches the URL room param
	if pClaims.RoomID != uint(roomID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Token room mismatch"})
		return
	}

	// Delete only the player identified by the verified JWT claims
	if err := db.DB.Where("id = ? AND room_id = ?", pClaims.PlayerID, uint(roomID)).Delete(&model.Player{}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove player"})
		return
	}

	// Fetch room to get PIN code for publishing leave event
	var room model.Room
	if err := db.DB.First(&room, uint(roomID)).Error; err == nil {
		realtime.Client.Publish(fmt.Sprintf("rooms:%s", room.PinCode), "player:left", gin.H{
			"player_id": pClaims.PlayerID,
		})
	}

	c.JSON(http.StatusOK, gin.H{"message": "Left room successfully"})
}

// GetRoomByPin retrieves room info by its PIN code
func GetRoomByPin(c *gin.Context) {
	pin := c.Param("pin")
	if len(pin) != 6 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid PIN format"})
		return
	}
	var room model.Room
	if err := db.DB.Where("pin_code = ?", pin).First(&room).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Room not found or invalid PIN"})
		return
	}

	ents, _ := license.GetEntitlements(room.HostID)
	var playerCount int64
	db.DB.Model(&model.Player{}).Where("room_id = ?", room.ID).Count(&playerCount)

	c.JSON(http.StatusOK, gin.H{
		"id":           room.ID,
		"status":       room.Status,
		"is_private":   room.IsPrivate,
		"theme_config": room.ThemeConfig,
		"max_players":  ents.MaxPlayersPerRoom,
		"player_count": playerCount,
		"plan_id":      ents.PlanID,
		"plan_name":    ents.PlanName,
	})
}

func getRoomPlayers(roomID uint, status string) []model.Player {
	var players []model.Player
	if status == "finished" {
		var session model.GameSession
		if db.DB.Where("room_id = ?", roomID).First(&session).Error == nil {
			type Ranking struct {
				Nickname       string `json:"nickname"`
				Score          int    `json:"score"`
				CorrectAnswers int    `json:"correct_answers"`
			}
			var rankings []Ranking
			if json.Unmarshal([]byte(session.Rankings), &rankings) == nil {
				players = make([]model.Player, len(rankings))
				for i, r := range rankings {
					players[i] = model.Player{
						ID:             uint(i + 1),
						RoomID:         roomID,
						Nickname:       r.Nickname,
						Score:          r.Score,
						CorrectAnswers: r.CorrectAnswers,
					}
				}
			}
		}
	} else {
		db.DB.Where("room_id = ?", roomID).Find(&players)
		
		type PlayerCorrectCount struct {
			PlayerID uint
			Count    int
		}
		var counts []PlayerCorrectCount
		db.DB.Model(&model.AnswerLog{}).
			Select("player_id, count(*) as count").
			Where("room_id = ? AND is_correct = ?", roomID, true).
			Group("player_id").
			Scan(&counts)

		countMap := make(map[uint]int)
		for _, c := range counts {
			countMap[c.PlayerID] = c.Count
		}
		for i := range players {
			players[i].CorrectAnswers = countMap[players[i].ID]
		}
	}
	return players
}
