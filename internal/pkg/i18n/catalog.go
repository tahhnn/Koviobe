// Package i18n translates user-facing API messages.
//
// The catalog is keyed by the English string the handlers already emit, rather
// than by a symbolic code. That is deliberate: there are 126 distinct messages
// spread over every handler, and rewriting all of them into code constants
// would be a large, risky diff through code paths that have no test coverage.
// Keying on the emitted text keeps the handlers untouched and puts the whole
// translation surface in one reviewable file.
//
// Consequence to be aware of: changing a message string in a handler silently
// drops its translation. Keep this file in step, or the text falls back to
// English rather than breaking.
package i18n

// vi maps the English message a handler emits to its Vietnamese equivalent.
//
// Sentinel values the frontend branches on — NO_MORE_QUESTIONS, ROOM_FINISHED —
// are intentionally absent: they are protocol, not prose, and translating them
// would break the client.
var vi = map[string]string{
	// Auth / session
	"Account is deactivated":                      "Tài khoản đã bị vô hiệu hoá",
	"Email is already registered":                 "Email này đã được đăng ký",
	"Invalid email or password":                   "Email hoặc mật khẩu không đúng",
	"Incorrect current password":                  "Mật khẩu hiện tại không đúng",
	"Invalid or expired token":                    "Phiên đăng nhập không hợp lệ hoặc đã hết hạn",
	"Invalid or expired refresh token":            "Phiên làm mới không hợp lệ hoặc đã hết hạn",
	"Invalid or expired player token":             "Phiên người chơi không hợp lệ hoặc đã hết hạn",
	"Invalid Authorization header":                "Header Authorization không hợp lệ",
	"Authorization header is required":            "Thiếu header Authorization",
	"Authorization header must be Bearer token":   "Header Authorization phải có dạng Bearer token",
	"Forbidden: insufficient role permissions":    "Bạn không đủ quyền để thực hiện thao tác này",
	"Forbidden: missing permission":               "Bạn thiếu quyền cần thiết cho thao tác này",
	"Role not found in context":                   "Không đọc được vai trò từ phiên đăng nhập",
	"Permissions not found in context":            "Không đọc được phân quyền từ phiên đăng nhập",
	"Invalid permissions format in context":       "Dữ liệu phân quyền không hợp lệ",
	"Unauthorized":                                "Bạn không có quyền thực hiện thao tác này",
	"User not found":                              "Không tìm thấy người dùng",
	"Role not found":                              "Không tìm thấy vai trò",
	"Verification code has expired or is invalid": "Mã xác thực đã hết hạn hoặc không hợp lệ",
	"Too many failed verification attempts. Your verification code has been invalidated. Please register again.": "Nhập sai mã xác thực quá nhiều lần. Mã đã bị huỷ, vui lòng đăng ký lại.",
	"Authentication required: provide Authorization or X-Player-Token header":                                    "Cần đăng nhập: thiếu header Authorization hoặc X-Player-Token",
	"X-Player-Token header is required":                              "Thiếu header X-Player-Token",
	"X-Player-Token header is required for anonymous realtime token": "Cần header X-Player-Token để lấy token realtime ẩn danh",

	// Room / gameplay — the messages players actually hit
	"Room not found":                                        "Không tìm thấy phòng",
	"Room not found or invalid PIN":                         "Không tìm thấy phòng hoặc mã PIN không đúng",
	"Room not found or access denied":                       "Không tìm thấy phòng hoặc bạn không có quyền truy cập",
	"Room session not found":                                "Không tìm thấy phiên chơi",
	"Active room not found":                                 "Không tìm thấy phòng đang hoạt động",
	"Room is not active":                                    "Phòng chưa bắt đầu",
	"Failed to start game":                                  "Không bắt đầu được trận đấu",
	"Question has no explanation slide":                     "Câu hỏi này không có slide giải thích",
	"Room can only be started from waiting status":          "Chỉ có thể bắt đầu phòng đang ở trạng thái chờ",
	"Privacy can only be changed while the room is waiting": "Chỉ đổi được chế độ riêng tư khi phòng đang chờ",
	"You do not own this room":                              "Bạn không phải chủ phòng này",
	"Token room mismatch":                                   "Token không khớp với phòng này",
	"Nickname is already taken in this room":                "Biệt danh này đã có người dùng trong phòng",
	"Joining is closed, the game already started":           "Phòng đã bắt đầu, không thể vào thêm",
	"Player not found":                                      "Không tìm thấy người chơi",
	"Player session is no longer active":                    "Phiên người chơi đã kết thúc",
	"You have already answered this question":               "Bạn đã trả lời câu hỏi này rồi",
	"Time limit exceeded for this question":                 "Đã hết thời gian trả lời câu hỏi này",
	"Question is not currently active":                      "Câu hỏi này hiện không mở",
	"Question does not belong to this quiz room":            "Câu hỏi không thuộc phòng chơi này",
	"No active question":                                    "Không có câu hỏi nào đang mở",
	"No more questions. Use EndGame to close room.":         "Đã hết câu hỏi. Kết thúc trò chơi để đóng phòng.",
	"Host question controls are not available in solo mode": "Chế độ tự chơi không dùng điều khiển câu hỏi của người tổ chức",
	"Invalid PIN format":                                    "Mã PIN không đúng định dạng",
	"Invalid room ID":                                       "ID phòng không hợp lệ",
	"Invalid room_id":                                       "room_id không hợp lệ",
	"Invalid question index":                                "Vị trí câu hỏi không hợp lệ",

	// Quiz / template
	"Quiz not found":                                      "Không tìm thấy quiz",
	"This shared quiz is read-only — duplicate it to make changes": "Quiz này chỉ cho xem — hãy nhân bản rồi sửa trên bản của bạn",
	"Failed to prepare room settings":                     "Không chuẩn bị được cấu hình phòng",
	"Quiz is being played right now":                      "Quiz đang có phòng chơi, không sửa được lúc này",
	"Quiz was changed by someone else":                    "Quiz vừa được người khác sửa, hãy tải lại rồi lưu lại",
	"Failed to update quiz sharing":                       "Không cập nhật được chế độ chia sẻ",
	"Failed to fetch shared quizzes":                      "Không tải được danh sách quiz chia sẻ",
	"Failed to check running rooms":                       "Không kiểm tra được phòng đang chạy",
	"Quiz has no questions":                               "Quiz chưa có câu hỏi nào",
	"Quiz has no questions to save":                       "Quiz không có câu hỏi nào để lưu",
	"Question not found":                                  "Không tìm thấy câu hỏi",
	"Template not found":                                  "Không tìm thấy mẫu",
	"Question bank pack is empty or invalid":              "Bộ câu hỏi rỗng hoặc không hợp lệ",
	"Question bank pack must include at least 1 question": "Bộ câu hỏi phải có ít nhất 1 câu",
	"No valid questions selected":                         "Chưa chọn câu hỏi hợp lệ nào",
	"No fields to update":                                 "Không có trường nào để cập nhật",
	"Invalid quiz ID":                                     "ID quiz không hợp lệ",
	"Invalid quiz_id":                                     "quiz_id không hợp lệ",
	"Invalid template ID":                                 "ID mẫu không hợp lệ",
	"Invalid user ID":                                     "ID người dùng không hợp lệ",
	"quiz_id query param is required":                     "Thiếu tham số quiz_id",
	"room_id query parameter is required":                 "Thiếu tham số room_id",
	"title cannot be empty":                               "Tiêu đề không được để trống",
	"is_active is required":                               "Thiếu trường is_active",
	"role must be user or admin":                          "Vai trò phải là user hoặc admin",

	// License / plan gates
	"Player-paced mode requires Pro plan":                      "Chế độ tự chơi cần gói Pro",
	"This quiz uses player-paced mode which requires Pro plan": "Quiz này dùng chế độ tự chơi, cần gói Pro",
	"Custom branding (logo) requires Pro plan":                 "Tuỳ chỉnh thương hiệu (logo) cần gói Pro",
	"Removing watermark requires Pro plan":                     "Gỡ watermark cần gói Pro",
	"Exporting detailed logs requires Pro plan":                "Xuất log chi tiết cần gói Pro",
	"Failed to resolve license":                                "Không đọc được thông tin gói dịch vụ",
	"Failed to resolve entitlements":                           "Không đọc được hạn mức của gói",
	"Activation code is required":                              "Thiếu mã kích hoạt",
	"Could not activate the code, please try again later":      "Không kích hoạt được, thử lại sau",
	"Specify ends_at_days or set lifetime":                     "Cần chỉ rõ ends_at_days hoặc lifetime",
	"License enforcement readiness check failed":               "Chưa đủ điều kiện để bật enforcement",
	"Enforcement is pinned by LICENSE_ENFORCEMENT env":         "Enforcement đang bị ghim bởi biến môi trường LICENSE_ENFORCEMENT",
	"Failed to save enforcement setting":                       "Không lưu được cài đặt enforcement",
	"Failed to claw back the code":                             "Không thu hồi được mã",
	"License code not found":                                   "Không tìm thấy mã kích hoạt",

	// Admin guards
	"Cannot demote the last admin":                      "Không thể hạ quyền quản trị viên cuối cùng",
	"Cannot demote the seeded system admin account":     "Không thể hạ quyền tài khoản quản trị hệ thống",
	"Cannot deactivate the seeded system admin account": "Không thể vô hiệu hoá tài khoản quản trị hệ thống",

	// Infrastructure failures — the user cannot act on the cause, only retry
	"Service temporarily unavailable. Please try again.":  "Hệ thống đang quá tải, vui lòng thử lại.",
	"Too many requests. Please wait before trying again.": "Quá nhiều yêu cầu, vui lòng đợi một lát rồi thử lại.",
	"Too many answer submissions. Please slow down.":      "Bạn gửi câu trả lời quá nhanh, vui lòng chậm lại.",
	"Could not allocate a free room PIN. Please retry.":   "Chưa cấp được mã PIN trống. Vui lòng thử lại.",
	"Failed to allocate room PIN":                         "Không cấp được mã PIN cho phòng",
	"Failed to generate room PIN":                         "Không tạo được mã PIN cho phòng",
	"Failed to create room":                               "Không tạo được phòng",
	"Failed to join room":                                 "Không vào được phòng",
	"Failed to remove player":                             "Không xoá được người chơi",
	"Failed to submit answer":                             "Không gửi được câu trả lời",
	"Failed to end game":                                  "Không kết thúc được trò chơi",
	"Failed to update privacy":                            "Không cập nhật được chế độ riêng tư",
	"Failed to create quiz":                               "Không tạo được quiz",
	"Failed to update quiz":                               "Không cập nhật được quiz",
	"Failed to delete quiz":                               "Không xoá được quiz",
	"Failed to commit quiz":                               "Không lưu được quiz",
	"Failed to fetch quizzes":                             "Không tải được danh sách quiz",
	"Failed to create question":                           "Không tạo được câu hỏi",
	"Failed to create questions":                          "Không tạo được các câu hỏi",
	"Failed to update question":                           "Không cập nhật được câu hỏi",
	"Failed to delete old question":                       "Không xoá được câu hỏi cũ",
	"Failed to load questions":                            "Không tải được câu hỏi",
	"Failed to fetch existing questions":                  "Không tải được câu hỏi hiện có",
	"Failed to serialize questions":                       "Không xử lý được dữ liệu câu hỏi",
	"Failed to import questions":                          "Không nhập được câu hỏi",
	"Failed to commit import":                             "Không lưu được dữ liệu nhập",
	"Failed to create template":                           "Không tạo được mẫu",
	"Failed to update template":                           "Không cập nhật được mẫu",
	"Failed to delete template":                           "Không xoá được mẫu",
	"Failed to fetch templates":                           "Không tải được danh sách mẫu",
	"Failed to create question bank pack":                 "Không tạo được bộ câu hỏi",
	"Failed to create user":                               "Không tạo được tài khoản",
	"Failed to update role":                               "Không cập nhật được vai trò",
	"Failed to update user status":                        "Không cập nhật được trạng thái tài khoản",
	"Failed to update password":                           "Không đổi được mật khẩu",
	"Failed to hash new password":                         "Không xử lý được mật khẩu mới",
	"Failed to process password":                          "Không xử lý được mật khẩu",
	"Failed to process data":                              "Không xử lý được dữ liệu",
	"Failed to process verification data":                 "Không xử lý được dữ liệu xác thực",
	"Failed to save verification code":                    "Không lưu được mã xác thực",
	"Failed to generate OTP":                              "Không tạo được mã OTP",
	"Failed to generate token":                            "Không tạo được token",
	"Failed to generate access token":                     "Không tạo được access token",
	"Failed to generate refresh token":                    "Không tạo được refresh token",
	"Failed to generate player token":                     "Không tạo được token người chơi",
	"Failed to generate realtime token":                   "Không tạo được token realtime",
	"Failed to generate connection token":                 "Không tạo được token kết nối",
	"Failed to build user session":                        "Không dựng được phiên đăng nhập",
	"Failed to locate default user role":                  "Không tìm thấy vai trò mặc định",
	"Failed to list rooms":                                "Không tải được danh sách phòng",
	"Failed to list users":                                "Không tải được danh sách người dùng",
	"Failed to list codes":                                "Không tải được danh sách mã",
	"Failed to list redemptions":                          "Không tải được lịch sử kích hoạt",
	"Failed to load history":                              "Không tải được lịch sử",
	"Failed to load plans":                                "Không tải được danh sách gói",
	"Failed to fetch logs history":                        "Không tải được lịch sử log",
	"Failed to summarize revenue":                         "Không tổng hợp được doanh thu",
}

// Translate returns the message in the requested locale, or the original when
// there is nothing better to offer. English is the source language, so "en" is
// always a pass-through.
func Translate(locale, message string) string {
	if locale != "vi" {
		return message
	}
	if out, ok := vi[message]; ok {
		return out
	}
	return message
}
