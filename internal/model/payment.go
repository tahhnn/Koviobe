package model

import "time"

// PaymentProduct is one purchasable SKU: a plan sold for a fixed term at a
// fixed price. Separate from PricingPlan because one plan sells at several
// terms, and the plan row describes limits, not what a term costs.
//
// No column here carries a gorm `default`: GORM drops zero-valued fields from
// an INSERT when the column has one, which is how a 0 once became 30 in
// license_codes.duration_days (see LICENSING.md).
type PaymentProduct struct {
	ID           string    `gorm:"primaryKey;size:50" json:"id"` // pro_1m, pro_3m, pro_12m
	PlanID       string    `gorm:"size:50;not null;index" json:"plan_id"`
	Name         string    `gorm:"size:100;not null" json:"name"`
	DurationDays int       `gorm:"not null" json:"duration_days"`
	AmountVND    int       `gorm:"not null" json:"amount_vnd"`
	IsActive     bool      `gorm:"not null" json:"is_active"`
	SortOrder    int       `gorm:"not null" json:"sort_order"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}
