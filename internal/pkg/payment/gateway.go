package payment

// SePay Payment Gateway (cổng thanh toán): the buyer is sent to SePay's hosted
// page with a signed form, pays there (bank QR, NAPAS QR or card — whatever the
// merchant has enabled), and SePay confirms with an IPN.
//
// Protocol source: developer.sepay.vn/vi/cong-thanh-toan (form-thanh-toan,
// IPN) and SePay's own SDKs (github.com/sepayvn/sepay-pg-node, sepay-pg-php).
//
// The order, settle and code-minting rules are the ones the bank-transfer
// webhook uses: an IPN becomes a bank_transactions row with provider
// "sepay_pg" and goes through settle, so the admin ledger, review queue,
// receipts and alerts work the same for both.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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
)

// ProviderSePayPG names the gateway in config (PAYMENT_PROVIDER) and in
// bank_transactions.provider.
const ProviderSePayPG = "sepay_pg"

// UsesGateway reports whether checkout goes through the SePay Payment Gateway.
func UsesGateway() bool {
	c := config.AppConfig
	return c != nil && c.PaymentProvider == ProviderSePayPG
}

func gatewayConfigured() bool {
	c := config.AppConfig
	return c != nil && c.SePayPGMerchantID != "" && c.SePayPGSecretKey != "" && c.PublicAppURL != ""
}

// GatewayCheckoutURL is where the browser posts the signed form.
func GatewayCheckoutURL() string {
	if config.AppConfig != nil && config.AppConfig.SePayPGEnv == "production" {
		return "https://pay.sepay.vn/v1/checkout/init"
	}
	return "https://pay-sandbox.sepay.vn/v1/checkout/init"
}

// FormField is one hidden input. A list, not a map: SePay checks the
// signature against the fields in the order they are posted.
type FormField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// GatewayForm is what the browser needs to start a gateway payment.
type GatewayForm struct {
	Action string      `json:"action"`
	Fields []FormField `json:"fields"`
}

// gatewayFields is SePay's signing order (form-thanh-toan, "signFields"):
// every signed field present in the form, in this order, joined as
// "name=value,name=value". The form is rendered in the same order.
var gatewayFields = []string{
	"order_amount", "merchant", "currency", "operation", "order_description",
	"order_invoice_number", "customer_id", "payment_method",
	"success_url", "error_url", "cancel_url",
}

// signGatewayFields computes base64(HMAC-SHA256(secret, "k=v,k=v,...")).
func signGatewayFields(fields []FormField, secret string) string {
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, f.Name+"="+f.Value)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strings.Join(parts, ",")))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// GatewayFormFor builds the signed checkout form for a pending order.
//
// payment_method is left out on purpose: SePay then offers every method the
// merchant enabled. The return URLs come from PUBLIC_APP_URL, never from the
// request, so a forged Host cannot send a buyer elsewhere.
func GatewayFormFor(o *model.PaymentOrder) *GatewayForm {
	c := config.AppConfig
	if c == nil {
		return nil
	}
	back := func(result string) string {
		return c.PublicAppURL + "/pricing?order=" + o.OrderCode + "&result=" + result
	}
	values := map[string]string{
		"order_amount":         strconv.Itoa(o.AmountVND),
		"merchant":             c.SePayPGMerchantID,
		"currency":             "VND",
		"operation":            "PURCHASE",
		"order_description":    clip("Quizzzone "+o.ProductName+" "+o.OrderCode, 255),
		"order_invoice_number": o.OrderCode,
		"customer_id":          strconv.FormatUint(uint64(o.UserID), 10),
		"success_url":          back("success"),
		"error_url":            back("error"),
		"cancel_url":           back("cancel"),
	}
	fields := make([]FormField, 0, len(gatewayFields)+1)
	for _, name := range gatewayFields {
		if v := values[name]; v != "" {
			fields = append(fields, FormField{Name: name, Value: v})
		}
	}
	sig := signGatewayFields(fields, c.SePayPGSecretKey)
	fields = append(fields, FormField{Name: "signature", Value: sig})
	return &GatewayForm{Action: GatewayCheckoutURL(), Fields: fields}
}

// flexString accepts a JSON string or number: the IPN documents strings, the
// order API sends numeric-looking values, and a type change on SePay's side
// must not turn every payment into a 400.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*f = ""
		return nil
	}
	*f = flexString(strings.Trim(s, `"`))
	return nil
}

// GatewayIPN is the body SePay posts to the IPN URL (cong-thanh-toan/IPN).
type GatewayIPN struct {
	Timestamp        int64  `json:"timestamp"`
	NotificationType string `json:"notification_type"` // ORDER_PAID | TRANSACTION_VOID
	Order            struct {
		ID                 flexString `json:"id"`
		OrderID            flexString `json:"order_id"`
		OrderStatus        string     `json:"order_status"` // CAPTURED | CANCELLED | AUTHENTICATION_NOT_NEEDED
		OrderCurrency      string     `json:"order_currency"`
		OrderAmount        flexString `json:"order_amount"`
		OrderInvoiceNumber flexString `json:"order_invoice_number"`
		OrderDescription   string     `json:"order_description"`
	} `json:"order"`
	Transaction struct {
		ID                  flexString `json:"id"`
		PaymentMethod       string     `json:"payment_method"`
		TransactionID       flexString `json:"transaction_id"`
		TransactionType     string     `json:"transaction_type"` // PAYMENT | REFUND | VOID
		TransactionDate     string     `json:"transaction_date"`
		TransactionStatus   string     `json:"transaction_status"` // APPROVED | DECLINED
		TransactionAmount   flexString `json:"transaction_amount"`
		TransactionCurrency string     `json:"transaction_currency"`
	} `json:"transaction"`
}

const (
	ipnOrderPaid       = "ORDER_PAID"
	ipnTransactionVoid = "TRANSACTION_VOID"
)

// ErrIPNInvalid is a body that cannot identify a transaction at all.
var ErrIPNInvalid = errors.New("IPN carries no transaction id or invoice number")

// parseVND reads "50000", "50000.00" or 50000 as whole đồng. Anything with
// a non-zero fraction is refused rather than rounded: VND has no subunit, so
// a fraction means the field is not what we think it is.
func parseVND(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		if strings.Trim(s[i+1:], "0") != "" {
			return 0, false
		}
		s = s[:i]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// LedgerRow converts the IPN into a ledger row. A void gets its own id so it
// is recorded next to the payment it reverses instead of reading as a replay.
func (n GatewayIPN) LedgerRow(raw []byte) (*model.BankTransaction, error) {
	id := strings.TrimSpace(string(n.Transaction.ID))
	if id == "" {
		id = strings.TrimSpace(string(n.Transaction.TransactionID))
	}
	invoice := strings.ToUpper(strings.TrimSpace(string(n.Order.OrderInvoiceNumber)))
	if id == "" || invoice == "" {
		return nil, ErrIPNInvalid
	}
	transferType := "in"
	if n.NotificationType == ipnTransactionVoid {
		id += ":void"
		transferType = "void"
	}
	amountRaw := string(n.Transaction.TransactionAmount)
	if strings.TrimSpace(amountRaw) == "" {
		amountRaw = string(n.Order.OrderAmount)
	}
	amount, _ := parseVND(amountRaw)
	when, err := time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(n.Transaction.TransactionDate), vnLocation)
	if err != nil {
		when = time.Now()
	}
	return &model.BankTransaction{
		Provider:      ProviderSePayPG,
		ProviderTxnID: clip(id, 64),
		Gateway:       clip("SePay PG "+n.Transaction.PaymentMethod, 50),
		TransferType:  transferType,
		DetectedCode:  clip(invoice, 32),
		AmountVND:     amount,
		Content:       clip(n.Order.OrderDescription, 500),
		Description: clip(fmt.Sprintf("%s order=%s status=%s txn=%s/%s/%s %s",
			n.NotificationType, n.Order.OrderID, n.Order.OrderStatus,
			n.Transaction.TransactionType, n.Transaction.TransactionStatus,
			n.Transaction.TransactionCurrency, amountRaw), 500),
		ReferenceCode:   clip(string(n.Transaction.TransactionID), 64),
		TransactionDate: when,
		RawPayload:      string(raw),
	}, nil
}

// paidProblem says why an ORDER_PAID notification does not describe money we
// can settle, or "" when it does.
func (n GatewayIPN) paidProblem() string {
	switch {
	case n.Order.OrderStatus != "CAPTURED":
		return "order_status " + n.Order.OrderStatus
	case n.Transaction.TransactionStatus != "" && n.Transaction.TransactionStatus != "APPROVED":
		return "transaction_status " + n.Transaction.TransactionStatus
	case n.Transaction.TransactionType != "" && n.Transaction.TransactionType != "PAYMENT":
		return "transaction_type " + n.Transaction.TransactionType
	}
	cur := n.Transaction.TransactionCurrency
	if cur == "" {
		cur = n.Order.OrderCurrency
	}
	if cur != "" && cur != "VND" {
		return "currency " + cur
	}
	amountRaw := string(n.Transaction.TransactionAmount)
	if strings.TrimSpace(amountRaw) == "" {
		amountRaw = string(n.Order.OrderAmount)
	}
	if _, ok := parseVND(amountRaw); !ok {
		return "amount " + amountRaw
	}
	return ""
}

// ApplyGatewayIPN records one IPN and, when it reports an exact payment for
// a known order, pays it and mints its code — in one transaction, like
// ApplySePay. Idempotent on the gateway transaction id.
//
//   - ORDER_PAID, captured + approved → settle (exact amount → paid; anything
//     else → review, by the same rules as a bank transfer).
//   - ORDER_PAID that is not a captured, approved VND payment → ignored, kept
//     in the ledger with the reason.
//   - TRANSACTION_VOID → review: the money went back, and whether the code it
//     bought should be revoked is an admin's call.
//   - other notification types → ignored, kept in the ledger.
func ApplyGatewayIPN(n GatewayIPN, raw []byte) (*model.BankTransaction, Outcome, error) {
	var out Outcome
	t, err := n.LedgerRow(raw)
	if err != nil {
		return nil, out, err
	}
	err = db.DB.Transaction(func(tx *gorm.DB) error {
		t.MatchStatus = model.TxnUnmatched
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(t)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			out.Duplicate = true
			return nil
		}

		var order model.PaymentOrder
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("order_code = ?", t.DetectedCode).First(&order).Error
		found := err == nil
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		switch n.NotificationType {
		case ipnOrderPaid:
		case ipnTransactionVoid:
			out.Status = model.TxnNeedsReview
			if found {
				out.Order = &order
				return setTxn(tx, t, model.TxnNeedsReview, &order, "gateway reported TRANSACTION_VOID")
			}
			return setTxn(tx, t, model.TxnNeedsReview, nil, "gateway reported TRANSACTION_VOID")
		default:
			out.Status = model.TxnIgnored
			return setTxn(tx, t, model.TxnIgnored, nil, clip("notification_type "+n.NotificationType, 255))
		}

		if why := n.paidProblem(); why != "" {
			out.Status = model.TxnIgnored
			return setTxn(tx, t, model.TxnIgnored, nil, clip(why, 255))
		}
		if !found {
			out.Status = model.TxnUnmatched
			return setTxn(tx, t, model.TxnUnmatched, nil, "")
		}

		status, paid, err := settle(tx, t, &order, license.GrantContext{
			Source:      license.SourcePaymentGateway,
			ExternalRef: clip(fmt.Sprintf("sepay_pg:%s txn:%s", n.Order.OrderID, n.Transaction.TransactionID), 120),
		})
		if err != nil {
			return err
		}
		out.Status, out.Paid, out.Order = status, paid, &order
		return nil
	})
	return t, out, err
}
