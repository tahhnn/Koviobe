package handler

// Integration tests for the reset-link flow. They need a real Postgres and a
// real Redis (GETDEL, the Lua queue push) and are skipped unless RESET_TEST_DSN
// and RESET_TEST_REDIS point at throwaway instances — never real ones: the
// users table is truncated and Redis is flushed.
//
//	RESET_TEST_DSN='host=... user=postgres password=x dbname=postgres sslmode=disable' \
//	RESET_TEST_REDIS='host:6379' go test ./internal/handler/ -run Reset

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/quizzzone/backend/internal/cache"
	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/email"
)

type sentMail struct{ to, subject, html string }

func setupResetDB(t *testing.T) chan sentMail {
	t.Helper()
	dsn, raddr := os.Getenv("RESET_TEST_DSN"), os.Getenv("RESET_TEST_REDIS")
	if dsn == "" || raddr == "" {
		t.Skip("RESET_TEST_DSN / RESET_TEST_REDIS not set")
	}
	g, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	db.DB = g
	config.AppConfig = &config.Config{PublicAppURL: "https://app.test"}
	db.AutoMigrate()
	g.Exec(`TRUNCATE users, audit_logs RESTART IDENTITY CASCADE`)

	cache.RDB = redis.NewClient(&redis.Options{Addr: raddr})
	cache.RDB.FlushDB(context.Background())

	sent := make(chan sentMail, 10)
	restore := email.SetSendFuncForTest(func(to, subject, html string) error {
		sent <- sentMail{to, subject, html}
		return nil
	})
	t.Cleanup(restore)
	email.StartWorkers()
	return sent
}

func resetUser(t *testing.T, addr, password string) uint {
	t.Helper()
	h, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	u := model.User{Email: addr, Password: string(h), IsActive: true, Nickname: "N"}
	if err := db.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func post(t *testing.T, h gin.HandlerFunc, body any) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/", bytes.NewReader(b))
	c.Request.Header.Set("Content-Type", "application/json")
	h(c)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

var linkRe = regexp.MustCompile(`https://app\.test/reset-password#token=([A-Za-z0-9_-]+)`)

func waitResetMail(t *testing.T, sent chan sentMail) (sentMail, string) {
	t.Helper()
	select {
	case m := <-sent:
		match := linkRe.FindStringSubmatch(m.html)
		if match == nil {
			t.Fatalf("mail has no reset link: %s", m.html)
		}
		return m, match[1]
	case <-time.After(5 * time.Second):
		t.Fatal("no reset mail sent")
		return sentMail{}, ""
	}
}

func TestResetUnknownEmailLooksTheSameAndSendsNothing(t *testing.T) {
	sent := setupResetDB(t)
	code, out := post(t, ForgotPassword, map[string]string{"email": "nobody@example.com"})
	if code != 200 || out["message"] != resetSentMessage {
		t.Fatalf("got %d %v", code, out)
	}
	select {
	case m := <-sent:
		t.Fatalf("mailed an unknown address: %+v", m)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestResetLinkFlow(t *testing.T) {
	sent := setupResetDB(t)
	uid := resetUser(t, "u@example.com", "oldpassword")

	code, _ := post(t, ForgotPassword, map[string]string{"email": "U@Example.com"})
	if code != 200 {
		t.Fatalf("forgot: %d", code)
	}
	m, token := waitResetMail(t, sent)
	if m.to != "u@example.com" {
		t.Fatalf("mailed %q", m.to)
	}

	// Only the hash is a key; the raw token appears nowhere in Redis keys.
	keys, _ := cache.RDB.Keys(context.Background(), "*").Result()
	for _, k := range keys {
		if strings.Contains(k, token) {
			t.Fatalf("raw token stored as key %q", k)
		}
	}

	if code, _ := post(t, CheckResetToken, map[string]string{"token": token}); code != 200 {
		t.Fatalf("check live token: %d", code)
	}
	if code, _ := post(t, ResetPassword, map[string]string{"token": token, "new_password": "short"}); code != 400 {
		t.Fatalf("short password accepted: %d", code)
	}
	// A rejected attempt must not burn the link.
	if code, _ := post(t, CheckResetToken, map[string]string{"token": token}); code != 200 {
		t.Fatalf("validation failure consumed the token: %d", code)
	}
	if code, out := post(t, ResetPassword, map[string]string{"token": token, "new_password": "newpassword1"}); code != 200 {
		t.Fatalf("reset: %d %v", code, out)
	}

	var u model.User
	db.DB.First(&u, uid)
	if bcrypt.CompareHashAndPassword([]byte(u.Password), []byte("newpassword1")) != nil {
		t.Fatal("password not changed")
	}

	// Single use.
	if code, _ := post(t, ResetPassword, map[string]string{"token": token, "new_password": "another-pass"}); code != 400 {
		t.Fatalf("token reused: %d", code)
	}
	if code, _ := post(t, CheckResetToken, map[string]string{"token": token}); code != 400 {
		t.Fatalf("used token still checks out: %d", code)
	}
}

func TestResetNewRequestRevokesOldLinkAndCooldownApplies(t *testing.T) {
	sent := setupResetDB(t)
	resetUser(t, "v@example.com", "oldpassword")

	post(t, ForgotPassword, map[string]string{"email": "v@example.com"})
	_, first := waitResetMail(t, sent)

	if code, _ := post(t, ForgotPassword, map[string]string{"email": "v@example.com"}); code != 429 {
		t.Fatalf("second request within a minute: %d", code)
	}

	cache.RDB.Del(context.Background(), resetCooldownKey("v@example.com"))
	post(t, ForgotPassword, map[string]string{"email": "v@example.com"})
	_, second := waitResetMail(t, sent)

	if code, _ := post(t, CheckResetToken, map[string]string{"token": first}); code != 400 {
		t.Fatalf("older link still valid: %d", code)
	}
	if code, _ := post(t, CheckResetToken, map[string]string{"token": second}); code != 200 {
		t.Fatalf("newest link invalid: %d", code)
	}
}
