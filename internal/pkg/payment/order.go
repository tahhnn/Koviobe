package payment

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/license"
	"github.com/quizzzone/backend/internal/pkg/settings"
)

// OrderTTL is how long a QR stays payable. Short on purpose: a code that lives
// for days is a code a buyer pays twice, after forgetting the first transfer.
// Money that arrives after expiry is still kept — it goes to review.
const OrderTTL = 30 * time.Minute

var (
	ErrCheckoutClosed     = errors.New("Online checkout is not available")
	ErrCheckoutNotReady   = errors.New("Checkout is not configured on the server")
	ErrProductUnavailable = errors.New("Product is not available")
	ErrOrderNotFound      = errors.New("Order not found")
	ErrOrderNotPending    = errors.New("Order is no longer pending")
	ErrOrderAlreadyPaid   = errors.New("Order is already paid")
	ErrOrderNotCancelable = errors.New("Order cannot be cancelled")
	ErrConfirmFields      = errors.New("amount_vnd, external_ref and note are required to confirm an order")
)

// CheckoutConfigured reports whether the server holds everything a QR needs.
// Checked before checkout can be switched on, so the switch cannot open a
// checkout that shows a QR for an empty account.
func CheckoutConfigured() bool {
	c := config.AppConfig
	return c != nil && c.PaymentBankName != "" && c.PaymentBankAccount != "" &&
		c.PaymentQRImageBase != "" && c.SePayAPIKey != ""
}

// CheckoutEnabled is the runtime switch AND the configuration. A failed
// settings read counts as closed.
func CheckoutEnabled() bool {
	on, err := settings.GetBool(settings.KeyPaymentEnabled, false)
	return err == nil && on && CheckoutConfigured()
}

// CreateOrder opens a checkout for productID. Any pending order the user
// already has is cancelled in the same transaction: one live QR per buyer.
func CreateOrder(userID uint, productID string) (*model.PaymentOrder, error) {
	if !CheckoutEnabled() {
		return nil, ErrCheckoutClosed
	}
	var order *model.PaymentOrder
	var err error
	// A retry covers the two unique indexes: an order-code collision (rare at
	// 30^8) and a concurrent checkout by the same user having just inserted its
	// pending row — the retry cancels that one and goes ahead.
	for attempt := 0; attempt < 3; attempt++ {
		order, err = createOrderOnce(userID, productID)
		if !isUniqueViolation(err) {
			return order, err
		}
	}
	return nil, err
}

func createOrderOnce(userID uint, productID string) (*model.PaymentOrder, error) {
	code, err := NewOrderCode()
	if err != nil {
		return nil, err
	}
	var order model.PaymentOrder
	err = db.DB.Transaction(func(tx *gorm.DB) error {
		var p model.PaymentProduct
		if err := tx.Where("id = ? AND is_active = ?", productID, true).First(&p).Error; err != nil {
			return ErrProductUnavailable
		}

		if err := tx.Model(&model.PaymentOrder{}).
			Where("user_id = ? AND status = ?", userID, model.OrderPending).
			Updates(map[string]any{"status": model.OrderCancelled, "note": "superseded by a new checkout"}).Error; err != nil {
			return err
		}

		order = model.PaymentOrder{
			OrderCode:    code,
			UserID:       userID,
			ProductID:    p.ID,
			ProductName:  p.Name,
			PlanID:       p.PlanID,
			DurationDays: p.DurationDays,
			AmountVND:    p.AmountVND,
			Status:       model.OrderPending,
			ExpiresAt:    time.Now().Add(OrderTTL),
		}
		return tx.Create(&order).Error
	})
	if err != nil {
		return nil, err
	}
	return &order, nil
}

// GetUserOrder returns an order only to its owner. Someone else's code reads
// as not found, so a guessed code reveals nothing.
func GetUserOrder(userID uint, code string) (*model.PaymentOrder, error) {
	var o model.PaymentOrder
	if err := db.DB.Where("order_code = ? AND user_id = ?", strings.ToUpper(strings.TrimSpace(code)), userID).
		First(&o).Error; err != nil {
		return nil, ErrOrderNotFound
	}
	// Report expiry as soon as it happens rather than when the sweep runs, so
	// the checkout page stops waiting on a QR that can no longer be matched.
	if o.Status == model.OrderPending && time.Now().After(o.ExpiresAt) {
		o.Status = model.OrderExpired
	}
	return &o, nil
}

// UserOrderRow is an order as its buyer sees it: with whether the code it
// bought has been used, and whether it was used on the buyer's own account.
type UserOrderRow struct {
	model.PaymentOrder
	CodeUsed     bool `json:"code_used"`
	RedeemedByMe bool `json:"redeemed_by_me"`
}

// codeJoins attaches the order's code and its (single) redemption.
const codeJoins = "LEFT JOIN license_codes c ON c.code = o.license_code " +
	"LEFT JOIN license_redemptions lr ON lr.code = o.license_code"

// ListUserOrders returns the user's most recent orders with their code state.
func ListUserOrders(userID uint, limit int) ([]UserOrderRow, error) {
	var out []UserOrderRow
	err := db.DB.Table("payment_orders AS o").
		Select("o.*, COALESCE(c.used_count >= c.max_uses, false) AS code_used, "+
			"COALESCE(lr.user_id = o.user_id, false) AS redeemed_by_me").
		Joins(codeJoins).
		Where("o.user_id = ?", userID).
		Order("o.created_at DESC").Limit(limit).Scan(&out).Error
	return out, err
}

// UserOrderView wraps GetUserOrder with the code state, for the checkout page.
func UserOrderView(userID uint, code string) (*UserOrderRow, error) {
	o, err := GetUserOrder(userID, code)
	if err != nil {
		return nil, err
	}
	row := &UserOrderRow{PaymentOrder: *o}
	if o.LicenseCode != "" {
		var st struct {
			Used, Mine bool
		}
		db.DB.Table("license_codes AS c").
			Select("c.used_count >= c.max_uses AS used, "+
				"EXISTS (SELECT 1 FROM license_redemptions lr WHERE lr.code = c.code AND lr.user_id = ?) AS mine", userID).
			Where("c.code = ?", o.LicenseCode).Scan(&st)
		row.CodeUsed, row.RedeemedByMe = st.Used, st.Mine
	}
	return row, nil
}

// CancelUserOrder lets a buyer abandon their own pending checkout.
func CancelUserOrder(userID uint, code string) (*model.PaymentOrder, error) {
	var o model.PaymentOrder
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("order_code = ? AND user_id = ?", strings.ToUpper(strings.TrimSpace(code)), userID).
			First(&o).Error; err != nil {
			return ErrOrderNotFound
		}
		if o.Status != model.OrderPending {
			return ErrOrderNotPending
		}
		o.Status = model.OrderCancelled
		o.Note = "cancelled by buyer"
		return tx.Save(&o).Error
	})
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// MarkPaidTx marks an order paid and mints the activation code it bought,
// inside the caller's transaction. The only place an order turns into
// something of value — the webhook, the admin confirm and an attached transfer
// all come through here.
//
// It grants no plan. The code (one use, the order's term, the money and bank
// reference that paid for it) is the product: the buyer redeems it here or
// hands it to another account, and redeeming stacks onto a live term.
func MarkPaidTx(tx *gorm.DB, o *model.PaymentOrder, paidAmount int, gc license.GrantContext) error {
	note := "QR order " + o.OrderCode
	if gc.Note != "" {
		note += " — " + gc.Note
	}
	codes, err := license.GenerateCodesTx(tx, license.GenerateOptions{
		PlanID:       o.PlanID,
		Count:        1,
		DurationDays: o.DurationDays,
		MaxUses:      1,
		Batch:        "order:" + o.OrderCode,
		Note:         clip(note, 255),
		AmountVND:    paidAmount,
		ExternalRef:  gc.ExternalRef,
		CreatedBy:    gc.ActorUserID,
	})
	if err != nil {
		return err
	}
	now := time.Now()
	o.Status = model.OrderPaid
	o.ReviewReason = ""
	o.PaidAt = &now
	o.PaidAmountVND = paidAmount
	o.ExternalRef = gc.ExternalRef
	o.LicenseCode = codes[0].Code
	if gc.ActorUserID != 0 {
		actor := gc.ActorUserID
		o.ConfirmedBy = &actor
	}
	if gc.Note != "" {
		o.Note = gc.Note
	}
	return tx.Save(o).Error
}

// ConfirmInput is an admin settling an order by hand.
type ConfirmInput struct {
	AmountVND   int
	ExternalRef string
	Note        string
	ActorUserID uint
	IPAddress   string
}

// AdminConfirm marks an order paid on an admin's word — the webhook missed it,
// the amount was off, or the money came after expiry. Every field is required:
// an admin-settled payment with no reference and no reason is one nobody can
// audit later.
func AdminConfirm(code string, in ConfirmInput) (*model.PaymentOrder, error) {
	in.ExternalRef = strings.TrimSpace(in.ExternalRef)
	in.Note = strings.TrimSpace(in.Note)
	if in.AmountVND <= 0 || in.ExternalRef == "" || in.Note == "" {
		return nil, ErrConfirmFields
	}
	var o model.PaymentOrder
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("order_code = ?", strings.ToUpper(strings.TrimSpace(code))).First(&o).Error; err != nil {
			return ErrOrderNotFound
		}
		if o.Status == model.OrderPaid {
			return ErrOrderAlreadyPaid
		}
		return MarkPaidTx(tx, &o, in.AmountVND, license.GrantContext{
			Source:      license.SourcePaymentManual,
			ExternalRef: in.ExternalRef,
			ActorUserID: in.ActorUserID,
			Note:        in.Note,
			IPAddress:   in.IPAddress,
		})
	})
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// AdminCancel closes an order without granting anything.
func AdminCancel(code, note string, actorID uint) (*model.PaymentOrder, error) {
	var o model.PaymentOrder
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("order_code = ?", strings.ToUpper(strings.TrimSpace(code))).First(&o).Error; err != nil {
			return ErrOrderNotFound
		}
		switch o.Status {
		case model.OrderPending, model.OrderNeedsReview, model.OrderExpired:
		default:
			return ErrOrderNotCancelable
		}
		o.Status = model.OrderCancelled
		o.ConfirmedBy = &actorID
		if n := strings.TrimSpace(note); n != "" {
			o.Note = n
		}
		return tx.Save(&o).Error
	})
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// ExpireStale closes pending orders past their TTL. A transfer that still
// arrives for one is not lost: it is matched by code and sent to review.
func ExpireStale() (int64, error) {
	res := db.DB.Model(&model.PaymentOrder{}).
		Where("status = ? AND expires_at < ?", model.OrderPending, time.Now()).
		Update("status", model.OrderExpired)
	return res.RowsAffected, res.Error
}

// AdminOrderFilter narrows the admin order list.
type AdminOrderFilter struct {
	Status string
	Q      string // order code or buyer email, substring
	From   *time.Time
	To     *time.Time
	Limit  int
}

// AdminOrderRow is an order with the buyer's email for the admin console.
type AdminOrderRow struct {
	model.PaymentOrder
	Email string `json:"email"`
	// Code state: whether it was redeemed, by whom and when. The buyer may
	// legitimately redeem on another account, so RedeemedBy can differ from Email.
	CodeUsed   bool       `json:"code_used"`
	RedeemedBy string     `json:"redeemed_by,omitempty"`
	RedeemedAt *time.Time `json:"redeemed_at,omitempty"`
}

// ListAdminOrders lists orders newest first, orders that need a human on top.
func ListAdminOrders(f AdminOrderFilter) ([]AdminOrderRow, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	q := db.DB.Table("payment_orders AS o").
		Select("o.*, u.email AS email, COALESCE(c.used_count >= c.max_uses, false) AS code_used, " +
			"ru.email AS redeemed_by, lr.created_at AS redeemed_at").
		Joins("LEFT JOIN users u ON u.id = o.user_id").
		Joins(codeJoins).
		Joins("LEFT JOIN users ru ON ru.id = lr.user_id")
	if f.Status != "" {
		q = q.Where("o.status = ?", f.Status)
	}
	if s := strings.TrimSpace(f.Q); s != "" {
		like := "%" + strings.ToLower(s) + "%"
		q = q.Where("LOWER(o.order_code) LIKE ? OR LOWER(u.email) LIKE ? OR LOWER(o.license_code) LIKE ?", like, like, like)
	}
	if f.From != nil {
		q = q.Where("o.created_at >= ?", *f.From)
	}
	if f.To != nil {
		q = q.Where("o.created_at < ?", *f.To)
	}
	var rows []AdminOrderRow
	err := q.Order("CASE WHEN o.status = 'needs_review' THEN 0 ELSE 1 END, o.created_at DESC").
		Limit(f.Limit).Scan(&rows).Error
	return rows, err
}

// CheckoutInfo is what the buyer needs to pay an order.
type CheckoutInfo struct {
	BankName        string `json:"bank_name"`
	BankAccount     string `json:"bank_account"`
	BankHolder      string `json:"bank_holder,omitempty"`
	TransferContent string `json:"transfer_content"`
	QRURL           string `json:"qr_url"`
}

// CheckoutFor builds the transfer details for an order from server config.
// The QR encodes the exact amount and the order code as content, so a buyer who
// scans it cannot get either wrong.
func CheckoutFor(o *model.PaymentOrder) CheckoutInfo {
	c := config.AppConfig
	if c == nil {
		return CheckoutInfo{TransferContent: o.OrderCode}
	}
	return CheckoutInfo{
		BankName:        c.PaymentBankName,
		BankAccount:     c.PaymentBankAccount,
		BankHolder:      c.PaymentBankHolder,
		TransferContent: o.OrderCode,
		QRURL:           qrURL(c.PaymentQRImageBase, c.PaymentBankAccount, c.PaymentBankName, o.AmountVND, o.OrderCode),
	}
}

func qrURL(base, account, bank string, amount int, content string) string {
	if base == "" || account == "" || bank == "" {
		return ""
	}
	v := url.Values{}
	v.Set("acc", account)
	v.Set("bank", bank)
	v.Set("amount", strconv.Itoa(amount))
	v.Set("des", content)
	v.Set("template", "compact")
	return base + "?" + v.Encode()
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
