package payment

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
)

func setupGatewayDB(t *testing.T) {
	t.Helper()
	setupDB(t)
	config.AppConfig = gatewayConfig()
}

// ipn builds an ORDER_PAID body the way SePay documents it (amounts as
// decimal strings).
func ipn(t *testing.T, typ, invoice, txnID string, amount int, mutate func(*GatewayIPN)) (GatewayIPN, []byte) {
	t.Helper()
	raw := []byte(fmt.Sprintf(`{"timestamp":1757058220,"notification_type":%q,
	 "order":{"id":"o-%s","order_id":"NPS-%s","order_status":"CAPTURED","order_currency":"VND",
	  "order_amount":"%d.00","order_invoice_number":%q,"order_description":"test"},
	 "transaction":{"id":%q,"payment_method":"BANK_TRANSFER","transaction_id":"T-%s","transaction_type":"PAYMENT",
	  "transaction_date":"2026-10-12 10:00:00","transaction_status":"APPROVED","transaction_amount":"%d","transaction_currency":"VND"}}`,
		typ, txnID, txnID, amount, invoice, txnID, txnID, amount))
	var n GatewayIPN
	must(t, json.Unmarshal(raw, &n))
	if mutate != nil {
		mutate(&n)
	}
	return n, raw
}

func applyIPN(t *testing.T, n GatewayIPN, raw []byte) (Outcome, *model.BankTransaction) {
	t.Helper()
	tx, out, err := ApplyGatewayIPN(n, raw)
	must(t, err)
	return out, tx
}

func TestGatewayExactPaymentMintsCodeOnce(t *testing.T) {
	setupGatewayDB(t)
	uid := newUser(t, "a@x.vn", "free", nil)
	o, err := CreateOrder(uid, "pro_1m")
	must(t, err)
	if info := CheckoutFor(o); info.Gateway == nil {
		t.Fatalf("no gateway form: %+v", info)
	}

	n, raw := ipn(t, ipnOrderPaid, o.OrderCode, "txn-1", 199000, nil)
	out, _ := applyIPN(t, n, raw)
	if !out.Paid || out.Status != model.TxnMatched {
		t.Fatalf("outcome %+v", out)
	}
	got := reload(t, o.OrderCode)
	if got.Status != model.OrderPaid || got.LicenseCode == "" || got.PaidAmountVND != 199000 {
		t.Fatalf("order %+v", got)
	}

	// SePay retrying the same IPN changes nothing.
	again, _ := applyIPN(t, n, raw)
	if !again.Duplicate {
		t.Fatalf("replay outcome %+v", again)
	}
	var codes int64
	db.DB.Model(&model.LicenseCode{}).Count(&codes)
	if codes != 1 {
		t.Fatalf("codes=%d want 1", codes)
	}
}

func TestGatewayWrongAmountAndUnknownInvoice(t *testing.T) {
	setupGatewayDB(t)
	uid := newUser(t, "a@x.vn", "free", nil)
	o, err := CreateOrder(uid, "pro_1m")
	must(t, err)

	n, raw := ipn(t, ipnOrderPaid, o.OrderCode, "txn-short", 100000, nil)
	out, _ := applyIPN(t, n, raw)
	if out.Paid || out.Status != model.TxnAmountMismatch {
		t.Fatalf("short payment outcome %+v", out)
	}
	if got := reload(t, o.OrderCode); got.Status != model.OrderNeedsReview {
		t.Fatalf("order status %s", got.Status)
	}

	n, raw = ipn(t, ipnOrderPaid, "KVNOSUCH01", "txn-unknown", 199000, nil)
	out, tx := applyIPN(t, n, raw)
	if out.Status != model.TxnUnmatched || tx.MatchStatus != model.TxnUnmatched {
		t.Fatalf("unknown invoice outcome %+v", out)
	}
}

func TestGatewayNotCapturedIsIgnored(t *testing.T) {
	setupGatewayDB(t)
	uid := newUser(t, "a@x.vn", "free", nil)
	o, err := CreateOrder(uid, "pro_1m")
	must(t, err)

	cases := map[string]func(*GatewayIPN){
		"declined":   func(n *GatewayIPN) { n.Transaction.TransactionStatus = "DECLINED" },
		"cancelled":  func(n *GatewayIPN) { n.Order.OrderStatus = "CANCELLED" },
		"refund":     func(n *GatewayIPN) { n.Transaction.TransactionType = "REFUND" },
		"usd":        func(n *GatewayIPN) { n.Transaction.TransactionCurrency = "USD" },
		"fractional": func(n *GatewayIPN) { n.Transaction.TransactionAmount = "199000.50" },
	}
	for name, mutate := range cases {
		n, raw := ipn(t, ipnOrderPaid, o.OrderCode, "txn-"+name, 199000, mutate)
		out, _ := applyIPN(t, n, raw)
		if out.Paid || out.Status != model.TxnIgnored {
			t.Fatalf("%s: outcome %+v", name, out)
		}
	}
	if got := reload(t, o.OrderCode); got.Status != model.OrderPending {
		t.Fatalf("order status %s, want pending", got.Status)
	}
}

func TestGatewayLatePaymentAndVoid(t *testing.T) {
	setupGatewayDB(t)
	uid := newUser(t, "a@x.vn", "free", nil)
	o, err := CreateOrder(uid, "pro_1m")
	must(t, err)
	must(t, db.DB.Model(&model.PaymentOrder{}).Where("id = ?", o.ID).
		Update("expires_at", time.Now().Add(-time.Hour)).Error)
	_, err = ExpireStale()
	must(t, err)

	// Paid on SePay's page after our QR window: still settles.
	n, raw := ipn(t, ipnOrderPaid, o.OrderCode, "txn-late", 199000, nil)
	out, _ := applyIPN(t, n, raw)
	if !out.Paid {
		t.Fatalf("late payment outcome %+v", out)
	}

	// A void for it goes to a human; the order and its code are left alone.
	v, vraw := ipn(t, ipnTransactionVoid, o.OrderCode, "txn-late", 199000, nil)
	vout, vtx := applyIPN(t, v, vraw)
	if !vout.NeedsHuman() || vtx.ProviderTxnID != "txn-late:void" || vtx.OrderCode != o.OrderCode {
		t.Fatalf("void outcome %+v txn %+v", vout, vtx)
	}
	if got := reload(t, o.OrderCode); got.Status != model.OrderPaid {
		t.Fatalf("order status %s after void, want paid", got.Status)
	}
}
