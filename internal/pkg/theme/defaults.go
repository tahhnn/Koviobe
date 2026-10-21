package theme

import (
	"encoding/json"
	"strings"
)

// Defaults is the room look an admin sets for the whole deployment, applied to
// every quiz that has not set its own.
//
// Only the visual keys: game mode, explanation timing and avatars are decisions
// about how a game runs, and a deployment-wide default for those would change a
// host's game behind their back. A look does not.
type Defaults struct {
	Enabled        bool    `json:"enabled"`
	BgHostURL      string  `json:"bg_host_url"`
	BgPlayerURL    string  `json:"bg_player_url"`
	LogoURL        string  `json:"logo_url"`
	BgOverlay      float64 `json:"bg_overlay"`
	BgOverlayColor string  `json:"bg_overlay_color"`
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
		return Defaults{Enabled: d.Enabled}
	}
	clean, err := Sanitize(string(raw))
	if err != nil {
		return Defaults{Enabled: d.Enabled}
	}
	c := Parse(clean)
	return Defaults{
		Enabled:        d.Enabled,
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
// independent — a host who set a background but no logo still gets the
// deployment's logo.
//
// Every other key is passed through untouched, the same way withGameMode does,
// so nothing a room reads today changes because a default exists.
func ApplyDefaults(raw string, d Defaults) (string, error) {
	if !d.Enabled {
		return raw, nil
	}
	d = SanitizeDefaults(d)

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
