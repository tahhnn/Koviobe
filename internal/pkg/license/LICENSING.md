# License / Pro gating

**Status:** Code đầy đủ (mã kích hoạt, giao mã qua email, lịch sử đối soát, tự cắt khi
hết hạn), nhưng **cờ `LICENSE_ENFORCEMENT` đang TẮT trên prod** — mọi host vẫn nhận
`openEntitlements()`, không gate nào chạy. Viết code 2026-09-13; chưa bật.

Đừng đọc mục này là "đã enforce". Trạng thái thật luôn lấy từ `GET /api/license/me`
(trường `enforcement`), không lấy từ tài liệu.

Còn phải làm trước khi bật — xem "Quy trình bật enforcement" bên dưới:

- [ ] `01_lock_free_plan.sql` (chưa chạy)
- [ ] `02_grandfather_hosts.sql` (chưa chạy — **bỏ bước này là khoá luôn admin**)
- [x] `03_fix_code_duration_default.sql`
- [x] `04_fix_questions_per_quiz_default.sql`

Thanh toán: hiện vẫn **ngoài app** — khách liên hệ qua Zalo (link `PAYMENT_ZALO_URL`, trả ở `GET /api/payments/products`), trả tiền, admin gán gói (ghi `amount_vnd` + `external_ref`) hoặc đúc mã. QR tự động qua SePay đang làm theo plan `/root/plans/2026-10-06-payment-sepay-zalo.md` — xem mục "Thanh toán" cuối file.

## Kill switch

`LICENSE_ENFORCEMENT` (env) → `config.AppConfig.LicenseEnforcement` → `license.Enforcing()`.

Là config, không phải build-time const: bật/tắt chỉ cần restart, không rebuild image.

```bash
LICENSE_ENFORCEMENT=true   # gate hoạt động
LICENSE_ENFORCEMENT=false  # mọi host nhận openEntitlements() — không chặn gì
```

Mặc định `false`. Nếu config lỗi (`AppConfig == nil`) thì `Enforcing()` trả `false` —
config hỏng không được phép khoá toàn bộ host ra khỏi sản phẩm.

## Mô hình: khoá ngay từ đăng ký

`free` không còn là gói dùng thử. Nó là **trạng thái chưa kích hoạt**: mọi limit = 0.

Gate so sánh `count >= limit`, nên `0 >= 0` chặn ngay từ lần tạo đầu tiên.
`IsUnlimited` chỉ coi số **âm** là vô hạn, nên 0 không lọt.

| Limit / feature | free (chưa kích hoạt) | pro |
|---|---|---|
| Players / room | 0 | 2000 |
| Questions / quiz | 0 | 100 |
| Concurrent rooms | 0 | 10 |
| Quizzes | 0 | ∞ (-1) |
| Templates | 0 | ∞ (-1) |
| Solo (player-paced) | no | yes |
| Custom branding / logo | no | yes |
| Export detailed logs | no | yes |
| Remove watermark | no | yes |

Cấp quyền có hai đường:

1. **Mã kích hoạt** — host tự nhập, xem phần dưới.
2. **Admin gán tay** — `POST /api/admin/license/assign`
   (`{user_id, plan_id, ends_at_days}`; bỏ `ends_at_days` = vĩnh viễn), hoặc UI
   `frontend/app/admin/license`.

Cả hai đi qua cùng một hàm ghi `setPlanTx`, nên một lần redeem cấp gói và ghi sổ
lượt dùng trong cùng transaction — không có chuyện ăn mất lượt mã mà không được gói.

Hết hạn: `GetEntitlements` tự hạ về `free` khi `ends_at` đã qua — host bị khoá lại.
Lưu ý phòng **đang chạy** không bị cắt giữa chừng; gate chỉ chạy lúc gọi API.

## Gate đang cắm ở đâu

| Hành động | Vị trí |
|---|---|
| `CreateQuiz` — số quiz, số câu, player-paced, branding | `internal/handler/quiz.go:66` |
| `CreateRoom` — phòng đồng thời, số câu, player-paced | `internal/handler/quiz.go:271` |
| `JoinRoom` — số người / phòng | `internal/handler/quiz.go:542` |
| `UpdateQuiz` — số câu | `internal/handler/quiz.go:1427` |
| `CreateTemplate` / instantiate | `internal/handler/template.go:147,416,542` |
| `ExportRoomLogs` | `internal/handler/logs.go:65` |

## Quy trình bật enforcement trên một DB đã có dữ liệu

`seedPricingPlans()` chỉ INSERT khi thiếu row — cố ý, để không ghi đè limit admin đã
chỉnh tay. Nên DB cũ vẫn giữ gói free rộng rãi (20 quiz / 1 phòng / 20 người).
Phải reconcile tay, theo đúng thứ tự:

```bash
psql -U postgres -d quizzzone -v ON_ERROR_STOP=1 -f scripts/license/01_lock_free_plan.sql
psql -U postgres -d quizzzone -v ON_ERROR_STOP=1 -f scripts/license/02_grandfather_hosts.sql
# rồi mới đặt LICENSE_ENFORCEMENT=true và restart
```

Kiểm lại trước khi bật: `subscription_events` phải có dòng cho từng host được
grandfather, và không user đang hoạt động nào còn `plan_id = 'free'`. Nếu
`select count(*) from subscription_events` vẫn là 0 thì bước 02 chưa chạy.

`02_grandfather_hosts.sql` cấp pro vĩnh viễn cho mọi admin và mọi user đã có
quiz/template/room. **Bỏ bước này là tự khoá luôn tài khoản admin** — admin cũng đi
qua `GetEntitlements` (`internal/handler/admin_users.go:82`).

## Mã kích hoạt (tầng 2)

Admin đúc một lô mã, bán offline (chuyển khoản, đại lý, voucher sự kiện), người mua
tự nhập mã. Không cần cổng thanh toán.

Dạng mã: `KOVIO-XXXX-XXXX-XXXX`. Bảng chữ cái bỏ `0 1 I L O U` — những ký tự người ta
nhầm khi đọc qua điện thoại. 30 ký tự x 12 vị trí ≈ 5.3e17 mã.

`NormalizeCode` làm việc so khớp độc lập với cách gõ: hoa/thường, khoảng trắng, dấu gạch
đều quy về một dạng. `kovio 23ab 45cd 67ef` tìm ra `KOVIO-23AB-45CD-67EF`.

| Endpoint | Ai dùng | Làm gì |
|---|---|---|
| `POST /api/license/redeem` | host đã đăng nhập | kích hoạt tài khoản của chính mình |
| `POST /api/admin/license/codes` | admin | đúc lô (tối đa 500 mã/lần) |
| `GET /api/admin/license/codes` | admin | liệt kê, lọc theo `status`/`batch`/`q` |
| `POST /api/admin/license/codes/:code/revoke` | admin | thu hồi, không xóa lịch sử |
| `GET /api/admin/license/redemptions` | admin | ai đã dùng mã nào |

Tham số khi đúc mã:

- `duration_days` — số ngày của gói sau khi kích hoạt. **0 = vĩnh viễn.**
- `max_uses` — một mã dùng được bao nhiêu lượt (1 là bán lẻ, >1 cho lớp học / đại lý).
- `expires_in_days` — hạn dùng của **chính mã**, khác với hạn của gói nó cấp.

### Chống tranh chấp

`RedeemCode` chạy toàn bộ trong một transaction, khóa dòng mã bằng `SELECT ... FOR
UPDATE`. Hai người cùng tiêu lượt cuối thì người sau chời khóa, đọc lại `used_count`
rồi thất bại `ErrCodeExhausted` — không bao giờ vượt `max_uses`.

Unique index `(code, user_id)` trên `license_redemptions` là lớp thứ hai: một user bấm
hai lần chỉ được một lượt, trả `ErrCodeAlreadyUsed`.

Đã verify bằng 10 request đồng thời vào một mã `max_uses = 3`: đúng 3 thành công,
`used_count = 3`, đúng 3 dòng redemption.

### Rate limit

`middleware.RedeemRateLimit` — 8 lần/phút theo **user_id**, kèm chặn IP 60/phút, cả
hai fail closed.

Khóa theo user chứ không theo IP là có chủ đích: một trường hay văn phòng kích hoạt
hàng loạt tài khoản đều đi chung một NAT, khóa theo IP thì họ tự chặn nhau.

## Frontend

| Đường dẫn | Nội dung |
|---|---|
| `components/license-redeem.tsx` | ô nhập mã, tự format khi gõ |
| `components/license-locked-banner.tsx` | banner "Tài khoản chưa kích hoạt" |
| `app/dashboard/page.tsx` | hiện banner khi `isLicenseLocked` |
| `app/profile/settings/page.tsx` | ô nhập mã + hạn mức hiện tại |
| `app/admin/license/page.tsx` | tab "Mã kích hoạt" (đúc lô, copy, gửi email, lọc, thu hồi) và tab "Lịch sử / Đối soát" |
| `lib/license.ts` | `isLicenseLocked` |

`isLicenseLocked` nằm ở `lib/` chứ không ở `app/actions/license.ts`: file đó là module
`'use server'`, mọi export phải là async server action — một hàm đồng bộ ở đó làm
fail build (tsc không bắt, chỉ `next build` bắt).

`isLicenseLocked(null)` trả `false`: `/license/me` lỗi không được phép sơn cả dashboard
thành "chưa kích hoạt".

`GET /api/license/me` trả thêm `enforcement` (bool) để banner admin nói đúng trạng thái
thực thay vì một câu hardcode.

## Cạm bẫy đã gặp

**GORM bỏ field zero-value khi cột có `default`.** `DurationDays` từng khai
`gorm:"not null;default:30"`, nên mã đúc với `duration_days = 0` (vĩnh viễn) bị lưu
thành 30 và chỉ cấp gói 30 ngày. Đã bỏ tag `default` và drop default ở DB
(`scripts/license/03_fix_code_duration_default.sql`). Đừng thêm lại.

Cùng lỗi đó còn sót ở `PricingPlan.MaxQuestionsPerQuiz` (`default:50`) tới 2026-09-14:
`seedPricingPlans()` khai free với `MaxQuestionsPerQuiz: 0`, GORM bỏ field khỏi INSERT,
DB lấy default → free được 50 câu/quiz thay vì 0. Không lộ ra vì `max_quizzes = 0` chặn
trước khi tới gate câu hỏi, nên mọi DB dựng mới đều sai âm thầm. Đã bỏ tag và drop
default (`scripts/license/04_fix_questions_per_quiz_default.sql`).

Quy tắc rút ra: trong `pricing_plans`, **0 là một giá trị có nghĩa** ("không được gì"),
nên không cột limit nào trong bảng này được phép có `default` ở tầng DB.

## Giao mã qua email

`POST /api/admin/license/codes/send` — `{codes: [...], email, name}`. Gửi cả lô trong
một email, rồi ghi `delivered_to` / `delivered_at` lên từng mã.

Đánh dấu **sau** khi SMTP trả về thành công, không phải trước: một mã trông như đã gửi
nhưng thực tế không tới nơi còn tệ hơn là không ghi gì — admin sẽ ngừng tìm một khách
không nhận được gì cả.

Mọi giá trị chèn vào email đều escape (`html.EscapeString`). Tên người mua và ghi chú là
chữ admin gõ, không đủ tin để nhét thẳng vào tài liệu khách mở.

Chưa cấu hình `SMTP_EMAIL`/`SMTP_PASSWORD` thì `email.SendEmail` chạy đường mock (ghi log,
trả nil) — nên mã vẫn bị đánh dấu đã gửi trên máy dev. Trên prod phải cấu hình SMTP thật.

`SendEmail` là SMTP đồng bộ, block request. Một lô = một email nên chỉ một vòng, nhưng
đừng biến nó thành vòng lặp gửi từng người.

## Lịch sử và đối soát

Bảng `subscription_events` — **chỉ ghi thêm, không sửa**. Sửa sai là một dòng mới.

`subscriptions` mỗi user đúng một dòng, bị ghi đè mỗi lần cấp gói — nó trả lời được
"bây giờ user này có gì" và không gì khác. Câu hỏi "tháng 9 đã cấp Pro cho ai, vì sao,
thu bao nhiêu" cần đúng cái dòng bị ghi đè đó.

Dòng lịch sử được ghi **trong cùng transaction** với việc cấp gói (`setPlanTx`). Gói đã cấp
mà không ghi sổ là gói không đối soát được.

| `action` | Khi nào | Tính tiền |
|---|---|---|
| `grant` | cấp gói mới / từ free lên pro | có |
| `renew` | cấp lại đúng gói đang có | có |
| `downgrade` | admin ngắt về free | không |
| `expire` | worker tự hạ khi hết hạn | không |

Thứ tự phân loại trong `classifyAction` có chủ đích: hết hạn cũng rơi về free, nên phải
xét `source` trước hình dạng chuyển đổi — không thì mọi lần hết hạn đều bị ghi thành
admin ngắt, và báo cáo không phân biệt được hai thứ đó.

`amount_vnd` và `external_ref` được **mang theo**, không suy ra từ giá niêm yết của gói.
Giá đổi theo thời gian; suy ra thì một lần đổi giá sẽ viết lại số tiền khách cũ đã trả.

- `GET /api/admin/license/history` — lọc theo `from`/`to`/`email`/`action`/`source`/`external_ref`.
  Trả kèm `summary` (số lượt, số có tiền, tổng VND).
- `GET /api/admin/license/history.csv` — cùng bộ lọc, xuất CSV.

CSV có BOM UTF-8: thiếu BOM thì Excel trên Windows đọc theo codepage hệ thống và vỡ hết
tên tiếng Việt. Mặc định 5000 dòng chứ không 200 như trên màn hình — bản xuất để đối soát
mà bị cắt âm thầm là tệ hơn không xuất.

Tải CSV ở frontend đi qua server action (`apiRequestText`) rồi dựng Blob, không phải
thẻ `<a href>`: token nằm trong cookie httpOnly của origin frontend, request thẳng từ
trình duyệt sang API sẽ không có auth.

## Tự cắt khi hết hạn

`cron.StartLicenseExpiryWorker` — chạy mỗi **5 phút**.

`GetEntitlements` đã hạ gói lười biếng, ở lần gọi API tiếp theo của chủ tài khoản. Nhưng
lười biếng không đủ cho một phòng **đang chạy**: không có gì đọc lại entitlement giữa game,
nên host hết hạn vẫn host tiếp cho tới khi tình cờ gọi một endpoint bị gate — điều mà
giữa trận họ không bao giờ làm.

Worker làm hai việc:

1. `license.SweepExpiredSubscriptions()` — hạ mọi gói trả phí đã qua `ends_at` về free,
   ghi dòng lịch sử `expire`, và trả về danh sách phòng họ còn đang mở.
2. `handler.FinalizeRoom(roomID)` cho từng phòng — **cùng đường** với nút End Game của host:
   lưu bảng xếp hạng, bắn `game:ended` cho player, lên lịch dọn. Viết lại logic đó trong
   cron sẽ làm hai đường trôi xa nhau.

Sweep là no-op khi `LICENSE_ENFORCEMENT=false` — tắt enforcement không bao giờ kết thúc
trận của ai.

Package `license` không tự đóng phòng: archive là việc của package `handler`, kéo vào đây
là đảo ngược phụ thuộc. Nó trả id, cron gọi handler.

## Thanh toán (2026-10-06, Phase 1)

Quyết định "cố ý không làm cổng thanh toán" đã được đảo: QR tự động qua SePay, còn mua trực tiếp
thì app **chỉ hiện link Zalo** — không có đơn hàng, admin cấp gói bằng công cụ có sẵn.

**Cộng dồn hạn (`AssignOptions.Extend`).** Trước đây mọi lần cấp gói đặt `StartsAt = now`,
`EndsAt = now + N`: khách còn 10 ngày mà gia hạn sớm thì mất 10 ngày. `computeTerm` (thuần,
có test `term_test.go`) giờ quyết định ngày:

- `Extend` + cùng gói + chưa hết hạn → `EndsAt = EndsAt cũ + N`, giữ `StartsAt` cũ.
- `Extend` + cùng gói đang **vĩnh viễn** → vẫn vĩnh viễn. Mua 30 ngày không bao giờ được rút
  ngắn một gói vĩnh viễn.
- Hết hạn rồi (kể cả chưa bị cron hạ), khác gói, hoặc không `Extend` → `now + N` như cũ.

`Extend` mặc định `false`: API cũ giữ nguyên hành vi. `POST /api/admin/license/assign` nhận
`"extend": true`; UI tab Subscriptions tự tick khi user đang có Pro còn hạn và hiện "hạn mới"
trước khi bấm. **Chưa** bật cho `RedeemCode` — chờ quyết định.

**Có tiền thì phải có mã tham chiếu.** `assign` trả 400 khi `amount_vnd > 0` mà
`external_ref` rỗng, và từ chối `amount_vnd < 0`.

**Bảng `payment_products`** — SKU bán theo thời hạn (`pro_1m`, `pro_3m`, `pro_12m`). Seed
**tắt** (`is_active = false`), giá seed chỉ là nháp = 199.000đ × số tháng; admin phải duyệt giá
rồi bật. Không cột nào có `default` (bẫy GORM ở trên).

| Endpoint | Ai | Việc |
|---|---|---|
| `GET /api/payments/products` | public | SKU đang bán + `payment_enabled` + `contact.zalo_url` |
| `GET /api/admin/payments/products` | admin | mọi SKU |
| `PUT /api/admin/payments/products/:id` | admin | sửa tên / thời hạn (1–3660 ngày) / giá (≥ 1.000đ) / bật tắt; audit `admin_payment_product_update` |

`payment.enabled` (system_settings, mặc định tắt) chỉ chặn checkout QR. Link Zalo không phụ
thuộc cờ này.

`PAYMENT_ZALO_URL` là **env, không phải setting admin sửa được**: một phiên admin bị chiếm mà
đổi được link thì khách thật nhắn và chuyển tiền cho kẻ gian. Phải là URL `https://`, sai thì
bị bỏ qua lúc khởi động (log WARNING) và nút ẩn đi.

Source mới trong `subscription_events`: `payment_sepay` (webhook tự khớp),
`payment_manual` (admin xác nhận tay một đơn QR).

### Đơn QR (Phase 2, package `internal/pkg/payment`)

Bảng `payment_orders`: snapshot gói/thời hạn/giá lúc tạo, mã `KV` + 8 ký tự (bảng chữ cái của mã
license, không dấu gạch — ngân hàng xoá dấu). Mỗi user tối đa **một** đơn `pending` (partial
unique index `uq_payment_orders_one_pending`, tạo lúc khởi động); tạo đơn mới huỷ đơn cũ trong
cùng transaction. Hết hạn sau 30 phút (`OrderTTL`); `GetUserOrder` báo `expired` ngay, cron 5 phút
ghi xuống bảng.

`MarkPaidTx` là chỗ **duy nhất** biến đơn thành gói: gọi `license.GrantInTx` với `Extend: true`
trong cùng transaction, `SourceRef` = mã đơn, `AmountVND` = tiền thật về.

Checkout chỉ mở khi **cả hai**: `payment.enabled = true` và server có `PAYMENT_BANK_NAME`,
`PAYMENT_BANK_ACCOUNT`, `SEPAY_API_KEY` (`CheckoutConfigured`). API bật công tắc từ chối khi
thiếu cấu hình. `SEPAY_API_KEY` trên prod phải ≥ 24 ký tự.

| Endpoint | Ai | Việc |
|---|---|---|
| `POST /api/payments/orders` | đăng nhập, 5/phút/user | `{product_id}` → đơn + thông tin CK + `qr_url`; 409 khi đóng / đã có gói vĩnh viễn |
| `GET /api/payments/orders[/:code]` | chủ đơn | đơn của người khác trả 404 |
| `POST /api/payments/orders/:code/cancel` | chủ đơn | chỉ khi `pending` |
| `GET /api/admin/payments/orders` | admin | `status`/`q`/`from`/`to`, `needs_review` lên đầu |
| `POST /api/admin/payments/orders/:code/confirm` | admin | bắt buộc `amount_vnd`, `external_ref`, `note`; mọi trạng thái trừ `paid` |
| `POST /api/admin/payments/orders/:code/cancel` | admin | `pending`/`needs_review`/`expired` |
| `GET/PUT /api/admin/payments/checkout` | admin | công tắc `payment.enabled` |

Ảnh QR: `PAYMENT_QR_IMAGE_BASE` (mặc định `https://vietqr.app/img`, dịch vụ VietQR của SePay)
với `acc`, `bank`, `amount`, `des` = mã đơn. Trình duyệt người mua tải ảnh trực tiếp từ đó.

Test DB thật: `order_db_test.go`, chạy khi có `PAYMENT_TEST_DSN` (xem đầu file) — gồm 8 admin
confirm đồng thời (đúng 1 lần cấp) và 6 checkout đồng thời (đúng 1 đơn pending).

Không có permission `payment:confirm` riêng: `RequirePermission` cho admin qua hết, nên route
chỉ cần `RequireRole("admin")`.

### Webhook SePay (Phase 3)

`POST /api/payments/webhook/sepay` — không qua auth user; SePay gửi
`Authorization: Apikey <SEPAY_API_KEY>`, so sánh constant-time. Không cấu hình key = mọi request
401. `SEPAY_IP_ALLOWLIST` (danh sách IP SePay công bố, cách nhau dấu phẩy) chặn thêm theo
`ClientIP()`; để trống thì bỏ kiểm IP. Rate limit 120/phút/IP, **fail-open** khi Redis lỗi:
từ chối webhook chỉ khiến SePay retry, nhưng quá 5 giờ retry là mất một khoản tiền thật.

Hợp đồng phản hồi của SePay: 200 + `{"success": true}` trong 30 s, nếu không nó retry tối đa 7
lần / 5 giờ. Nên mọi kết quả nghiệp vụ (không có mã, sai tiền, tiền ra, trùng id) đều trả 200;
chỉ lỗi hạ tầng (DB) trả 500 để SePay gửi lại.

Bảng `bank_transactions` ghi **mọi** giao dịch SePay báo, khớp hay không, kèm payload gốc.
Unique `(provider, provider_txn_id)` + `ON CONFLICT DO NOTHING` → webhook gửi lại là no-op.
Ghi giao dịch, tìm đơn (`FOR UPDATE`), cấp gói nằm trong **một** transaction.

Tìm mã đơn (`OrderCodeCandidates`): trường `code` SePay tự nhận diện → mã đứng nguyên trong
nội dung → mã sau khi bỏ khoảng trắng/gạch/chấm (khách gõ `KV7K9 Q2MXA`). Bước cuối có thể
ghép nhầm chữ của tên người thành mã giả, nên chỉ nhận ứng viên **có thật** trong DB, theo thứ tự.

Quy tắc (`settle`, dùng chung cho webhook và admin gán tay):

| Đơn | Số tiền | Kết quả |
|---|---|---|
| `pending` hoặc `expired` | đúng | **cấp gói** (`expired` ghi chú "paid after QR expiry") |
| bất kỳ chưa trả | sai | đơn → `needs_review` (`amount_mismatch`), không cấp |
| `cancelled` | bất kỳ | đơn → `needs_review` (`late_payment`) |
| `needs_review` | bất kỳ | giữ review, cộng dồn `paid_amount_vnd` (khách chuyển bù) |
| `paid` | bất kỳ | giao dịch `duplicate_payment`, đơn giữ nguyên (thường là hoàn tiền) |
| không tìm thấy | — | `unmatched` |
| tiền ra / STK khác | — | `ignored` |

Đơn `expired` trả đúng tiền vẫn cấp gói (khác plan ban đầu là đưa vào review): QR hết hạn mà
khách vẫn trả đủ là ý định mua rõ ràng, và hạn cộng dồn nên kể cả khách trả thêm một đơn mới
cũng không mất gì.

Cần người xử lý (`unmatched`, `duplicate_payment`, `amount_mismatch`, `needs_review`) → Telegram
P1. Lỗi ghi DB → P0. Đơn được trả → email biên nhận (goroutine sau commit; lỗi SMTP chỉ log).

Admin:

| Endpoint | Việc |
|---|---|
| `GET /api/admin/payments/bank-transactions?status=open\|<match_status>&q=` | `open` = `unmatched` + `duplicate_payment` |
| `POST .../bank-transactions/:id/attach` `{order_code, note}` | chỉ từ `unmatched`; chạy `settle` với `payment_manual` + actor |
| `POST .../bank-transactions/:id/dismiss` `{note}` | `unmatched`/`duplicate_payment` → `dismissed` (không phải thanh toán app / đã hoàn) |

Sai tiền / trả trễ không nằm trong hàng chờ giao dịch mà nằm trên **đơn** (`needs_review`),
xử lý bằng `orders/:code/confirm` hoặc `cancel`.

## Chưa có (tầng 3+)

- Cron đối soát kéo lịch sử giao dịch từ API SePay để vớt webhook mất quá 5 giờ retry.
- Export CSV đơn thanh toán.
- Không tự khớp sao kê ngân hàng. `summary` là số hệ thống ghi nhận, phải đối chiếu tay.
- Không xuất hoá đơn / VAT.
- Không nhắc hạn trước khi gói sắp hết — host bị cắt mà không được báo trước.
- Phòng bị cắt giữa chừng không báo lý do cho player, chỉ nhận `game:ended` như bình thường.
