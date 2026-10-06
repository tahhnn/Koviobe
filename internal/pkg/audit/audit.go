package audit

import (
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/notify"
)

// allowedMetaKeys is the whole vocabulary of audit metadata. Anything else is
// dropped before the row is written: audit rows are read by every admin and
// exported as CSV, so a caller passing a request body or a token through by
// accident must not be able to put it there.
var allowedMetaKeys = map[string]bool{
	"room_id": true, "pin": true, "quiz_id": true, "quiz_title": true,
	"player_count": true, "question_count": true, "reason": true,
	"email": true, "target_user_id": true, "role": true, "plan_id": true,
	"is_active": true, "count": true, "rows": true, "format": true,
	"filters":    true,
	"product_id": true, "amount_vnd": true, "duration_days": true,
}

// Entry is one audit row before it is written.
type Entry struct {
	UserID    uint
	Action    string
	Resource  string
	IP        string
	UserAgent string
	// Failed marks a refused attempt. Actions named *_failed* are failures
	// whether or not this is set, which is what keeps Record's callers right.
	Failed bool
	Meta   map[string]any
}

// Write stores an entry. It never fails the request: a lost audit row is
// reported (P1) rather than turned into an error for the user.
func Write(e Entry) {
	status := "success"
	if e.Failed || strings.Contains(e.Action, "failed") {
		status = "failed"
	}
	row := model.AuditLog{
		UserID:    e.UserID,
		Action:    e.Action,
		Resource:  truncate(e.Resource, 255),
		IPAddress: truncate(e.IP, 45),
		UserAgent: truncate(e.UserAgent, 255),
		Status:    status,
		Metadata:  encodeMeta(e.Action, e.Meta),
		CreatedAt: time.Now(),
	}
	if err := db.DB.Create(&row).Error; err != nil {
		log.Printf("[AUDIT ERROR] Failed to record audit log: %v", err)
		notify.P1("audit_write_failed", "Không ghi được audit log (user=%d action=%s resource=%s ip=%s): %v — dấu vết bảo mật đang mất.",
			e.UserID, e.Action, e.Resource, e.IP, err)
	}
}

// Record writes a security audit log to the database. Kept with its original
// signature for the existing call sites; new code should prefer Log.
func Record(userID uint, action string, resource string, ipAddress string) {
	Write(Entry{UserID: userID, Action: action, Resource: resource, IP: ipAddress})
}

// Log records an action taken in this request, taking the client address and
// user agent from it.
func Log(c *gin.Context, userID uint, action, resource string, meta map[string]any) {
	Write(Entry{
		UserID:    userID,
		Action:    action,
		Resource:  resource,
		IP:        c.ClientIP(),
		UserAgent: c.Request.UserAgent(),
		Meta:      meta,
	})
}

func encodeMeta(action string, meta map[string]any) *string {
	if len(meta) == 0 {
		return nil
	}
	clean := make(map[string]any, len(meta))
	for k, v := range meta {
		if !allowedMetaKeys[k] {
			log.Printf("[AUDIT] dropped metadata key %q on %s", k, action)
			continue
		}
		clean[k] = v
	}
	if len(clean) == 0 {
		return nil
	}
	b, err := json.Marshal(clean)
	if err != nil {
		log.Printf("[AUDIT] metadata for %s not encodable: %v", action, err)
		return nil
	}
	s := string(b)
	return &s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
