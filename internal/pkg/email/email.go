package email

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/quizzzone/backend/internal/config"
	"github.com/quizzzone/backend/internal/pkg/notify"
)

// Two things kill an SMTP send silently, and both are timeouts:
// net/smtp sets no deadline at all, so a server that never answers holds the
// caller until the OS gives up. license.DeliverCodes calls SendEmail inline
// from an HTTP handler, so that stall is a hung admin request, and the signup
// paths leak a blocked goroutine per attempt.
const (
	dialTimeout    = 10 * time.Second
	sessionTimeout = 30 * time.Second
)

// tlsMode reports how to secure the connection: "implicit" wraps the socket in
// TLS before the greeting (SMTPS, port 465), "starttls" upgrades an already
// plaintext session (submission, port 587).
//
// Getting this wrong fails in the least helpful way possible. smtp.SendMail
// always speaks plaintext first, so against a 465 listener it waits for a
// greeting that will never come — the server is waiting for a ClientHello.
// Verified against the configured host: a plaintext connect to 465 returns
// nothing, a TLS connect returns "220 ... ESMTP Postfix".
func tlsMode(cfg *config.Config) string {
	switch strings.ToLower(strings.TrimSpace(cfg.SMTPTLS)) {
	case "implicit", "ssl", "smtps":
		return "implicit"
	case "starttls", "tls":
		return "starttls"
	}
	if cfg.SMTPPort == "465" {
		return "implicit"
	}
	return "starttls"
}

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

	if err := send(cfg, to, msg); err != nil {
		log.Printf("Failed to send email to %s via SMTP (%s:%s, %s): %v",
			to, cfg.SMTPHost, cfg.SMTPPort, tlsMode(cfg), err)
		notify.P1("smtp_send_failed", "Gửi email thất bại qua %s:%s (%s): %v — OTP đăng ký và mã license không tới tay người dùng.",
			cfg.SMTPHost, cfg.SMTPPort, tlsMode(cfg), err)
		return err
	}

	log.Printf("Email successfully sent to %s", to)
	return nil
}

func send(cfg *config.Config, to string, msg []byte) error {
	addr := net.JoinHostPort(cfg.SMTPHost, cfg.SMTPPort)
	tlsConf := &tls.Config{ServerName: cfg.SMTPHost, MinVersion: tls.VersionTLS12}
	implicit := tlsMode(cfg) == "implicit"

	dialer := &net.Dialer{Timeout: dialTimeout}
	var conn net.Conn
	var err error
	if implicit {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConf)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	// Covers every read and write below, including the TLS handshake on the
	// STARTTLS path. Without it a half-open connection hangs indefinitely.
	if err := conn.SetDeadline(time.Now().Add(sessionTimeout)); err != nil {
		conn.Close()
		return err
	}

	client, err := smtp.NewClient(conn, cfg.SMTPHost)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp greeting: %w", err)
	}
	defer client.Close()

	if !implicit {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(tlsConf); err != nil {
				return fmt.Errorf("starttls: %w", err)
			}
		} else {
			// Refuse to hand the mailbox password to a plaintext session.
			return fmt.Errorf("server %s offers no STARTTLS; set SMTP_TLS=implicit or use a TLS port", addr)
		}
	}

	if ok, _ := client.Extension("AUTH"); ok {
		auth := smtp.PlainAuth("", cfg.SMTPEmail, cfg.SMTPPassword, cfg.SMTPHost)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}

	if err := client.Mail(cfg.SMTPEmail); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("rcpt to: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		w.Close()
		return fmt.Errorf("write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close body: %w", err)
	}
	return client.Quit()
}
