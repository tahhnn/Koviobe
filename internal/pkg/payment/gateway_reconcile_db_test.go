package payment

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
)

// detailServer answers GET /v1/order/detail/<invoice> with CAPTURED for the
// invoices in `captured` (amount per invoice), and 404 otherwise.
func detailServer(t *testing.T, captured map[string]int, customerID map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if u, _, ok := r.BasicAuth(); !ok || u == "" {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		inv := r.URL.Path[len("/v1/order/detail/"):]
		amt, ok := captured[inv]
		if !ok {
			rw.WriteHeader(http.StatusNotFound)
			return
		}
		cid := customerID[inv]
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"data":{"order_id":"PAY-` + inv + `","order_status":"CAPTURED","order_amount":"` +
			strconv.Itoa(amt) + `.00","customer_id":"` + cid + `","transactions":[]}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ageOrder(t *testing.T, id uint, d time.Duration) {
	t.Helper()
	must(t, db.DB.Model(&model.PaymentOrder{}).Where("id = ?", id).
		Update("created_at", time.Now().Add(-d)).Error)
}

func TestGatewayReconcileRecoversMissedIPN(t *testing.T) {
	setupGatewayDB(t)
	uid := newUser(t, "a@x.vn", "free", nil)
	o, err := CreateOrder(uid, "pro_1m")
	must(t, err)
	// Make it old enough that reconcile acts on it, and expired (IPN never came).
	ageOrder(t, o.ID, 40*time.Minute)
	_, _ = ExpireStale()

	srv := detailServer(t, map[string]int{o.OrderCode: 199000},
		map[string]string{o.OrderCode: strconv.FormatUint(uint64(uid), 10)})
	gatewayAPIBaseOverride = srv.URL
	defer func() { gatewayAPIBaseOverride = "" }()

	res, err := ReconcileGateway(context.Background(), 48*time.Hour, nil)
	must(t, err)
	if res.New != 1 || res.Paid != 1 {
		t.Fatalf("result %+v", res)
	}
	got := reload(t, o.OrderCode)
	if got.Status != model.OrderPaid || got.LicenseCode == "" {
		t.Fatalf("order %+v", got)
	}

	// A second run is a no-op: the order is paid, so it is no longer selected.
	res2, err := ReconcileGateway(context.Background(), 48*time.Hour, nil)
	must(t, err)
	if res2.New != 0 {
		t.Fatalf("second run granted again: %+v", res2)
	}
	var codes int64
	db.DB.Model(&model.LicenseCode{}).Count(&codes)
	if codes != 1 {
		t.Fatalf("codes=%d want 1", codes)
	}
}

func TestGatewayReconcileSkipsFreshAndUnpaid(t *testing.T) {
	setupGatewayDB(t)
	uid := newUser(t, "a@x.vn", "free", nil)

	// Fresh order (< 15 min): left to the IPN, not reconciled, even if CAPTURED.
	fresh, err := CreateOrder(uid, "pro_1m")
	must(t, err)
	// An old order SePay has no record of (404).
	unknown, err := CreateOrder(newUser(t, "b@x.vn", "free", nil), "pro_1m")
	must(t, err)
	ageOrder(t, unknown.ID, time.Hour)

	srv := detailServer(t, map[string]int{fresh.OrderCode: 199000}, map[string]string{})
	gatewayAPIBaseOverride = srv.URL
	defer func() { gatewayAPIBaseOverride = "" }()

	res, err := ReconcileGateway(context.Background(), 48*time.Hour, nil)
	must(t, err)
	if res.Paid != 0 {
		t.Fatalf("nothing should settle: %+v", res)
	}
	if got := reload(t, fresh.OrderCode); got.Status != model.OrderPending {
		t.Fatalf("fresh order %s", got.Status)
	}
}

func TestGatewayReconcileCustomerMismatchGoesToReview(t *testing.T) {
	setupGatewayDB(t)
	uid := newUser(t, "a@x.vn", "free", nil)
	o, err := CreateOrder(uid, "pro_1m")
	must(t, err)
	ageOrder(t, o.ID, 40*time.Minute)

	// SePay reports the right amount but a different customer id.
	srv := detailServer(t, map[string]int{o.OrderCode: 199000},
		map[string]string{o.OrderCode: "9999"})
	gatewayAPIBaseOverride = srv.URL
	defer func() { gatewayAPIBaseOverride = "" }()

	res, err := ReconcileGateway(context.Background(), 48*time.Hour, nil)
	must(t, err)
	if res.Paid != 0 || res.NeedHuman != 1 {
		t.Fatalf("result %+v", res)
	}
	got := reload(t, o.OrderCode)
	if got.Status != model.OrderNeedsReview || got.ReviewReason != "customer_mismatch" || got.LicenseCode != "" {
		t.Fatalf("order %+v", got)
	}
}
