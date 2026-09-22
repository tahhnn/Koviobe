package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/audit"
	"gorm.io/gorm"
)

// quizRights is what one user may do with one quiz.
//
// It is resolved from a quiz that has already been loaded, so the decision is a
// pure function of (quiz, user) and can be tested without a database. Every
// handler that used to filter on `host_id = ?` now loads the quiz by id and
// asks this instead — keeping the rule in one place, because a single handler
// that forgets it is a hole.
type quizRights struct {
	Owner bool // may change the sharing flag, and may delete
	View  bool // may read the quiz, including correct answers
	Host  bool // may open a room from it
	Copy  bool // may take an independent copy into their own quizzes
	Edit  bool // may change THIS quiz's questions — owner only
}

// rightsOn resolves what uid may do with quiz.
//
// Sharing always hands out reading, hosting and copying. Editing is the
// owner's separate decision: AllowEdit opens the original to everyone, and
// without it a reader who wants changes takes a copy instead.
//
// Two things sharing can never grant, whatever the flags say: deleting, and
// changing the flags themselves. Both stay with HostID.
func rightsOn(quiz *model.Quiz, uid uint) quizRights {
	if quiz == nil {
		return quizRights{}
	}
	if quiz.HostID == uid {
		return quizRights{Owner: true, View: true, Host: true, Copy: true, Edit: true}
	}
	if !quiz.IsPublic {
		return quizRights{}
	}
	// Gated on both columns rather than AllowEdit alone: a row unshared while
	// AllowEdit was set must not stay editable, and normalizeSharing keeps the
	// pair consistent going forward.
	return quizRights{View: true, Host: true, Copy: true, Edit: quiz.AllowEdit}
}

// denyQuizAccess writes the rejection for a caller who may not do what they
// asked. A quiz the caller cannot even see is reported as missing rather than
// forbidden, so the endpoint does not confirm that someone else's quiz id
// exists; one they can see but not change gets an honest 403.
func denyQuizAccess(c *gin.Context, r quizRights) {
	if !r.View {
		c.JSON(http.StatusNotFound, gin.H{"error": "Quiz not found"})
		return
	}
	c.JSON(http.StatusForbidden, gin.H{"error": "This shared quiz is read-only — duplicate it to make changes"})
}

// UpdateQuizSharingReq is the body of PATCH /quizzes/:id/sharing.
//
// The flag lives in its own request type, and its own endpoint, rather than in
// CreateQuizReq: UpdateQuiz saves every field it is given, and publishing must
// stay a decision only the owner can make.
//
// AllowEdit only means anything while IsPublic is set; normalizeSharing
// enforces that, so unpublishing can never leave editing quietly switched on.
type UpdateQuizSharingReq struct {
	IsPublic  bool `json:"is_public"`
	AllowEdit bool `json:"allow_edit"`
}

// normalizeSharing resolves the pair into a state that cannot surprise the
// owner later: editing exists only while the quiz is public, so unpublishing
// clears it. Without this, publishing again months on would silently reopen
// editing the owner believes they switched off.
func normalizeSharing(isPublic, allowEdit bool) (bool, bool) {
	if !isPublic {
		return false, false
	}
	return true, allowEdit
}

// UpdateQuizSharing publishes or unpublishes a quiz. Owner only — edit rights
// never extend to the sharing flags themselves.
func UpdateQuizSharing(c *gin.Context) {
	userID, _ := c.Get("user_id")
	uid := userID.(uint)

	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid quiz ID"})
		return
	}

	var req UpdateQuizSharingReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var quiz model.Quiz
	if err := db.DB.Where("id = ? AND host_id = ?", uint(id), uid).First(&quiz).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Quiz not found"})
		return
	}

	isPublic, allowEdit := normalizeSharing(req.IsPublic, req.AllowEdit)

	// A map, not a struct: Updates with a struct skips zero values, so
	// `Updates(model.Quiz{IsPublic: false})` would silently do nothing and
	// unpublishing would appear to succeed while changing no row.
	if err := db.DB.Model(&quiz).Updates(map[string]interface{}{
		"is_public":  isPublic,
		"allow_edit": allowEdit,
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update quiz sharing"})
		return
	}

	action := "unshare_quiz"
	if isPublic {
		action = "share_quiz"
	}
	audit.Record(uid, action, fmt.Sprintf("quiz_%d", quiz.ID), c.ClientIP())

	c.JSON(http.StatusOK, gin.H{
		"id":         quiz.ID,
		"is_public":  isPublic,
		"allow_edit": allowEdit,
	})
}

// Shared-quiz listing --------------------------------------------------------

const (
	sharedQuizDefaultPageSize = 20
	sharedQuizMaxPageSize     = 50
	sharedQuizMaxSearchLen    = 100
)

// sharedQuizQuery is the bounded, escaped form of the listing's query params.
// Kept separate from the handler so the clamping and the LIKE escaping — the
// two parts that are easy to get wrong — are directly testable.
type sharedQuizQuery struct {
	// Search is already escaped for LIKE and is empty when no filter applies.
	Search   string
	Page     int
	PageSize int
}

// Offset is the row offset for the resolved page.
func (q sharedQuizQuery) Offset() int { return (q.Page - 1) * q.PageSize }

// Pattern is the ILIKE pattern for Search. Only valid when Search is non-empty.
func (q sharedQuizQuery) Pattern() string { return "%" + q.Search + "%" }

// parseSharedQuizQuery clamps page and size into a range the database can serve
// cheaply, and neutralises the LIKE metacharacters in the search term. Without
// the escaping a search for "100%" matches every row, and "_" matches any
// single character — surprising rather than dangerous, but still wrong.
func parseSharedQuizQuery(rawSearch, rawPage, rawPageSize string) sharedQuizQuery {
	q := sharedQuizQuery{Page: 1, PageSize: sharedQuizDefaultPageSize}

	if n, err := strconv.Atoi(strings.TrimSpace(rawPage)); err == nil && n > 1 {
		q.Page = n
	}
	if n, err := strconv.Atoi(strings.TrimSpace(rawPageSize)); err == nil && n > 0 {
		if n > sharedQuizMaxPageSize {
			n = sharedQuizMaxPageSize
		}
		q.PageSize = n
	}

	search := strings.TrimSpace(rawSearch)
	if len(search) > sharedQuizMaxSearchLen {
		search = search[:sharedQuizMaxSearchLen]
	}
	q.Search = escapeLike(search)
	return q
}

// escapeLike escapes the three characters Postgres treats specially inside a
// LIKE/ILIKE pattern. The backslash must be escaped first, or it would double
// the escapes this function itself adds.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	s = strings.ReplaceAll(s, "_", `\_`)
	return s
}

// sharedQuizItem is the public shape of a shared quiz.
//
// Deliberately not model.Quiz: that would leak host_id and, once the author is
// preloaded, their email. The author is identified by display name only.
type sharedQuizItem struct {
	ID            uint      `json:"id"`
	Title         string    `json:"title"`
	Description   string    `json:"description"`
	QuestionCount int64     `json:"question_count"`
	AllowEdit     bool      `json:"allow_edit"`
	IsOwner       bool      `json:"is_owner"`
	AuthorName    string    `json:"author_name"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ListSharedQuizzes returns the quizzes their owners have made public.
//
// Paginated, unlike ListQuizzes: that one returns every row because it is
// scoped to one host, while this is a table that grows with the whole platform.
func ListSharedQuizzes(c *gin.Context) {
	userID, _ := c.Get("user_id")
	uid := userID.(uint)

	q := parseSharedQuizQuery(c.Query("q"), c.Query("page"), c.Query("page_size"))

	// Built fresh for each of the two queries rather than chained off one
	// shared *gorm.DB: a chain that has already run Count carries its state
	// into the next call.
	scoped := func() *gorm.DB {
		tx := db.DB.Model(&model.Quiz{}).Where("is_public = ?", true)
		if q.Search != "" {
			tx = tx.Where("title ILIKE ? OR description ILIKE ?", q.Pattern(), q.Pattern())
		}
		return tx
	}

	var total int64
	if err := scoped().Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch shared quizzes"})
		return
	}

	var quizzes []model.Quiz
	if err := scoped().
		Order("created_at DESC, id DESC").
		Limit(q.PageSize).Offset(q.Offset()).
		Find(&quizzes).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch shared quizzes"})
		return
	}

	out := make([]sharedQuizItem, 0, len(quizzes))
	if len(quizzes) > 0 {
		ids := make([]uint, len(quizzes))
		authorIDs := make([]uint, 0, len(quizzes))
		for i, quiz := range quizzes {
			ids[i] = quiz.ID
			authorIDs = append(authorIDs, quiz.HostID)
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
		counts := map[uint]int64{}
		for _, r := range rows {
			counts[r.QuizID] = r.Cnt
		}

		var authors []model.User
		_ = db.DB.Where("id IN ?", authorIDs).Find(&authors).Error
		names := map[uint]string{}
		for _, a := range authors {
			names[a.ID] = a.Nickname
		}

		for _, quiz := range quizzes {
			out = append(out, sharedQuizItem{
				ID:            quiz.ID,
				Title:         quiz.Title,
				Description:   quiz.Description,
				QuestionCount: counts[quiz.ID],
				AllowEdit:     quiz.AllowEdit,
				IsOwner:       quiz.HostID == uid,
				AuthorName:    authorDisplayName(names[quiz.HostID]),
				CreatedAt:     quiz.CreatedAt,
				UpdatedAt:     quiz.UpdatedAt,
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"quizzes":   out,
		"total":     total,
		"page":      q.Page,
		"page_size": q.PageSize,
	})
}

// authorDisplayName keeps the email out of the shared listing. Nickname is
// optional on User, so an author who never set one is shown generically rather
// than as a blank byline.
func authorDisplayName(nickname string) string {
	if n := strings.TrimSpace(nickname); n != "" {
		return n
	}
	return "quizzZone host"
}

// withGameMode returns themeConfig with game_mode set to mode.
//
// The mode is a property of one game, not of the quiz: two people can run the
// same quiz at the same time, one host-paced and one player-paced. It used to
// be written onto the quiz right before the room was created, which meant
// picking Solo silently rewrote the author's quiz for everyone — and, once
// quizzes could be shared, meant a guest could not start a game at all,
// because starting one required a write they are not allowed to make.
//
// Merged into the existing document rather than replacing it: theme_config
// also carries the colours, the background and explanation_duration, and the
// room reads its own copy of all of them.
func withGameMode(themeConfig, mode string) (string, error) {
	cfg := map[string]interface{}{}
	if trimmed := strings.TrimSpace(themeConfig); trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &cfg); err != nil {
			// A quiz with an unreadable theme still has to be playable, so the
			// mode is carried on a fresh document rather than failing the room.
			cfg = map[string]interface{}{}
		}
	}
	cfg["game_mode"] = mode
	out, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// normalizeGameMode maps what the client sends to what the gameplay handlers
// read. They compare against "player_paced" exactly, so an unrecognised value
// must land on the host-paced default rather than pass through.
func normalizeGameMode(raw string) string {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "solo", "player_paced":
		return "player_paced"
	default:
		return "host_paced"
	}
}

// quizHasActiveRoom reports whether a game is being played from this quiz right
// now. Every gameplay step reads the questions straight from the database, and
// UpdateQuiz deletes the questions that are missing from its payload, so an
// edit landing mid-game changes the question list under a running room.
//
// Waiting rooms deliberately do not count: StartGame reads the questions when
// it starts, so a room that has not started yet simply begins with the new
// list. Blocking on waiting rooms too would block on the stale lobbies that
// CreateRoom already has to clean up.
func quizHasActiveRoom(quizID uint) (bool, error) {
	var live int64
	if err := db.DB.Model(&model.Room{}).
		Where("quiz_id = ? AND status = ?", quizID, "active").
		Count(&live).Error; err != nil {
		return false, err
	}
	return live > 0, nil
}
