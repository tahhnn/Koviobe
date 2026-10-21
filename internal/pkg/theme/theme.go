// Package theme owns the shape of quizzes.theme_config and rooms.theme_config.
//
// That column is a free-form TEXT holding JSON, written by the frontend and
// read in a dozen places. Before this package each reader declared its own
// anonymous struct with the one key it cared about, so nothing described the
// whole schema and nothing validated it — a quiz could carry any key, with any
// value, including a background URL pointing at someone else's server.
//
// Everything that writes the column goes through Sanitize. Everything that
// reads a single key may keep its own narrow struct; those still work, because
// Sanitize only ever removes keys it does not know.
package theme

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Config is the full set of keys a theme may carry.
//
// omitempty throughout: a sanitized config is stored as-is, and a quiz that
// sets nothing should round-trip to "{}" rather than to a wall of zero values
// that later readers would have to tell apart from a deliberate zero.
type Config struct {
	GameMode            string `json:"game_mode,omitempty"`
	ExplanationDuration int    `json:"explanation_duration,omitempty"`

	// Branding. The two backgrounds are separate images because the host
	// screen is 16:9 on a projector and the player screen is a phone held
	// upright — one file cropped to both loses the subject in one of them.
	BgHostURL   string  `json:"bg_host_url,omitempty"`
	BgPlayerURL string  `json:"bg_player_url,omitempty"`
	LogoURL     string  `json:"logo_url,omitempty"`
	BgOverlay   float64 `json:"bg_overlay,omitempty"`

	// The scrim's colour. Separate from BgOverlay so a host can tint the key
	// visual in their own brand rather than only darken it — the whole point
	// of matching the event's mood, not just making room for text.
	//
	// Empty means the page background, which is what every theme written
	// before this key existed gets.
	BgOverlayColor string `json:"bg_overlay_color,omitempty"`

	PrimaryColor    string `json:"primary_color,omitempty"`
	RemoveWatermark bool   `json:"remove_watermark,omitempty"`

	// HideAvatars turns player emoji avatars off for this quiz (a formal exam,
	// say). Stored as the negative so every theme written before the key
	// existed — and "{}" — keeps avatars on. Not branding: every plan has it.
	HideAvatars bool `json:"hide_avatars,omitempty"`
}

// MinOverlay is the floor for the scrim drawn over a background image.
//
// Body text is #f2f0eb on #12141a. Over a bright key visual with no scrim it
// is unreadable on a projector at the back of a room, and the host setting the
// image is looking at a laptop 40cm away where it still looks fine. So the
// floor is enforced rather than advised: a host may darken further, not less.
const MinOverlay = 0.35

// DefaultOverlay applies when a background is set but no overlay was chosen.
const DefaultOverlay = 0.55

// DefaultOverlayColor is the page background, so a theme that sets no colour
// looks exactly as it did before the key existed.
const DefaultOverlayColor = "#12141a"

var (
	hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

	// Values accepted for game_mode. An unknown mode is dropped rather than
	// rejected: it means host_paced everywhere that reads it, and failing a
	// whole quiz save over a typo in a key nobody typed by hand is worse.
	gameModes = map[string]bool{"host_paced": true, "player_paced": true}
)

// UploadPrefix is the only location a theme may reference an image from.
const UploadPrefix = "/uploads/"

// isLocalUpload reports whether a URL points at this deployment's own upload
// store.
//
// A theme image is rendered on the projector and on every player's phone, so
// an off-site URL would hand a third party the IP of everyone in the room and
// would break the moment that host's server went down. Only files this
// deployment wrote are allowed, and they all live under /uploads/.
//
// The check is a prefix match on a path, not a URL parse: "//evil.com/x.png"
// and "https://evil.com/uploads/x.png" both fail it, the first because it does
// not start with /uploads/ and the second for the same reason.
func isLocalUpload(u string) bool {
	if !strings.HasPrefix(u, UploadPrefix) {
		return false
	}
	// A traversal inside the name would escape the upload directory when the
	// gateway resolves it against its root. Percent-encoding and backslashes
	// are refused too: nginx decodes "%2e%2e%2f" into "../" before resolving,
	// so "/uploads/theme-presets/%2e%2e%2fimg-u5-x.png" passed a check for
	// ".." and was served as the host's own upload. No name this deployment
	// writes contains either character.
	if strings.Contains(u, "..") || strings.ContainsAny(u, "%\\") {
		return false
	}
	return len(u) > len(UploadPrefix)
}

// Parse reads a stored theme_config. A blank or malformed value is an empty
// config, never an error: the column predates any validation and old rows hold
// whatever the frontend wrote at the time.
func Parse(raw string) Config {
	var c Config
	if strings.TrimSpace(raw) == "" {
		return c
	}
	_ = json.Unmarshal([]byte(raw), &c)
	return c
}

// Sanitize is the only way a theme should reach the database.
//
// It drops unknown keys, rejects image URLs that do not point at this
// deployment's upload store, and clamps every numeric range. It returns the
// JSON to store — never the caller's string — so an unknown key cannot survive
// by being echoed back.
func Sanitize(raw string) (string, error) {
	c := Parse(raw)

	if !gameModes[c.GameMode] {
		c.GameMode = ""
	}

	// Mirrors the frontend slider, which offers 5-60s.
	if c.ExplanationDuration < 0 || c.ExplanationDuration > 300 {
		c.ExplanationDuration = 0
	}

	for _, p := range []*string{&c.BgHostURL, &c.BgPlayerURL, &c.LogoURL} {
		if *p != "" && !isLocalUpload(*p) {
			*p = ""
		}
	}

	if !hexColor.MatchString(c.PrimaryColor) {
		c.PrimaryColor = ""
	}
	if !hexColor.MatchString(c.BgOverlayColor) {
		c.BgOverlayColor = ""
	}

	// The floor only applies once there is an image to sit under. Without one
	// the scrim would darken the gradient background for no reason.
	if c.BgHostURL != "" || c.BgPlayerURL != "" {
		if c.BgOverlay <= 0 {
			c.BgOverlay = DefaultOverlay
		}
		if c.BgOverlay < MinOverlay {
			c.BgOverlay = MinOverlay
		}
		if c.BgOverlay > 1 {
			c.BgOverlay = 1
		}
	} else {
		c.BgOverlay = 0
		// A scrim colour without an image to sit over is dead weight in the row.
		c.BgOverlayColor = ""
	}

	out, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// RequestsPlayerPaced reports whether a theme asks for solo mode.
func RequestsPlayerPaced(raw string) bool {
	return Parse(raw).GameMode == "player_paced"
}

// PresetPrefix is where the built-in themes live. They ship with the frontend
// (public/uploads/theme-presets/) rather than on the upload volume, and every
// plan may pick one: a preset is not custom branding.
const PresetPrefix = UploadPrefix + "theme-presets/"

// presetName is the shape of every file in public/uploads/theme-presets/.
// Matched whole rather than by prefix: anything looser lets a host name their
// own upload as a "preset" — an encoded "../" after the prefix is resolved by
// nginx — and so take custom branding on a plan without it.
var presetName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*\.svg$`)

// IsPreset reports whether an image is one of the built-in themes.
func IsPreset(u string) bool {
	return strings.HasPrefix(u, PresetPrefix) && presetName.MatchString(u[len(PresetPrefix):])
}

// BrandingKeys lists the custom branding a theme sets, for the license check.
// A built-in preset image is not custom branding and is not listed.
//
// Returned as a slice rather than a bool so the 403 can name the field the
// host actually touched; "custom branding requires Pro" on a quiz where they
// only picked a colour reads like a bug.
func BrandingKeys(raw string) []string {
	c := Parse(raw)
	var keys []string
	for _, f := range []struct{ key, url string }{
		{"bg_host_url", c.BgHostURL},
		{"bg_player_url", c.BgPlayerURL},
		{"logo_url", c.LogoURL},
	} {
		if f.url != "" && !IsPreset(f.url) {
			keys = append(keys, f.key)
		}
	}
	return keys
}

// WantsRemoveWatermark reports whether a theme asks to drop the watermark.
func WantsRemoveWatermark(raw string) bool {
	return Parse(raw).RemoveWatermark
}

// AvatarsEnabled reports whether players in a room with this theme get avatars.
func AvatarsEnabled(raw string) bool {
	return !Parse(raw).HideAvatars
}
