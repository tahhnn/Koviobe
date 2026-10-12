package captcha

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/quizzzone/backend/internal/config"
)

func withServer(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.PostForm.Get("secret") != "s3cret" {
			t.Errorf("siteverify called without the secret")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	oldURL, oldCfg := verifyURL, config.AppConfig
	verifyURL = srv.URL
	config.AppConfig = &config.Config{TurnstileSecretKey: "s3cret", TurnstileSiteKey: "site"}
	t.Cleanup(func() { verifyURL, config.AppConfig = oldURL, oldCfg })
}

func TestVerifyDisabledWithoutSecret(t *testing.T) {
	old := config.AppConfig
	config.AppConfig = &config.Config{TurnstileSiteKey: "site"}
	defer func() { config.AppConfig = old }()
	if err := Verify(context.Background(), "", "register", ""); err != nil {
		t.Fatalf("no secret must skip the check, got %v", err)
	}
	if SiteKey() != "" {
		t.Fatal("site key must not be published while the check is off")
	}
}

func TestVerifySuccessWithMatchingAction(t *testing.T) {
	withServer(t, 200, `{"success":true,"action":"register"}`)
	if err := Verify(context.Background(), "tok", "register", "1.2.3.4"); err != nil {
		t.Fatalf("want ok, got %v", err)
	}
}

func TestVerifyRejectsWrongAction(t *testing.T) {
	withServer(t, 200, `{"success":true,"action":"register"}`)
	if err := Verify(context.Background(), "tok", "forgot_password", ""); err != ErrFailed {
		t.Fatalf("token solved for another form must fail, got %v", err)
	}
}

func TestVerifyRejectsFailedAndEmpty(t *testing.T) {
	withServer(t, 200, `{"success":false,"error-codes":["timeout-or-duplicate"]}`)
	if err := Verify(context.Background(), "tok", "register", ""); err != ErrFailed {
		t.Fatalf("want ErrFailed, got %v", err)
	}
	if err := Verify(context.Background(), "  ", "register", ""); err != ErrFailed {
		t.Fatalf("empty token must fail, got %v", err)
	}
}

func TestVerifyFailsOpenOnCloudflareOutage(t *testing.T) {
	withServer(t, 503, `oops`)
	if err := Verify(context.Background(), "tok", "register", ""); err != nil {
		t.Fatalf("Cloudflare 5xx must not lock users out, got %v", err)
	}
}
