package theme

import (
	"strings"
	"testing"
)

func TestSanitizeRejectsOffsiteImages(t *testing.T) {
	cases := []string{
		`{"bg_host_url":"https://evil.example.com/x.png"}`,
		`{"bg_host_url":"//evil.example.com/x.png"}`,
		`{"bg_host_url":"http://localhost:9/x.png"}`,
		`{"bg_host_url":"/uploads/../../etc/passwd"}`,
		`{"bg_host_url":"/uploads/"}`,
		`{"logo_url":"https://evil.example.com/logo.png"}`,
		`{"bg_player_url":"data:image/png;base64,AAAA"}`,
	}
	for _, in := range cases {
		out, err := Sanitize(in)
		if err != nil {
			t.Fatalf("Sanitize(%s) errored: %v", in, err)
		}
		if strings.Contains(out, "evil.example.com") ||
			strings.Contains(out, "bg_host_url") && strings.Contains(out, "..") ||
			strings.Contains(out, "data:") ||
			strings.Contains(out, "localhost") {
			t.Errorf("Sanitize(%s) kept a rejected URL: %s", in, out)
		}
	}
}

func TestSanitizeKeepsLocalUploads(t *testing.T) {
	in := `{"bg_host_url":"/uploads/img-u1-123-abc.webp"}`
	out, err := Sanitize(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "/uploads/img-u1-123-abc.webp") {
		t.Errorf("a legitimate upload was dropped: %s", out)
	}
}

func TestSanitizeDropsUnknownKeys(t *testing.T) {
	in := `{"game_mode":"player_paced","evil_key":"payload","__proto__":{"a":1}}`
	out, err := Sanitize(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "evil_key") || strings.Contains(out, "__proto__") {
		t.Errorf("unknown key survived: %s", out)
	}
	if !strings.Contains(out, "player_paced") {
		t.Errorf("known key was lost: %s", out)
	}
}

func TestSanitizeClampsOverlay(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		// Below the floor with an image present: raised to the floor.
		{`{"bg_host_url":"/uploads/a.png","bg_overlay":0.01}`, MinOverlay},
		// Image but no overlay: the default.
		{`{"bg_host_url":"/uploads/a.png"}`, DefaultOverlay},
		// Above 1: clamped.
		{`{"bg_host_url":"/uploads/a.png","bg_overlay":9}`, 1},
		// Negative: treated as unset, so the default.
		{`{"bg_host_url":"/uploads/a.png","bg_overlay":-5}`, DefaultOverlay},
		// No image: no scrim at all.
		{`{"bg_overlay":0.9}`, 0},
	}
	for _, c := range cases {
		out, err := Sanitize(c.in)
		if err != nil {
			t.Fatal(err)
		}
		got := Parse(out).BgOverlay
		if got != c.want {
			t.Errorf("Sanitize(%s) overlay = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSanitizeValidatesColorAndMode(t *testing.T) {
	out, _ := Sanitize(`{"primary_color":"red; background:url(x)","game_mode":"turbo"}`)
	if strings.Contains(out, "red") {
		t.Errorf("a non-hex colour survived: %s", out)
	}
	if strings.Contains(out, "turbo") {
		t.Errorf("an unknown game_mode survived: %s", out)
	}

	out, _ = Sanitize(`{"primary_color":"#E85D4C"}`)
	if !strings.Contains(out, "#E85D4C") {
		t.Errorf("a valid hex colour was dropped: %s", out)
	}
}

func TestSanitizeTolerAtesLegacyRows(t *testing.T) {
	// What createQuiz has been writing since before this package existed.
	legacy := `{"primary_color":"#6d28d9","background_color":"#0f172a","card_background":"rgba(255, 255, 255, 0.05)"}`
	out, err := Sanitize(legacy)
	if err != nil {
		t.Fatalf("legacy row failed to sanitize: %v", err)
	}
	if !strings.Contains(out, "#6d28d9") {
		t.Errorf("legacy primary_color was dropped: %s", out)
	}

	// Blank and malformed must not error: the column predates validation.
	for _, in := range []string{"", "   ", "not json", "[]", "null"} {
		if _, err := Sanitize(in); err != nil {
			t.Errorf("Sanitize(%q) errored: %v", in, err)
		}
	}
}

func TestBrandingKeysNamesOnlyWhatIsSet(t *testing.T) {
	keys := BrandingKeys(`{"bg_host_url":"/uploads/a.png","primary_color":"#ffffff"}`)
	if len(keys) != 1 || keys[0] != "bg_host_url" {
		t.Errorf("BrandingKeys = %v, want [bg_host_url]", keys)
	}
	if got := BrandingKeys(`{"primary_color":"#ffffff"}`); len(got) != 0 {
		t.Errorf("a colour alone counted as branding: %v", got)
	}
}

func TestRequestsPlayerPacedMatchesLegacyHelper(t *testing.T) {
	if !RequestsPlayerPaced(`{"game_mode":"player_paced"}`) {
		t.Error("player_paced not detected")
	}
	for _, in := range []string{"", "{}", `{"game_mode":"host_paced"}`, "broken"} {
		if RequestsPlayerPaced(in) {
			t.Errorf("RequestsPlayerPaced(%q) = true", in)
		}
	}
}

func TestSanitizeValidatesOverlayColour(t *testing.T) {
	// A colour only survives alongside an image, and only in hex form.
	out, _ := Sanitize(`{"bg_host_url":"/uploads/a.png","bg_overlay_color":"#1A2B3C"}`)
	if !strings.Contains(out, "#1A2B3C") {
		t.Errorf("a valid scrim colour was dropped: %s", out)
	}

	for _, bad := range []string{
		`{"bg_host_url":"/uploads/a.png","bg_overlay_color":"red"}`,
		`{"bg_host_url":"/uploads/a.png","bg_overlay_color":"#fff"}`,
		`{"bg_host_url":"/uploads/a.png","bg_overlay_color":"rgba(0,0,0,.5)"}`,
		`{"bg_host_url":"/uploads/a.png","bg_overlay_color":"#12141a; background:url(x)"}`,
	} {
		out, _ := Sanitize(bad)
		if strings.Contains(out, "bg_overlay_color") {
			t.Errorf("Sanitize(%s) kept a non-hex colour: %s", bad, out)
		}
	}

	// No image means no scrim, so the colour goes with it.
	out, _ = Sanitize(`{"bg_overlay_color":"#1A2B3C","bg_overlay":0.8}`)
	if strings.Contains(out, "bg_overlay_color") || strings.Contains(out, "bg_overlay") {
		t.Errorf("a scrim survived with nothing to sit over: %s", out)
	}
}
