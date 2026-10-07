package payment

import (
	"fmt"
	"html"
	"log"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/email"
)

// SendReceipt mails the buyer after an order is paid. Called after commit and
// off the request path: SMTP is a synchronous dial, and a slow mail server
// must not hold SePay's webhook past its 30-second timeout. A failed mail is
// logged, never undone — the plan is granted either way.
func SendReceipt(orderID uint) {
	var o model.PaymentOrder
	if err := db.DB.First(&o, orderID).Error; err != nil || o.Status != model.OrderPaid {
		return
	}
	var u model.User
	if err := db.DB.Select("id", "email").First(&u, o.UserID).Error; err != nil || u.Email == "" {
		return
	}
	var sub model.Subscription
	until := "không giới hạn"
	if err := db.DB.Where("user_id = ?", o.UserID).First(&sub).Error; err == nil && sub.EndsAt != nil {
		until = sub.EndsAt.In(vnLocation).Format("02/01/2006")
	}
	if err := email.SendEmail(u.Email, "Thanh toán thành công — quizzZone", receiptHTML(&o, until)); err != nil {
		log.Printf("[payment] receipt for %s not sent: %v", o.OrderCode, err)
	}
}

func receiptHTML(o *model.PaymentOrder, until string) string {
	row := func(k, v string) string {
		return fmt.Sprintf(`<tr><td style="padding:6px 12px 6px 0;color:#666">%s</td><td style="padding:6px 0;color:#111"><strong>%s</strong></td></tr>`,
			html.EscapeString(k), html.EscapeString(v))
	}
	return `<div style="font-family:Arial,sans-serif;font-size:14px;line-height:1.5;color:#111">
<p>Xin chào,</p>
<p>Chúng tôi đã nhận được thanh toán của bạn. Gói dịch vụ đã được kích hoạt.</p>
<table style="border-collapse:collapse;margin:12px 0">` +
		row("Mã đơn", o.OrderCode) +
		row("Gói", o.ProductName) +
		row("Số tiền", fmt.Sprintf("%dđ", o.PaidAmountVND)) +
		row("Hiệu lực đến", until) +
		`</table>
<p style="color:#666;font-size:12px">Email này được gửi tự động. Nếu có thắc mắc, vui lòng liên hệ quản trị viên và cung cấp mã đơn ở trên.</p>
</div>`
}
