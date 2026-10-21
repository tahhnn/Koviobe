package theme

import (
	"encoding/json"
	"testing"
)

var sampleDefaults = Defaults{
	BgHostURL:      "/uploads/img-u1-host.webp",
	BgPlayerURL:    "/uploads/img-u1-player.webp",
	LogoURL:        "/uploads/img-u1-logo.png",
	BgOverlay:      0.6,
	BgOverlayColor: "#0b2a4a",
}

func decode(t *testing.T, raw string) map[string]interface{} {
	t.Helper()
	m := map[string]interface{}{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("not JSON: %s (%v)", raw, err)
	}
	return m
}

func TestApplyDefaultsEmptyIsANoOp(t *testing.T) {
	in := `{"game_mode":"host_paced"}`
	out, err := ApplyDefaults(in, Defaults{BgOverlay: 0.8, BgOverlayColor: "#000000"})
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("a host with no theme changed the room theme: %s", out)
	}
}

func TestApplyDefaultsFillsAnUnbrandedQuiz(t *testing.T) {
	out, err := ApplyDefaults(`{"game_mode":"player_paced","hide_avatars":true}`, sampleDefaults)
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	for key, want := range map[string]interface{}{
		"bg_host_url":      sampleDefaults.BgHostURL,
		"bg_player_url":    sampleDefaults.BgPlayerURL,
		"logo_url":         sampleDefaults.LogoURL,
		"bg_overlay":       0.6,
		"bg_overlay_color": "#0b2a4a",
		// Not branding: must survive untouched.
		"game_mode":    "player_paced",
		"hide_avatars": true,
	} {
		if m[key] != want {
			t.Errorf("%s = %v, want %v (theme %s)", key, m[key], want, out)
		}
	}
}

func TestApplyDefaultsKeepsTheQuizBackgroundAndItsScrim(t *testing.T) {
	in := `{"bg_host_url":"/uploads/img-u9-own.jpg","bg_overlay":0.8}`
	out, err := ApplyDefaults(in, sampleDefaults)
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	if m["bg_host_url"] != "/uploads/img-u9-own.jpg" || m["bg_overlay"] != 0.8 {
		t.Errorf("the quiz's own background was replaced: %s", out)
	}
	if _, ok := m["bg_player_url"]; ok {
		t.Errorf("a default image was mixed into a quiz's own background set: %s", out)
	}
	if _, ok := m["bg_overlay_color"]; ok {
		t.Errorf("the default scrim colour was laid over the quiz's own image: %s", out)
	}
	// The logo is independent of the background group.
	if m["logo_url"] != sampleDefaults.LogoURL {
		t.Errorf("logo_url = %v, want the default", m["logo_url"])
	}
}

func TestApplyDefaultsKeepsTheQuizLogo(t *testing.T) {
	out, err := ApplyDefaults(`{"logo_url":"/uploads/img-u9-logo.png"}`, sampleDefaults)
	if err != nil {
		t.Fatal(err)
	}
	if decode(t, out)["logo_url"] != "/uploads/img-u9-logo.png" {
		t.Errorf("the quiz's own logo was replaced: %s", out)
	}
}

func TestApplyDefaultsTreatsAnOffsiteImageAsUnset(t *testing.T) {
	// The frontend renders nothing for this, so the default should show.
	out, err := ApplyDefaults(`{"bg_host_url":"https://evil.example.com/x.png"}`, sampleDefaults)
	if err != nil {
		t.Fatal(err)
	}
	if decode(t, out)["bg_host_url"] != sampleDefaults.BgHostURL {
		t.Errorf("an unusable quiz image blocked the default: %s", out)
	}
}

func TestApplyDefaultsSurvivesAMalformedTheme(t *testing.T) {
	out, err := ApplyDefaults(`{not json`, sampleDefaults)
	if err != nil {
		t.Fatal(err)
	}
	if decode(t, out)["bg_host_url"] != sampleDefaults.BgHostURL {
		t.Errorf("a malformed theme did not take the default: %s", out)
	}
}

func TestSanitizeDefaultsUsesTheQuizRules(t *testing.T) {
	d := SanitizeDefaults(Defaults{
		BgHostURL:      "https://evil.example.com/x.png",
		BgPlayerURL:    "/uploads/img-u1-p.webp",
		LogoURL:        "/uploads/../../etc/passwd",
		BgOverlay:      0.1,
		BgOverlayColor: "red",
	})
	if d.BgHostURL != "" || d.LogoURL != "" {
		t.Errorf("an unsafe URL survived: %+v", d)
	}
	if d.BgPlayerURL != "/uploads/img-u1-p.webp" {
		t.Errorf("a local upload was dropped: %+v", d)
	}
	if d.BgOverlay != MinOverlay {
		t.Errorf("BgOverlay = %v, want the floor %v", d.BgOverlay, MinOverlay)
	}
	if d.BgOverlayColor != "" {
		t.Errorf("a non-hex colour survived: %q", d.BgOverlayColor)
	}
}

func TestSanitizeDefaultsWithoutImagesDropsTheScrim(t *testing.T) {
	d := SanitizeDefaults(Defaults{LogoURL: "/uploads/img-u1-l.png", BgOverlay: 0.7, BgOverlayColor: "#000000"})
	if d.BgOverlay != 0 || d.BgOverlayColor != "" {
		t.Errorf("a scrim with no image survived: %+v", d)
	}
}

const (
	presetHost   = "/uploads/theme-presets/aurora-host.svg"
	presetPlayer = "/uploads/theme-presets/aurora-player.svg"
)

func TestIsPreset(t *testing.T) {
	for u, want := range map[string]bool{
		presetHost:                                 true,
		"/uploads/theme-presets/":                  false,
		"/uploads/theme-presets/../img-u1-x.png":   false,
		"/uploads/theme-presets/sub/x.svg":         false,
		"/uploads/img-u1-123-abc.webp":             false,
		"https://evil.example.com/theme-presets/x": false,
	} {
		if got := IsPreset(u); got != want {
			t.Errorf("IsPreset(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestCustomKeysIgnoresPresets(t *testing.T) {
	d := Defaults{BgHostURL: presetHost, BgPlayerURL: presetPlayer}
	if keys := d.CustomKeys(); len(keys) != 0 {
		t.Errorf("a preset counted as custom branding: %v", keys)
	}
	d.LogoURL = "/uploads/img-u1-logo.png"
	if keys := d.CustomKeys(); len(keys) != 1 || keys[0] != "logo_url" {
		t.Errorf("CustomKeys = %v, want [logo_url]", keys)
	}
}

func TestWithoutCustomKeepsPresetsOnly(t *testing.T) {
	d := Defaults{BgHostURL: presetHost, BgPlayerURL: presetPlayer, BgOverlay: 0.5, LogoURL: "/uploads/img-u1-logo.png"}.WithoutCustom()
	if d.BgHostURL != presetHost || d.BgPlayerURL != presetPlayer || d.BgOverlay != 0.5 {
		t.Errorf("a preset was dropped: %+v", d)
	}
	if d.LogoURL != "" {
		t.Errorf("a custom logo survived: %+v", d)
	}

	// A pair with one custom image goes as a whole, scrim included.
	d = Defaults{BgHostURL: presetHost, BgPlayerURL: "/uploads/img-u1-p.webp", BgOverlay: 0.5, BgOverlayColor: "#000000"}.WithoutCustom()
	if d != (Defaults{}) {
		t.Errorf("half a custom background survived: %+v", d)
	}
}
