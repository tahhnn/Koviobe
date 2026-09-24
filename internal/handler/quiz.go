package handler

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/big"
	"net/http"
	"regexp"
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
	"github.com/quizzzone/backend/internal/pkg/notify"
	"github.com/quizzzone/backend/internal/pkg/theme"
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

// UpdateQuizReq is the body of PUT /quizzes/:id.
//
// It carries no sharing flags: is_public and allow_edit belong to the owner and
// are only writable through PATCH /quizzes/:id/sharing, or anyone holding edit
// rights on a public quiz could change who else may edit it.
type UpdateQuizReq struct {
	CreateQuizReq
	// ExpectedUpdatedAt is quizzes.updated_at as the editor loaded it. When
	// present the write only lands if the row has not moved since, which is
	// what stops two people editing a public quiz from overwriting each other.
	// Optional so an older client keeps working — it just keeps the old
	// last-write-wins behaviour.
	ExpectedUpdatedAt *time.Time `json:"expected_updated_at"`
}

// vetThemeConfig sanitizes a submitted theme and applies the plan gates,
// returning the JSON to store.
//
// Both CreateQuiz and UpdateQuiz call it. They used to differ: create checked
// the plan, update assigned req.ThemeConfig straight onto the row, so a host
// blocked from branding on POST got it by sending the same body as a PUT. One
// function is the only way those two stay in step.
//
// On a rejection it writes the response and returns false; the caller returns
// without touching the database.
// enforcing is passed in rather than read from license.Enforcing() inside, so
// the gates can be tested without reaching for package state a test in another
// package cannot set.
func vetThemeConfig(c *gin.Context, ents license.Entitlements, enforcing bool, raw string) (string, bool) {
	// Sanitize first. The gates below ask what the theme sets, and they should
	// be asking about the values that would actually be stored — an off-site
	// bg_host_url is dropped here, so it must not then trip the branding gate
	// and tell the host to upgrade for something they are not getting.
	clean, err := theme.Sanitize(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid theme_config"})
		return "", false
	}

	if !enforcing {
		return clean, true
	}

	if theme.RequestsPlayerPaced(clean) && !ents.AllowPlayerPaced {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   "Player-paced mode requires Pro plan",
			"plan_id": ents.PlanID,
			"feature": "allow_player_paced",
		})
		return "", false
	}

	if keys := theme.BrandingKeys(clean); len(keys) > 0 && !ents.AllowCustomBranding {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   "Custom branding requires Pro plan",
			"plan_id": ents.PlanID,
			"feature": "allow_custom_branding",
			"fields":  keys,
		})
		return "", false
	}

	if theme.WantsRemoveWatermark(clean) && !ents.AllowRemoveWatermark {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   "Removing watermark requires Pro plan",
			"plan_id": ents.PlanID,
			"feature": "allow_remove_watermark",
		})
		return "", false
	}

	return clean, true
}

type QuestionReq struct {
	ID      uint   `json:"id"`
	Content string `json:"content" binding:"required"`
	// [M-5 FIX] Restrict type to known values; duration 5-120s; points 0-2000
	Type          string      `json:"type" binding:"required,oneof=multiple_choice true_false short_answer pin_answer poll"`
	Options       interface{} `json:"options"`
	CorrectAnswer string      `json:"correct_answer" binding:"required"`
	Duration      int         `json:"duration" binding:"min=5,max=120"`
	Points        int         `json:"points" binding:"min=0,max=2000"`
	Order         int         `json:"order"`
	// Explanation is a pointer on purpose: nil means "leave whatever is in the
	// DB alone". Every editor mutation in the frontend is a read-modify-write
	// of the whole quiz (26 separate payload builders in app/actions/quizzes.ts),
	// so a plain string would let any one of them that forgets the field wipe a
	// host's slide as a side effect of renaming an option.
	Explanation *string `json:"explanation"`
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
	SelectedOption string `json:"selected_option"`  // Empty allowed on timeout (0 points)
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
	if license.Enforcing() && !license.IsUnlimited(ents.MaxQuizzes) && int(quizCount) >= ents.MaxQuizzes {
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

	if license.Enforcing() && !license.IsUnlimited(ents.MaxQuestionsPerQuiz) && len(req.Questions) > ents.MaxQuestionsPerQuiz {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   fmt.Sprintf("Too many questions (max %d on %s plan)", ents.MaxQuestionsPerQuiz, ents.PlanName),
			"plan_id": ents.PlanID,
		})
		return
	}

	themeConfig, themeOK := vetThemeConfig(c, ents, license.Enforcing(), req.ThemeConfig)
	if !themeOK {
		return
	}

	tx := db.DB.Begin()

	quiz := model.Quiz{
		HostID:      userID.(uint),
		Title:       req.Title,
		Description: req.Description,
		ThemeConfig: themeConfig,
	}

	if err := tx.Create(&quiz).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create quiz"})
		return
	}

	for idx, q := range req.Questions {
		if err := validateCorrectAnswer(q.Type, q.CorrectAnswer, q.Options); err != nil {
			tx.Rollback()
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Question %d: %v", idx+1, err)})
			return
		}

		optsJSON, err := json.Marshal(q.Options)
		if err != nil {
			tx.Rollback()
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid options format for question %d", idx)})
			return
		}

		explanation := ""
		if q.Explanation != nil {
			explanation, err = validateExplanation(*q.Explanation)
			if err != nil {
				tx.Rollback()
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Question %d: %v", idx+1, err)})
				return
			}
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
			Explanation:   explanation,
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
		// So the owner's own list can show which of their quizzes are shared
		// without a second round trip per card.
		IsPublic  bool `json:"is_public"`
		AllowEdit bool `json:"allow_edit"`
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
			IsPublic:      q.IsPublic,
			AllowEdit:     q.AllowEdit,
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

	// Loaded without the owner filter so a shared quiz resolves; rightsOn then
	// decides. The response carries the correct answers and the explanation
	// slides, which is exactly what publishing a quiz gives away — the warning
	// belongs on the share toggle in the UI, not here.
	var quiz model.Quiz
	if err := db.DB.Preload("Questions", func(db *gorm.DB) *gorm.DB {
		return db.Order("questions.order ASC, questions.id ASC")
	}).Where("id = ?", uint(id)).First(&quiz).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Quiz not found"})
		return
	}

	rights := rightsOn(&quiz, userID.(uint))
	if !rights.View {
		denyQuizAccess(c, rights)
		return
	}

	// Always a JSON array, never null: the model tag omits an empty list, and a
	// hand-built map would send null instead.
	questions := quiz.Questions
	if questions == nil {
		questions = []model.Question{}
	}

	c.JSON(http.StatusOK, gin.H{
		"id":           quiz.ID,
		"host_id":      quiz.HostID,
		"title":        quiz.Title,
		"description":  quiz.Description,
		"theme_config": quiz.ThemeConfig,
		"is_public":    quiz.IsPublic,
		"allow_edit":   quiz.AllowEdit,
		"questions":    questions,
		"created_at":   quiz.CreatedAt,
		"updated_at":   quiz.UpdatedAt,
		// What this caller may do, so the editor does not have to re-derive the
		// rule client-side and get it wrong.
		"is_owner": rights.Owner,
		"can_edit": rights.Edit,
		"can_host": rights.Host,
		"can_copy": rights.Copy,
	})
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
	if license.Enforcing() && !license.IsUnlimited(ents.MaxConcurrentRooms) && int(concurrent) >= ents.MaxConcurrentRooms {
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

	// Verify the quiz exists and that this host may run it: their own, or one
	// its owner published. The room still records uid as HostID, so the room,
	// its players and its logs belong to whoever opened it, not to the author.
	var quiz model.Quiz
	if err := db.DB.Preload("Questions").Where("id = ?", uint(quizID)).First(&quiz).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Quiz not found"})
		return
	}
	if rights := rightsOn(&quiz, uid); !rights.Host {
		denyQuizAccess(c, rights)
		return
	}

	if len(quiz.Questions) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Quiz has no questions"})
		return
	}
	if license.Enforcing() && !license.IsUnlimited(ents.MaxQuestionsPerQuiz) && len(quiz.Questions) > ents.MaxQuestionsPerQuiz {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   fmt.Sprintf("Quiz has %d questions (max %d on %s plan). Remove some questions or upgrade.", len(quiz.Questions), ents.MaxQuestionsPerQuiz, ents.PlanName),
			"plan_id": ents.PlanID,
			"limit":   ents.MaxQuestionsPerQuiz,
			"count":   len(quiz.Questions),
		})
		return
	}

	// The mode this room will run. Taken from the request, falling back to
	// whatever the quiz was last saved with so an older client that sends
	// nothing keeps behaving exactly as before.
	gameMode := ""
	if raw := c.Query("game_mode"); raw != "" {
		gameMode = normalizeGameMode(raw)
	} else if license.ThemeRequestsPlayerPaced(quiz.ThemeConfig) {
		gameMode = "player_paced"
	} else {
		gameMode = "host_paced"
	}

	// Gated on the mode this room will actually run, not on the quiz's stored
	// one: the host picks the mode per game now, so the quiz's saved value is
	// no longer what they are asking for.
	if license.Enforcing() && gameMode == "player_paced" && !ents.AllowPlayerPaced {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   "This quiz uses player-paced mode which requires Pro plan",
			"feature": "allow_player_paced",
		})
		return
	}

	roomTheme, err := withGameMode(quiz.ThemeConfig, gameMode)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare room settings"})
		return
	}

	// [L-3 FIX] Use crypto/rand instead of deprecated rand.Seed + math/rand for PIN generation
	// The uniqueness check must match the DB constraint, which is a global unique
	// index on pin_code covering finished and soft-deleted rooms too. Excluding
	// finished rooms here let a retired PIN pass this check and then violate the
	// constraint on insert, surfacing as a random 500 whose rate grew with the
	// table. Unscoped() so soft-deleted rows are counted as the index counts them.
	var pinCode string
	pinFound := false
	for attempt := 0; attempt < 20; attempt++ {
		n, err := rand.Int(rand.Reader, big.NewInt(1000000))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate room PIN"})
			return
		}
		candidate := fmt.Sprintf("%06d", n.Int64())
		var count int64
		if err := db.DB.Unscoped().Model(&model.Room{}).Where("pin_code = ?", candidate).Count(&count).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to allocate room PIN"})
			return
		}
		if count == 0 {
			pinCode = candidate
			pinFound = true
			break
		}
	}
	if !pinFound {
		// The PIN space is 10^6 and every retired room keeps its PIN forever, so
		// this is reachable by exhaustion rather than by bad luck. Say so plainly
		// instead of returning a generic failure.
		log.Printf("[CreateRoom] PIN allocation failed after 20 attempts — PIN space may be exhausted")
		notify.P0("pin_exhausted", "Cấp PIN thất bại sau 20 lần thử — không tạo được phòng mới.")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Could not allocate a free room PIN. Please retry."})
		return
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
		ThemeConfig:          roomTheme, // Quiz's theme, with this game's mode applied
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

	ents, _ := license.GetEntitlements(room.HostID)
	maxPlayers := ents.MaxPlayersPerRoom
	if !license.Enforcing() && maxPlayers <= 0 {
		// Enforcement off: openEntitlements always returns a positive cap, so a
		// zero here means the lookup failed, not that the host is unlicensed —
		// fall back rather than making the room unjoinable.
		//
		// With enforcement ON, zero IS the answer for an unactivated host, and
		// the count >= limit gate below turns the first joiner away. Keeping the
		// old unconditional floor would have let free grant 20 players.
		maxPlayers = 20
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

	// Count and insert inside one transaction with the room row locked. Counting
	// before the insert with nothing held let N simultaneous joins all read the
	// same count and all insert, so the cap could be overshot by the size of the
	// burst — exactly the case a full room attracts.
	roomFull := false
	roomStarted := false
	// The exclusive lock is only needed to make count-then-insert atomic, and
	// the count only matters when the cap is enforced. With enforcement off it
	// serialised every join in the room to protect a branch that never fires.
	// The status re-check below still needs the room held still, but SHARE does
	// that: it blocks StartGame's update until this transaction commits while
	// letting other joiners hold the same lock at the same time.
	lockStrength := "SHARE"
	if license.Enforcing() {
		lockStrength = "UPDATE"
	}
	txErr := db.DB.Transaction(func(tx *gorm.DB) error {
		var lockedRoom model.Room
		if err := tx.Clauses(clause.Locking{Strength: lockStrength}).First(&lockedRoom, room.ID).Error; err != nil {
			return err
		}

		// The status read above this transaction is stale by the time the lock is
		// held. Without re-checking it here a join could commit *after* StartGame
		// had already stamped every player with their first question, leaving the
		// new player in a running solo room with current_question_id NULL — which
		// GetPlayerQuestion reports as NO_MORE_QUESTIONS, stranding them on the
		// lobby screen. Measured at 29% of an 80-player burst before this check.
		if lockedRoom.Status != "waiting" {
			roomStarted = true
			return fmt.Errorf("room_started")
		}

		// Only counted when the answer can change the outcome. Outside
		// enforcement this was a full count of the room on every single join.
		if license.Enforcing() {
			var playerCount int64
			if err := tx.Model(&model.Player{}).Where("room_id = ?", lockedRoom.ID).Count(&playerCount).Error; err != nil {
				return err
			}
			if int(playerCount) >= maxPlayers {
				roomFull = true
				return fmt.Errorf("room_full")
			}
		}

		return tx.Create(&player).Error
	})

	if roomStarted {
		c.JSON(http.StatusConflict, gin.H{"error": "Joining is closed, the game already started"})
		return
	}
	if roomFull {
		c.JSON(http.StatusForbidden, gin.H{
			"error":       fmt.Sprintf("Room is full (max %d players on host's %s plan)", maxPlayers, ents.PlanName),
			"max_players": maxPlayers,
			"plan_id":     ents.PlanID,
		})
		return
	}
	if txErr != nil {
		if strings.Contains(strings.ToLower(txErr.Error()), "unique") || strings.Contains(strings.ToLower(txErr.Error()), "duplicate") {
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
		log.Printf("[JoinRoom] realtime publish failed room=%d: %v", room.ID, err)
		notify.P1("join_publish_failed", "Người chơi vào phòng nhưng push realtime thất bại (room=%d): %v — danh sách chờ của host không cập nhật.", room.ID, err)
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

	// Parse ThemeConfig to check game mode
	var themeCfg struct {
		GameMode string `json:"game_mode"`
	}
	json.Unmarshal([]byte(room.ThemeConfig), &themeCfg)
	isPlayerPaced := themeCfg.GameMode == "player_paced"

	// Flip the status and deal the first question under one row lock. Done
	// separately, a join could slip between the two: JoinRoom takes this same
	// lock, so serializing here means a concurrent join either commits first
	// (and is dealt in by the UPDATE below) or finds the room already active
	// and is turned away. Previously it could do neither and land in the room
	// with no question — see the JoinRoom comment.
	notWaiting := false
	txErr := db.DB.Transaction(func(tx *gorm.DB) error {
		var locked model.Room
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, room.ID).Error; err != nil {
			return err
		}
		if locked.Status != "waiting" {
			notWaiting = true
			return fmt.Errorf("not_waiting")
		}

		locked.Status = "active"
		locked.CurrentQuestionIndex = -1
		locked.CurrentQuestionID = nil
		if err := tx.Save(&locked).Error; err != nil {
			return err
		}
		room = locked

		if isPlayerPaced {
			// Assign first question but do NOT start the timer yet — timer starts
			// when the player actually loads the question (avoids desync vs client
			// clock).
			var firstQuestion model.Question
			if err := tx.Where("quiz_id = ?", locked.QuizID).
				Order("questions.order ASC, questions.id ASC").First(&firstQuestion).Error; err == nil {
				if err := tx.Model(&model.Player{}).Where("room_id = ?", locked.ID).
					UpdateColumns(map[string]interface{}{
						"current_question_id":   firstQuestion.ID,
						"question_active_until": nil,
					}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	if notWaiting {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Room can only be started from waiting status"})
		return
	}
	if txErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start game"})
		return
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
	activeAt := time.Now()
	activeUntil := activeAt.Add(time.Duration(activeQuestion.Duration) * time.Second)

	room.CurrentQuestionIndex = nextIndex
	room.CurrentQuestionID = &activeQuestion.ID
	room.QuestionActiveUntil = &activeUntil
	db.DB.Save(&room)

	options := sanitizePlayerOptions(activeQuestion.Options)

	realtime.Client.Publish(fmt.Sprintf("rooms:%s", room.PinCode), "question:active", gin.H{
		"question_id":  activeQuestion.ID,
		"content":      activeQuestion.Content,
		"type":         activeQuestion.Type,
		"options":      options,
		"duration":     activeQuestion.Duration,
		"active_at":    activeAt.UTC().Format(time.RFC3339),
		"active_until": activeUntil.UTC().Format(time.RFC3339),
		"index":        nextIndex,
		"total":        len(quiz.Questions),
	})

	c.JSON(http.StatusOK, gin.H{
		"message":        "Question activated",
		"question_index": nextIndex,
		"duration":       activeQuestion.Duration,
		"active_at":      activeAt.UTC().Format(time.RFC3339),
		"active_until":   activeUntil.UTC().Format(time.RFC3339),
	})
}

// validateCorrectAnswer rejects a question whose correct_answer can never match a
// submission. Nothing checked this before, so a quiz could be saved with a
// multiple_choice answer key naming an option that does not exist, or a
// pin_answer key that is not coordinates — in both cases evaluateAnswer returns
// false for every player and the question is silently unscoreable for the whole
// game. Validating at write time is the only place this is cheap to catch.
func validateCorrectAnswer(qType string, correctAnswer string, options interface{}) error {
	answer := strings.TrimSpace(correctAnswer)
	if answer == "" {
		return fmt.Errorf("correct_answer is required")
	}

	switch qType {
	case "poll":
		// Polls are never scored, so any answer key is inert.
		return nil

	case "pin_answer":
		if _, _, ok := parsePinCoords(answer); !ok {
			return fmt.Errorf("correct_answer for pin_answer must be \"x,y\" percentage coordinates, got %q", correctAnswer)
		}
		return nil

	case "short_answer":
		// Free text compared case-insensitively; any non-empty key is valid.
		return nil

	case "true_false":
		switch strings.ToLower(answer) {
		case "true", "false", "a", "b":
			return nil
		}
		return fmt.Errorf("correct_answer for true_false must be true or false, got %q", correctAnswer)

	default: // multiple_choice
		ids, err := optionIDs(options)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return fmt.Errorf("multiple_choice question requires options")
		}
		for _, id := range ids {
			if strings.EqualFold(id, answer) {
				return nil
			}
		}
		return fmt.Errorf("correct_answer %q does not match any option id (have: %s)", correctAnswer, strings.Join(ids, ", "))
	}
}

// optionIDs extracts the option identifiers from a question's options payload,
// which reaches the handler as free-form JSON.
// maxExplanationBytes caps a single slide document. A slide holds a title, a
// paragraph and an image URL — anything past this is not a slide, and the JSON
// rides along on every quiz save and every realtime publish.
const maxExplanationBytes = 64 * 1024

var explanationLayouts = map[string]bool{
	"text":        true,
	"image_left":  true,
	"image_right": true,
	"image_top":   true,
	"image_full":  true,
}

var hexColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// validateExplanation checks an explanation slide document. It returns the
// canonical JSON to store, or "" when the host cleared the slide.
//
// Colors are rendered as inline style values on the client, so only #rrggbb is
// accepted — a permissive string field there is a CSS injection sink. Image
// URLs are likewise restricted to our own uploads and https.
func validateExplanation(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	if len(trimmed) > maxExplanationBytes {
		return "", fmt.Errorf("explanation is too large (max %d KB)", maxExplanationBytes/1024)
	}

	var doc struct {
		Version int     `json:"v"`
		Layout  string  `json:"layout"`
		Overlay float64 `json:"overlay"`
		Bg      struct {
			Color string `json:"color"`
		} `json:"bg"`
		Elements []struct {
			ID    string `json:"id"`
			Kind  string `json:"kind"`
			Role  string `json:"role"`
			Text  string `json:"text"`
			URL   string `json:"url"`
			Fit   string `json:"fit"`
			Style struct {
				Color string `json:"color"`
				Size  string `json:"size"`
				Align string `json:"align"`
			} `json:"style"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(trimmed), &doc); err != nil {
		return "", fmt.Errorf("invalid explanation format")
	}
	if !explanationLayouts[doc.Layout] {
		return "", fmt.Errorf("unknown explanation layout %q", doc.Layout)
	}
	if doc.Overlay < 0 || doc.Overlay > 0.8 {
		return "", fmt.Errorf("explanation overlay must be between 0 and 0.8")
	}
	if doc.Bg.Color != "" && !hexColorRe.MatchString(doc.Bg.Color) {
		return "", fmt.Errorf("explanation background color must be #rrggbb")
	}
	if len(doc.Elements) > 16 {
		return "", fmt.Errorf("explanation has too many elements")
	}
	for i, el := range doc.Elements {
		switch el.Kind {
		case "text":
			if len(el.Text) > 4000 {
				return "", fmt.Errorf("explanation element %d: text is too long", i+1)
			}
		case "image":
			if !isAllowedMediaURL(el.URL) {
				return "", fmt.Errorf("explanation element %d: image URL is not allowed", i+1)
			}
		default:
			return "", fmt.Errorf("explanation element %d: unknown kind %q", i+1, el.Kind)
		}
		if el.Style.Color != "" && !hexColorRe.MatchString(el.Style.Color) {
			return "", fmt.Errorf("explanation element %d: color must be #rrggbb", i+1)
		}
	}
	return trimmed, nil
}

// isAllowedMediaURL accepts our own upload paths and plain https URLs (hosts
// paste GIF links from the picker). Everything else — data:, javascript:,
// protocol-relative — is refused.
func isAllowedMediaURL(u string) bool {
	if u == "" {
		return false
	}
	if len(u) > 2048 {
		return false
	}
	if strings.HasPrefix(u, "/uploads/") {
		return !strings.Contains(u, "..")
	}
	return strings.HasPrefix(u, "https://")
}

func optionIDs(options interface{}) ([]string, error) {
	raw, err := json.Marshal(options)
	if err != nil {
		return nil, fmt.Errorf("invalid options format")
	}
	var parsed []map[string]interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("options must be a list of objects")
	}
	ids := make([]string, 0, len(parsed))
	for _, opt := range parsed {
		if id, ok := opt["id"].(string); ok && strings.TrimSpace(id) != "" {
			ids = append(ids, strings.TrimSpace(id))
		}
	}
	return ids, nil
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
	return theme.Parse(themeConfig).GameMode
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

	qset, err := quizQuestions(room.QuizID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load questions"})
		return
	}
	question, _, inQuiz := qset.byID(req.QuestionID)
	if !inQuiz {
		// Off the hot path: only a bad or foreign id gets here, and the DB read
		// keeps the two error messages distinct as they always were.
		if err := db.DB.First(&question, req.QuestionID).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Question not found"})
			return
		}
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
	timedOut := false
	playerFinished := false

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
			// The status read before this transaction is not a guarantee: a submit
			// that passed it can still commit after finalizeRoom claimed the room,
			// landing a score on a player row that is about to be deleted and an
			// answer log that no archive will ever mention. The classic branch
			// below is already covered — it locks the room and finalizeRoom nulls
			// current_question_id — but solo mode locks only the player, so it has
			// to read the room's status under a lock of its own.
			//
			// SHARE, not UPDATE: every player in a solo room submits against this
			// one row, and an exclusive lock would serialize a thousand of them
			// behind each other. SHARE lets them proceed together while still
			// blocking finalizeRoom's UPDATE until they have committed.
			var lockedRoom model.Room
			if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).First(&lockedRoom, room.ID).Error; err != nil {
				return err
			}
			if lockedRoom.Status != "active" {
				return fmt.Errorf("room_finished")
			}
			if lockedPlayer.CurrentQuestionID == nil || *lockedPlayer.CurrentQuestionID != req.QuestionID {
				return fmt.Errorf("question_not_active")
			}
			// A submit that arrives before the question was ever fetched has no
			// server-side start time. Lazy-starting the clock here used to make
			// questionStartedAt == now, so response_time_ms was ~0 and the answer
			// scored a full speed bonus — a client that skipped the fetch was
			// rewarded for it. Start the clock but award no speed component.
			noRecordedStart := lockedPlayer.QuestionActiveUntil == nil
			if noRecordedStart {
				until := time.Now().Add(time.Duration(question.Duration) * time.Second)
				lockedPlayer.QuestionActiveUntil = &until
			}
			// 2s grace for client/server clock skew. A late submit used to roll the
			// whole transaction back, which left CurrentQuestionID pointing at a
			// question whose deadline had already passed — every retry hit
			// time_exceeded again and the next-question fetch 403'd, so the player
			// was permanently stuck. Instead, record the answer as a timed-out miss
			// (0 points) and let the flow below advance them to the next question.
			if time.Now().After(lockedPlayer.QuestionActiveUntil.Add(2 * time.Second)) {
				timedOut = true
				isCorrect = false
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
			if noRecordedStart {
				// No trustworthy start time: award correctness only, never speed.
				ratio = 0
				responseTimeMs = int(durationMs)
			}

			if isCorrect {
				pointsEarned = int(float64(question.Points) * (0.5 + 0.5*ratio))
				lockedPlayer.Score += pointsEarned
			}

			questions := qset.list
			nextIdx := -1
			if _, at, ok := qset.byID(req.QuestionID); ok {
				nextIdx = at + 1
			}
			if nextIdx > 0 && nextIdx < len(questions) {
				nextQ := questions[nextIdx]
				// Assign next question but delay timer until player loads it
				lockedPlayer.CurrentQuestionID = &nextQ.ID
				lockedPlayer.QuestionActiveUntil = nil
			} else {
				lockedPlayer.CurrentQuestionID = nil
				lockedPlayer.QuestionActiveUntil = nil
				playerFinished = true
			}
			if err := tx.Save(&lockedPlayer).Error; err != nil {
				return err
			}
			finalScore = lockedPlayer.Score
		} else {
			// SHARE, not UPDATE: this branch only reads the room — its status,
			// its current question and its deadline — and never writes the row.
			// An exclusive lock here put every answer in the room behind the
			// same row: measured 2026-09-23 under a 2000-player burst at 24.7ms
			// average for this one statement and 97% of all database time, with
			// zero disk reads, so it was queueing rather than working. SHARE
			// still blocks NextQuestion and finalizeRoom from moving the room
			// out from under a submission in flight, which is the whole reason
			// the lock is here, but submissions no longer block each other.
			var lockedRoom model.Room
			if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).First(&lockedRoom, room.ID).Error; err != nil {
				return err
			}
			// finalizeRoom nulls current_question_id in the same update that flips
			// the status, so the check below already catches an ended room. Say it
			// outright anyway: the player gets ROOM_FINISHED and goes to the
			// results screen, instead of "the question is not active" for a game
			// that no longer exists.
			if lockedRoom.Status != "active" {
				return fmt.Errorf("room_finished")
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
		case "room_finished":
			// Same sentinel and status the question fetch uses for a closed room,
			// so the client takes the path it already has: straight to results.
			c.JSON(http.StatusGone, gin.H{"error": "ROOM_FINISHED"})
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
		// Solo mode: the host scoreboard is the whole screen, so it applies this
		// payload directly instead of waiting for its next poll. "finished" is the
		// only way it can tell a player who stopped from one still thinking.
		"finished": playerFinished,
	}
	if question.Type != "poll" {
		answeredPayload["is_correct"] = isCorrect
	}
	// Only the host consumes player:answered (app/host/[sessionId]/page.tsx).
	// Publishing it on the room channel fanned every answer out to every player
	// in the room: one question in a 1000-player room produced ~1,000,000 frames,
	// 999,000 of which were parsed and discarded by clients that ignore the event.
	_ = realtime.Client.Publish(realtime.HostChannel(room.PinCode), "player:answered", answeredPayload)

	// Solo auto-finish: when the last unfinished player completes their final
	// question the host used to have to notice and click End manually — until
	// then game:ended never fired and rankings were never archived. Finalize
	// here; finalizeRoom's atomic status claim makes concurrent last-submits
	// (or a simultaneous host EndGame click) safe.
	if isPlayerPaced && playerFinished {
		var unfinished int64
		db.DB.Model(&model.Player{}).
			Where("room_id = ? AND current_question_id IS NOT NULL", room.ID).
			Count(&unfinished)
		if unfinished == 0 {
			go func(roomID, hostID uint) {
				if _, alreadyEnded, err := finalizeRoom(roomID, EndReasonHost); err != nil {
					log.Printf("[SubmitAnswer] solo auto-finish failed for room %d: %v", roomID, err)
					notify.P1("solo_autofinish_failed", "Tự kết thúc phòng solo thất bại (room=%d): %v — người chơi kẹt ở câu cuối.", roomID, err)
				} else if !alreadyEnded {
					audit.Record(hostID, "auto_end_game", fmt.Sprintf("room_%d", roomID), "system")
				}
			}(room.ID, room.HostID)
		}
	}

	resp := gin.H{
		"is_correct":    isCorrect,
		"points_earned": pointsEarned,
		"score":         finalScore,
		"question_type": question.Type,
	}
	if timedOut {
		resp["timed_out"] = true
	}
	if question.Type == "poll" {
		resp["is_correct"] = false
		resp["recorded"] = true
	}
	// Solo mode has no host to press "reveal", so a player used to learn only
	// whether they were right, never what the right answer was. A slide is the
	// host opting that question into a teaching moment: when one exists, the
	// answer comes back with the submission (right or wrong) so the client can
	// highlight it before showing the slide. Questions without a slide keep the
	// old response shape exactly.
	//
	// Solo only. In host-paced mode everyone answers the same question at once,
	// so handing the answer to whoever submits first would let them leak it to
	// the room before the host reveals.
	if isPlayerPaced && question.Explanation != "" && question.Type != "poll" {
		resp["correct_answer"] = question.CorrectAnswer
		resp["explanation"] = question.Explanation
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
		// The host screen shows its "Explain" button only when there is
		// something to show; the payload carries the flag, not the slide, so
		// the document travels once on the explain step instead of on reveal.
		"has_explanation": question.Explanation != "",
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
		"message":         "Question ended",
		"type":            question.Type,
		"leaderboard":     leaderboard,
		"has_explanation": question.Explanation != "",
	}
	if question.Type != "poll" {
		resp["correct_answer"] = question.CorrectAnswer
	} else {
		resp["option_stats"] = endedPayload["option_stats"]
		resp["total_votes"] = endedPayload["total_votes"]
	}
	c.JSON(http.StatusOK, resp)
}

// ExplainQuestion pushes the current question's explanation slide to every
// screen in the room. Host-paced only: it sits between the reveal step and the
// next question, and the host decides how long it stays up.
//
// The slide is published here rather than inside question:ended so that a
// question without a slide costs nothing, and so the host can hold the answer
// screen for as long as they want before moving on.
func ExplainQuestion(c *gin.Context) {
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
	if err := db.DB.First(&question, *room.CurrentQuestionID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Question not found"})
		return
	}
	if question.Explanation == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Question has no explanation slide"})
		return
	}

	payload := gin.H{
		"question_id": question.ID,
		"explanation": question.Explanation,
	}
	realtime.Client.Publish(fmt.Sprintf("rooms:%s", room.PinCode), "question:explain", payload)

	c.JSON(http.StatusOK, payload)
}

// leaderboardTopN is how long the between-questions leaderboard slide is. Five
// is what question:ended already broadcasts, and as many rows as a phone sheet
// shows without scrolling.
const leaderboardTopN = 5

// StandingRow is one line of that slide.
type StandingRow struct {
	ID       uint   `json:"id"`
	Nickname string `json:"nickname"`
	Score    int    `json:"score"`
	Rank     int    `json:"rank"`
}

// roomStandings reads the top rows of a room plus the caller's own row.
//
// The caller's row is returned separately rather than left for the client to
// pick out of the list: most players are not in the top five, and "47th, 820
// points" is the only line of the slide that is about them.
//
// Ordering is score DESC, id ASC everywhere, the rank arithmetic included, so
// two players on the same score are never both shown as third.
func roomStandings(room *model.Room, callerNickname string) ([]StandingRow, *StandingRow, int) {
	if room.Status == "finished" {
		players := getRoomPlayers(room.ID, room.Status)
		sort.SliceStable(players, func(i, j int) bool {
			if players[i].Score != players[j].Score {
				return players[i].Score > players[j].Score
			}
			return players[i].ID < players[j].ID
		})
		top := make([]StandingRow, 0, leaderboardTopN)
		var me *StandingRow
		for i, p := range players {
			row := StandingRow{ID: p.ID, Nickname: p.Nickname, Score: p.Score, Rank: i + 1}
			if i < leaderboardTopN {
				top = append(top, row)
			}
			if me == nil && callerNickname != "" && p.Nickname == callerNickname {
				self := row
				me = &self
			}
		}
		return top, me, len(players)
	}

	// Live room: everyone else's rows come from a snapshot shared by the room
	// for a second (standings_cache.go). A failed read degrades to an empty
	// board, as the per-request queries it replaces did.
	rows, _ := roomSnapshot(room.ID)
	top := make([]StandingRow, 0, leaderboardTopN)
	for i := 0; i < len(rows) && i < leaderboardTopN; i++ {
		top = append(top, rows[i])
	}

	// The caller's own row is read live, never from the snapshot: in solo mode
	// a player opens the board right after their own answer lands, and showing
	// them the score from before it would read as a lost answer. One indexed
	// lookup; the rank is placed against the snapshot rather than counted.
	var me *StandingRow
	if callerNickname != "" {
		var self model.Player
		if err := db.DB.Select("id, nickname, score").
			Where("room_id = ? AND nickname = ?", room.ID, callerNickname).First(&self).Error; err == nil {
			me = &StandingRow{ID: self.ID, Nickname: self.Nickname, Score: self.Score,
				Rank: rankAgainst(rows, self.Score, self.ID)}
			// And the same score on their own line of the top list, so the slide
			// never shows one player two different scores. top is this request's
			// copy, not the shared snapshot.
			for i := range top {
				if top[i].ID == self.ID {
					top[i].Score = self.Score
				}
			}
		}
	}
	return top, me, len(rows)
}

// roomViewerAuth identifies the caller of a read-only room endpoint: a player
// holding an X-Player-Token bound to the room, or the host holding a Bearer
// JWT. It writes the failure response itself and reports ok=false.
//
// The player token is preferred for the same reason GetRoom prefers it: a host
// often has a play tab open in the same browser.
func roomViewerAuth(c *gin.Context, roomID uint) (nickname string, hostID uint, ok bool) {
	playerTokenStr := c.GetHeader("X-Player-Token")
	authHeader := c.GetHeader("Authorization")

	switch {
	case playerTokenStr != "":
		pClaims, err := jwt.VerifyPlayerToken(playerTokenStr)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired player token"})
			return "", 0, false
		}
		if pClaims.RoomID != roomID {
			c.JSON(http.StatusForbidden, gin.H{"error": "Token room mismatch"})
			return "", 0, false
		}
		return pClaims.Nickname, 0, true
	case authHeader != "":
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || parts[0] != "Bearer" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid Authorization header"})
			return "", 0, false
		}
		hostClaims, err := jwt.VerifyToken(parts[1])
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired token"})
			return "", 0, false
		}
		return "", hostClaims.UserID, true
	default:
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required: provide Authorization or X-Player-Token header"})
		return "", 0, false
	}
}

// GetRoomStandings serves the leaderboard slide that follows a question: the
// top rows, the caller's own rank, and how many players are in the room.
//
// Deliberately not GetRoomResults, which returns the whole roster. That is read
// once, at the end of a game; this is read by every player after every
// question, so it stays O(top N) however large the room is.
func GetRoomStandings(c *gin.Context) {
	roomID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid room ID"})
		return
	}

	// Authenticate before touching the DB, so walking room ids cannot tell
	// "Room not found" from "Unauthorized".
	callerNickname, hostID, ok := roomViewerAuth(c, uint(roomID))
	if !ok {
		return
	}

	var room model.Room
	if err := db.DB.First(&room, uint(roomID)).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Room not found"})
		return
	}
	if hostID != 0 && room.HostID != hostID {
		c.JSON(http.StatusForbidden, gin.H{"error": "You do not own this room"})
		return
	}

	top, me, total := roomStandings(&room, callerNickname)
	resp := gin.H{
		"room_id": room.ID,
		"top":     top,
		"total":   total,
	}
	if me != nil {
		resp["me"] = me
	}
	c.JSON(http.StatusOK, resp)
}

// ShowLeaderboard pushes the leaderboard slide onto every screen in the room.
// Host-paced only, and the last beat of a question: the answer is revealed, the
// explanation slide follows if the question has one, then the scoreboard.
//
// The push carries no scores. Each screen reads its own standings, because the
// line that matters to a player is their own rank — broadcasting 2000 rows to
// 2000 clients so each can find one of them is not a payload, it is a flood.
func ShowLeaderboard(c *gin.Context) {
	userID, _ := c.Get("user_id")
	roomID, _ := strconv.ParseUint(c.Param("id"), 10, 32)

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

	payload := gin.H{}
	if room.CurrentQuestionID != nil {
		payload["question_id"] = *room.CurrentQuestionID
	}
	realtime.Client.Publish(fmt.Sprintf("rooms:%s", room.PinCode), "question:leaderboard", payload)

	c.JSON(http.StatusOK, payload)
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

	rankings, alreadyEnded, endErr := finalizeRoom(room.ID, EndReasonHost)
	if endErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to end game"})
		return
	}
	if alreadyEnded {
		// 200 because ending an already-ended game is not a client error and the
		// desired end state holds. But a bare 200 is indistinguishable from having
		// done the work, so say so in a machine-readable field: a client must not
		// have to substring-match prose to learn the room was closed by someone
		// else. The rankings are the archived ones, and finalizeRoom has just
		// replayed game:ended for anyone who missed it.
		c.JSON(http.StatusOK, gin.H{
			"message":       "Game already ended",
			"rankings":      rankings,
			"already_ended": true,
		})
		return
	}

	// Record Audit Log
	audit.Record(userID.(uint), "end_game", fmt.Sprintf("room_%d", room.ID), c.ClientIP())

	c.JSON(http.StatusOK, gin.H{
		"message":       "Game ended and room closed",
		"rankings":      rankings,
		"already_ended": false,
	})
}

type finalRanking struct {
	Nickname       string `json:"nickname"`
	Score          int    `json:"score"`
	CorrectAnswers int    `json:"correct_answers"`
}

// finalizeRoom transitions a room to finished, archives rankings, broadcasts
// game:ended and schedules cleanup. Shared by host-triggered EndGame and the
// solo-mode auto-finish (all players done). Safe under concurrent callers:
// a second finalize would collect an empty ranking set (the first call's
// cleanup deleted the players) and broadcast game:ended with rankings: [],
// wiping the results every client is already displaying. Claim the transition
// atomically so only one caller proceeds — a plain status check here would
// still let two concurrent callers through the gap between the read and the
// write.
// FinalizeRoom ends a room from outside the request path — the license expiry
// sweep uses it so an expired host's live room is closed the same way the host's
// own End Game closes it: archived, players notified over game:ended, cleanup
// scheduled. Reimplementing any of that in the cron would let the two paths drift.
func FinalizeRoom(roomID uint) error {
	return FinalizeRoomWithReason(roomID, EndReasonHost)
}

// CorrectAnswerCounts returns, for one room, how many correct answers each
// player gave — in a single grouped query rather than one per player. Shared
// with the cron that closes abandoned rooms, which had the same loop.
// Served by idx_answer_logs_room_id.
func CorrectAnswerCounts(roomID uint) map[uint]int64 {
	type row struct {
		PlayerID uint
		N        int64
	}
	var rows []row
	if err := db.DB.Model(&model.AnswerLog{}).
		Select("player_id, count(*) AS n").
		Where("room_id = ? AND is_correct = ?", roomID, true).
		Group("player_id").
		Scan(&rows).Error; err != nil {
		// A missing count costs a zero in the rankings; losing the archive
		// would cost the whole game, so this never aborts the close.
		log.Printf("[CorrectAnswerCounts] room %d: %v", roomID, err)
		return map[uint]int64{}
	}
	out := make(map[uint]int64, len(rows))
	for _, r := range rows {
		out[r.PlayerID] = r.N
	}
	return out
}

// Why a room closed. Protocol values, never prose: the client branches on them,
// so they are deliberately absent from the i18n catalog — same discipline as the
// NO_MORE_QUESTIONS / ROOM_FINISHED sentinels.
const (
	EndReasonHost           = "" // the host pressed End Game, or solo mode finished
	EndReasonLicenseExpired = "license_expired"
	EndReasonLicenseRevoked = "license_revoked"
)

// FinalizeRoomWithReason is FinalizeRoom plus the cause, stored on the room and
// pushed to players. A player who is bounced out of a game mid-question deserves
// to know it was not their connection.
func FinalizeRoomWithReason(roomID uint, reason string) error {
	_, _, err := finalizeRoom(roomID, reason)
	return err
}

func finalizeRoom(roomID uint, reason string) (rankings []finalRanking, alreadyEnded bool, err error) {
	var room model.Room
	if err := db.DB.First(&room, roomID).Error; err != nil {
		return nil, false, err
	}

	claim := db.DB.Model(&model.Room{}).
		Where("id = ? AND status != ?", room.ID, "finished").
		Updates(map[string]interface{}{
			"status":              "finished",
			"current_question_id": nil,
			// Written in the same claim as the status, so the stored reason is
			// atomic with the transition and costs no extra query.
			"ended_reason": reason,
		})
	if claim.Error != nil {
		return nil, false, claim.Error
	}
	if claim.RowsAffected == 0 {
		// Already finished. The only reason anyone asks a second time is that a
		// client never acted on the first game:ended — a host tab that missed the
		// push, or the license sweep racing the host's own click — so replay it
		// rather than returning a silent 200 that leaves those players sitting in
		// a game that is already archived.
		//
		// Read the archive, never the live rows: the first caller's cleanup has
		// very likely deleted them, and publishing rankings: [] would wipe the
		// podium off every screen already showing it. No archive yet means the
		// first caller is still between the claim and the insert; it publishes on
		// its own a moment later, so staying quiet here is correct.
		archived := archivedRankings(room.ID)
		if len(archived) > 0 {
			realtime.Client.Publish(fmt.Sprintf("rooms:%s", room.PinCode), "game:ended", gin.H{
				"room_id":  room.ID,
				"rankings": archived,
				// The STORED reason, not this caller's argument: a host clicking
				// End Game on a room the licence sweep already closed must not
				// replay it as an ordinary ending.
				"reason": room.EndedReason,
			})
		}
		return archived, true, nil
	}

	// Collect final rankings BEFORE cleanup
	var players []model.Player
	db.DB.Where("room_id = ?", room.ID).Find(&players)

	// One grouped count for the whole room, not one query per player: a
	// 2000-seat room used to close with 2000 round trips.
	correctByPlayer := CorrectAnswerCounts(room.ID)

	for _, p := range players {
		rankings = append(rankings, finalRanking{
			Nickname:       p.Nickname,
			Score:          p.Score,
			CorrectAnswers: int(correctByPlayer[p.ID]),
		})
	}
	sort.Slice(rankings, func(i, j int) bool {
		return rankings[i].Score > rankings[j].Score
	})

	// Archive to game_sessions (permanent record). If this fails the transient
	// rows below are the only remaining copy, so the cleanup is skipped.
	archiveErr := archiveGameLogs(room, rankings)
	if archiveErr != nil {
		log.Printf("[finalizeRoom] archive failed for room %d, keeping players and answer logs: %v", room.ID, archiveErr)
		notify.P1("finalize_archive_failed", "Archive khi kết thúc phòng thất bại (room=%d): %v — kết quả trận đấu có nguy cơ mất.", room.ID, archiveErr)
	}

	// Broadcast result to all connected clients
	realtime.Client.Publish(fmt.Sprintf("rooms:%s", room.PinCode), "game:ended", gin.H{
		"room_id":  room.ID,
		"rankings": rankings,
		"reason":   reason,
	})

	// Cleanup: delete transient player records and answer logs for this room —
	// but only once the rankings are safely persisted in game_sessions.
	// Running in a goroutine to not block the response.
	if archiveErr == nil {
		go func(roomID uint) {
			if err := db.DB.Where("room_id = ?", roomID).Delete(&model.Player{}).Error; err != nil {
				fmt.Printf("[CLEANUP] Warning: failed to delete players for room %d: %v\n", roomID, err)
			}
			if err := db.DB.Where("room_id = ?", roomID).Delete(&model.AnswerLog{}).Error; err != nil {
				fmt.Printf("[CLEANUP] Warning: failed to delete answer logs for room %d: %v\n", roomID, err)
			}
			fmt.Printf("[CLEANUP] Removed players and answer logs for finished room %d\n", roomID)
		}(room.ID)
	}

	return rankings, false, nil
}

// archiveGameLogs records the final state of a completed game.
// [C-4 FIX] Now writes to the dedicated game_sessions table instead of
// polluting answer_logs with synthetic "GAME_ENDED" records that corrupted analytics.
// It returns an error so the caller can refuse to delete the source rows: the
// archive is the only surviving copy of a finished game, and deleting players
// and answer logs after a failed archive loses the game permanently.
// archivedRankings replays what finalizeRoom wrote to game_sessions. Returns
// nil when the room has no archive yet or its JSON cannot be read — callers
// must treat that as "unknown", never as "nobody scored".
func archivedRankings(roomID uint) []finalRanking {
	var session model.GameSession
	if err := db.DB.Where("room_id = ?", roomID).First(&session).Error; err != nil {
		return nil
	}
	var rankings []finalRanking
	if err := json.Unmarshal([]byte(session.Rankings), &rankings); err != nil {
		log.Printf("[archivedRankings] room %d has unreadable archive: %v", roomID, err)
		notify.P1("archive_unreadable", "Bản archive của phòng %d không đọc được: %v — kết quả trận đấu đã hỏng.", roomID, err)
		return nil
	}
	return rankings
}

func archiveGameLogs(room model.Room, rankings interface{}) error {
	rankingsJSON, err := json.Marshal(rankings)
	if err != nil {
		return fmt.Errorf("marshal rankings for room %d: %w", room.ID, err)
	}

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
		return fmt.Errorf("archive game session for room %d: %w", room.ID, err)
	}

	fmt.Printf("[AUDIT LOG] Game Room %d Ended. Rankings archived to game_sessions: %s\n", room.ID, string(rankingsJSON))
	return nil
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

	var req UpdateQuizReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var quiz model.Quiz
	if err := db.DB.Where("id = ?", uint(id)).First(&quiz).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Quiz not found"})
		return
	}
	if rights := rightsOn(&quiz, userID.(uint)); !rights.Edit {
		denyQuizAccess(c, rights)
		return
	}

	// Every gameplay step reads the questions live, and the sync below deletes
	// the ones missing from the payload — so an edit landing mid-game rewrites
	// the question list under a room that is already running it. A shared quiz
	// can be edited and hosted by different people at once, so the room at risk
	// is very often not the editor's own and not on their screen. That is
	// exactly why this check cannot be left to the UI.
	live, err := quizHasActiveRoom(quiz.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check running rooms"})
		return
	}
	if live {
		c.JSON(http.StatusConflict, gin.H{"error": "Quiz is being played right now"})
		return
	}

	// Entitlements come from the quiz owner, not the editor. The limits belong
	// to whoever owns the resource: otherwise a Free editor saving a Pro host's
	// 60-question quiz is rejected for a quota that is not theirs, and
	// vetThemeConfig below would strip the owner's branding. For the owner
	// editing their own quiz this is the same lookup as before.
	ents, err := license.GetEntitlements(quiz.HostID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to resolve license"})
		return
	}
	if license.Enforcing() && !license.IsUnlimited(ents.MaxQuestionsPerQuiz) && len(req.Questions) > ents.MaxQuestionsPerQuiz {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   fmt.Sprintf("Too many questions (max %d on %s plan)", ents.MaxQuestionsPerQuiz, ents.PlanName),
			"plan_id": ents.PlanID,
		})
		return
	}

	tx := db.DB.Begin()

	// Update quiz fields
	themeConfig, themeOK := vetThemeConfig(c, ents, license.Enforcing(), req.ThemeConfig)
	if !themeOK {
		tx.Rollback()
		return
	}

	// Compare-and-swap on updated_at rather than Save: this handler replaces the
	// whole question list, so two people editing the same quiz would otherwise
	// silently overwrite each other. The check runs inside the UPDATE, because
	// comparing in Go and then saving leaves a window between the two.
	//
	// The flags are absent from the map on purpose — is_public and allow_edit
	// are the owner's, and are only writable through PATCH /quizzes/:id/sharing.
	updates := map[string]interface{}{
		"title":        req.Title,
		"description":  req.Description,
		"theme_config": themeConfig,
	}
	cas := tx.Model(&model.Quiz{}).Where("id = ?", quiz.ID)
	if req.ExpectedUpdatedAt != nil {
		cas = cas.Where("updated_at = ?", *req.ExpectedUpdatedAt)
	}
	res := cas.Updates(updates)
	if res.Error != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update quiz"})
		return
	}
	if res.RowsAffected == 0 {
		tx.Rollback()
		c.JSON(http.StatusConflict, gin.H{"error": "Quiz was changed by someone else"})
		return
	}
	quiz.Title = req.Title
	quiz.Description = req.Description
	quiz.ThemeConfig = themeConfig

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
		if err := validateCorrectAnswer(q.Type, q.CorrectAnswer, q.Options); err != nil {
			tx.Rollback()
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Question %d: %v", idx+1, err)})
			return
		}

		optsJSON, err := json.Marshal(q.Options)
		if err != nil {
			tx.Rollback()
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid options format for question %d", idx)})
			return
		}

		// nil means the caller did not touch the slide. Almost every editor
		// action rebuilds the whole quiz payload from scratch, so treating an
		// absent field as "clear it" would make renaming an option delete the
		// question's slide.
		var explanation *string
		if q.Explanation != nil {
			cleaned, err := validateExplanation(*q.Explanation)
			if err != nil {
				tx.Rollback()
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Question %d: %v", idx+1, err)})
				return
			}
			explanation = &cleaned
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
			if explanation != nil {
				question.Explanation = *explanation
			}

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
			if explanation != nil {
				question.Explanation = *explanation
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
	invalidateQuizQuestions(quiz.ID)

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
	invalidateQuizQuestions(quiz.ID)

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

		// The play page reads seven scalar fields off this response and never
		// touches the roster (app/play/[sessionId]/page.tsx). getRoomPlayers costs
		// an unfiltered SELECT over every player in the room plus a GROUP BY over
		// its whole answer_logs table, and at 1000 players polling every 2.5s that
		// ran ~400 times a second for ~176KB of data the client throws away.
		//
		// The host path below still returns the full roster — it is the one caller
		// that actually renders it.
		// player_count, max_players and plan_name used to be computed here too —
		// a COUNT over the room's players and a license lookup on every poll,
		// the single largest DB cost under load (measured 2026-09-24), for three
		// fields the play page never reads. The host branch still returns them.
		qset, err := quizQuestions(room.QuizID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load questions"})
			return
		}
		qCount := int64(len(qset.list))

		var currentPlayer model.Player
		var activeQuestionInfo interface{}
		db.DB.Where("id = ?", pClaims.PlayerID).First(&currentPlayer)

		var config struct {
			GameMode string `json:"game_mode"`
		}
		json.Unmarshal([]byte(room.ThemeConfig), &config)
		isPlayerPaced := config.GameMode == "player_paced"

		if isPlayerPaced {
			ensureSoloQuestionAssigned(&room, &currentPlayer)
		}
		if isPlayerPaced && currentPlayer.CurrentQuestionID != nil {
			if question, qIndex, ok := qset.byID(*currentPlayer.CurrentQuestionID); ok {
				// Start per-player timer on first view of the question
				if currentPlayer.QuestionActiveUntil == nil {
					until := time.Now().Add(time.Duration(question.Duration) * time.Second)
					currentPlayer.QuestionActiveUntil = &until
					db.DB.Model(&currentPlayer).Update("question_active_until", until)
				}
				activeInfo := gin.H{
					"id":       question.ID,
					"content":  question.Content,
					"type":     question.Type,
					"options":  sanitizePlayerOptions(question.Options),
					"duration": question.Duration,
					"index":    qIndex,
					"total":    len(qset.list),
				}
				if currentPlayer.QuestionActiveUntil != nil {
					activeInfo["active_until"] = currentPlayer.QuestionActiveUntil.UTC().Format(time.RFC3339)
					activeInfo["active_at"] = currentPlayer.QuestionActiveUntil.UTC().
						Add(-time.Duration(question.Duration) * time.Second).Format(time.RFC3339)
				}
				activeQuestionInfo = activeInfo
			}
		}

		// Return limited info: no quiz details, no host_id, no pin_code
		if !isPlayerPaced && room.CurrentQuestionID != nil {
			// Host-paced rooms used to fall through with current_question: null even
			// mid-question, so a player who reloaded had no server deadline to resync
			// against and their timer restarted at full duration. The room row already
			// carries the active question and its deadline — this just reports them.
			if question, _, ok := qset.byID(*room.CurrentQuestionID); ok {
				activeInfo := gin.H{
					"id":       question.ID,
					"content":  question.Content,
					"type":     question.Type,
					"options":  sanitizePlayerOptions(question.Options),
					"duration": question.Duration,
					"index":    room.CurrentQuestionIndex,
					"total":    int(qCount),
				}
				if room.QuestionActiveUntil != nil {
					activeInfo["active_until"] = room.QuestionActiveUntil.UTC().Format(time.RFC3339)
					activeInfo["active_at"] = room.QuestionActiveUntil.UTC().
						Add(-time.Duration(question.Duration) * time.Second).Format(time.RFC3339)
				}
				activeQuestionInfo = activeInfo
			}
		}
		c.JSON(http.StatusOK, gin.H{
			"id":           room.ID,
			"status":       room.Status,
			"theme_config": room.ThemeConfig,
			// "players" is deliberately absent, and so are player_count,
			// max_players and plan_name — lib/game-session.ts defaults all four
			// and the play page reads none of them.
			"current_question":       activeQuestionInfo,
			"question_count":         qCount,
			"current_question_index": room.CurrentQuestionIndex,
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

// GetRoomResults returns the final standings of a room to everyone who took
// part: the host (Bearer JWT, must own the room) or a player (X-Player-Token
// bound to the room).
//
// GetRoom's player response deliberately omits the roster — it is polled every
// 2.5s during play, so returning it cost an unfiltered SELECT plus a GROUP BY
// per poll. That left the results screen with no source for the leaderboard and
// it rendered empty for players. This is the one endpoint that serves it, it is
// read once at the end of a game, and it exposes scores only: no quiz, no
// answer keys, no host details.
//
// For a finished room getRoomPlayers replays the archived rankings, whose ids
// are synthetic positions rather than player ids (the real rows are deleted on
// cleanup). So the caller's own row is marked server-side via the nickname in
// their token — nicknames are unique per room — instead of leaving the client
// to match on an id that no longer means anything.
func GetRoomResults(c *gin.Context) {
	roomIDStr := c.Param("id")
	roomID, err := strconv.ParseUint(roomIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid room ID"})
		return
	}

	// Authenticate before touching the DB: a room lookup ahead of this would
	// answer "Room not found" vs. "Unauthorized" to anyone walking room ids.
	callerNickname, hostID, ok := roomViewerAuth(c, uint(roomID))
	if !ok {
		return
	}

	var room model.Room
	if err := db.DB.First(&room, uint(roomID)).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Room not found"})
		return
	}
	if hostID != 0 && room.HostID != hostID {
		c.JSON(http.StatusForbidden, gin.H{"error": "You do not own this room"})
		return
	}

	players := getRoomPlayers(room.ID, room.Status)
	// Archived rankings arrive sorted, a live room's rows do not. Sort here so
	// the rank each client shows is the rank the server computed.
	sort.SliceStable(players, func(i, j int) bool {
		return players[i].Score > players[j].Score
	})

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

	c.JSON(http.StatusOK, gin.H{
		"room_id": room.ID,
		"status":  room.Status,
		"players": standings,
		// Why the game ended, for a client that arrived after the push (or
		// reloaded the results page, where the websocket payload is long gone).
		"ended_reason": room.EndedReason,
		// The host's branding. The results screen is where a room spends its
		// last minute — the host reads out the podium off the projector while
		// everyone checks their own rank — so it is the one screen where the
		// event's key visual is most worth keeping, and it was the only game
		// screen still falling back to the plain gradient.
		"theme_config": room.ThemeConfig,
	})
}

// ensureSoloQuestionAssigned repairs a solo-mode player sitting in a running
// room with no question assigned.
//
// current_question_id being NULL means two different things that look identical
// on the row: the player answered their last question and is done, or the
// player was never dealt a first question at all. The answer log tells them
// apart — nobody who has answered nothing can have finished. Only the second
// case is repaired; the first must keep reporting NO_MORE_QUESTIONS, which is
// what sends the player to the results screen.
//
// StartGame and JoinRoom no longer race (both serialize on the room row), so
// this should not trigger for new rooms. It stays because it costs one COUNT on
// a path that is already reading the player, it heals rooms that were started
// before that fix, and a player stranded here has no way out of the lobby
// screen but a page reload.
func ensureSoloQuestionAssigned(room *model.Room, pl *model.Player) {
	if pl == nil || pl.ID == 0 || pl.CurrentQuestionID != nil || room.Status != "active" {
		return
	}
	var answered int64
	if err := db.DB.Model(&model.AnswerLog{}).
		Where("room_id = ? AND player_id = ?", room.ID, pl.ID).Count(&answered).Error; err != nil {
		return
	}
	if answered > 0 {
		return // genuinely finished
	}
	var first model.Question
	if err := db.DB.Where("quiz_id = ?", room.QuizID).
		Order("questions.order ASC, questions.id ASC").First(&first).Error; err != nil {
		return
	}
	if err := db.DB.Model(pl).UpdateColumns(map[string]interface{}{
		"current_question_id":   first.ID,
		"question_active_until": nil,
	}).Error; err != nil {
		return
	}
	log.Printf("[solo-repair] room %d player %d had no question assigned; dealt question %d", room.ID, pl.ID, first.ID)
	pl.CurrentQuestionID = &first.ID
	pl.QuestionActiveUntil = nil
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

	qset, err := quizQuestions(room.QuizID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load questions"})
		return
	}
	questions := qset.list
	if index >= len(questions) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Question not found"})
		return
	}

	q := questions[index]

	// Player-paced: start the timer when the player loads their current question
	isPlayerPaced := roomGameMode(room.ThemeConfig) == "player_paced"
	var activeUntil *time.Time
	if isPlayerPaced {
		var pl model.Player
		if err := db.DB.First(&pl, pClaims.PlayerID).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Player not found"})
			return
		}
		ensureSoloQuestionAssigned(&room, &pl)
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
			pl.QuestionActiveUntil = &until
		}
		activeUntil = pl.QuestionActiveUntil
	} else {
		if room.Status != "active" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Room is not active"})
			return
		}
		if room.CurrentQuestionIndex < 0 || index != room.CurrentQuestionIndex {
			c.JSON(http.StatusForbidden, gin.H{"error": "Question is not currently active"})
			return
		}
		activeUntil = room.QuestionActiveUntil
	}

	resp := gin.H{
		"id":       q.ID,
		"quiz_id":  q.QuizID,
		"content":  q.Content,
		"type":     q.Type,
		"options":  sanitizePlayerOptions(q.Options),
		"duration": q.Duration,
		"order":    q.Order,
		"index":    index,
		"total":    len(questions),
	}
	// Report the server deadline so a reloading client resyncs its countdown
	// instead of restarting the timer at full duration (the server deadline is
	// unchanged, so a desynced client submits "on time" and still gets rejected).
	if activeUntil != nil {
		resp["active_until"] = activeUntil.UTC().Format(time.RFC3339)
		resp["active_at"] = activeUntil.UTC().Add(-time.Duration(q.Duration) * time.Second).Format(time.RFC3339)
	}
	c.JSON(http.StatusOK, resp)
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

	var room model.Room
	roomErr := db.DB.First(&room, uint(roomID)).Error

	// The frontend fires this from beforeunload, which also runs on a page
	// REFRESH — deleting the row there erased the player's score and progress
	// (fatal in solo mode, where both live on the player row) and their next
	// load found no player and dumped them to the results page. While the game
	// is running, only mark the player disconnected; the row survives a reload
	// and the finished-game cleanup in finalizeRoom removes it later.
	if roomErr == nil && room.Status == "active" {
		if err := db.DB.Model(&model.Player{}).
			Where("id = ? AND room_id = ?", pClaims.PlayerID, uint(roomID)).
			Update("is_connected", false).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove player"})
			return
		}
	} else {
		// Lobby (waiting) or finished room: delete as before so the nickname
		// frees up and the lobby list stays accurate.
		if err := db.DB.Where("id = ? AND room_id = ?", pClaims.PlayerID, uint(roomID)).Delete(&model.Player{}).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove player"})
			return
		}
	}

	if roomErr == nil {
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
					// CurrentQuestionID is nil here, which the host screen reads as
					// "done" — correct for an archived game.
				}
			}
		}
		if len(players) > 0 {
			return players
		}
		// Archive not readable yet — fall through to the live rows.
		//
		// finalizeRoom claims the "finished" status first, writes the
		// game_sessions archive second, and deletes the player rows last (in a
		// goroutine). Between the first and second step the room reads as
		// finished while the archive does not exist, and this returned nothing.
		// In solo mode the player whose own last answer ends the game is routed
		// to the results screen inside exactly that window, so the person most
		// certain to hit it is a real player, not a straggler. Measured live on
		// a 1500-player room: the leaderboard came back with zero rows.
		//
		// The live rows are still present for the whole window, so reading them
		// is both correct and the same data the archive is about to hold.
	}
	{
		db.DB.Where("room_id = ?", roomID).Find(&players)

		// One aggregate for both numbers. Correct-only used to be its own query;
		// solo mode also needs the total submitted (see Player.AnsweredCount), and
		// a second GROUP BY over the same answer_logs table would double the cost
		// of the host's 2s poll for nothing.
		type PlayerAnswerCount struct {
			PlayerID uint
			Answered int
			Correct  int
		}
		var counts []PlayerAnswerCount
		db.DB.Model(&model.AnswerLog{}).
			Select("player_id, count(*) as answered, count(*) FILTER (WHERE is_correct) as correct").
			Where("room_id = ?", roomID).
			Group("player_id").
			Scan(&counts)

		type tally struct{ answered, correct int }
		countMap := make(map[uint]tally, len(counts))
		for _, c := range counts {
			countMap[c.PlayerID] = tally{answered: c.Answered, correct: c.Correct}
		}
		for i := range players {
			t := countMap[players[i].ID]
			players[i].CorrectAnswers = t.correct
			players[i].AnsweredCount = t.answered
		}
	}
	return players
}
