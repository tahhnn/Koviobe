package model

import "time"

// SystemSetting is one runtime knob an admin can change without a restart.
//
// Key/value rather than a column per flag: the next flag is then a constant, not
// a migration. The cost is losing type safety at the storage layer, paid back by
// keeping every parse inside internal/pkg/settings — nothing else in the
// codebase reads Value as a string.
//
// Two deliberate omissions:
//
//   - No `default:` tag on Value. Same house rule as PricingPlan's limits: GORM
//     omits a zero-valued field from the INSERT when the column declares a
//     default, and here "" and "false" are different states.
//   - No gorm.DeletedAt, unlike every other model. A soft-deleted row would make
//     First miss it and silently fall back to the default — i.e. silently turn
//     enforcement off. A setting is overwritten, never deleted.
type SystemSetting struct {
	Key   string `gorm:"primaryKey;size:100" json:"key"`
	Value string `gorm:"size:255;not null" json:"value"`
	// UpdatedBy is the admin who last wrote it; 0 for anything the system set.
	UpdatedBy uint      `gorm:"index" json:"updated_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
