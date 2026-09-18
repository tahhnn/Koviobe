package license

import (
	"errors"
	"log"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
)

// What happened to one redeemer of a clawed-back code. Protocol values the admin
// console maps to text, so they are never translated here.
const (
	OutcomeRevoked     = "revoked"      // the plan was taken back
	OutcomeSuperseded  = "superseded"   // a later grant owns this plan now
	OutcomeAlreadyFree = "already_free" // nothing left to take
	OutcomeFailed      = "failed"
)

// ClawBackResult is one redeemer of a code and what happened to them.
//
// Every redeemer is reported, including the ones deliberately left alone: "3 of 5
// taken back, and here is why the other 2 were not" is the answer an admin needs,
// and a filtered list is how an admin comes to believe a claw-back was complete
// when it was not.
type ClawBackResult struct {
	UserID       uint   `json:"user_id"`
	Email        string `json:"email"`
	Outcome      string `json:"outcome"`
	PreviousPlan string `json:"previous_plan,omitempty"`
	// Reason is the superseding source, or the error, depending on Outcome.
	Reason string `json:"reason,omitempty"`
	// RoomIDs are the host's still-open rooms. This package does not close them
	// — see ClawBackCode.
	RoomIDs []uint `json:"room_ids,omitempty"`
}

// OpenRoomIDs returns the rooms a host still has open (waiting or active).
//
// Exported so a caller that has just taken a plan away can close them; the
// expiry sweep uses the same query.
func OpenRoomIDs(userID uint) ([]uint, error) {
	var roomIDs []uint
	err := db.DB.Model(&model.Room{}).
		Where("host_id = ? AND status IN ?", userID, []string{"waiting", "active"}).
		Pluck("id", &roomIDs).Error
	return roomIDs, err
}

// ClawBackCode revokes a code and takes the plan back from every redeemer whose
// current subscription is still the one this code created.
//
// It does not close rooms, for the same reason SweepExpiredSubscriptions does
// not: archiving a room belongs to the handler package, and reaching for it here
// would invert the dependency. The room ids come back in each result and the
// caller finalizes them.
func ClawBackCode(rawCode string, gc GrantContext) (*model.LicenseCode, []ClawBackResult, error) {
	// Revoke first, outside the loop: this shuts the door so nobody can redeem
	// the code while we are working through the people who already did.
	code, err := RevokeCode(rawCode)
	if err != nil {
		return nil, nil, err
	}

	var redemptions []model.LicenseRedemption
	if err := db.DB.Where("code = ?", code.Code).Order("created_at ASC").Find(&redemptions).Error; err != nil {
		return code, nil, err
	}

	gc.Source = SourceAdminRevoke
	gc.SourceRef = code.Code

	results := make([]ClawBackResult, 0, len(redemptions))
	for _, r := range redemptions {
		results = append(results, clawBackOne(r.UserID, code.Code, gc))
	}
	return code, results, nil
}

// ClawBackUser takes a single user's plan back.
//
// The "is this still the grant that code created" rule is NOT applied: an admin
// naming one user is asserting the authority the rule exists to infer for them.
func ClawBackUser(userID uint, gc GrantContext) (ClawBackResult, error) {
	if gc.Source == "" {
		gc.Source = SourceAdminRevoke
	}
	res := ClawBackResult{UserID: userID}

	var sub model.Subscription
	if err := db.DB.Where("user_id = ?", userID).First(&sub).Error; err != nil {
		res.Outcome = OutcomeAlreadyFree
		return res, nil
	}
	if sub.PlanID == PlanFree {
		res.Outcome = OutcomeAlreadyFree
		return res, nil
	}
	res.PreviousPlan = sub.PlanID
	res.RoomIDs, _ = OpenRoomIDs(userID)

	if _, err := AdminSetPlanWithContext(userID, PlanFree, AssignOptions{}, gc); err != nil {
		res.Outcome = OutcomeFailed
		res.Reason = err.Error()
		return res, err
	}
	res.Outcome = OutcomeRevoked
	return res, nil
}

// clawBackOne handles one redeemer, in its own transaction.
//
// One transaction per user rather than one for the whole code: a single bad row
// (deleted user, deactivated plan) must not roll back the revoke stamp and tell
// the admin nothing happened, when the truth is "4 of 5 done, here is the one
// that failed". The expiry sweep makes the same choice for the same reason.
func clawBackOne(userID uint, code string, gc GrantContext) ClawBackResult {
	res := ClawBackResult{UserID: userID}

	var u model.User
	if err := db.DB.First(&u, userID).Error; err == nil {
		res.Email = u.Email
	}

	// Cheap pre-check outside the lock. It only decides whether to open a
	// transaction at all — the authoritative check runs inside it.
	if ok, reason := shouldClawBack(code, currentPlanOf(userID), lastEventOf(userID)); !ok {
		res.Outcome = outcomeFor(reason)
		res.Reason = reason
		return res
	}

	roomIDs, err := OpenRoomIDs(userID)
	if err != nil {
		log.Printf("[LICENSE] could not list open rooms for user %d: %v", userID, err)
	}

	txErr := db.DB.Transaction(func(tx *gorm.DB) error {
		var sub model.Subscription
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ?", userID).First(&sub).Error; err != nil {
			return errNothingToClawBack
		}

		var last model.SubscriptionEvent
		var lastPtr *model.SubscriptionEvent
		if err := tx.Where("user_id = ?", userID).
			Order("created_at DESC, id DESC").First(&last).Error; err == nil {
			lastPtr = &last
		}

		ok, reason := shouldClawBack(code, sub.PlanID, lastPtr)
		if !ok {
			res.Reason = reason
			return errNothingToClawBack
		}

		res.PreviousPlan = sub.PlanID
		_, err := setPlanTx(tx, userID, PlanFree, AssignOptions{}, gc)
		return err
	})

	switch {
	case errors.Is(txErr, errNothingToClawBack):
		res.Outcome = outcomeFor(res.Reason)
	case txErr != nil:
		res.Outcome = OutcomeFailed
		res.Reason = txErr.Error()
	default:
		res.Outcome = OutcomeRevoked
		res.RoomIDs = roomIDs
	}
	return res
}

// errNothingToClawBack rolls the transaction back without reporting a failure:
// leaving a superseded grant alone is the correct outcome, not an error.
var errNothingToClawBack = errors.New("nothing to claw back")

// reasonAlreadyFree marks the "there was nothing left to take" skips, so the
// console can word them differently from "somebody else granted this".
const reasonAlreadyFree = "already_free"

// shouldClawBack decides whether one redeemer still holds the grant this code
// created.
//
// The rule: take a grant back only if it is still the last thing that happened to
// the account — the newest subscription event is this code's redemption. Anything
// granted afterwards (another code, an admin assignment, a renewal) was not
// created by this code and is not this code's to take. That protects the case
// that actually costs money: a customer who redeemed a trial code and later paid
// for a real plan.
//
// Pure, so the rule can be tested without a database. It is the whole feature —
// a mistake here silently takes away a plan somebody paid for.
func shouldClawBack(normalizedCode, currentPlan string, last *model.SubscriptionEvent) (bool, string) {
	if currentPlan == "" || currentPlan == PlanFree {
		// Already lapsed, already downgraded, or already clawed back.
		return false, reasonAlreadyFree
	}
	if last == nil {
		// Never assert a grant we cannot prove — a seeded row has no event.
		return false, "no_history"
	}
	if last.Source != SourceCodeRedeem {
		return false, last.Source
	}
	if NormalizeCode(last.SourceRef) != NormalizeCode(normalizedCode) {
		return false, last.Source
	}
	return true, ""
}

func outcomeFor(reason string) string {
	if reason == reasonAlreadyFree {
		return OutcomeAlreadyFree
	}
	return OutcomeSuperseded
}

func currentPlanOf(userID uint) string {
	var sub model.Subscription
	if err := db.DB.Where("user_id = ?", userID).First(&sub).Error; err != nil {
		return ""
	}
	return sub.PlanID
}

func lastEventOf(userID uint) *model.SubscriptionEvent {
	var e model.SubscriptionEvent
	if err := db.DB.Where("user_id = ?", userID).
		Order("created_at DESC, id DESC").First(&e).Error; err != nil {
		return nil
	}
	return &e
}
