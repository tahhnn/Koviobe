package payment

import (
	"encoding/json"
	"testing"
)

func TestFlexIntAcceptsSePayShapes(t *testing.T) {
	var v struct {
		A, B, C, D, E flexInt
	}
	must(t, json.Unmarshal([]byte(`{"A":2000,"B":"2000","C":"2000.00","D":null,"E":""}`), &v))
	if v.A != 2000 || v.B != 2000 || v.C != 2000 || v.D != 0 || v.E != 0 {
		t.Fatalf("%+v", v)
	}
	var bad struct{ A flexInt }
	if json.Unmarshal([]byte(`{"A":"abc"}`), &bad) == nil {
		t.Fatal("non-numeric must fail")
	}
}

func TestListRowToWebhook(t *testing.T) {
	in := sepayListTxn{ID: 7, AccountNumber: "0123", AmountIn: 5000, TransactionContent: "KV", ReferenceNumber: "FT7"}
	w := in.webhook()
	if w.TransferType != "in" || w.TransferAmount != 5000 || w.ReferenceCode != "FT7" || w.ID != 7 {
		t.Fatalf("%+v", w)
	}
	out := sepayListTxn{ID: 8, AmountOut: 300}
	if w := out.webhook(); w.TransferType != "out" || w.TransferAmount != 300 {
		t.Fatalf("%+v", w)
	}
}
