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

	"gorm.io/gorm/clause"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
)

// Keys. One constant per knob so a typo is a compile error, not a silent default.
const (
	KeyLicenseEnforcement = "license.enforcement"
)

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

// parseBool accepts the same spellings as config.getEnvBool, so "true" in the
// database and "true" in the environment mean the same thing.
func parseBool(raw string, def bool) (bool, error) {
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return def, fmt.Errorf("unparseable boolean setting %q: %w", raw, err)
	}
	return v, nil
}
