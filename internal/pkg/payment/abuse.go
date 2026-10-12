package payment

// Payment abuse controls, split from the order lifecycle so the thresholds and
// the alerting live in one place.
//
//   - orderChurnCap: how many orders one user may open per rolling hour.
//     Enforced inside CreateOrder (order.go), counted in the DB.
//   - RegisterCancel: a per-user cancel counter in Redis. It never blocks a
//     cancel; crossing the threshold raises one Telegram alert per window, so
//     an operator sees a buyer who is spamming create/cancel.

import (
	"context"
	"strconv"
	"time"

	"github.com/quizzzone/backend/internal/cache"
	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/pkg/notify"
)

func orderChurnCap() int {
	if config.AppConfig == nil {
		return 10
	}
	return config.AppConfig.PaymentMaxOrdersPerHour
}

func cancelAlertThreshold() int {
	if config.AppConfig == nil {
		return 5
	}
	return config.AppConfig.PaymentCancelAlertThreshold
}

func cancelAlertWindow() time.Duration {
	m := 10
	if config.AppConfig != nil && config.AppConfig.PaymentCancelAlertWindowMin > 0 {
		m = config.AppConfig.PaymentCancelAlertWindowMin
	}
	return time.Duration(m) * time.Minute
}

// RegisterCancel records that userID cancelled a checkout and alerts once when
// the count first crosses the threshold inside the window.
//
// Best-effort: a disabled alert (threshold <= 0) or a Redis outage simply
// skips it — a missed spam alert must never fail a buyer's cancel. The alert
// fires only on the exact crossing (count == threshold), so a burst raises one
// message, not one per cancel.
func RegisterCancel(userID uint, email string) {
	threshold := cancelAlertThreshold()
	if threshold <= 0 || cache.RDB == nil {
		return
	}
	window := cancelAlertWindow()
	key := "abuse:pay_cancel:" + strconv.FormatUint(uint64(userID), 10)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	count, err := cache.RDB.Incr(ctx, key).Result()
	if err != nil {
		return
	}
	if count == 1 {
		cache.RDB.Expire(ctx, key, window)
	}
	if count == int64(threshold) {
		who := email
		if who == "" {
			who = "user #" + strconv.FormatUint(uint64(userID), 10)
		}
		notify.P1("pay-cancel-spam-"+strconv.FormatUint(uint64(userID), 10),
			"Nghi vấn spam thanh toán: %s đã huỷ %d đơn trong %s. Kiểm tra hành vi tạo/huỷ checkout.",
			who, threshold, window.String())
	}
}
