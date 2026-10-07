package payment

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/license"
	"github.com/quizzzone/backend/internal/pkg/notify"
)

// ProviderSePay names SePay in bank_transactions.provider.
const ProviderSePay = "sepay"

// SePayWebhook is the body SePay posts for each bank transaction
// (docs.sepay.vn/tich-hop-webhooks.html).
type SePayWebhook struct {
	ID              int64  `json:"id"`
	Gateway         string `json:"gateway"`
	TransactionDate string `json:"transactionDate"` // "2024-07-02 11:08:33", Vietnam time
	AccountNumber   string `json:"accountNumber"`
	SubAccount      string `json:"subAccount"`
	Code            string `json:"code"`
	Content         string `json:"content"`
	TransferType    string `json:"transferType"` // "in" | "out"
	Description     string `json:"description"`
	TransferAmount  int64  `json:"transferAmount"`
	Accumulated     int64  `json:"accumulated"`
	ReferenceCode   string `json:"referenceCode"`
}

var vnLocation = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Ho_Chi_Minh"); err == nil {
		return loc
	}
	return time.FixedZone("ICT", 7*3600)
}()

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary so a Vietnamese name is never split mid-character.
	r := []rune(s)
	for len(string(r)) > n {
		r = r[:len(r)-1]
	}
	return string(r)
}

// Transaction converts the webhook into a ledger row. raw is kept verbatim so
// a dispute can be answered from what SePay actually sent.
func (w SePayWebhook) Transaction(raw []byte) *model.BankTransaction {
	when, err := time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(w.TransactionDate), vnLocation)
	if err != nil {
		when = time.Now()
	}
	return &model.BankTransaction{
		Provider:        ProviderSePay,
		ProviderTxnID:   strconv.FormatInt(w.ID, 10),
		Gateway:         clip(w.Gateway, 50),
		AccountNumber:   clip(strings.TrimSpace(w.AccountNumber), 50),
		TransferType:    clip(strings.ToLower(strings.TrimSpace(w.TransferType)), 8),
		DetectedCode:    clip(strings.TrimSpace(w.Code), 32),
		AmountVND:       w.TransferAmount,
		Content:         clip(w.Content, 500),
		Description:     clip(w.Description, 500),
		ReferenceCode:   clip(w.ReferenceCode, 64),
		TransactionDate: when,
		RawPayload:      string(raw),
	}
}

// Outcome is what applying a transaction did, for the caller's side effects
// (receipt email, alert) which must run after commit, never inside it.
type Outcome struct {
	Duplicate bool // this SePay id was already recorded; nothing done
	Status    string
	Order     *model.PaymentOrder
	Paid      bool // an order was marked paid and its plan granted
}

// NeedsHuman reports whether an admin has to act on this transaction.
func (o Outcome) NeedsHuman() bool {
	switch o.Status {
	case model.TxnUnmatched, model.TxnAmountMismatch, model.TxnDuplicatePayment, model.TxnNeedsReview:
		return true
	}
	return false
}

// ApplySePay records one SePay transaction and, when it pays an order exactly,
// grants the plan — all in one transaction, so a crash between "money seen"
// and "plan granted" leaves neither, and SePay's retry starts clean.
//
// Idempotent on the SePay id: a retried or replayed webhook is a no-op.
func ApplySePay(t *model.BankTransaction) (Outcome, error) {
	var out Outcome
	// The same bank transfer under a different SePay id (webhook vs list API)
	// is still the same money; recording it twice would flag the buyer's one
	// payment as a duplicate.
	if knownReference(t) {
		out.Duplicate = true
		return out, nil
	}
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		t.MatchStatus = model.TxnUnmatched
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(t)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			out.Duplicate = true
			return nil
		}

		account := ""
		if config.AppConfig != nil {
			account = config.AppConfig.PaymentBankAccount
		}
		if t.TransferType != "in" || account == "" || t.AccountNumber != account {
			out.Status = model.TxnIgnored
			return setTxn(tx, t, model.TxnIgnored, nil, "")
		}

		var order model.PaymentOrder
		found := false
		for _, code := range OrderCodeCandidates(t.DetectedCode, t.Content+" "+t.Description) {
			err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("order_code = ?", code).First(&order).Error
			if err == nil {
				found = true
				break
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		if !found {
			out.Status = model.TxnUnmatched
			return setTxn(tx, t, model.TxnUnmatched, nil, "")
		}

		status, paid, err := settle(tx, t, &order, license.GrantContext{
			Source:      license.SourcePaymentSePay,
			ExternalRef: sepayRef(t),
		})
		if err != nil {
			return err
		}
		out.Status, out.Paid, out.Order = status, paid, &order
		return nil
	})
	return out, err
}

func sepayRef(t *model.BankTransaction) string {
	ref := "sepay:" + t.ProviderTxnID
	if t.ReferenceCode != "" {
		ref += " ref:" + t.ReferenceCode
	}
	return clip(ref, 120)
}

// settle decides what a transaction found for an order does to it. Shared by
// the webhook and by an admin placing an unmatched transaction on an order, so
// the two can never disagree about when money becomes a plan.
//
//   - exact amount, order pending or expired → paid, plan granted. An expired
//     QR paid in full is still a clear intent to buy; terms stack, so even a
//     buyer who also paid a newer order loses nothing.
//   - order already paid → duplicate; the order is left alone (likely refund).
//   - order cancelled → review: the buyer, or a newer checkout, called it off.
//   - order already in review → stays there; the extra money is added to it
//     (a buyer topping up a short transfer).
//   - wrong amount → review. Never grant on a partial or surplus transfer.
func settle(tx *gorm.DB, t *model.BankTransaction, o *model.PaymentOrder, gc license.GrantContext) (string, bool, error) {
	exact := t.AmountVND == int64(o.AmountVND)
	switch {
	case o.Status == model.OrderPaid:
		return model.TxnDuplicatePayment, false, setTxn(tx, t, model.TxnDuplicatePayment, o, "")
	case o.Status == model.OrderNeedsReview:
		// Keep the existing reason; the money still adds to what arrived.
		err := review(tx, t, o, o.ReviewReason)
		return t.MatchStatus, false, err
	case o.Status == model.OrderCancelled:
		return model.TxnNeedsReview, false, review(tx, t, o, "late_payment")
	case !exact:
		return model.TxnAmountMismatch, false, review(tx, t, o, "amount_mismatch")
	}
	if o.Status == model.OrderExpired {
		gc.Note = strings.TrimSpace(gc.Note + " paid after QR expiry")
	}
	if err := MarkPaidTx(tx, o, int(t.AmountVND), gc); err != nil {
		return "", false, err
	}
	status := model.TxnMatched
	if gc.ActorUserID != 0 {
		status = model.TxnAttached
	}
	return status, true, setTxn(tx, t, status, o, "")
}

func review(tx *gorm.DB, t *model.BankTransaction, o *model.PaymentOrder, reason string) error {
	o.Status = model.OrderNeedsReview
	o.ReviewReason = reason
	o.PaidAmountVND += int(t.AmountVND)
	if err := tx.Save(o).Error; err != nil {
		return err
	}
	status := model.TxnNeedsReview
	if reason == "amount_mismatch" {
		status = model.TxnAmountMismatch
	}
	return setTxn(tx, t, status, o, "")
}

func setTxn(tx *gorm.DB, t *model.BankTransaction, status string, o *model.PaymentOrder, note string) error {
	t.MatchStatus = status
	updates := map[string]any{"match_status": status}
	if o != nil {
		t.OrderID, t.OrderCode = &o.ID, o.OrderCode
		updates["order_id"], updates["order_code"] = o.ID, o.OrderCode
	}
	if note != "" {
		t.Note = note
		updates["note"] = note
	}
	return tx.Model(&model.BankTransaction{}).Where("id = ?", t.ID).Updates(updates).Error
}

var (
	ErrTxnNotFound    = errors.New("Bank transaction not found")
	ErrTxnNotOpen     = errors.New("Bank transaction is already handled")
	ErrTxnNoteMissing = errors.New("A note is required")
)

// AttachTransaction places an unmatched transfer on an order an admin
// identified (buyer typed the code wrong, or not at all), then settles it by
// the same rules as the webhook.
func AttachTransaction(id uint, orderCode string, actorID uint, note string) (Outcome, error) {
	note = strings.TrimSpace(note)
	if note == "" {
		return Outcome{}, ErrTxnNoteMissing
	}
	var out Outcome
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		var t model.BankTransaction
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&t, id).Error; err != nil {
			return ErrTxnNotFound
		}
		if t.MatchStatus != model.TxnUnmatched {
			return ErrTxnNotOpen
		}
		var o model.PaymentOrder
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("order_code = ?", strings.ToUpper(strings.TrimSpace(orderCode))).First(&o).Error; err != nil {
			return ErrOrderNotFound
		}
		if err := tx.Model(&t).Updates(map[string]any{"handled_by": actorID, "note": clip(note, 255)}).Error; err != nil {
			return err
		}
		status, paid, err := settle(tx, &t, &o, license.GrantContext{
			Source:      license.SourcePaymentManual,
			ExternalRef: sepayRef(&t),
			ActorUserID: actorID,
			Note:        note,
		})
		if err != nil {
			return err
		}
		out.Status, out.Paid, out.Order = status, paid, &o
		return nil
	})
	return out, err
}

// DismissTransaction marks a transfer as not a checkout payment (a transfer
// for something else into the same account) so it leaves the review queue.
// The row itself stays.
func DismissTransaction(id uint, actorID uint, note string) error {
	note = strings.TrimSpace(note)
	if note == "" {
		return ErrTxnNoteMissing
	}
	return db.DB.Transaction(func(tx *gorm.DB) error {
		var t model.BankTransaction
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&t, id).Error; err != nil {
			return ErrTxnNotFound
		}
		if t.MatchStatus != model.TxnUnmatched && t.MatchStatus != model.TxnDuplicatePayment {
			return ErrTxnNotOpen
		}
		return tx.Model(&t).Updates(map[string]any{
			"match_status": model.TxnDismissed, "handled_by": actorID, "note": clip(note, 255),
		}).Error
	})
}

// TxnFilter narrows the admin ledger view.
type TxnFilter struct {
	Status string // one status, "open" for everything needing a human, or "" for all
	Q      string // content, reference or order code
	Limit  int
}

// ListTransactions returns ledger rows newest first.
func ListTransactions(f TxnFilter) ([]model.BankTransaction, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	q := db.DB.Model(&model.BankTransaction{})
	switch f.Status {
	case "":
	case "open":
		q = q.Where("match_status IN ?", []string{model.TxnUnmatched, model.TxnDuplicatePayment})
	default:
		q = q.Where("match_status = ?", f.Status)
	}
	if s := strings.TrimSpace(f.Q); s != "" {
		like := "%" + strings.ToLower(s) + "%"
		q = q.Where("LOWER(content) LIKE ? OR LOWER(reference_code) LIKE ? OR LOWER(order_code) LIKE ?", like, like, like)
	}
	var rows []model.BankTransaction
	err := q.Order("created_at DESC").Limit(f.Limit).Find(&rows).Error
	return rows, err
}

// Describe is a one-line summary for alerts.
func Describe(t *model.BankTransaction, out Outcome) string {
	code := t.OrderCode
	if code == "" {
		code = "—"
	}
	return fmt.Sprintf("SePay #%s %s %dđ, đơn %s, nội dung %q", t.ProviderTxnID, out.Status, t.AmountVND, code, clip(t.Content, 120))
}

// AfterApplied runs the side effects of a newly recorded transfer, after its
// transaction committed: the buyer's receipt and the admin alert. Shared by
// the webhook and reconciliation so both tell people the same things.
func AfterApplied(t *model.BankTransaction, out Outcome) {
	if out.Duplicate {
		return
	}
	if out.Paid && out.Order != nil {
		go SendReceipt(out.Order.ID)
	}
	if out.NeedsHuman() {
		notify.P1("sepay-review-"+t.ProviderTxnID, "Cần xử lý thanh toán: %s", Describe(t, out))
	}
}
