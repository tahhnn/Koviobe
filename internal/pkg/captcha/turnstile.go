// Package captcha verifies Cloudflare Turnstile tokens.
//
// It guards the endpoints that make us send mail to an address the caller
// types (signup, password reset). The per-IP and per-recipient limits bound
// what one host or one inbox can cost; they do not stop a script spread over
// many IPs mailing many different addresses, which fills the outgoing queue and
// delays real users' codes. A human check per request does.
package captcha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/pkg/notify"
)

const verifyTimeout = 5 * time.Second

// Swappable in tests.
var (
	verifyURL  = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	httpClient = &http.Client{Timeout: verifyTimeout}
)

// ErrFailed means Cloudflare looked at the token and said no (missing,
// expired, reused, wrong action). The English text is the i18n catalog key.
var ErrFailed = errors.New("Human verification failed. Please try again.")

// Enabled reports whether a secret key is configured. Without one the check is
// skipped, so the feature can ship before the Cloudflare keys exist.
func Enabled() bool {
	return config.AppConfig != nil && config.AppConfig.TurnstileSecretKey != ""
}

// SiteKey is the public key the browser widget renders with.
func SiteKey() string {
	if !Enabled() {
		return ""
	}
	return config.AppConfig.TurnstileSiteKey
}

type verifyResponse struct {
	Success    bool     `json:"success"`
	Action     string   `json:"action"`
	Hostname   string   `json:"hostname"`
	ErrorCodes []string `json:"error-codes"`
}

// Verify checks token for the given action ("register", "forgot_password").
//
// The action must match what the widget was rendered with, so a token solved
// on one form cannot be spent on another.
//
// When Cloudflare cannot be reached (timeout, 5xx) the request is let through
// and an alert is raised. An attacker cannot cause that outage, so failing open
// costs no protection against them, while failing closed would turn a
// Cloudflare incident into "nobody can sign up". The other limits stay on.
func Verify(ctx context.Context, token, action, remoteIP string) error {
	if !Enabled() {
		return nil
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 2048 {
		return ErrFailed
	}

	form := url.Values{}
	form.Set("secret", config.AppConfig.TurnstileSecretKey)
	form.Set("response", token)
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}

	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, verifyURL, strings.NewReader(form.Encode()))
	if err != nil {
		return failOpen(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return failOpen(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return failOpen(fmt.Errorf("siteverify HTTP %d", resp.StatusCode))
	}

	var out verifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return failOpen(fmt.Errorf("siteverify decode: %w", err))
	}
	if !out.Success {
		// A bad secret is our misconfiguration, not the visitor's fault —
		// every request will fail until it is fixed, so say so loudly.
		for _, code := range out.ErrorCodes {
			if code == "invalid-input-secret" || code == "missing-input-secret" {
				notify.P1("turnstile_bad_secret", "TURNSTILE_SECRET_KEY sai (%s) — đăng ký và quên mật khẩu đang bị chặn hết.", code)
			}
		}
		return ErrFailed
	}
	if action != "" && out.Action != action {
		return ErrFailed
	}
	return nil
}

func failOpen(err error) error {
	log.Printf("[captcha] Turnstile unreachable, letting request through: %v", err)
	notify.P1("turnstile_unreachable", "Không gọi được Cloudflare Turnstile: %v — đang cho qua không kiểm tra captcha.", err)
	return nil
}
