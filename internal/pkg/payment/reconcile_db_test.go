package payment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/model"
)

func TestReconcileRecoversMissedWebhook(t *testing.T) {
	setupDB(t)
	uid := newUser(t, "a@x.vn", "free", nil)
	missed, err := CreateOrder(uid, "pro_1m")
	must(t, err)
	uid2 := newUser(t, "b@x.vn", "free", nil)
	seen, err := CreateOrder(uid2, "pro_1m")
	must(t, err)

	// The second order's webhook did arrive (SePay id 501, ref FT-SEEN).
	w := hook(seen.OrderCode, 199000)
	w.ID, w.ReferenceCode = 501, "FT-SEEN"
	if out, _ := apply(t, w); !out.Paid {
		t.Fatalf("webhook: %+v", out)
	}

	old := time.Now().In(vnLocation).Add(-time.Hour).Format("2006-01-02 15:04:05")
	recent := time.Now().In(vnLocation).Format("2006-01-02 15:04:05")
	var gotAuth, gotAccount string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccount = r.URL.Query().Get("account_number")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"status": 200, "error": nil, "messages": map[string]any{"success": true},
			"transactions": []map[string]any{
				// Webhook never came: must be recovered and paid.
				{"id": "700", "account_number": "0123456789", "transaction_date": old,
					"amount_in": "199000.00", "amount_out": "0.00", "transaction_content": "CK " + missed.OrderCode,
					"reference_number": "FT-MISSED"},
				// Same transfer as webhook 501 under another id: must not be recorded twice.
				{"id": "701", "account_number": "0123456789", "transaction_date": old,
					"amount_in": "199000.00", "amount_out": "0.00", "transaction_content": seen.OrderCode,
					"reference_number": "FT-SEEN"},
				// Too recent: left to the webhook.
				{"id": "702", "account_number": "0123456789", "transaction_date": recent,
					"amount_in": "5000", "amount_out": "0", "transaction_content": "x", "reference_number": "FT-NEW"},
				// Outgoing.
				{"id": "703", "account_number": "0123456789", "transaction_date": old,
					"amount_in": "0", "amount_out": "1000", "transaction_content": "fee", "reference_number": "FT-OUT"},
			},
		})
	}))
	defer srv.Close()
	config.AppConfig.SePayAPIBase = srv.URL
	config.AppConfig.SePayAPIToken = "tok"

	var applied []Outcome
	res, err := Reconcile(context.Background(), time.Now().Add(-48*time.Hour), func(_ *model.BankTransaction, o Outcome) {
		applied = append(applied, o)
	})
	must(t, err)
	if gotAuth != "Bearer tok" || gotAccount != "0123456789" {
		t.Fatalf("request auth=%q account=%q", gotAuth, gotAccount)
	}
	if res.Fetched != 4 || res.New != 1 || res.Paid != 1 || res.Skipped != 3 {
		t.Fatalf("result %+v", res)
	}
	if got := reload(t, missed.OrderCode); got.Status != model.OrderPaid {
		t.Fatalf("missed order %+v", got)
	}
	if countCodes(t, seen.OrderCode) != 1 {
		t.Fatal("seen order granted twice")
	}

	// Running again changes nothing.
	res2, err := Reconcile(context.Background(), time.Now().Add(-48*time.Hour), nil)
	must(t, err)
	if res2.New != 0 {
		t.Fatalf("second run %+v", res2)
	}
}

func TestReconcileNotConfigured(t *testing.T) {
	setupDB(t)
	config.AppConfig.SePayAPIToken = ""
	if _, err := Reconcile(context.Background(), time.Now(), nil); err != ErrReconcileNotConfigured {
		t.Fatalf("err=%v", err)
	}
}
