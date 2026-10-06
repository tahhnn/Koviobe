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

// Payment order statuses.
const (
	OrderPending     = "pending"
	OrderPaid        = "paid"
	OrderExpired     = "expired"
	OrderCancelled   = "cancelled"
	OrderNeedsReview = "needs_review"
)

// PaymentOrder is one self-service checkout: a host asked to buy a product and
// was shown a QR whose transfer content is OrderCode. Product terms are copied
// in at creation so a later price change never rewrites what an open order
// asks for or what a paid one cost.
//
// Buying through Zalo creates no order — an admin grants the plan directly.
type PaymentOrder struct {
	ID           uint   `gorm:"primaryKey" json:"id"`
	OrderCode    string `gorm:"size:16;uniqueIndex;not null" json:"order_code"`
	UserID       uint   `gorm:"index;not null" json:"user_id"`
	ProductID    string `gorm:"size:50;not null" json:"product_id"`
	ProductName  string `gorm:"size:100;not null" json:"product_name"`
	PlanID       string `gorm:"size:50;not null" json:"plan_id"`
	DurationDays int    `gorm:"not null" json:"duration_days"`
	AmountVND    int    `gorm:"not null" json:"amount_vnd"`
	Status       string `gorm:"size:16;not null;index" json:"status"`
	// ReviewReason says why an order needs a human: amount_mismatch,
	// late_payment, duplicate_payment. Empty otherwise.
	ReviewReason string     `gorm:"size:32" json:"review_reason,omitempty"`
	ExpiresAt    time.Time  `gorm:"index;not null" json:"expires_at"`
	PaidAt       *time.Time `json:"paid_at,omitempty"`
	// PaidAmountVND is the money that actually arrived, which is what the
	// history row carries — not AmountVND, the price asked.
	PaidAmountVND int       `gorm:"not null" json:"paid_amount_vnd"`
	ExternalRef   string    `gorm:"size:120;index" json:"external_ref,omitempty"`
	ConfirmedBy   *uint     `gorm:"index" json:"confirmed_by,omitempty"`
	Note          string    `gorm:"size:255" json:"note,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Bank transaction match outcomes.
const (
	TxnMatched          = "matched"           // paid an order automatically
	TxnUnmatched        = "unmatched"         // no order code found — a human must place it
	TxnAmountMismatch   = "amount_mismatch"   // order found, wrong amount; order sent to review
	TxnDuplicatePayment = "duplicate_payment" // order was already paid; likely a refund
	TxnNeedsReview      = "needs_review"      // order was already under review
	TxnIgnored          = "ignored"           // outgoing, or another account
	TxnDismissed        = "dismissed"         // admin: not a checkout payment
	TxnAttached         = "attached"          // admin placed it on an order by hand
)

// BankTransaction is the raw ledger of every transfer SePay reports, matched
// or not. Money that exists only in SePay's dashboard is a customer who paid
// and got nothing; keeping every row here is what makes that visible.
type BankTransaction struct {
	ID              uint      `gorm:"primaryKey" json:"id"`
	Provider        string    `gorm:"size:20;not null;uniqueIndex:uq_bank_txn_provider" json:"provider"`
	ProviderTxnID   string    `gorm:"size:64;not null;uniqueIndex:uq_bank_txn_provider" json:"provider_txn_id"`
	Gateway         string    `gorm:"size:50" json:"gateway"`
	AccountNumber   string    `gorm:"size:50" json:"account_number"`
	TransferType    string    `gorm:"size:8;not null" json:"transfer_type"`
	DetectedCode    string    `gorm:"size:32" json:"detected_code,omitempty"`
	AmountVND       int64     `gorm:"not null" json:"amount_vnd"`
	Content         string    `gorm:"size:500" json:"content"`
	Description     string    `gorm:"size:500" json:"description,omitempty"`
	ReferenceCode   string    `gorm:"size:64;index" json:"reference_code"`
	TransactionDate time.Time `json:"transaction_date"`
	OrderCode       string    `gorm:"size:16;index" json:"order_code,omitempty"`
	OrderID         *uint     `gorm:"index" json:"order_id,omitempty"`
	MatchStatus     string    `gorm:"size:24;not null;index" json:"match_status"`
	Note            string    `gorm:"size:255" json:"note,omitempty"`
	HandledBy       *uint     `json:"handled_by,omitempty"`
	RawPayload      string    `gorm:"type:jsonb" json:"-"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}
