package license

import (
	"time"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"gorm.io/gorm"
)

// HistoryFilter narrows a subscription-history query.
type HistoryFilter struct {
	UserID      uint
	Email       string
	PlanID      string
	Action      string
	Source      string
	ExternalRef string
	From        *time.Time
	To          *time.Time
	Limit       int
}

// ListHistory returns subscription events newest first.
func ListHistory(f HistoryFilter) ([]model.SubscriptionEvent, error) {
	limit := f.Limit
	if limit <= 0 || limit > 5000 {
		limit = 200
	}

	tx := db.DB.Model(&model.SubscriptionEvent{}).Order("created_at DESC").Limit(limit)
	if f.UserID > 0 {
		tx = tx.Where("user_id = ?", f.UserID)
	}
	if f.Email != "" {
		tx = tx.Where("email ILIKE ?", "%"+f.Email+"%")
	}
	if f.PlanID != "" {
		tx = tx.Where("plan_id = ?", f.PlanID)
	}
	if f.Action != "" {
		tx = tx.Where("action = ?", f.Action)
	}
	if f.Source != "" {
		tx = tx.Where("source = ?", f.Source)
	}
	if f.ExternalRef != "" {
		tx = tx.Where("external_ref ILIKE ?", "%"+f.ExternalRef+"%")
	}
	if f.From != nil {
		tx = tx.Where("created_at >= ?", *f.From)
	}
	if f.To != nil {
		tx = tx.Where("created_at < ?", *f.To)
	}

	var out []model.SubscriptionEvent
	if err := tx.Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// RevenueSummary is the reconciliation total for a period.
type RevenueSummary struct {
	Grants     int64 `json:"grants"`
	PaidGrants int64 `json:"paid_grants"`
	TotalVND   int64 `json:"total_vnd"`
}

// SummarizeRevenue totals what was recorded as paid over a period.
//
// This is what we booked, not what the bank received — payment happens through an
// intermediary and never touches this system. It is the left-hand column of a
// reconciliation, and a mismatch against the statement is the point of keeping it.
//
// Only grant and renew count. Downgrades and expiries carry no money, and
// counting them would net real revenue away to nothing.
func SummarizeRevenue(from, to *time.Time) (RevenueSummary, error) {
	var s RevenueSummary

	// Each aggregate builds its own query rather than reusing one *gorm.DB.
	// Chaining onto a handle that has already run a Count silently accumulates
	// the previous conditions, so the "paid only" filter would leak into totals
	// computed afterwards.
	scope := func() *gorm.DB {
		tx := db.DB.Model(&model.SubscriptionEvent{}).Where("action IN ?", revenueActions())
		if from != nil {
			tx = tx.Where("created_at >= ?", *from)
		}
		if to != nil {
			tx = tx.Where("created_at < ?", *to)
		}
		return tx
	}

	if err := scope().Count(&s.Grants).Error; err != nil {
		return s, err
	}
	if err := scope().Where("amount_vnd > 0").Count(&s.PaidGrants).Error; err != nil {
		return s, err
	}
	if err := scope().Select("COALESCE(SUM(amount_vnd), 0)").Scan(&s.TotalVND).Error; err != nil {
		return s, err
	}
	return s, nil
}

// revenueActions is the scope of SummarizeRevenue, kept next to isRevenueAction
// so the SQL filter and the predicate cannot drift apart.
func revenueActions() []string {
	all := []string{"grant", "renew", "downgrade", "expire"}
	out := make([]string, 0, len(all))
	for _, a := range all {
		if isRevenueAction(a) {
			out = append(out, a)
		}
	}
	return out
}
