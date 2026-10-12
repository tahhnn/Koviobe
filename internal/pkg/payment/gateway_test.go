package payment

import (
	"encoding/json"
	"testing"

	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/model"
)

func gatewayConfig() *config.Config {
	return &config.Config{
		PaymentProvider:   ProviderSePayPG,
		SePayPGEnv:        "production",
		SePayPGMerchantID: "SP-TEST-0001",
		SePayPGSecretKey:  "test-secret-key-0123456789abcdef",
		PublicAppURL:      "https://quizzzone.com",
	}
}

// The expected signature was computed outside Go, with Python's hmac/hashlib
// following SePay's documented signFields() (form-thanh-toan): fixed field
// order, "name=value" joined by ",", base64(HMAC-SHA256).
func TestGatewayFormSignatureAndOrder(t *testing.T) {
	prev := config.AppConfig
	defer func() { config.AppConfig = prev }()
	config.AppConfig = gatewayConfig()

	o := &model.PaymentOrder{OrderCode: "KVABCDEFGH", UserID: 7, ProductName: "Pro 1 tháng", AmountVND: 199000}
	f := GatewayFormFor(o)
	if f.Action != "https://pay.sepay.vn/v1/checkout/init" {
		t.Fatalf("action %s", f.Action)
	}
	want := []string{"order_amount", "merchant", "currency", "operation", "order_description",
		"order_invoice_number", "customer_id", "success_url", "error_url", "cancel_url", "signature"}
	if len(f.Fields) != len(want) {
		t.Fatalf("fields %+v", f.Fields)
	}
	for i, name := range want {
		if f.Fields[i].Name != name {
			t.Fatalf("field %d is %s, want %s", i, f.Fields[i].Name, name)
		}
	}
	if got := f.Fields[len(f.Fields)-1].Value; got != "Zr4q3LlfwiIEaM3oHLtCy9N8MUYPDRbq4NEAuk9XBQM=" {
		t.Fatalf("signature %s", got)
	}
	if f.Fields[7].Value != "https://quizzzone.com/pricing?order=KVABCDEFGH&result=success" {
		t.Fatalf("success_url %s", f.Fields[7].Value)
	}

	config.AppConfig.SePayPGEnv = "sandbox"
	if got := GatewayFormFor(o).Action; got != "https://pay-sandbox.sepay.vn/v1/checkout/init" {
		t.Fatalf("sandbox action %s", got)
	}
}

func TestCheckoutUsesGatewayWhenSelected(t *testing.T) {
	prev := config.AppConfig
	defer func() { config.AppConfig = prev }()
	config.AppConfig = gatewayConfig()
	if !CheckoutConfigured() {
		t.Fatal("gateway config not accepted")
	}
	info := CheckoutFor(&model.PaymentOrder{OrderCode: "KVABCDEFGH", AmountVND: 1000})
	if info.Gateway == nil || info.QRURL != "" || info.BankAccount != "" {
		t.Fatalf("checkout %+v", info)
	}
	config.AppConfig.PublicAppURL = ""
	if CheckoutConfigured() {
		t.Fatal("gateway without PUBLIC_APP_URL counted as configured")
	}
}

func TestParseVND(t *testing.T) {
	for in, want := range map[string]int64{"50000": 50000, "50000.00": 50000, " 300000.0 ": 300000} {
		if got, ok := parseVND(in); !ok || got != want {
			t.Fatalf("parseVND(%q) = %d,%v", in, got, ok)
		}
	}
	for _, in := range []string{"", "50000.5", "-1", "abc"} {
		if _, ok := parseVND(in); ok {
			t.Fatalf("parseVND(%q) accepted", in)
		}
	}
}

// The documented sample body, with numbers where SePay documents strings, must
// still decode.
func TestGatewayIPNDecodesDocumentedShape(t *testing.T) {
	raw := []byte(`{"timestamp":1757058220,"notification_type":"ORDER_PAID",
	 "order":{"id":"e2c195be","order_id":"NPSETVI00101000042R","order_status":"CAPTURED","order_currency":"VND",
	  "order_amount":"50000.00","order_invoice_number":"kvabcdefgh","custom_data":[],"order_description":"x"},
	 "transaction":{"id":"384c66dd","payment_method":"CARD","transaction_id":68123,"transaction_type":"PAYMENT",
	  "transaction_date":"2025-09-01 00:00:15","transaction_status":"APPROVED","transaction_amount":50000,"transaction_currency":"VND"},
	 "customer":{"id":"bae1","customer_id":"7"}}`)
	var n GatewayIPN
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatal(err)
	}
	tx, err := n.LedgerRow(raw)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Provider != ProviderSePayPG || tx.ProviderTxnID != "384c66dd" || tx.DetectedCode != "KVABCDEFGH" ||
		tx.AmountVND != 50000 || tx.ReferenceCode != "68123" || tx.TransferType != "in" {
		t.Fatalf("txn %+v", tx)
	}
	if why := n.paidProblem(); why != "" {
		t.Fatalf("captured payment flagged: %s", why)
	}

	n.NotificationType = ipnTransactionVoid
	v, _ := n.LedgerRow(raw)
	if v.ProviderTxnID != "384c66dd:void" || v.TransferType != "void" {
		t.Fatalf("void txn %+v", v)
	}

	var empty GatewayIPN
	if _, err := empty.LedgerRow(nil); err != ErrIPNInvalid {
		t.Fatalf("empty IPN err=%v", err)
	}
}
