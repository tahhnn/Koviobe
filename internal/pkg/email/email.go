package email

import (
	"fmt"
	"log"
	"net/smtp"
	"time"

	"github.com/quizzzone/backend/internal/config"
)

// SendEmail sends a real email to the recipient via SMTP.
// If SMTP credentials (SMTP_EMAIL, SMTP_PASSWORD) are not configured,
// it prints a mock email message to the server logs.
func SendEmail(to string, subject string, htmlBody string) error {
	cfg := config.AppConfig
	if cfg.SMTPEmail == "" || cfg.SMTPPassword == "" {
		log.Printf("\n[EMAIL MOCK - NO CREDENTIALS]\nTo: %s\nSubject: %s\nBody: (redacted — contains secrets)\n\n", to, subject)
		return nil
	}

	// Prepare mail headers & message body
	fromHeader := fmt.Sprintf("From: QUIZBATTLE <%s>\n", cfg.SMTPEmail)
	toHeader := fmt.Sprintf("To: %s\n", to)
	subjectHeader := fmt.Sprintf("Subject: %s\n", subject)
	dateHeader := fmt.Sprintf("Date: %s\n", time.Now().Format(time.RFC1123Z))
	msgIDHeader := fmt.Sprintf("Message-ID: <%d-%s@quizzzone>\n", time.Now().UnixNano(), cfg.SMTPEmail)
	mimeHeader := "MIME-version: 1.0;\nContent-Type: text/html; charset=\"UTF-8\";\n\n"

	msg := []byte(fromHeader + toHeader + subjectHeader + dateHeader + msgIDHeader + mimeHeader + htmlBody)

	// Authenticate and send
	auth := smtp.PlainAuth("", cfg.SMTPEmail, cfg.SMTPPassword, cfg.SMTPHost)
	addr := fmt.Sprintf("%s:%s", cfg.SMTPHost, cfg.SMTPPort)

	err := smtp.SendMail(addr, auth, cfg.SMTPEmail, []string{to}, msg)
	if err != nil {
		log.Printf("Failed to send email to %s via SMTP: %v", to, err)
		return err
	}

	log.Printf("Email successfully sent to %s", to)
	return nil
}
