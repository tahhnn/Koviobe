package payment

// Database tests for the order lifecycle. They need a real Postgres (partial
// unique index, SELECT ... FOR UPDATE) and are skipped unless
// PAYMENT_TEST_DSN points at a throwaway database — never a real one: every
// table used here is truncated.
//
//	docker run -d --name pay-pg -e POSTGRES_PASSWORD=x -p 55432:5432 postgres:16-alpine
//	PAYMENT_TEST_DSN='host=... user=postgres password=x dbname=postgres sslmode=disable' go test ./internal/pkg/payment/

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/license"
	"github.com/quizzzone/backend/internal/pkg/settings"
)

func setupDB(t *testing.T) {
	t.Helper()
	dsn := os.Getenv("PAYMENT_TEST_DSN")
	if dsn == "" {
		t.Skip("PAYMENT_TEST_DSN not set")
	}
	g, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	db.DB = g
	if err := g.AutoMigrate(&model.User{}, &model.PricingPlan{}, &model.Subscription{},
		&model.SubscriptionEvent{}, &model.SystemSetting{}, &model.PaymentProduct{}, &model.PaymentOrder{}); err != nil {
		t.Fatal(err)
	}
	g.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_orders_one_pending ON payment_orders (user_id) WHERE status = 'pending'`)
	g.Exec(`TRUNCATE users, pricing_plans, subscriptions, subscription_events, system_settings, payment_products, payment_orders RESTART IDENTITY CASCADE`)

	must(t, g.Create(&model.PricingPlan{ID: "free", Name: "Free", IsActive: true}).Error)
	must(t, g.Create(&model.PricingPlan{ID: "pro", Name: "Pro", IsActive: true, MaxPlayersPerRoom: 2000}).Error)
	must(t, g.Create(&model.PaymentProduct{ID: "pro_1m", PlanID: "pro", Name: "Pro 1m", DurationDays: 30, AmountVND: 199000, IsActive: true}).Error)
	must(t, g.Create(&model.PaymentProduct{ID: "off", PlanID: "pro", Name: "Off", DurationDays: 30, AmountVND: 1000, IsActive: false}).Error)

	config.AppConfig = &config.Config{
		PaymentBankName: "Vietcombank", PaymentBankAccount: "0123456789",
		PaymentQRImageBase: "https://vietqr.app/img", SePayAPIKey: "k",
	}
	must(t, settings.SetBool(settings.KeyPaymentEnabled, true, 0))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newUser(t *testing.T, email, plan string, endsAt *time.Time) uint {
	t.Helper()
	u := model.User{Email: email, Password: "x", IsActive: true}
	must(t, db.DB.Create(&u).Error)
	must(t, db.DB.Create(&model.Subscription{UserID: u.ID, PlanID: plan, Status: "active",
		StartsAt: time.Now().AddDate(0, 0, -5), EndsAt: endsAt}).Error)
	return u.ID
}

func TestCreateOrderLifecycle(t *testing.T) {
	setupDB(t)
	uid := newUser(t, "a@x.vn", "free", nil)

	o1, err := CreateOrder(uid, "pro_1m")
	must(t, err)
	if o1.Status != model.OrderPending || o1.AmountVND != 199000 || o1.DurationDays != 30 {
		t.Fatalf("bad order %+v", o1)
	}
	if info := CheckoutFor(o1); info.QRURL == "" || info.TransferContent != o1.OrderCode {
		t.Fatalf("bad checkout %+v", info)
	}

	// A second checkout supersedes the first.
	o2, err := CreateOrder(uid, "pro_1m")
	must(t, err)
	var first model.PaymentOrder
	must(t, db.DB.Where("order_code = ?", o1.OrderCode).First(&first).Error)
	if first.Status != model.OrderCancelled {
		t.Fatalf("first order status %s, want cancelled", first.Status)
	}

	// Owner only.
	other := newUser(t, "b@x.vn", "free", nil)
	if _, err := GetUserOrder(other, o2.OrderCode); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("foreign read err=%v", err)
	}

	if _, err := CreateOrder(uid, "off"); !errors.Is(err, ErrProductUnavailable) {
		t.Fatalf("inactive product err=%v", err)
	}
}

func TestCheckoutClosed(t *testing.T) {
	setupDB(t)
	uid := newUser(t, "a@x.vn", "free", nil)
	must(t, settings.SetBool(settings.KeyPaymentEnabled, false, 0))
	if _, err := CreateOrder(uid, "pro_1m"); !errors.Is(err, ErrCheckoutClosed) {
		t.Fatalf("err=%v", err)
	}
	must(t, settings.SetBool(settings.KeyPaymentEnabled, true, 0))
	config.AppConfig.SePayAPIKey = ""
	if _, err := CreateOrder(uid, "pro_1m"); !errors.Is(err, ErrCheckoutClosed) {
		t.Fatalf("unconfigured err=%v", err)
	}
}

func TestLifetimeHostCannotBuy(t *testing.T) {
	setupDB(t)
	uid := newUser(t, "a@x.vn", "pro", nil)
	if _, err := CreateOrder(uid, "pro_1m"); !errors.Is(err, ErrLifetimePlan) {
		t.Fatalf("err=%v", err)
	}
}

func TestAdminConfirmStacksAndRecords(t *testing.T) {
	setupDB(t)
	in10 := time.Now().AddDate(0, 0, 10)
	uid := newUser(t, "a@x.vn", "pro", &in10)
	o, err := CreateOrder(uid, "pro_1m")
	must(t, err)

	if _, err := AdminConfirm(o.OrderCode, ConfirmInput{AmountVND: 199000, ExternalRef: "FT1"}); !errors.Is(err, ErrConfirmFields) {
		t.Fatalf("missing note err=%v", err)
	}
	paid, err := AdminConfirm(o.OrderCode, ConfirmInput{AmountVND: 199000, ExternalRef: "FT1", Note: "webhook down", ActorUserID: 7})
	must(t, err)
	if paid.Status != model.OrderPaid || paid.PaidAmountVND != 199000 || paid.ConfirmedBy == nil || *paid.ConfirmedBy != 7 {
		t.Fatalf("bad paid order %+v", paid)
	}

	var sub model.Subscription
	must(t, db.DB.Where("user_id = ?", uid).First(&sub).Error)
	want := in10.AddDate(0, 0, 30)
	if sub.EndsAt == nil || sub.EndsAt.Sub(want).Abs() > time.Minute {
		t.Fatalf("ends_at=%v want ≈%v (10 days left + 30)", sub.EndsAt, want)
	}

	var ev model.SubscriptionEvent
	must(t, db.DB.Order("id DESC").First(&ev).Error)
	if ev.Source != license.SourcePaymentManual || ev.SourceRef != o.OrderCode || ev.AmountVND != 199000 ||
		ev.ExternalRef != "FT1" || ev.Action != "renew" {
		t.Fatalf("bad event %+v", ev)
	}

	if _, err := AdminConfirm(o.OrderCode, ConfirmInput{AmountVND: 1, ExternalRef: "x", Note: "x"}); !errors.Is(err, ErrOrderAlreadyPaid) {
		t.Fatalf("double confirm err=%v", err)
	}
}

// Two admins confirming the same order at once grant exactly one term.
func TestConcurrentConfirmGrantsOnce(t *testing.T) {
	setupDB(t)
	uid := newUser(t, "a@x.vn", "free", nil)
	o, err := CreateOrder(uid, "pro_1m")
	must(t, err)

	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, already := 0, 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := AdminConfirm(o.OrderCode, ConfirmInput{AmountVND: 199000, ExternalRef: "FT", Note: "n"})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrOrderAlreadyPaid):
				already++
			default:
				t.Errorf("unexpected err %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || already != 7 {
		t.Fatalf("ok=%d already=%d", ok, already)
	}
	var n int64
	db.DB.Model(&model.SubscriptionEvent{}).Where("source_ref = ?", o.OrderCode).Count(&n)
	if n != 1 {
		t.Fatalf("events=%d want 1", n)
	}
}

// Concurrent checkouts by one user leave exactly one pending order.
func TestConcurrentCreateLeavesOnePending(t *testing.T) {
	setupDB(t)
	uid := newUser(t, "a@x.vn", "free", nil)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = CreateOrder(uid, "pro_1m")
		}()
	}
	wg.Wait()
	var n int64
	db.DB.Model(&model.PaymentOrder{}).Where("user_id = ? AND status = ?", uid, model.OrderPending).Count(&n)
	if n != 1 {
		t.Fatalf("pending=%d want 1", n)
	}
}

func TestExpireAndCancel(t *testing.T) {
	setupDB(t)
	uid := newUser(t, "a@x.vn", "free", nil)
	o, err := CreateOrder(uid, "pro_1m")
	must(t, err)
	must(t, db.DB.Model(&model.PaymentOrder{}).Where("id = ?", o.ID).
		Update("expires_at", time.Now().Add(-time.Minute)).Error)

	got, err := GetUserOrder(uid, o.OrderCode)
	must(t, err)
	if got.Status != model.OrderExpired {
		t.Fatalf("read status %s, want expired before the sweep", got.Status)
	}
	if _, err := CancelUserOrder(uid, o.OrderCode); err != nil {
		// Still pending in the table until the sweep; the buyer may cancel it.
		t.Fatalf("cancel before sweep: %v", err)
	}

	o2, err := CreateOrder(uid, "pro_1m")
	must(t, err)
	must(t, db.DB.Model(&model.PaymentOrder{}).Where("id = ?", o2.ID).
		Update("expires_at", time.Now().Add(-time.Minute)).Error)
	n, err := ExpireStale()
	must(t, err)
	if n != 1 {
		t.Fatalf("expired %d want 1", n)
	}
	if _, err := CancelUserOrder(uid, o2.OrderCode); !errors.Is(err, ErrOrderNotPending) {
		t.Fatalf("cancel expired err=%v", err)
	}
	if _, err := AdminCancel(o2.OrderCode, "no payment", 1); err != nil {
		t.Fatalf("admin cancel expired: %v", err)
	}
}
