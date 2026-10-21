// Package settings stores runtime knobs an admin can flip without a restart.
//
// It exists so the license package does not own a general facility: the next
// flag would otherwise drag an unrelated import into the commercial-gating code.
// Dependency direction is settings → db/model, and license → settings.
package settings

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
)

// Keys. One constant per knob so a typo is a compile error, not a silent default.
const (
	KeyLicenseEnforcement = "license.enforcement"
	// KeyPaymentEnabled gates self-service QR checkout. Off until the SePay
	// webhook is live; the Zalo contact link does not depend on it.
	KeyPaymentEnabled = "payment.enabled"

	// The default room theme, one row per field rather than one JSON row:
	// Value is varchar(255), and three upload paths plus the rest in a single
	// document would sit close enough to that limit to fail on a long name.
	// Read and written together through GetStrings / SetStrings.
	KeyThemeDefaultEnabled        = "theme.default.enabled"
	KeyThemeDefaultBgHostURL      = "theme.default.bg_host_url"
	KeyThemeDefaultBgPlayerURL    = "theme.default.bg_player_url"
	KeyThemeDefaultLogoURL        = "theme.default.logo_url"
	KeyThemeDefaultBgOverlay      = "theme.default.bg_overlay"
	KeyThemeDefaultBgOverlayColor = "theme.default.bg_overlay_color"
)

// MaxValueLen is the width of system_settings.value.
const MaxValueLen = 255

// ErrNotFound is returned by Get when the key has never been written.
var ErrNotFound = errors.New("setting not found")

// Get returns the raw row, so a caller can report who changed it and when.
func Get(key string) (*model.SystemSetting, error) {
	if db.DB == nil {
		return nil, errors.New("database not initialised")
	}
	var s model.SystemSetting
	if err := db.DB.Where("key = ?", key).First(&s).Error; err != nil {
		return nil, ErrNotFound
	}
	return &s, nil
}

// GetBool reads a boolean setting.
//
// A missing row returns def with no error: "never written" is a valid state, not
// a failure, and the caller must be able to tell it apart from a broken read.
func GetBool(key string, def bool) (bool, error) {
	s, err := Get(key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return def, nil
		}
		return def, err
	}
	return parseBool(s.Value, def)
}

// SetBool writes a boolean setting.
//
// One statement, upserting on the primary key: two admins racing produce a
// last-writer-wins row rather than a duplicate-key 500.
func SetBool(key string, v bool, actorID uint) error {
	if db.DB == nil {
		return errors.New("database not initialised")
	}
	row := model.SystemSetting{
		Key:       key,
		Value:     strconv.FormatBool(v),
		UpdatedBy: actorID,
		UpdatedAt: time.Now(),
	}
	return db.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value", "updated_by", "updated_at"}),
	}).Create(&row).Error
}

// GetStrings reads several settings in one query.
//
// A key that was never written is absent from the map rather than present as
// "", so a caller can tell "unset" from "cleared".
func GetStrings(keys ...string) (map[string]string, error) {
	if db.DB == nil {
		return nil, errors.New("database not initialised")
	}
	var rows []model.SystemSetting
	if err := db.DB.Where("key IN ?", keys).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Key] = r.Value
	}
	return out, nil
}

// SetStrings writes several settings in one transaction, so a reader never
// sees half of a change — a new background with the old background's scrim.
func SetStrings(values map[string]string, actorID uint) error {
	if db.DB == nil {
		return errors.New("database not initialised")
	}
	for k, v := range values {
		if len(v) > MaxValueLen {
			return fmt.Errorf("setting %q is %d bytes, over the %d limit", k, len(v), MaxValueLen)
		}
	}
	now := time.Now()
	return db.DB.Transaction(func(tx *gorm.DB) error {
		for k, v := range values {
			row := model.SystemSetting{Key: k, Value: v, UpdatedBy: actorID, UpdatedAt: now}
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "key"}},
				DoUpdates: clause.AssignmentColumns([]string{"value", "updated_by", "updated_at"}),
			}).Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// parseBool accepts the same spellings as config.getEnvBool, so "true" in the
// database and "true" in the environment mean the same thing.
func parseBool(raw string, def bool) (bool, error) {
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return def, fmt.Errorf("unparseable boolean setting %q: %w", raw, err)
	}
	return v, nil
}
