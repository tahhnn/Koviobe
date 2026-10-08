package payment

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
)

var hookID atomic.Int64

func init() { hookID.Store(90000) }

// hook builds a SePay payload into the configured account.
func hook(content string, amount int64) SePayWebhook {
	return SePayWebhook{
		ID: hookID.Add(1), Gateway: "Vietcombank", TransactionDate: "2026-10-06 10:00:00",
		AccountNumber: "0123456789", Content: content, TransferType: "in",
		TransferAmount: amount, ReferenceCode: "FT" + fmt.Sprint(hookID.Load()),
	}
}

func apply(t *testing.T, w SePayWebhook) (Outcome, *model.BankTransaction) {
	t.Helper()
	tx := w.Transaction([]byte(`{}`))
	out, err := ApplySePay(tx)
	must(t, err)
	return out, tx
}

func reload(t *testing.T, code string) model.PaymentOrder {
	t.Helper()
	var o model.PaymentOrder
	must(t, db.DB.Where("order_code = ?", code).First(&o).Error)
	return o
}

func TestSePayExactPaymentGrants(t *testing.T) {
	setupDB(t)
	uid := newUser(t, "a@x.vn", "free", nil)
	o, err := CreateOrder(uid, "pro_1m")
	must(t, err)

	w := hook("NGUYEN VAN A chuyen tien "+o.OrderCode, 199000)
	out, tx := apply(t, w)
	if !out.Paid || out.Status != model.TxnMatched || tx.OrderCode != o.OrderCode {
		t.Fatalf("outcome %+v txn %+v", out, tx)
	}
	got := reload(t, o.OrderCode)
	if got.Status != model.OrderPaid || got.PaidAmountVND != 199000 {
		t.Fatalf("order %+v", got)
	}
	code := paidCode(t, &got)
	if code.AmountVND != 199000 || code.CreatedBy != 0 || code.ExternalRef == "" || code.MaxUses != 1 {
		t.Fatalf("code %+v", code)
	}

	// SePay retries the same id: nothing new happens.
	out2, err := ApplySePay(w.Transaction([]byte(`{}`)))
	must(t, err)
	if !out2.Duplicate || countCodes(t, o.OrderCode) != 1 {
		t.Fatalf("replay outcome %+v events %d", out2, countCodes(t, o.OrderCode))
	}
}

func TestSePaySplitCodeAndDetectedCode(t *testing.T) {
	setupDB(t)
	uid := newUser(t, "a@x.vn", "free", nil)
	o, err := CreateOrder(uid, "pro_1m")
	must(t, err)
	split := o.OrderCode[:5] + " " + o.OrderCode[5:]
	out, _ := apply(t, hook("ck "+split, 199000))
	if !out.Paid {
		t.Fatalf("split code not matched: %+v", out)
	}

	uid2 := newUser(t, "b@x.vn", "free", nil)
	o2, err := CreateOrder(uid2, "pro_1m")
	must(t, err)
	w := hook("mua goi", 199000)
	w.Code = o2.OrderCode
	if out, _ := apply(t, w); !out.Paid {
		t.Fatalf("SePay detected code not used: %+v", out)
	}
}

func TestSePayWrongAmountGoesToReview(t *testing.T) {
	setupDB(t)
	uid := newUser(t, "a@x.vn", "free", nil)
	o, err := CreateOrder(uid, "pro_1m")
	must(t, err)

	out, _ := apply(t, hook(o.OrderCode, 100000))
	if out.Paid || out.Status != model.TxnAmountMismatch || !out.NeedsHuman() {
		t.Fatalf("short payment outcome %+v", out)
	}
	got := reload(t, o.OrderCode)
	if got.Status != model.OrderNeedsReview || got.ReviewReason != "amount_mismatch" || got.PaidAmountVND != 100000 {
		t.Fatalf("order %+v", got)
	}
	// Top-up: still review (never auto-grant), amount accumulates.
	apply(t, hook(o.OrderCode, 99000))
	got = reload(t, o.OrderCode)
	if got.Status != model.OrderNeedsReview || got.PaidAmountVND != 199000 || countCodes(t, o.OrderCode) != 0 {
		t.Fatalf("after top-up %+v events %d", got, countCodes(t, o.OrderCode))
	}
}

func TestSePayIgnoredAndUnmatched(t *testing.T) {
	setupDB(t)
	uid := newUser(t, "a@x.vn", "free", nil)
	o, err := CreateOrder(uid, "pro_1m")
	must(t, err)

	out := hook(o.OrderCode, 199000)
	out.TransferType = "out"
	if r, _ := apply(t, out); r.Status != model.TxnIgnored {
		t.Fatalf("outgoing: %+v", r)
	}
	other := hook(o.OrderCode, 199000)
	other.AccountNumber = "999"
	if r, _ := apply(t, other); r.Status != model.TxnIgnored {
		t.Fatalf("other account: %+v", r)
	}
	if reload(t, o.OrderCode).Status != model.OrderPending {
		t.Fatal("ignored transfers must not touch the order")
	}

	r, tx := apply(t, hook("chuyen tien mua goi pro", 199000))
	if r.Status != model.TxnUnmatched || !r.NeedsHuman() {
		t.Fatalf("no code: %+v", r)
	}

	// Admin places it on the order.
	if _, err := AttachTransaction(tx.ID, o.OrderCode, 5, ""); !errors.Is(err, ErrTxnNoteMissing) {
		t.Fatalf("attach without note: %v", err)
	}
	att, err := AttachTransaction(tx.ID, o.OrderCode, 5, "buyer forgot the code")
	must(t, err)
	if !att.Paid || att.Status != model.TxnAttached {
		t.Fatalf("attach outcome %+v", att)
	}
	attached := reload(t, o.OrderCode)
	if code := paidCode(t, &attached); code.CreatedBy != 5 {
		t.Fatalf("code %+v", code)
	}
	if _, err := AttachTransaction(tx.ID, o.OrderCode, 5, "again"); !errors.Is(err, ErrTxnNotOpen) {
		t.Fatalf("second attach: %v", err)
	}
}

func TestSePayLateAndCancelled(t *testing.T) {
	setupDB(t)
	uid := newUser(t, "a@x.vn", "free", nil)
	o, err := CreateOrder(uid, "pro_1m")
	must(t, err)
	must(t, db.DB.Model(&model.PaymentOrder{}).Where("id = ?", o.ID).
		Updates(map[string]any{"status": model.OrderExpired, "expires_at": time.Now().Add(-time.Hour)}).Error)
	if out, _ := apply(t, hook(o.OrderCode, 199000)); !out.Paid {
		t.Fatalf("exact payment on expired QR should grant: %+v", out)
	}

	uid2 := newUser(t, "b@x.vn", "free", nil)
	o2, err := CreateOrder(uid2, "pro_1m")
	must(t, err)
	_, err = CancelUserOrder(uid2, o2.OrderCode)
	must(t, err)
	out, _ := apply(t, hook(o2.OrderCode, 199000))
	if out.Paid || out.Status != model.TxnNeedsReview {
		t.Fatalf("payment on cancelled order: %+v", out)
	}
	if got := reload(t, o2.OrderCode); got.Status != model.OrderNeedsReview || got.ReviewReason != "late_payment" {
		t.Fatalf("cancelled order %+v", got)
	}
}

func TestSePayDuplicatePaymentAndDismiss(t *testing.T) {
	setupDB(t)
	uid := newUser(t, "a@x.vn", "free", nil)
	o, err := CreateOrder(uid, "pro_1m")
	must(t, err)
	apply(t, hook(o.OrderCode, 199000))
	out, tx := apply(t, hook(o.OrderCode, 199000))
	if out.Status != model.TxnDuplicatePayment || !out.NeedsHuman() || countCodes(t, o.OrderCode) != 1 {
		t.Fatalf("second payment %+v events %d", out, countCodes(t, o.OrderCode))
	}
	must(t, DismissTransaction(tx.ID, 5, "refunded"))
	open, err := ListTransactions(TxnFilter{Status: "open"})
	must(t, err)
	if len(open) != 0 {
		t.Fatalf("open queue %+v", open)
	}
}

// Concurrent deliveries: the same id many times records once; two different
// transfers racing for one order pay it once and flag the other.
func TestSePayConcurrentDeliveries(t *testing.T) {
	setupDB(t)
	uid := newUser(t, "a@x.vn", "free", nil)
	o, err := CreateOrder(uid, "pro_1m")
	must(t, err)

	w := hook(o.OrderCode, 199000)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ApplySePay(w.Transaction([]byte(`{}`))); err != nil {
				t.Errorf("apply: %v", err)
			}
		}()
	}
	wg.Wait()
	var rows int64
	db.DB.Model(&model.BankTransaction{}).Where("provider_txn_id = ?", fmt.Sprint(w.ID)).Count(&rows)
	if rows != 1 || countCodes(t, o.OrderCode) != 1 {
		t.Fatalf("same id: rows=%d events=%d", rows, countCodes(t, o.OrderCode))
	}

	uid2 := newUser(t, "b@x.vn", "free", nil)
	o2, err := CreateOrder(uid2, "pro_1m")
	must(t, err)
	a, b := hook(o2.OrderCode, 199000), hook(o2.OrderCode, 199000)
	var paid, dup atomic.Int32
	for _, x := range []SePayWebhook{a, b} {
		wg.Add(1)
		go func(x SePayWebhook) {
			defer wg.Done()
			out, err := ApplySePay(x.Transaction([]byte(`{}`)))
			if err != nil {
				t.Errorf("apply: %v", err)
				return
			}
			if out.Paid {
				paid.Add(1)
			} else if out.Status == model.TxnDuplicatePayment {
				dup.Add(1)
			}
		}(x)
	}
	wg.Wait()
	if paid.Load() != 1 || dup.Load() != 1 || countCodes(t, o2.OrderCode) != 1 {
		t.Fatalf("race: paid=%d dup=%d events=%d", paid.Load(), dup.Load(), countCodes(t, o2.OrderCode))
	}
}
