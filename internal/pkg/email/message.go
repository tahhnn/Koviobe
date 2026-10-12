package email

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"strings"
	"time"
)

// message is one outgoing mail, before it is rendered to RFC 5322 bytes.
type message struct {
	FromName string
	From     string
	ReplyTo  string
	To       string
	Subject  string
	HTML     string
	Date     time.Time
}

// render builds the bytes handed to SMTP DATA.
//
// Each choice here is something spam filters score:
//   - Non-ASCII in From/Subject is RFC 2047-encoded; raw UTF-8 headers are
//     malformed and are penalised (and some servers mangle them).
//   - Message-ID has exactly one "@" and the sender's own domain. The old one,
//     "<nanos-user@domain@quizzzone>", was syntactically invalid.
//   - multipart/alternative with a text/plain part: HTML-only mail is a
//     classic spam signal.
//   - Bodies are quoted-printable, so no line exceeds the 998-octet limit and
//     8-bit text survives relays that are not 8BITMIME-clean.
//   - Auto-Submitted marks it as machine mail (RFC 3834), so autoresponders
//     do not reply to it.
//
// Lines end in CRLF as RFC 5322 requires.
func (m message) render() ([]byte, error) {
	if strings.ContainsAny(m.To, "\r\n") || strings.ContainsAny(m.Subject, "\r\n") {
		return nil, fmt.Errorf("header injection attempt")
	}
	from := (&mail.Address{Name: m.FromName, Address: m.From}).String()
	to := (&mail.Address{Address: m.To}).String()

	msgID, err := newMessageID(m.From)
	if err != nil {
		return nil, err
	}
	boundary, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	boundary = "qz-" + boundary

	var b bytes.Buffer
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", from)
	h("To", to)
	if m.ReplyTo != "" {
		h("Reply-To", (&mail.Address{Address: m.ReplyTo}).String())
	}
	h("Subject", mime.QEncoding.Encode("UTF-8", m.Subject))
	h("Date", m.Date.Format(time.RFC1123Z))
	h("Message-ID", msgID)
	h("MIME-Version", "1.0")
	h("Auto-Submitted", "auto-generated")
	h("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString("\r\n")

	part := func(contentType, body string) error {
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString("Content-Type: " + contentType + "; charset=UTF-8\r\n")
		b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		w := quotedprintable.NewWriter(&b)
		if _, err := w.Write([]byte(body)); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		b.WriteString("\r\n")
		return nil
	}
	if err := part("text/plain", htmlToText(m.HTML)); err != nil {
		return nil, err
	}
	if err := part("text/html", m.HTML); err != nil {
		return nil, err
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes(), nil
}

// newMessageID returns "<random@sender-domain>".
func newMessageID(from string) (string, error) {
	domain := "quizzzone.com"
	if at := strings.LastIndex(from, "@"); at >= 0 && at < len(from)-1 {
		domain = from[at+1:]
	}
	id, err := randomHex(16)
	if err != nil {
		return "", err
	}
	return "<" + id + "@" + domain + ">", nil
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

var (
	reDropBlocks = regexp.MustCompile(`(?is)<(style|script|head)[^>]*>.*?</(style|script|head)>`)
	reHref       = regexp.MustCompile(`(?is)<a\s[^>]*href="([^"]*)"[^>]*>(.*?)</a>`)
	reBreaks     = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|h[1-6]|li|tr|ol|ul)>`)
	reTags       = regexp.MustCompile(`(?s)<[^>]+>`)
	reSpaces     = regexp.MustCompile(`[ \t]+`)
	reBlankLines = regexp.MustCompile(`\n\s*\n+`)
)

// htmlToText is the text/plain alternative. Links keep their URL, since a
// reader on a text client still needs somewhere to click.
func htmlToText(s string) string {
	s = reDropBlocks.ReplaceAllString(s, "")
	s = reHref.ReplaceAllString(s, "$2 ($1)")
	s = reBreaks.ReplaceAllString(s, "\n")
	s = reTags.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(reSpaces.ReplaceAllString(l, " "))
	}
	s = strings.Join(lines, "\n")
	s = reBlankLines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s) + "\n"
}
