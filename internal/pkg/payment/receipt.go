package payment

import (
	"fmt"
	"html"
	"log"
	"time"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/email"
)

// SendReceipt mails the buyer the activation code their payment bought.
// Called after commit and off the request path: SMTP is a synchronous dial,
// and a slow mail server must not hold SePay's webhook past its 30-second
// timeout. A failed mail is logged, never undone — the code is on the order and
// on the buyer's payment history either way, and the admin can resend it from
// the codes tab.
func SendReceipt(orderID uint) {
	var o model.PaymentOrder
	if err := db.DB.First(&o, orderID).Error; err != nil || o.Status != model.OrderPaid || o.LicenseCode == "" {
		return
	}
	var u model.User
	if err := db.DB.Select("id", "email").First(&u, o.UserID).Error; err != nil || u.Email == "" {
		return
	}
	if err := email.SendEmail(u.Email, "Mã kích hoạt quizzZone — đơn "+o.OrderCode, receiptHTML(&o)); err != nil {
		log.Printf("[payment] receipt for %s not sent: %v", o.OrderCode, err)
		return
	}
	// Same bookkeeping as an admin sending codes: marked only after the mail
	// left, so "delivered" never claims more than happened.
	now := time.Now()
	db.DB.Model(&model.LicenseCode{}).Where("code = ?", o.LicenseCode).
		Updates(map[string]any{"delivered_to": u.Email, "delivered_at": now})
}

func receiptHTML(o *model.PaymentOrder) string {
	row := func(k, v string) string {
		return fmt.Sprintf(`<tr><td style="padding:6px 12px 6px 0;color:#666">%s</td><td style="padding:6px 0;color:#111"><strong>%s</strong></td></tr>`,
			html.EscapeString(k), html.EscapeString(v))
	}
	return `<div style="font-family:Arial,sans-serif;font-size:14px;line-height:1.5;color:#111">
<p>Xin chào,</p>
<p>Chúng tôi đã nhận được thanh toán của bạn. Đây là mã kích hoạt bạn đã mua:</p>
<div style="margin:12px 0;padding:14px 16px;background:#f5f5f7;border-radius:8px;font-family:monospace;font-size:20px;letter-spacing:1px"><strong>` +
		html.EscapeString(o.LicenseCode) + `</strong></div>
<table style="border-collapse:collapse;margin:12px 0">` +
		row("Mã đơn", o.OrderCode) +
		row("Gói", o.ProductName) +
		row("Thời hạn khi kích hoạt", fmt.Sprintf("%d ngày", o.DurationDays)) +
		row("Số tiền", fmt.Sprintf("%dđ", o.PaidAmountVND)) +
		`</table>
<p>Gói <strong>chưa</strong> được kích hoạt. Nhập mã trong <em>Cài đặt tài khoản → Mã kích hoạt</em> trên tài khoản bạn muốn dùng (mã dùng được 1 lần). Nếu tài khoản đó đang có gói Pro còn hạn, thời hạn mới được cộng dồn.</p>
<p style="color:#666;font-size:12px">Hãy giữ email này. Nếu cần hỗ trợ, cung cấp mã đơn và mã kích hoạt ở trên cho quản trị viên.</p>
</div>`
}
