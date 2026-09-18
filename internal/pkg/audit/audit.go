package audit

import (
	"log"
	"time"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
	"github.com/quizzzone/backend/internal/pkg/notify"
)

// Record writes a security audit log to the database.
func Record(userID uint, action string, resource string, ipAddress string) {
	auditLog := model.AuditLog{
		UserID:    userID,
		Action:    action,
		Resource:  resource,
		IPAddress: ipAddress,
		CreatedAt: time.Now(),
	}

	if err := db.DB.Create(&auditLog).Error; err != nil {
		log.Printf("[AUDIT ERROR] Failed to record audit log: %v", err)
		notify.P1("audit_write_failed", "Không ghi được audit log (user=%d action=%s resource=%s ip=%s): %v — dấu vết bảo mật đang mất.",
			userID, action, resource, ipAddress, err)
	}
}
