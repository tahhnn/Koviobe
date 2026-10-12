package handler

// Database tests for email sharing. They need a real Postgres (row locks, the
// FK cascade, ILIKE) and are skipped unless SHARING_TEST_DSN points at a
// throwaway database — never a real one: every table used here is truncated.
//
//	docker run -d --name share-pg -e POSTGRES_PASSWORD=x -p 55433:5432 postgres:16-alpine
//	SHARING_TEST_DSN='host=... user=postgres password=x dbname=postgres sslmode=disable' go test ./internal/handler/ -run DB

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/license"
)

func setupSharingDB(t *testing.T) {
	t.Helper()
	dsn := os.Getenv("SHARING_TEST_DSN")
	if dsn == "" {
		t.Skip("SHARING_TEST_DSN not set")
	}
	g, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	db.DB = g
	config.AppConfig = &config.Config{PublicAppURL: "https://app.test"}
	db.AutoMigrate()
	g.Exec(`TRUNCATE users, quizzes, questions, rooms, quiz_collaborators, audit_logs,
		subscriptions, system_settings RESTART IDENTITY CASCADE`)
	// Plans survive the truncate; put back what earlier tests (the cap test,
	// the migration test) changed, so each test starts from the seeded values.
	g.Model(&model.PricingPlan{}).Where("id = ?", "pro").Updates(map[string]any{
		"allow_quiz_sharing": true, "max_collaborators_per_quiz": model.DefaultProCollaboratorsPerQuiz})
	g.Model(&model.PricingPlan{}).Where("id = ?", "free").Updates(map[string]any{
		"allow_quiz_sharing": false, "max_collaborators_per_quiz": 0})
	// Enforcement on: the plan gates are what is under test.
	if err := license.SetEnforcement(true, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = license.SetEnforcement(false, 0) })
}

func shareUser(t *testing.T, email, plan string) uint {
	t.Helper()
	u := model.User{Email: email, Password: "x", IsActive: true, Nickname: "N-" + email}
	if err := db.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.DB.Create(&model.Subscription{UserID: u.ID, PlanID: plan, Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func shareQuiz(t *testing.T, ownerID uint, nQuestions int) uint {
	t.Helper()
	q := model.Quiz{HostID: ownerID, Title: "Sự kiện khách A", Description: "nội bộ"}
	if err := db.DB.Create(&q).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < nQuestions; i++ {
		db.DB.Create(&model.Question{QuizID: q.ID, Content: fmt.Sprintf("Q%d", i), Options: `[]`, CorrectAnswer: "A"})
	}
	return q.ID
}

func shareRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		var uid uint
		fmt.Sscan(c.GetHeader("X-UID"), &uid)
		c.Set("user_id", uid)
	})
	r.GET("/quizzes/shared-with-me", ListSharedWithMe)
	r.GET("/quizzes/:id", GetQuiz)
	r.PUT("/quizzes/:id", UpdateQuiz)
	r.DELETE("/quizzes/:id", DeleteQuiz)
	r.POST("/rooms", CreateRoom)
	r.GET("/quizzes/:id/collaborators", ListQuizCollaborators)
	r.POST("/quizzes/:id/collaborators", AddQuizCollaborator)
	r.DELETE("/quizzes/:id/collaborators/me", LeaveSharedQuiz)
	r.DELETE("/quizzes/:id/collaborators/:cid", RemoveQuizCollaborator)
	return r
}

func call(t *testing.T, r *gin.Engine, uid uint, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-UID", fmt.Sprint(uid))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func updateBody(n int) map[string]any {
	qs := make([]map[string]any, n)
	for i := range qs {
		qs[i] = map[string]any{
			"content": fmt.Sprintf("edited %d", i), "type": "multiple_choice",
			"options":        []map[string]any{{"id": "A", "text": "a", "isCorrect": true}, {"id": "B", "text": "b"}},
			"correct_answer": "A", "duration": 20, "points": 1000, "order": i,
		}
	}
	return map[string]any{"title": "edited", "questions": qs}
}

func TestDBStrangerCannotReachAQuiz(t *testing.T) {
	setupSharingDB(t)
	r := shareRouter()
	owner := shareUser(t, "owner@x.vn", "pro")
	stranger := shareUser(t, "stranger@x.vn", "pro")
	qid := shareQuiz(t, owner, 2)
	// The retired flag must open nothing.
	db.DB.Model(&model.Quiz{}).Where("id = ?", qid).Updates(map[string]any{"is_public": true, "allow_edit": true})

	if code, _ := call(t, r, stranger, "GET", fmt.Sprintf("/quizzes/%d", qid), nil); code != 404 {
		t.Fatalf("stranger GET = %d, want 404", code)
	}
	if code, _ := call(t, r, stranger, "PUT", fmt.Sprintf("/quizzes/%d", qid), updateBody(1)); code != 404 {
		t.Fatalf("stranger PUT = %d, want 404", code)
	}
	if code, _ := call(t, r, stranger, "POST", fmt.Sprintf("/rooms?quiz_id=%d", qid), nil); code != 404 {
		t.Fatalf("stranger CreateRoom = %d, want 404", code)
	}
}

func TestDBFreeOwnerCannotShare(t *testing.T) {
	setupSharingDB(t)
	r := shareRouter()
	owner := shareUser(t, "owner@x.vn", "free")
	qid := shareQuiz(t, owner, 1)
	code, body := call(t, r, owner, "POST", fmt.Sprintf("/quizzes/%d/collaborators", qid), map[string]any{"email": "ed@x.vn"})
	if code != 403 || body["feature"] != "allow_quiz_sharing" {
		t.Fatalf("free owner share = %d %v, want 403 allow_quiz_sharing", code, body)
	}
}

// The scenario the feature exists for: a Pro owner shares by email, and a Free
// editor edits a quiz bigger than their own plan would allow.
func TestDBFreeEditorEditsAProOwnersQuiz(t *testing.T) {
	setupSharingDB(t)
	r := shareRouter()
	owner := shareUser(t, "owner@x.vn", "pro")
	editor := shareUser(t, "Editor@X.vn", "free") // mixed case on purpose
	qid := shareQuiz(t, owner, 3)

	code, body := call(t, r, owner, "POST", fmt.Sprintf("/quizzes/%d/collaborators", qid), map[string]any{"email": " editor@x.VN "})
	if code != 201 || body["status"] != "active" || body["email"] != "editor@x.vn" {
		t.Fatalf("share = %d %v, want 201 active editor@x.vn", code, body)
	}

	code, body = call(t, r, editor, "GET", fmt.Sprintf("/quizzes/%d", qid), nil)
	if code != 200 || body["can_edit"] != true || body["can_host"] != false || body["can_copy"] != false || body["is_owner"] != false {
		t.Fatalf("editor GET = %d %v", code, body)
	}
	if _, leaked := body["is_public"]; leaked {
		t.Fatal("GetQuiz still returns is_public")
	}
	if body["owner_name"] != "N-owner@x.vn" {
		t.Fatalf("owner_name = %v", body["owner_name"])
	}

	// Free plan has max_questions_per_quiz = 0; the owner's Pro limit applies.
	if code, body := call(t, r, editor, "PUT", fmt.Sprintf("/quizzes/%d", qid), updateBody(5)); code != 200 {
		t.Fatalf("editor PUT = %d %v, want 200", code, body)
	}

	if code, _ := call(t, r, editor, "POST", fmt.Sprintf("/rooms?quiz_id=%d", qid), nil); code == 200 || code == 201 {
		t.Fatalf("editor opened a room: %d", code)
	}
	if code, _ := call(t, r, editor, "DELETE", fmt.Sprintf("/quizzes/%d", qid), nil); code != 404 {
		t.Fatalf("editor DELETE quiz = %d, want 404", code)
	}
	if code, _ := call(t, r, editor, "POST", fmt.Sprintf("/quizzes/%d/collaborators", qid), map[string]any{"email": "x@x.vn"}); code != 404 {
		t.Fatalf("editor re-share = %d, want 404", code)
	}
	if code, _ := call(t, r, editor, "GET", fmt.Sprintf("/quizzes/%d/collaborators", qid), nil); code != 404 {
		t.Fatalf("editor list collaborators = %d, want 404", code)
	}

	code, body = call(t, r, editor, "GET", "/quizzes/shared-with-me", nil)
	list, _ := body["quizzes"].([]any)
	if code != 200 || len(list) != 1 {
		t.Fatalf("shared-with-me = %d %v", code, body)
	}
	item := list[0].(map[string]any)
	if item["active"] != true || item["role"] != "editor" || item["question_count"] != float64(5) {
		t.Fatalf("shared-with-me item = %v", item)
	}
	if _, leaked := item["host_id"]; leaked {
		t.Fatal("shared-with-me leaks host_id")
	}

	// The owner's other quizzes stay invisible.
	other := shareQuiz(t, owner, 1)
	if code, _ := call(t, r, editor, "GET", fmt.Sprintf("/quizzes/%d", other), nil); code != 404 {
		t.Fatalf("editor reached an unshared quiz: %d", code)
	}
}

func TestDBPendingInviteIsClaimedOnSignup(t *testing.T) {
	setupSharingDB(t)
	r := shareRouter()
	owner := shareUser(t, "owner@x.vn", "pro")
	qid := shareQuiz(t, owner, 1)

	code, body := call(t, r, owner, "POST", fmt.Sprintf("/quizzes/%d/collaborators", qid), map[string]any{"email": "new@x.vn"})
	if code != 201 || body["status"] != "pending" {
		t.Fatalf("invite = %d %v, want 201 pending", code, body)
	}
	// Idempotent: a second invite returns the row and records no second send.
	if code, _ := call(t, r, owner, "POST", fmt.Sprintf("/quizzes/%d/collaborators", qid), map[string]any{"email": "NEW@x.vn"}); code != 200 {
		t.Fatalf("repeat invite = %d, want 200", code)
	}
	var sends int64
	db.DB.Model(&model.AuditLog{}).Where("action = ?", "quiz_share_add").Count(&sends)
	if sends != 1 {
		t.Fatalf("repeat invite recorded %d sends, want 1", sends)
	}

	newbie := shareUser(t, "new@x.vn", "free")
	if code, _ := call(t, r, newbie, "GET", fmt.Sprintf("/quizzes/%d", qid), nil); code != 404 {
		t.Fatalf("unclaimed invite already granted access: %d", code)
	}
	claimPendingInvites(newbie, "New@x.vn")
	if code, _ := call(t, r, newbie, "GET", fmt.Sprintf("/quizzes/%d", qid), nil); code != 200 {
		t.Fatalf("claimed invite GET = %d, want 200", code)
	}
}

func TestDBRevokeAndLeaveTakeEffectImmediately(t *testing.T) {
	setupSharingDB(t)
	r := shareRouter()
	owner := shareUser(t, "owner@x.vn", "pro")
	ed1 := shareUser(t, "ed1@x.vn", "free")
	ed2 := shareUser(t, "ed2@x.vn", "free")
	qid := shareQuiz(t, owner, 1)

	_, b1 := call(t, r, owner, "POST", fmt.Sprintf("/quizzes/%d/collaborators", qid), map[string]any{"email": "ed1@x.vn"})
	call(t, r, owner, "POST", fmt.Sprintf("/quizzes/%d/collaborators", qid), map[string]any{"email": "ed2@x.vn"})

	if code, _ := call(t, r, owner, "DELETE", fmt.Sprintf("/quizzes/%d/collaborators/%v", qid, b1["id"]), nil); code != 200 {
		t.Fatalf("revoke = %d", code)
	}
	if code, _ := call(t, r, ed1, "GET", fmt.Sprintf("/quizzes/%d", qid), nil); code != 404 {
		t.Fatalf("revoked editor GET = %d, want 404", code)
	}
	if code, _ := call(t, r, ed2, "DELETE", fmt.Sprintf("/quizzes/%d/collaborators/me", qid), nil); code != 200 {
		t.Fatalf("leave = %d", code)
	}
	if code, _ := call(t, r, ed2, "GET", fmt.Sprintf("/quizzes/%d", qid), nil); code != 404 {
		t.Fatalf("editor who left GET = %d, want 404", code)
	}
}

func TestDBCollaboratorCapAndOwnAddress(t *testing.T) {
	setupSharingDB(t)
	r := shareRouter()
	owner := shareUser(t, "owner@x.vn", "pro")
	qid := shareQuiz(t, owner, 1)
	db.DB.Model(&model.PricingPlan{}).Where("id = ?", "pro").Update("max_collaborators_per_quiz", 2)

	if code, _ := call(t, r, owner, "POST", fmt.Sprintf("/quizzes/%d/collaborators", qid), map[string]any{"email": "OWNER@x.vn"}); code != 400 {
		t.Fatalf("self invite = %d, want 400", code)
	}
	for i := 0; i < 2; i++ {
		if code, _ := call(t, r, owner, "POST", fmt.Sprintf("/quizzes/%d/collaborators", qid), map[string]any{"email": fmt.Sprintf("e%d@x.vn", i)}); code != 201 {
			t.Fatalf("invite %d = %d", i, code)
		}
	}
	if code, body := call(t, r, owner, "POST", fmt.Sprintf("/quizzes/%d/collaborators", qid), map[string]any{"email": "e9@x.vn"}); code != 403 || body["limit"] != float64(2) {
		t.Fatalf("over cap = %d %v, want 403 limit 2", code, body)
	}
	if code, _ := call(t, r, owner, "POST", fmt.Sprintf("/quizzes/%d/collaborators", qid), map[string]any{"email": "Mai <m@x.vn>"}); code != 400 {
		t.Fatalf("display-name address = %d, want 400", code)
	}
}

// D4: an owner who loses Pro suspends their editors; renewing restores them.
func TestDBOwnerDowngradeSuspendsEditors(t *testing.T) {
	setupSharingDB(t)
	r := shareRouter()
	owner := shareUser(t, "owner@x.vn", "pro")
	editor := shareUser(t, "ed@x.vn", "free")
	qid := shareQuiz(t, owner, 1)
	call(t, r, owner, "POST", fmt.Sprintf("/quizzes/%d/collaborators", qid), map[string]any{"email": "ed@x.vn"})

	db.DB.Model(&model.Subscription{}).Where("user_id = ?", owner).Update("plan_id", "free")
	if code, _ := call(t, r, editor, "GET", fmt.Sprintf("/quizzes/%d", qid), nil); code != 404 {
		t.Fatalf("editor of a downgraded owner GET = %d, want 404", code)
	}
	_, body := call(t, r, editor, "GET", "/quizzes/shared-with-me", nil)
	if list, _ := body["quizzes"].([]any); len(list) != 1 || list[0].(map[string]any)["active"] != false {
		t.Fatalf("suspended grant should list as inactive: %v", body)
	}

	db.DB.Model(&model.Subscription{}).Where("user_id = ?", owner).Update("plan_id", "pro")
	if code, _ := call(t, r, editor, "GET", fmt.Sprintf("/quizzes/%d", qid), nil); code != 200 {
		t.Fatalf("editor after renewal GET = %d, want 200", code)
	}
}

func TestDBDeletingAQuizDropsItsGrants(t *testing.T) {
	setupSharingDB(t)
	r := shareRouter()
	owner := shareUser(t, "owner@x.vn", "pro")
	shareUser(t, "ed@x.vn", "free")
	qid := shareQuiz(t, owner, 1)
	call(t, r, owner, "POST", fmt.Sprintf("/quizzes/%d/collaborators", qid), map[string]any{"email": "ed@x.vn"})

	if code, _ := call(t, r, owner, "DELETE", fmt.Sprintf("/quizzes/%d", qid), nil); code != 200 {
		t.Fatalf("delete = %d", code)
	}
	var left int64
	db.DB.Model(&model.QuizCollaborator{}).Where("quiz_id = ?", qid).Count(&left)
	if left != 0 {
		t.Fatalf("%d grants survived the quiz", left)
	}
}

// The one-time steps run on the boot that introduces sharing, and only then.
func TestDBMigrationRetiresPublicQuizzesAndBackfillsProOnce(t *testing.T) {
	setupSharingDB(t)
	owner := shareUser(t, "owner@x.vn", "pro")
	qid := shareQuiz(t, owner, 1)

	// Back to the pre-sharing schema.
	db.DB.Exec(`DROP TABLE quiz_collaborators`)
	db.DB.Exec(`ALTER TABLE pricing_plans DROP COLUMN allow_quiz_sharing, DROP COLUMN max_collaborators_per_quiz`)
	db.DB.Model(&model.Quiz{}).Where("id = ?", qid).Updates(map[string]any{"is_public": true, "allow_edit": true})

	db.AutoMigrate()

	var q model.Quiz
	db.DB.First(&q, qid)
	if q.IsPublic || q.AllowEdit {
		t.Fatal("migration left a public quiz public")
	}
	var pro model.PricingPlan
	db.DB.First(&pro, "id = ?", "pro")
	if !pro.AllowQuizSharing || pro.MaxCollaboratorsPerQuiz != model.DefaultProCollaboratorsPerQuiz {
		t.Fatalf("pro not backfilled: %+v", pro)
	}
	var free model.PricingPlan
	db.DB.First(&free, "id = ?", "free")
	if free.AllowQuizSharing {
		t.Fatal("free plan got sharing")
	}
	var audits int64
	db.DB.Model(&model.AuditLog{}).Where("action = ?", "system_unshare_public_quiz").Count(&audits)
	if audits != 1 {
		t.Fatalf("expected 1 retire audit row, got %d", audits)
	}

	// An admin turns sharing off on Pro; the next boot must not undo it.
	db.DB.Model(&model.PricingPlan{}).Where("id = ?", "pro").Update("allow_quiz_sharing", false)
	db.AutoMigrate()
	db.DB.First(&pro, "id = ?", "pro")
	if pro.AllowQuizSharing {
		t.Fatal("second boot re-applied the backfill over the admin's choice")
	}
}
