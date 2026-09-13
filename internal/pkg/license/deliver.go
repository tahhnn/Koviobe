package license

import (
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/email"
)

// DeliverCodes emails a set of activation codes to a buyer and records the
// delivery on each code.
//
// Delivery is recorded only after the send succeeds. Marking first would leave
// codes that look delivered but never arrived, which is worse than no record at
// all — the admin would stop chasing a customer who never got anything.
//
// Note SendEmail is a synchronous SMTP dial. A batch to one address is one mail,
// so this stays a single round trip, but it does block the request.
func DeliverCodes(codes []string, to, buyerName string) error {
	if strings.TrimSpace(to) == "" {
		return fmt.Errorf("thiếu email người nhận")
	}
	if len(codes) == 0 {
		return fmt.Errorf("không có mã nào để gửi")
	}

	normalized := make([]string, 0, len(codes))
	for _, c := range codes {
		normalized = append(normalized, NormalizeCode(c))
	}

	var rows []model.LicenseCode
	if err := db.DB.Where("code IN ?", normalized).Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) != len(normalized) {
		return fmt.Errorf("một số mã không tồn tại")
	}
	for _, r := range rows {
		if r.RevokedAt != nil {
			return fmt.Errorf("mã %s đã bị thu hồi, không gửi", r.Code)
		}
	}

	subject := "Mã kích hoạt quizzZone"
	if err := email.SendEmail(to, subject, codeEmailHTML(rows, buyerName)); err != nil {
		return err
	}

	now := time.Now()
	return db.DB.Model(&model.LicenseCode{}).
		Where("code IN ?", normalized).
		Updates(map[string]interface{}{"delivered_to": to, "delivered_at": now}).Error
}

// codeEmailHTML renders the buyer-facing mail.
//
// Every interpolated value is escaped: buyerName comes from an admin form and
// the note from whoever minted the batch, and neither is trusted enough to be
// injected raw into a document the customer opens.
func codeEmailHTML(codes []model.LicenseCode, buyerName string) string {
	greeting := "Xin chào,"
	if strings.TrimSpace(buyerName) != "" {
		greeting = "Xin chào " + html.EscapeString(strings.TrimSpace(buyerName)) + ","
	}

	var list strings.Builder
	for _, c := range codes {
		term := "vĩnh viễn"
		if c.DurationDays > 0 {
			term = fmt.Sprintf("%d ngày", c.DurationDays)
		}
		uses := ""
		if c.MaxUses > 1 {
			uses = fmt.Sprintf(" · dùng được %d lượt", c.MaxUses)
		}
		shelf := ""
		if c.ExpiresAt != nil {
			shelf = fmt.Sprintf(" · hạn nhập mã: %s", c.ExpiresAt.Format("02/01/2006"))
		}
		list.WriteString(fmt.Sprintf(
			`<div style="margin:0 0 10px;padding:12px 14px;background:#f5f5f7;border-radius:8px">
				<div style="font-family:monospace;font-size:18px;letter-spacing:1px;color:#111"><strong>%s</strong></div>
				<div style="font-size:12px;color:#666;margin-top:4px">Gói %s · %s%s%s</div>
			</div>`,
			html.EscapeString(c.Code),
			html.EscapeString(c.PlanID),
			html.EscapeString(term),
			html.EscapeString(uses),
			html.EscapeString(shelf),
		))
	}

	plural := "mã"
	if len(codes) > 1 {
		plural = fmt.Sprintf("%d mã", len(codes))
	}

	return fmt.Sprintf(`<div style="font-family:system-ui,-apple-system,'Segoe UI',sans-serif;max-width:560px;margin:0 auto;color:#111">
	<h2 style="margin:0 0 4px">Mã kích hoạt quizzZone</h2>
	<p style="color:#555;margin:0 0 18px">%s đây là %s kích hoạt của bạn.</p>
	%s
	<h3 style="margin:22px 0 8px;font-size:15px">Cách kích hoạt</h3>
	<ol style="color:#444;font-size:14px;padding-left:18px;margin:0 0 18px">
		<li>Đăng nhập tài khoản quizzZone của bạn.</li>
		<li>Vào <strong>Cài đặt tài khoản</strong>, mục <strong>Mã kích hoạt</strong>.</li>
		<li>Dán mã ở trên rồi bấm <strong>Kích hoạt</strong>.</li>
	</ol>
	<p style="color:#777;font-size:12px;margin:0">
		Mã không phân biệt chữ hoa/thường và dấu gạch. Giữ mã riêng cho mình —
		ai nhập trước sẽ dùng được lượt đó.
	</p>
</div>`, greeting, plural, list.String())
}
