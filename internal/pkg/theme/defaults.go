package theme

import (
	"encoding/json"
	"strings"
)

// Defaults is the room look a host picks once for all of their rooms, applied
// to each of their quizzes that has not set its own.
//
// Only the visual keys: game mode, explanation timing and avatars are decisions
// about one game, set on the quiz where the host can see them.
type Defaults struct {
	BgHostURL      string  `json:"bg_host_url"`
	BgPlayerURL    string  `json:"bg_player_url"`
	LogoURL        string  `json:"logo_url"`
	BgOverlay      float64 `json:"bg_overlay"`
	BgOverlayColor string  `json:"bg_overlay_color"`
}

// PresetPrefix is where the built-in room looks live. They ship with the
// frontend (public/uploads/theme-presets/) rather than on the upload volume,
// and every plan may use them: picking one is not custom branding.
const PresetPrefix = UploadPrefix + "theme-presets/"

// IsPreset reports whether an image is one of the built-in looks.
func IsPreset(u string) bool {
	if !strings.HasPrefix(u, PresetPrefix) || !isLocalUpload(u) {
		return false
	}
	name := u[len(PresetPrefix):]
	return name != "" && !strings.Contains(name, "/")
}

// CustomKeys lists the images in d that are the host's own rather than a
// preset — what the custom-branding entitlement covers. Named so the 403 can
// say which field needs the plan.
func (d Defaults) CustomKeys() []string {
	var keys []string
	for _, f := range []struct{ key, url string }{
		{"bg_host_url", d.BgHostURL},
		{"bg_player_url", d.BgPlayerURL},
		{"logo_url", d.LogoURL},
	} {
		if f.url != "" && !IsPreset(f.url) {
			keys = append(keys, f.key)
		}
	}
	return keys
}

// WithoutCustom drops the host's own images and keeps any preset.
//
// For a host whose plan no longer covers custom branding: their saved theme
// still holds the images they uploaded on the old plan, and a room must not
// show them. A background pair is kept or dropped as a whole — half a custom
// set beside half a preset is neither look.
func (d Defaults) WithoutCustom() Defaults {
	if (d.BgHostURL != "" && !IsPreset(d.BgHostURL)) || (d.BgPlayerURL != "" && !IsPreset(d.BgPlayerURL)) {
		d.BgHostURL, d.BgPlayerURL = "", ""
	}
	if d.LogoURL != "" && !IsPreset(d.LogoURL) {
		d.LogoURL = ""
	}
	return SanitizeDefaults(d)
}

// SanitizeDefaults applies exactly the rules Sanitize applies to a quiz theme,
// by running the same code: a default is copied into rooms, so anything a quiz
// could not store must not reach a room this way either.
func SanitizeDefaults(d Defaults) Defaults {
	raw, err := json.Marshal(Config{
		BgHostURL:      d.BgHostURL,
		BgPlayerURL:    d.BgPlayerURL,
		LogoURL:        d.LogoURL,
		BgOverlay:      d.BgOverlay,
		BgOverlayColor: d.BgOverlayColor,
	})
	if err != nil {
		return Defaults{}
	}
	clean, err := Sanitize(string(raw))
	if err != nil {
		return Defaults{}
	}
	c := Parse(clean)
	return Defaults{
		BgHostURL:      c.BgHostURL,
		BgPlayerURL:    c.BgPlayerURL,
		LogoURL:        c.LogoURL,
		BgOverlay:      c.BgOverlay,
		BgOverlayColor: c.BgOverlayColor,
	}
}

// ApplyDefaults fills the parts of a room theme that the quiz left empty.
//
// Backgrounds travel as a group: the scrim was chosen for the image under it,
// so a quiz with its own background keeps its own scrim too, and a quiz with
// none takes the default image together with the default scrim. The logo is
// independent — a quiz with its own background but no logo still gets the
// host's logo.
//
// Every other key is passed through untouched, the same way withGameMode does,
// so nothing a room reads today changes because a default exists.
func ApplyDefaults(raw string, d Defaults) (string, error) {
	d = SanitizeDefaults(d)
	if d == (Defaults{}) {
		return raw, nil
	}

	cfg := map[string]interface{}{}
	if trimmed := strings.TrimSpace(raw); trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &cfg); err != nil {
			cfg = map[string]interface{}{}
		}
	}

	// An unusable URL counts as no URL: the frontend refuses to render one,
	// so a legacy row holding an off-site image shows nothing at all today.
	has := func(key string) bool {
		s, _ := cfg[key].(string)
		return isLocalUpload(s)
	}

	changed := false
	if !has("bg_host_url") && !has("bg_player_url") && (d.BgHostURL != "" || d.BgPlayerURL != "") {
		for key, val := range map[string]string{
			"bg_host_url":      d.BgHostURL,
			"bg_player_url":    d.BgPlayerURL,
			"bg_overlay_color": d.BgOverlayColor,
		} {
			if val == "" {
				delete(cfg, key)
			} else {
				cfg[key] = val
			}
		}
		cfg["bg_overlay"] = d.BgOverlay
		changed = true
	}
	if !has("logo_url") && d.LogoURL != "" {
		cfg["logo_url"] = d.LogoURL
		changed = true
	}
	if !changed {
		return raw, nil
	}

	out, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
