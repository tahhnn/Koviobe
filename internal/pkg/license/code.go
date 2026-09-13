package license

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// codePrefix is fixed so a support agent can recognise one of our codes on sight.
const codePrefix = "KOVIO"

// codeAlphabet drops 0/1/I/L/O/U: the pairs a buyer mistypes when reading a code
// off a receipt or hearing it over the phone. 30 symbols x 12 positions is ~5.3e17
// codes, so guessing is not a realistic attack even before rate limiting.
const codeAlphabet = "23456789ABCDEFGHJKMNPQRSTVWXYZ"

const codePayloadLen = 12

// Redeem failures the caller is allowed to show the user verbatim. Anything else
// is an internal error and must not leak.
var (
	ErrCodeNotFound     = errors.New("mã kích hoạt không tồn tại")
	ErrCodeRevoked      = errors.New("mã kích hoạt đã bị thu hồi")
	ErrCodeExpired      = errors.New("mã kích hoạt đã hết hạn sử dụng")
	ErrCodeExhausted    = errors.New("mã kích hoạt đã dùng hết lượt")
	ErrCodeAlreadyUsed  = errors.New("bạn đã dùng mã này rồi")
	ErrCodePlanInactive = errors.New("gói của mã này không còn được bán")
)

// NormalizeCode makes matching independent of how the buyer typed the code:
// case, spaces, and dash placement all collapse to one canonical form. Every
// read and write of a code goes through this, so a code typed
// "kovio 23ab 45cd 67ef" finds the row stored as "KOVIO-23AB-45CD-67EF".
func NormalizeCode(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(raw)) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	s := b.String()
	s = strings.TrimPrefix(s, codePrefix)
	if len(s) != codePayloadLen {
		// Not our shape. Return it normalized anyway so the caller gets a clean
		// ErrCodeNotFound instead of a confusing partial match.
		return strings.ToUpper(strings.TrimSpace(raw))
	}
	return fmt.Sprintf("%s-%s-%s-%s", codePrefix, s[0:4], s[4:8], s[8:12])
}

func randomPayload() (string, error) {
	max := big.NewInt(int64(len(codeAlphabet)))
	b := make([]byte, codePayloadLen)
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = codeAlphabet[n.Int64()]
	}
	return string(b), nil
}

// GenerateOptions describes one batch of codes.
type GenerateOptions struct {
	PlanID       string
	Count        int
	DurationDays int        // 0 = lifetime subscription once redeemed
	MaxUses      int        // >= 1
	ExpiresAt    *time.Time // shelf life of the code itself
	Batch        string
	Note         string
	// AmountVND / ExternalRef record what the buyer paid and the intermediary's
	// reference. Carried onto the code, then onto the history row when it is
	// redeemed, so a grant can be traced back to a payment.
	AmountVND   int
	ExternalRef string
	CreatedBy   uint
}

// GenerateCodes mints a batch of unused activation codes.
//
// Codes are inserted one at a time with OnConflict DoNothing rather than as a
// single bulk insert: a collision on the primary key would otherwise abort the
// whole batch. A collision is astronomically unlikely, but retrying one row is
// cheaper than losing 500.
func GenerateCodes(opts GenerateOptions) ([]model.LicenseCode, error) {
	if opts.Count < 1 || opts.Count > 500 {
		return nil, errors.New("count must be between 1 and 500")
	}
	if opts.MaxUses < 1 {
		opts.MaxUses = 1
	}
	if opts.DurationDays < 0 {
		return nil, errors.New("duration_days must be >= 0 (0 = lifetime)")
	}

	var plan model.PricingPlan
	if err := db.DB.Where("id = ? AND is_active = ?", opts.PlanID, true).First(&plan).Error; err != nil {
		return nil, errors.New("plan not found or inactive")
	}
	if opts.PlanID == PlanFree {
		return nil, errors.New("cannot mint codes for the free tier")
	}

	out := make([]model.LicenseCode, 0, opts.Count)
	for len(out) < opts.Count {
		payload, err := randomPayload()
		if err != nil {
			return nil, err
		}
		code := model.LicenseCode{
			Code:         NormalizeCode(codePrefix + payload),
			PlanID:       opts.PlanID,
			DurationDays: opts.DurationDays,
			MaxUses:      opts.MaxUses,
			ExpiresAt:    opts.ExpiresAt,
			Batch:        opts.Batch,
			Note:         opts.Note,
			AmountVND:    opts.AmountVND,
			ExternalRef:  opts.ExternalRef,
			CreatedBy:    opts.CreatedBy,
		}
		res := db.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&code)
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 0 {
			continue // collided with an existing code; draw again
		}
		out = append(out, code)
	}
	return out, nil
}

// RedeemCode applies a code to a user's subscription.
//
// Everything runs in one transaction with the code row locked FOR UPDATE, so two
// concurrent redemptions of the last remaining use cannot both succeed: the
// second blocks on the lock, then re-reads UsedCount and fails ErrCodeExhausted.
// The unique index on (code, user_id) is the second line of defence — it turns a
// double-submit by the same user into ErrCodeAlreadyUsed rather than two grants.
func RedeemCode(userID uint, rawCode, ip string) (*model.Subscription, Entitlements, error) {
	normalized := NormalizeCode(rawCode)
	var sub *model.Subscription

	err := db.DB.Transaction(func(tx *gorm.DB) error {
		var code model.LicenseCode
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("code = ?", normalized).First(&code).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrCodeNotFound
			}
			return err
		}

		if code.RevokedAt != nil {
			return ErrCodeRevoked
		}
		if code.ExpiresAt != nil && time.Now().After(*code.ExpiresAt) {
			return ErrCodeExpired
		}
		if code.UsedCount >= code.MaxUses {
			return ErrCodeExhausted
		}

		var plan model.PricingPlan
		if err := tx.Where("id = ? AND is_active = ?", code.PlanID, true).First(&plan).Error; err != nil {
			return ErrCodePlanInactive
		}

		redemption := model.LicenseRedemption{
			Code:      code.Code,
			UserID:    userID,
			PlanID:    code.PlanID,
			IPAddress: ip,
		}
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&redemption)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrCodeAlreadyUsed
		}

		if err := tx.Model(&model.LicenseCode{}).Where("code = ?", code.Code).
			UpdateColumn("used_count", gorm.Expr("used_count + 1")).Error; err != nil {
			return err
		}

		opts := AssignOptions{}
		if code.DurationDays > 0 {
			d := code.DurationDays
			opts.EndsAtDays = &d
		}
		s, err := setPlanTx(tx, userID, code.PlanID, opts, GrantContext{
			Source:      SourceCodeRedeem,
			SourceRef:   code.Code,
			AmountVND:   code.AmountVND,
			ExternalRef: code.ExternalRef,
			ActorUserID: userID,
			Note:        code.Note,
			IPAddress:   ip,
		})
		if err != nil {
			return err
		}
		sub = s
		return nil
	})
	if err != nil {
		return nil, Entitlements{}, err
	}

	ents, _ := GetEntitlements(userID)
	return sub, ents, nil
}

// RevokeCode blocks further redemptions without deleting the row, so the
// redemption history stays auditable.
func RevokeCode(rawCode string) (*model.LicenseCode, error) {
	normalized := NormalizeCode(rawCode)
	var code model.LicenseCode
	if err := db.DB.Where("code = ?", normalized).First(&code).Error; err != nil {
		return nil, ErrCodeNotFound
	}
	if code.RevokedAt == nil {
		now := time.Now()
		code.RevokedAt = &now
		if err := db.DB.Save(&code).Error; err != nil {
			return nil, err
		}
	}
	return &code, nil
}
