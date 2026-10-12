package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/config"
)

// The IPN marks orders paid, so everything short of the exact secret must be
// turned away before the body is read — including when no secret is set.
func TestGatewayIPNRejectsMissingOrWrongSecret(t *testing.T) {
	gin.SetMode(gin.TestMode)
	prev := config.AppConfig
	defer func() { config.AppConfig = prev }()

	cases := map[string]struct{ configured, header string }{
		"no header":          {"secret-0123456789abcdefghij", ""},
		"wrong secret":       {"secret-0123456789abcdefghij", "secret-0123456789abcdefghiX"},
		"prefix of secret":   {"secret-0123456789abcdefghij", "secret-0123456789"},
		"nothing configured": {"", ""},
	}
	for name, tc := range cases {
		config.AppConfig = &config.Config{SePayPGSecretKey: tc.configured}
		r := gin.New()
		r.POST("/ipn", SePayGatewayIPN)
		req := httptest.NewRequest(http.MethodPost, "/ipn", strings.NewReader(`{}`))
		if tc.header != "" {
			req.Header.Set("X-Secret-Key", tc.header)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status %d, want 401", name, w.Code)
		}
	}

	// The right secret gets past auth; an empty body is then a 400, not a 401.
	config.AppConfig = &config.Config{SePayPGSecretKey: "secret-0123456789abcdefghij"}
	r := gin.New()
	r.POST("/ipn", SePayGatewayIPN)
	req := httptest.NewRequest(http.MethodPost, "/ipn", strings.NewReader(`{}`))
	req.Header.Set("X-Secret-Key", "secret-0123456789abcdefghij")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("authorized empty body: status %d, want 400", w.Code)
	}
}
