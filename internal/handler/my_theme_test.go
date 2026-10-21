package handler

import (
	"errors"
	"net/http"
	"testing"

	"github.com/quizzzone/backend/internal/pkg/license"
	"github.com/quizzzone/backend/internal/pkg/theme"
)

const (
	presetHostURL   = "/uploads/theme-presets/aurora-host.svg"
	presetPlayerURL = "/uploads/theme-presets/aurora-player.svg"
	ownUpload       = "/uploads/img-u5-1789354565049-c430b2cc.jpg"
)

func entsFor(branding bool) func() (license.Entitlements, error) {
	return func() (license.Entitlements, error) {
		return license.Entitlements{PlanID: "free", AllowCustomBranding: branding}, nil
	}
}

func TestVetUserThemePresetsAreOpenToEveryPlan(t *testing.T) {
	req := theme.Defaults{BgHostURL: presetHostURL, BgPlayerURL: presetPlayerURL, BgOverlay: 0.4, BgOverlayColor: "#050b1a"}
	clean, status, body := vetUserTheme(req, true, entsFor(false))
	if status != 0 {
		t.Fatalf("a preset was refused on a plan without branding: %d %v", status, body)
	}
	if clean.BgHostURL != presetHostURL || clean.BgPlayerURL != presetPlayerURL {
		t.Errorf("the preset was not kept: %+v", clean)
	}
}

func TestVetUserThemeCustomImagesNeedBranding(t *testing.T) {
	for name, req := range map[string]theme.Defaults{
		"background": {BgHostURL: ownUpload},
		"logo":       {BgHostURL: presetHostURL, BgPlayerURL: presetPlayerURL, LogoURL: ownUpload},
		"player":     {BgPlayerURL: ownUpload},
	} {
		_, status, body := vetUserTheme(req, true, entsFor(false))
		if status != http.StatusForbidden || body["feature"] != "allow_custom_branding" {
			t.Errorf("%s: custom image on a plan without branding got %d %v, want 403", name, status, body)
		}
	}
}

// The bypass this test was written for: an own upload named as a preset with
// an encoded "../", which nginx resolves to the upload itself.
func TestVetUserThemeRefusesEncodedTraversalDressedAsPreset(t *testing.T) {
	for _, u := range []string{
		"/uploads/theme-presets/%2e%2e%2fimg-u5-1789354565049-c430b2cc.jpg",
		"/uploads/theme-presets/%2E%2E%2Fimg-u5-1789354565049-c430b2cc.jpg",
		"/uploads/theme-presets/..%2fimg-u5-1789354565049-c430b2cc.jpg",
		"/uploads/theme-presets/..%5cimg-u5-1789354565049-c430b2cc.jpg",
		"/uploads/theme-presets/aurora-host.svg%00.jpg",
		"/uploads/theme-presets/img-u5-1789354565049-c430b2cc.jpg",
	} {
		for _, req := range []theme.Defaults{{LogoURL: u}, {BgHostURL: u}} {
			clean, status, _ := vetUserTheme(req, true, entsFor(false))
			if status == 0 {
				t.Errorf("%q was accepted on a plan without branding: %+v", u, clean)
			}
		}
	}
}

func TestVetUserThemeBrandingPlanMayUseOwnImages(t *testing.T) {
	req := theme.Defaults{BgHostURL: ownUpload, LogoURL: ownUpload}
	clean, status, body := vetUserTheme(req, true, entsFor(true))
	if status != 0 || clean.BgHostURL != ownUpload || clean.LogoURL != ownUpload {
		t.Errorf("a plan with branding was refused its own images: %d %v %+v", status, body, clean)
	}
}

func TestVetUserThemeEnforcementOffOpensEverything(t *testing.T) {
	called := false
	_, status, _ := vetUserTheme(theme.Defaults{LogoURL: ownUpload}, false, func() (license.Entitlements, error) {
		called = true
		return license.Entitlements{}, nil
	})
	if status != 0 {
		t.Errorf("enforcement off still gated a custom image: %d", status)
	}
	if called {
		t.Error("entitlements were looked up with enforcement off")
	}
}

func TestVetUserThemeOffsiteImageIsRefused(t *testing.T) {
	_, status, _ := vetUserTheme(theme.Defaults{LogoURL: "https://evil.example.com/logo.png"}, true, entsFor(true))
	if status != http.StatusBadRequest {
		t.Errorf("an off-site image got %d, want 400", status)
	}
}

func TestVetUserThemeLicenseFailureIsNotAPass(t *testing.T) {
	_, status, _ := vetUserTheme(theme.Defaults{LogoURL: ownUpload}, true, func() (license.Entitlements, error) {
		return license.Entitlements{}, errors.New("db down")
	})
	if status != http.StatusInternalServerError {
		t.Errorf("a failed license lookup got %d, want 500", status)
	}
}
