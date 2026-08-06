package audit

import (
	"log"
	"time"

	"github.com/quizzzone/backend/internal/db"
	"github.com/quizzzone/backend/internal/model"
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
	}
}
