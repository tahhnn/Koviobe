package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/pkg/license"
)

func init() { gin.SetMode(gin.TestMode) }

// freePlan is a host with no branding and no solo mode.
var freePlan = license.Entitlements{
	PlanID:               "free",
	PlanName:             "Free",
	AllowPlayerPaced:     false,
	AllowCustomBranding:  false,
	AllowRemoveWatermark: false,
}

var proPlan = license.Entitlements{
	PlanID:               "pro",
	PlanName:             "Pro",
	AllowPlayerPaced:     true,
	AllowCustomBranding:  true,
	AllowRemoveWatermark: true,
}

func vet(t *testing.T, ents license.Entitlements, enforcing bool, raw string) (string, bool, int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/quizzes/1", strings.NewReader("{}"))

	out, ok := vetThemeConfig(c, ents, enforcing, raw)

	var body map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
	}
	return out, ok, rec.Code, body
}

// The gate only runs when enforcement is on; without this the free-plan cases
// below would pass for the wrong reason.
func TestVetThemeConfigStripsOffsiteImagesEvenWithoutEnforcement(t *testing.T) {
	out, ok, _, _ := vet(t, proPlan, false, `{"bg_host_url":"https://evil.example.com/kv.png"}`)
	if !ok {
		t.Fatal("a Pro host should not be blocked, only cleaned")
	}
	if strings.Contains(out, "evil.example.com") {
		t.Fatalf("an off-site background survived the vet: %s", out)
	}
}

func TestVetThemeConfigDropsUnknownKeys(t *testing.T) {
	out, ok, _, _ := vet(t, proPlan, false, `{"game_mode":"host_paced","injected":"<script>"}`)
	if !ok {
		t.Fatal("expected the theme to be accepted")
	}
	if strings.Contains(out, "injected") || strings.Contains(out, "<script>") {
		t.Fatalf("an unknown key was echoed back into storage: %s", out)
	}
}

func TestVetThemeConfigAcceptsALocalBackground(t *testing.T) {
	out, ok, _, _ := vet(t, proPlan, false, `{"bg_host_url":"/uploads/img-u1-123-abc.webp","bg_overlay":0.6}`)
	if !ok {
		t.Fatal("a local upload should be accepted")
	}
	if !strings.Contains(out, "/uploads/img-u1-123-abc.webp") {
		t.Fatalf("the background was dropped: %s", out)
	}
}

// The regression this whole change exists for: CreateQuiz checked the plan and
// UpdateQuiz did not, so the same body rejected as a POST was accepted as a
// PUT. Both now route through vetThemeConfig, so one test covers both.
func TestVetThemeConfigBlocksBrandingOnFreePlan(t *testing.T) {

	out, ok, code, body := vet(t, freePlan, true, `{"bg_host_url":"/uploads/img-u1-123-abc.webp"}`)
	if ok {
		t.Fatalf("a free host got a custom background: %s", out)
	}
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", code)
	}
	if body["feature"] != "allow_custom_branding" {
		t.Fatalf("response did not name the feature: %v", body)
	}
	// The 403 should say which field tripped it, not just "branding".
	fields, _ := body["fields"].([]any)
	if len(fields) != 1 || fields[0] != "bg_host_url" {
		t.Fatalf("response did not name the offending field: %v", body["fields"])
	}
}

// A colour is not branding. Blocking it would tell a free host to upgrade for
// something the plan never restricted.
func TestVetThemeConfigAllowsPlainThemeOnFreePlan(t *testing.T) {

	if _, ok, _, _ := vet(t, freePlan, true, `{"primary_color":"#e85d4c"}`); !ok {
		t.Fatal("a free host was blocked from picking a colour")
	}
}

// An off-site URL is stripped before the gate runs, so a free host who somehow
// submits one gets a clean save, not an upsell for an image they are not
// getting.
func TestVetThemeConfigDoesNotUpsellOnAStrippedURL(t *testing.T) {

	out, ok, _, _ := vet(t, freePlan, true, `{"bg_host_url":"https://evil.example.com/kv.png"}`)
	if !ok {
		t.Fatalf("the host was told to upgrade for a URL that was discarded: %s", out)
	}
}

func TestVetThemeConfigBlocksSoloAndWatermarkOnFreePlan(t *testing.T) {

	if _, ok, code, _ := vet(t, freePlan, true, `{"game_mode":"player_paced"}`); ok || code != http.StatusForbidden {
		t.Fatalf("free host got player-paced mode (ok=%v code=%d)", ok, code)
	}
	if _, ok, code, _ := vet(t, freePlan, true, `{"remove_watermark":true}`); ok || code != http.StatusForbidden {
		t.Fatalf("free host removed the watermark (ok=%v code=%d)", ok, code)
	}
}
