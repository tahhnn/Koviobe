package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/audit"
	"github.com/quizzzone/backend/internal/pkg/email"
	"github.com/quizzzone/backend/internal/pkg/license"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// quizRights is what one user may do with one quiz.
//
// Resolved by rightsOn from rows that are already loaded, so the decision is a
// pure function and can be tested without a database. Every handler that
// reaches a quiz by id goes through loadQuizRights — keeping the rule in one
// place, because a single handler that forgets it is a hole.
type quizRights struct {
	Owner bool // may delete, and may manage who the quiz is shared with
	View  bool // may read the quiz, including correct answers
	Host  bool // may open a room from it
	Copy  bool // may take an independent copy into their own quizzes
	Edit  bool // may change THIS quiz's title, theme and questions
}

// rightsOn resolves what uid may do with quiz.
//
// A quiz is reachable by exactly two kinds of people: its owner, and an editor
// the owner invited by email. There is no public mode — that was the leak this
// replaced: one flag opened a customer's questions, answers and event content
// to every account on the platform.
//
// An editor may read and edit, nothing more. Not hosting: the room, its players
// and its logs would belong to the editor. Not copying: a copy outlives the
// grant, so revoking would not take the content back.
//
// ownerCanShare is the owner's plan. Sharing is a Pro feature of the owner, so
// when their plan lapses the grants are suspended — kept, not deleted — and
// come back by themselves on renewal.
func rightsOn(quiz *model.Quiz, uid uint, collab *model.QuizCollaborator, ownerCanShare bool) quizRights {
	if quiz == nil {
		return quizRights{}
	}
	if quiz.HostID == uid {
		return quizRights{Owner: true, View: true, Host: true, Copy: true, Edit: true}
	}
	// Each condition is checked even though the loader already filtered on it:
	// this function is the rule, and must not depend on how its caller queried.
	if !ownerCanShare || collab == nil || collab.QuizID != quiz.ID ||
		collab.UserID == nil || *collab.UserID != uid {
		return quizRights{}
	}
	if collab.Role == model.CollaboratorRoleEditor {
		return quizRights{View: true, Edit: true}
	}
	return quizRights{}
}

// loadQuizRights is rightsOn with the collaborator row and the owner's plan
// looked up. The owner path costs no query.
func loadQuizRights(quiz *model.Quiz, uid uint) (quizRights, error) {
	if quiz == nil {
		return quizRights{}, nil
	}
	if quiz.HostID == uid {
		return rightsOn(quiz, uid, nil, true), nil
	}
	var collab model.QuizCollaborator
	err := db.DB.Where("quiz_id = ? AND user_id = ?", quiz.ID, uid).First(&collab).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return quizRights{}, nil
	}
	if err != nil {
		return quizRights{}, err
	}
	return rightsOn(quiz, uid, &collab, ownerCanShare(quiz.HostID)), nil
}

// ownerCanShare reports whether the owner's plan currently includes sharing.
// Fails closed: an owner whose plan cannot be read shares with nobody.
func ownerCanShare(ownerID uint) bool {
	ents, err := license.GetEntitlements(ownerID)
	return err == nil && ents.AllowQuizSharing
}

// writeRightsError answers a loadQuizRights failure.
func writeRightsError(c *gin.Context) {
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check quiz access"})
}

// denyQuizAccess writes the rejection for a caller who may not do what they
// asked. A quiz the caller cannot even see is reported as missing rather than
// forbidden, so the endpoint does not confirm that someone else's quiz id
// exists; an editor asking for an owner-only action gets an honest 403.
func denyQuizAccess(c *gin.Context, r quizRights) {
	if !r.View {
		c.JSON(http.StatusNotFound, gin.H{"error": "Quiz not found"})
		return
	}
	c.JSON(http.StatusForbidden, gin.H{"error": "Only the quiz owner can do this"})
}

// ownerNameFor is the owner's display name for a caller who is not the owner,
// and "" for the owner themselves (they need no banner).
func ownerNameFor(quiz *model.Quiz, r quizRights) string {
	if r.Owner || !r.View {
		return ""
	}
	var owner model.User
	if err := db.DB.Select("id, nickname").First(&owner, quiz.HostID).Error; err != nil {
		return authorDisplayName("")
	}
	return authorDisplayName(owner.Nickname)
}

// Collaborators ----------------------------------------------------------------

const (
	// shareInvitesPerHour caps new invitations per owner. Every invitation is
	// an email from our SMTP to an address the owner typed, so without a cap
	// the share dialog is a way to send mail on our behalf.
	shareInvitesPerHour = 30
	maxEmailLen         = 255
)

// normalizeCollaboratorEmail returns the stored form of an invited address, or
// false when it is not a single plain address. Display-name forms ("A <a@b>")
// and lists are rejected rather than unwrapped: the owner should see exactly
// the address that will get access.
func normalizeCollaboratorEmail(raw string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" || len(s) > maxEmailLen {
		return "", false
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s || addr.Name != "" {
		return "", false
	}
	return s, true
}

// collaboratorItem is what the owner sees in the share dialog.
type collaboratorItem struct {
	ID        uint      `json:"id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	Status    string    `json:"status"` // active | pending
	CreatedAt time.Time `json:"created_at"`
}

func toCollaboratorItem(c model.QuizCollaborator) collaboratorItem {
	status := "pending"
	if c.UserID != nil {
		status = "active"
	}
	return collaboratorItem{ID: c.ID, Email: c.Email, Role: c.Role, Status: status, CreatedAt: c.CreatedAt}
}

// loadOwnedQuiz loads quiz :id only if uid owns it. Anything else is a 404, so
// an editor probing the owner-only endpoints learns nothing new.
func loadOwnedQuiz(c *gin.Context, uid uint) (*model.Quiz, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid quiz ID"})
		return nil, false
	}
	var quiz model.Quiz
	if err := db.DB.Where("id = ? AND host_id = ?", uint(id), uid).First(&quiz).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Quiz not found"})
		return nil, false
	}
	return &quiz, true
}

// sharingStatus is the owner-side summary the share dialog renders: who has
// access, and whether the owner's plan lets them add more.
func sharingStatus(quiz *model.Quiz, ents license.Entitlements, rows []model.QuizCollaborator) gin.H {
	items := make([]collaboratorItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, toCollaboratorItem(r))
	}
	return gin.H{
		"quiz_id":       quiz.ID,
		"collaborators": items,
		"can_share":     ents.AllowQuizSharing,
		"limit":         ents.MaxCollaboratorsPerQuiz,
		"plan_id":       ents.PlanID,
	}
}

// ListQuizCollaborators is GET /quizzes/:id/collaborators. Owner only.
func ListQuizCollaborators(c *gin.Context) {
	uid := c.MustGet("user_id").(uint)
	quiz, ok := loadOwnedQuiz(c, uid)
	if !ok {
		return
	}
	ents, err := license.GetEntitlements(uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to resolve license"})
		return
	}
	var rows []model.QuizCollaborator
	if err := db.DB.Where("quiz_id = ?", quiz.ID).Order("created_at ASC, id ASC").Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load collaborators"})
		return
	}
	c.JSON(http.StatusOK, sharingStatus(quiz, ents, rows))
}

// AddQuizCollaboratorReq is the body of POST /quizzes/:id/collaborators.
type AddQuizCollaboratorReq struct {
	Email string `json:"email" binding:"required"`
}

// AddQuizCollaborator is POST /quizzes/:id/collaborators: the owner grants one
// email address edit access. Owner only, and only on a plan with sharing.
//
// Idempotent per address: inviting someone who already has access returns
// their row unchanged and sends no second email.
func AddQuizCollaborator(c *gin.Context) {
	uid := c.MustGet("user_id").(uint)

	var req AddQuizCollaboratorReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid email address"})
		return
	}
	addr, ok := normalizeCollaboratorEmail(req.Email)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid email address"})
		return
	}

	quiz, ok := loadOwnedQuiz(c, uid)
	if !ok {
		return
	}

	// Only the domain can be checked; see email.CheckDomain. Done before the
	// row exists, so a typo is fixed now rather than living on as a pending
	// invite nobody can ever claim.
	if err := email.CheckDomain(c.Request.Context(), addr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ents, err := license.GetEntitlements(uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to resolve license"})
		return
	}
	if !ents.AllowQuizSharing {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   "Sharing quizzes requires a Pro plan",
			"plan_id": ents.PlanID,
			"feature": "allow_quiz_sharing",
		})
		return
	}

	var owner model.User
	if err := db.DB.Select("id, email, nickname").First(&owner, uid).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to add collaborator"})
		return
	}
	if strings.EqualFold(owner.Email, addr) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "You already own this quiz"})
		return
	}

	// Counted from the audit trail, not from collaborator rows: revoking
	// hard-deletes the row, so revoke-and-reinvite would otherwise reset the
	// count and mail the same address without limit.
	var sentLastHour int64
	if err := db.DB.Model(&model.AuditLog{}).
		Where("user_id = ? AND action = ? AND created_at > ?", uid, "quiz_share_add", time.Now().Add(-time.Hour)).
		Count(&sentLastHour).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to add collaborator"})
		return
	}
	if sentLastHour >= shareInvitesPerHour {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many invitations — try again later"})
		return
	}

	// Linked at once when the address already has an account. Matched without
	// regard to case: Register lower-cases, but accounts made by other paths
	// may not have been.
	var invitee model.User
	var inviteeID *uint
	if err := db.DB.Select("id").Where("LOWER(email) = ?", addr).First(&invitee).Error; err == nil {
		id := invitee.ID
		inviteeID = &id
	}

	var row model.QuizCollaborator
	created := false
	limitHit := false
	err = db.DB.Transaction(func(tx *gorm.DB) error {
		// Serialises concurrent invites to one quiz, so two requests racing
		// cannot both pass the count below and overshoot the cap.
		var locked model.Quiz
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").First(&locked, quiz.ID).Error; err != nil {
			return err
		}

		err := tx.Where("quiz_id = ? AND email = ?", quiz.ID, addr).First(&row).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		var count int64
		if err := tx.Model(&model.QuizCollaborator{}).Where("quiz_id = ?", quiz.ID).Count(&count).Error; err != nil {
			return err
		}
		if !license.IsUnlimited(ents.MaxCollaboratorsPerQuiz) && int(count) >= ents.MaxCollaboratorsPerQuiz {
			limitHit = true
			return nil
		}

		row = model.QuizCollaborator{
			QuizID:    quiz.ID,
			Email:     addr,
			UserID:    inviteeID,
			Role:      model.CollaboratorRoleEditor,
			InvitedBy: uid,
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to add collaborator"})
		return
	}
	if limitHit {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   fmt.Sprintf("Collaborator limit reached (%d per quiz)", ents.MaxCollaboratorsPerQuiz),
			"plan_id": ents.PlanID,
			"limit":   ents.MaxCollaboratorsPerQuiz,
		})
		return
	}

	if !created {
		c.JSON(http.StatusOK, toCollaboratorItem(row))
		return
	}

	audit.Record(uid, "quiz_share_add", fmt.Sprintf("quiz_%d", quiz.ID), c.ClientIP())
	sendShareInvite(addr, owner.Nickname, quiz.ID, quiz.Title, inviteeID != nil)

	c.JSON(http.StatusCreated, toCollaboratorItem(row))
}

// RemoveQuizCollaborator is DELETE /quizzes/:id/collaborators/:cid. Owner only.
// Takes effect on the editor's next request: nothing caches a grant.
func RemoveQuizCollaborator(c *gin.Context) {
	uid := c.MustGet("user_id").(uint)
	quiz, ok := loadOwnedQuiz(c, uid)
	if !ok {
		return
	}
	cid, err := strconv.ParseUint(c.Param("cid"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid collaborator ID"})
		return
	}
	res := db.DB.Where("id = ? AND quiz_id = ?", uint(cid), quiz.ID).Delete(&model.QuizCollaborator{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove collaborator"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Collaborator not found"})
		return
	}
	audit.Record(uid, "quiz_share_remove", fmt.Sprintf("quiz_%d", quiz.ID), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"message": "Collaborator removed"})
}

// LeaveSharedQuiz is DELETE /quizzes/:id/collaborators/me: an editor gives up
// their own access.
func LeaveSharedQuiz(c *gin.Context) {
	uid := c.MustGet("user_id").(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid quiz ID"})
		return
	}
	res := db.DB.Where("quiz_id = ? AND user_id = ?", uint(id), uid).Delete(&model.QuizCollaborator{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to leave quiz"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Quiz not found"})
		return
	}
	audit.Record(uid, "quiz_share_leave", fmt.Sprintf("quiz_%d", id), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"message": "You left the quiz"})
}

// claimPendingInvites links invitations sent to email before it had an
// account. Called after Register and after every Login.
//
// Safe to key on the address because owning it has been proven: Register only
// creates an account after an OTP sent to that address is entered.
func claimPendingInvites(userID uint, addr string) {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if addr == "" {
		return
	}
	if err := db.DB.Model(&model.QuizCollaborator{}).
		Where("email = ? AND user_id IS NULL", addr).
		Update("user_id", userID).Error; err != nil {
		log.Printf("[SHARE] could not claim invites for user %d: %v", userID, err)
	}
}

// sendShareInvite queues the mail telling the invited address it was given
// access. Best effort: the grant already exists, and is claimed on sign-in
// whether or not the mail arrives, so a dropped send is logged, not surfaced.
func sendShareInvite(to, ownerNickname string, quizID uint, quizTitle string, hasAccount bool) {
	if err := email.Enqueue(to, "Bạn được mời cùng sửa một quiz trên quizzZone",
		shareInviteHTML(ownerNickname, quizID, quizTitle, hasAccount)); err != nil {
		log.Printf("[SHARE] invite email to quiz %d not queued: %v", quizID, err)
	}
}

func shareInviteHTML(ownerNickname string, quizID uint, quizTitle string, hasAccount bool) string {
	base := ""
	if config.AppConfig != nil {
		base = config.AppConfig.PublicAppURL
	}
	link := fmt.Sprintf("%s/quizzes/%d", base, quizID)
	action := "Mở quiz"
	hint := "Đăng nhập bằng đúng địa chỉ email này để thấy quiz trong mục <strong>Được chia sẻ với tôi</strong>."
	if !hasAccount {
		link = base + "/register"
		action = "Tạo tài khoản"
		hint = "Bạn chưa có tài khoản. Hãy đăng ký bằng <strong>đúng địa chỉ email này</strong> — quiz sẽ tự xuất hiện trong mục <strong>Được chia sẻ với tôi</strong>. Không cần mua gói."
	}
	return fmt.Sprintf(`<div style="font-family:system-ui,-apple-system,'Segoe UI',sans-serif;max-width:560px;margin:0 auto;color:#111">
	<h2 style="margin:0 0 4px">Bạn được mời cùng sửa quiz</h2>
	<p style="color:#555;margin:0 0 18px"><strong>%s</strong> đã cho bạn quyền sửa quiz <strong>%s</strong>.</p>
	<p style="color:#444;font-size:14px;margin:0 0 18px">%s</p>
	<p style="margin:0 0 18px"><a href="%s" style="display:inline-block;background:#111;color:#fff;padding:10px 16px;border-radius:8px;text-decoration:none">%s</a></p>
	<p style="color:#777;font-size:12px;margin:0">Nếu bạn không quen người gửi, hãy bỏ qua email này.</p>
</div>`,
		html.EscapeString(authorDisplayName(ownerNickname)),
		html.EscapeString(quizTitle),
		hint,
		html.EscapeString(link),
		action)
}

// Shared-with-me listing -------------------------------------------------------

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

// sharedQuizItem is one quiz someone shared with the caller.
//
// Deliberately not model.Quiz: that would carry host_id, and once the owner is
// preloaded, their email. The owner is identified by display name only.
type sharedQuizItem struct {
	ID            uint   `json:"id"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	QuestionCount int64  `json:"question_count"`
	Role          string `json:"role"`
	// Active is false while the owner's plan does not include sharing. The row
	// is still listed so the editor sees why the quiz stopped opening.
	Active     bool      `json:"active"`
	AuthorName string    `json:"author_name"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ListSharedWithMe is GET /quizzes/shared-with-me: the quizzes other owners
// have invited the caller to edit. Paginated like the old public listing, but
// scoped to the caller's own grants — nothing here is visible to anyone else.
func ListSharedWithMe(c *gin.Context) {
	uid := c.MustGet("user_id").(uint)

	q := parseSharedQuizQuery(c.Query("q"), c.Query("page"), c.Query("page_size"))

	// Built fresh for each of the two queries rather than chained off one
	// shared *gorm.DB: a chain that has already run Count carries its state
	// into the next call.
	scoped := func() *gorm.DB {
		tx := db.DB.Model(&model.Quiz{}).
			Joins("JOIN quiz_collaborators qc ON qc.quiz_id = quizzes.id AND qc.user_id = ?", uid)
		if q.Search != "" {
			tx = tx.Where("quizzes.title ILIKE ? OR quizzes.description ILIKE ?", q.Pattern(), q.Pattern())
		}
		return tx
	}

	var total int64
	if err := scoped().Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch shared quizzes"})
		return
	}

	type sharedRow struct {
		model.Quiz
		Role string `gorm:"column:role"`
	}
	var rows []sharedRow
	if err := scoped().
		Select("quizzes.*, qc.role AS role").
		Order("qc.created_at DESC, quizzes.id DESC").
		Limit(q.PageSize).Offset(q.Offset()).
		Scan(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch shared quizzes"})
		return
	}

	out := make([]sharedQuizItem, 0, len(rows))
	if len(rows) > 0 {
		ids := make([]uint, len(rows))
		ownerIDs := make([]uint, 0, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
			ownerIDs = append(ownerIDs, r.HostID)
		}

		type countRow struct {
			QuizID uint  `gorm:"column:quiz_id"`
			Cnt    int64 `gorm:"column:cnt"`
		}
		var counts []countRow
		_ = db.DB.Model(&model.Question{}).
			Select("quiz_id, COUNT(*) AS cnt").
			Where("quiz_id IN ?", ids).
			Group("quiz_id").
			Scan(&counts).Error
		countOf := map[uint]int64{}
		for _, r := range counts {
			countOf[r.QuizID] = r.Cnt
		}

		var owners []model.User
		_ = db.DB.Select("id, nickname").Where("id IN ?", ownerIDs).Find(&owners).Error
		names := map[uint]string{}
		for _, o := range owners {
			names[o.ID] = o.Nickname
		}

		// One plan lookup per distinct owner on the page, not per row.
		canShare := map[uint]bool{}
		for _, r := range rows {
			if _, seen := canShare[r.HostID]; !seen {
				canShare[r.HostID] = ownerCanShare(r.HostID)
			}
		}

		for _, r := range rows {
			out = append(out, sharedQuizItem{
				ID:            r.ID,
				Title:         r.Title,
				Description:   r.Description,
				QuestionCount: countOf[r.ID],
				Role:          r.Role,
				Active:        canShare[r.HostID],
				AuthorName:    authorDisplayName(names[r.HostID]),
				CreatedAt:     r.CreatedAt,
				UpdatedAt:     r.UpdatedAt,
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

// authorDisplayName keeps the email out of everything an editor sees. Nickname
// is optional on User, so an owner who never set one is shown generically
// rather than as a blank byline.
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
