package license

import (
	"log"
	"time"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
)

// ExpiredGrant describes one subscription the sweep downgraded.
type ExpiredGrant struct {
	UserID       uint
	Email        string
	PreviousPlan string
	RoomIDs      []uint
}

// SweepExpiredSubscriptions downgrades every paid subscription whose EndsAt has
// passed, and reports the rooms those hosts still had open.
//
// GetEntitlements already downgrades lazily, on the owner's next API call — but
// lazily is not enough for a room that is already running. Nothing re-reads
// entitlements mid-game, so an expired host kept hosting until they happened to
// hit a gated endpoint, which during a live game they never do. This sweep is
// what makes an expiry take effect at the time it expires.
//
// It does not close the rooms itself: archiving a room is the handler package's
// job, and pulling that in here would invert the dependency. It returns the ids
// and lets the caller finalize them.
func SweepExpiredSubscriptions() ([]ExpiredGrant, error) {
	if !Enforcing() {
		return nil, nil
	}

	now := time.Now()
	var due []model.Subscription
	err := db.DB.
		Where("ends_at IS NOT NULL AND ends_at < ?", now).
		Where("plan_id <> ?", PlanFree).
		Find(&due).Error
	if err != nil {
		return nil, err
	}
	if len(due) == 0 {
		return nil, nil
	}

	grants := make([]ExpiredGrant, 0, len(due))
	for _, sub := range due {
		var u model.User
		_ = db.DB.First(&u, sub.UserID).Error

		var roomIDs []uint
		if err := db.DB.Model(&model.Room{}).
			Where("host_id = ? AND status IN ?", sub.UserID, []string{"waiting", "active"}).
			Pluck("id", &roomIDs).Error; err != nil {
			log.Printf("[LICENSE] could not list open rooms for user %d: %v", sub.UserID, err)
		}

		// DowngradeToFree writes the history row for us (source expiry_auto).
		if err := DowngradeToFree(sub.UserID); err != nil {
			log.Printf("[LICENSE] downgrade failed for user %d, leaving rooms open: %v", sub.UserID, err)
			continue
		}

		grants = append(grants, ExpiredGrant{
			UserID:       sub.UserID,
			Email:        u.Email,
			PreviousPlan: sub.PlanID,
			RoomIDs:      roomIDs,
		})
	}
	return grants, nil
}
