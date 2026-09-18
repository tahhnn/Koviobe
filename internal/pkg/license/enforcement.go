package license

import (
	"errors"
	"log"
	"sync/atomic"

	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/pkg/notify"
	"github.com/quizzzone/backend/internal/pkg/settings"
)

// enforcement is the hot-path copy of the runtime toggle. Enforcing() runs on
// every gated request, so it must never touch the database.
//
// The zero value is false, which is the old fail-open guard expressed as a type:
// a process that has not resolved the flag — a failed startup read, a unit test
// with no database — cannot answer "on".
var enforcement atomic.Bool

// envPinned records that LICENSE_ENFORCEMENT=true forced the flag on. While it
// is set the admin toggle is read-only, so the env var stays a break-glass
// override that still works when the console does not.
var envPinned atomic.Bool

// ErrEnforcementPinned is returned when the env var owns the flag. The English
// text is what internal/pkg/i18n/catalog.go keys on.
var ErrEnforcementPinned = errors.New("Enforcement is pinned by LICENSE_ENFORCEMENT env")

// EnvPinned reports whether the environment is overriding the stored toggle.
func EnvPinned() bool { return envPinned.Load() }

// InitEnforcement resolves the flag once at startup and primes the cache.
//
// Precedence: LICENSE_ENFORCEMENT=true wins and pins; anything else defers to
// the stored setting. A failed read leaves the cache at false — a broken
// settings table must never lock every host out of the product.
//
// Must run after db.AutoMigrate (the table has to exist) and before the expiry
// worker, whose first sweep fires 20s after boot and is a no-op or not depending
// on this flag.
func InitEnforcement() {
	if config.AppConfig != nil && config.AppConfig.LicenseEnforcement {
		enforcement.Store(true)
		envPinned.Store(true)
		log.Println("[LICENSE] enforcement pinned ON by LICENSE_ENFORCEMENT; the admin toggle is read-only")
		return
	}
	v, err := settings.GetBool(settings.KeyLicenseEnforcement, false)
	if err != nil {
		log.Printf("[LICENSE] could not read %s: %v — enforcement stays OFF", settings.KeyLicenseEnforcement, err)
		notify.P1("license_settings_read",
			"Không đọc được cờ enforcement từ system_settings: %v — enforcement giữ TẮT (fail-open).", err)
		enforcement.Store(false)
		return
	}
	enforcement.Store(v)
	log.Printf("[LICENSE] enforcement=%t (source: database)", v)
}

// RefreshEnforcement re-reads the stored flag into the cache.
//
// A failed read keeps the last good value rather than falling back to false: a
// transient blip must not silently un-arm gates somebody deliberately armed.
func RefreshEnforcement() {
	if envPinned.Load() {
		return
	}
	v, err := settings.GetBool(settings.KeyLicenseEnforcement, enforcement.Load())
	if err != nil {
		return
	}
	enforcement.Store(v)
}

// SetEnforcement is the single writer: database first, cache only on success, so
// the two can never disagree in the direction "cache says on, row says off".
func SetEnforcement(v bool, actorID uint) error {
	if envPinned.Load() {
		return ErrEnforcementPinned
	}
	if err := settings.SetBool(settings.KeyLicenseEnforcement, v, actorID); err != nil {
		return err
	}
	enforcement.Store(v)
	return nil
}
