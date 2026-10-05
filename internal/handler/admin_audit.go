package handler

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/audit"
	"github.com/quizzzone/backend/internal/pkg/license"
	"gorm.io/gorm"
)

// The admin audit trail: who did what, from where, and one account's activity
// at a glance. Admin-only by route (RequireRole("admin")). Reading it is not
// itself recorded (decision 2026-10-05); exporting it is.

const (
	auditPageDefault = 50
	auditPageMax     = 200
	auditCSVMax      = 50_000
)

type auditRow struct {
	ID        uint            `json:"id"`
	UserID    uint            `json:"user_id"`
	UserEmail string          `json:"user_email"`
	Action    string          `json:"action"`
	Resource  string          `json:"resource"`
	IPAddress string          `json:"ip_address"`
	UserAgent string          `json:"user_agent"`
	Status    string          `json:"status"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// auditFilter applies the query-string filters shared by the list and the CSV.
// It returns the filters as given, for the export's own audit row.
func auditFilter(c *gin.Context, tx *gorm.DB) (*gorm.DB, map[string]string, error) {
	used := map[string]string{}
	if v := c.Query("user_id"); v != "" {
		id, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid user_id")
		}
		tx = tx.Where("audit_logs.user_id = ?", uint(id))
		used["user_id"] = v
	}
	if v := strings.TrimSpace(c.Query("action")); v != "" {
		tx = tx.Where("audit_logs.action = ?", v)
		used["action"] = v
	}
	if v := c.Query("status"); v != "" {
		if v != "success" && v != "failed" {
			return nil, nil, fmt.Errorf("invalid status")
		}
		tx = tx.Where("audit_logs.status = ?", v)
		used["status"] = v
	}
	if v := strings.TrimSpace(c.Query("ip")); v != "" {
		tx = tx.Where("audit_logs.ip_address = ?", v)
		used["ip"] = v
	}
	// Free text over the resource and the account email: the only way to find
	// a refused login for an address that has no account (user_id 0).
	if v := strings.TrimSpace(c.Query("q")); v != "" {
		like := "%" + v + "%"
		tx = tx.Where("audit_logs.resource ILIKE ? OR users.email ILIKE ?", like, like)
		used["q"] = v
	}
	for _, k := range []string{"from", "to"} {
		v := c.Query(k)
		if v == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid %s (RFC3339 expected)", k)
		}
		if k == "from" {
			tx = tx.Where("audit_logs.created_at >= ?", t)
		} else {
			tx = tx.Where("audit_logs.created_at < ?", t)
		}
		used[k] = v
	}
	return tx, used, nil
}

func auditBase() *gorm.DB {
	return db.DB.Table("audit_logs").
		Select("audit_logs.id, audit_logs.user_id, COALESCE(users.email, '') AS user_email, audit_logs.action, " +
			"audit_logs.resource, audit_logs.ip_address, audit_logs.user_agent, audit_logs.status, " +
			"audit_logs.metadata, audit_logs.created_at").
		Joins("LEFT JOIN users ON users.id = audit_logs.user_id")
}

type auditScan struct {
	ID        uint
	UserID    uint
	UserEmail string
	Action    string
	Resource  string
	IPAddress string
	UserAgent string
	Status    string
	Metadata  *string
	CreatedAt time.Time
}

func (s auditScan) row() auditRow {
	r := auditRow{ID: s.ID, UserID: s.UserID, UserEmail: s.UserEmail, Action: s.Action, Resource: s.Resource,
		IPAddress: s.IPAddress, UserAgent: s.UserAgent, Status: s.Status, CreatedAt: s.CreatedAt}
	if s.Metadata != nil && *s.Metadata != "" {
		r.Metadata = json.RawMessage(*s.Metadata)
	}
	return r
}

// AdminListAudit — GET /admin/audit
// Filters: user_id, action, status, ip, q, from, to. Newest first, keyset
// paginated: pass the previous page's next_cursor as `cursor`.
func AdminListAudit(c *gin.Context) {
	limit := auditPageDefault
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 {
		limit = min(v, auditPageMax)
	}
	tx, _, err := auditFilter(c, auditBase())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if cur := c.Query("cursor"); cur != "" {
		ts, id, ok := parseAuditCursor(cur)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid cursor"})
			return
		}
		tx = tx.Where("(audit_logs.created_at, audit_logs.id) < (?, ?)", ts, id)
	}
	var rows []auditScan
	if err := tx.Order("audit_logs.created_at DESC, audit_logs.id DESC").Limit(limit + 1).Scan(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load audit log"})
		return
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		next = fmt.Sprintf("%d_%d", last.CreatedAt.UnixMicro(), last.ID)
	}
	out := make([]auditRow, len(rows))
	for i, r := range rows {
		out[i] = r.row()
	}
	c.JSON(http.StatusOK, gin.H{"items": out, "next_cursor": next})
}

// The cursor carries microseconds, the precision Postgres stores, so the row
// that ended a page compares equal to itself and is not repeated or skipped.
func parseAuditCursor(s string) (time.Time, uint, bool) {
	a, b, ok := strings.Cut(s, "_")
	if !ok {
		return time.Time{}, 0, false
	}
	us, err1 := strconv.ParseInt(a, 10, 64)
	id, err2 := strconv.ParseUint(b, 10, 32)
	if err1 != nil || err2 != nil {
		return time.Time{}, 0, false
	}
	return time.UnixMicro(us), uint(id), true
}

// AdminListAuditActions — GET /admin/audit/actions: the action names present,
// for the filter dropdown.
func AdminListAuditActions(c *gin.Context) {
	var actions []string
	if err := db.DB.Model(&model.AuditLog{}).Distinct("action").Order("action").Pluck("action", &actions).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load actions"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"actions": actions})
}

// AdminExportAuditCSV — GET /admin/audit.csv, same filters as the list,
// capped at auditCSVMax rows. The export itself is recorded.
func AdminExportAuditCSV(c *gin.Context) {
	tx, used, err := auditFilter(c, auditBase())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var rows []auditScan
	if err := tx.Order("audit_logs.created_at DESC, audit_logs.id DESC").Limit(auditCSVMax).Scan(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to export audit log"})
		return
	}

	adminID, _ := c.Get("user_id")
	filters, _ := json.Marshal(used)
	audit.Log(c, adminID.(uint), "admin_export_audit", "audit_logs", map[string]any{
		"rows": len(rows), "format": "csv", "filters": string(filters),
	})

	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="audit-%s.csv"`, time.Now().Format("20060102-150405")))
	// BOM so Excel opens Vietnamese text as UTF-8 instead of mojibake.
	_, _ = c.Writer.Write([]byte("\xEF\xBB\xBF"))
	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{"time_utc", "user_id", "email", "action", "status", "resource", "ip", "user_agent", "metadata"})
	for _, r := range rows {
		meta := ""
		if r.Metadata != nil {
			meta = *r.Metadata
		}
		_ = w.Write([]string{
			r.CreatedAt.UTC().Format(time.RFC3339), strconv.FormatUint(uint64(r.UserID), 10), csvSafe(r.UserEmail),
			r.Action, r.Status, csvSafe(r.Resource), r.IPAddress, csvSafe(r.UserAgent), csvSafe(meta),
		})
	}
	w.Flush()
}

// csvSafe defuses spreadsheet formula injection: an email or quiz title that
// starts with = + - @ would otherwise run as a formula when the CSV is opened.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// AdminUserActivity — GET /admin/users/:id/activity: one account at a glance.
func AdminUserActivity(c *gin.Context) {
	uid64, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}
	uid := uint(uid64)

	var user model.User
	if err := db.DB.Preload("Role").First(&user, uid).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}
	role := ""
	if user.Role != nil {
		role = user.Role.Name
	}
	plan := license.PlanFree
	if sub := subscriptionsByUser([]model.User{user})[uid]; sub != nil {
		plan = sub.PlanID
	}

	since30 := time.Now().AddDate(0, 0, -30)
	var stats struct {
		Quizzes      int64 `json:"quizzes"`
		Rooms        int64 `json:"rooms"`
		Rooms30d     int64 `json:"rooms_30d"`
		Players      int64 `json:"players"`
		FailedLogins int64 `json:"failed_logins_30d"`
	}
	db.DB.Model(&model.Quiz{}).Where("host_id = ?", uid).Count(&stats.Quizzes)
	db.DB.Model(&model.Room{}).Where("host_id = ?", uid).Count(&stats.Rooms)
	db.DB.Model(&model.Room{}).Where("host_id = ? AND created_at >= ?", uid, since30).Count(&stats.Rooms30d)
	db.DB.Model(&model.GameSession{}).Where("host_id = ?", uid).Select("COALESCE(SUM(player_count), 0)").Scan(&stats.Players)
	db.DB.Model(&model.AuditLog{}).Where("user_id = ? AND status = 'failed' AND created_at >= ?", uid, since30).Count(&stats.FailedLogins)

	type loginIP struct {
		IP     string    `json:"ip"`
		Count  int64     `json:"count"`
		LastAt time.Time `json:"last_at"`
	}
	var ips []loginIP
	db.DB.Model(&model.AuditLog{}).
		Select("ip_address AS ip, COUNT(*) AS count, MAX(created_at) AS last_at").
		Where("user_id = ? AND action = 'login_success' AND created_at >= ?", uid, time.Now().AddDate(0, 0, -90)).
		Group("ip_address").Order("last_at DESC").Limit(10).Scan(&ips)

	type roomRow struct {
		ID          uint       `json:"id"`
		PinCode     string     `json:"pin_code"`
		QuizID      uint       `json:"quiz_id"`
		QuizTitle   string     `json:"quiz_title"`
		Status      string     `json:"status"`
		EndedReason string     `json:"ended_reason"`
		CreatedAt   time.Time  `json:"created_at"`
		EndedAt     *time.Time `json:"ended_at"`
		PlayerCount *int       `json:"player_count"`
	}
	var rooms []roomRow
	db.DB.Table("rooms").
		Select("rooms.id, rooms.pin_code, rooms.quiz_id, COALESCE(quizzes.title, '') AS quiz_title, rooms.status, "+
			"COALESCE(rooms.ended_reason, '') AS ended_reason, rooms.created_at, game_sessions.ended_at, game_sessions.player_count").
		Joins("LEFT JOIN quizzes ON quizzes.id = rooms.quiz_id").
		Joins("LEFT JOIN game_sessions ON game_sessions.room_id = rooms.id").
		Where("rooms.host_id = ? AND rooms.deleted_at IS NULL", uid).
		Order("rooms.id DESC").Limit(100).Scan(&rooms)

	c.JSON(http.StatusOK, gin.H{
		"user": gin.H{
			"id": user.ID, "email": user.Email, "nickname": user.Nickname, "role": role,
			"is_active": user.IsActive, "created_at": user.CreatedAt, "plan_id": plan,
			"last_login_at": user.LastLoginAt, "last_login_ip": user.LastLoginIP,
		},
		"stats":     stats,
		"login_ips": ips,
		"rooms":     rooms,
	})
}

// rooms30dByHost counts each listed account's rooms in the last 30 days in one
// query, for the users table.
func rooms30dByHost(users []model.User) map[uint]int64 {
	out := map[uint]int64{}
	if len(users) == 0 {
		return out
	}
	ids := make([]uint, len(users))
	for i, u := range users {
		ids[i] = u.ID
	}
	type n struct {
		HostID uint
		N      int64
	}
	var rows []n
	db.DB.Model(&model.Room{}).Select("host_id, COUNT(*) AS n").
		Where("host_id IN ? AND created_at >= ?", ids, time.Now().AddDate(0, 0, -30)).
		Group("host_id").Scan(&rows)
	for _, r := range rows {
		out[r.HostID] = r.N
	}
	return out
}
