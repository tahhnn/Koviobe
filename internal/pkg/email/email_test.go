package email

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func renderOK(t *testing.T, m message) *mail.Message {
	t.Helper()
	raw, err := m.render()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\n")) {
		t.Fatal("message has a bare LF; RFC 5322 needs CRLF")
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("not a parseable RFC 5322 message: %v", err)
	}
	return msg
}

func sample() message {
	return message{
		FromName: "quizzZone",
		From:     "no-reply@quizzzone.com",
		To:       "mai@example.com",
		Subject:  "Mã xác thực quizzZone",
		HTML:     `<div><h2>Xin chào</h2><p>Mã của bạn: <b>123456</b></p><a href="https://quizzzone.com/x">Mở</a></div>`,
		Date:     time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC),
	}
}

func TestRenderHeadersAreWellFormed(t *testing.T) {
	msg := renderOK(t, sample())

	dec := new(mime.WordDecoder)
	subj, err := dec.DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || subj != "Mã xác thực quizzZone" {
		t.Fatalf("subject = %q, %v", subj, err)
	}
	if raw := msg.Header.Get("Subject"); strings.ContainsFunc(raw, func(r rune) bool { return r > 127 }) {
		t.Fatalf("subject header carries raw non-ASCII: %q", raw)
	}

	from, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil || from.Address != "no-reply@quizzzone.com" || from.Name != "quizzZone" {
		t.Fatalf("from = %+v, %v", from, err)
	}

	id := msg.Header.Get("Message-ID")
	if strings.Count(id, "@") != 1 || !strings.HasSuffix(id, "@quizzzone.com>") || !strings.HasPrefix(id, "<") {
		t.Fatalf("invalid Message-ID %q", id)
	}
	if v := msg.Header.Get("MIME-Version"); v != "1.0" {
		t.Fatalf("MIME-Version = %q", v)
	}
	if _, err := mail.ParseDate(msg.Header.Get("Date")); err != nil {
		t.Fatalf("Date unparseable: %v", err)
	}
	if msg.Header.Get("Reply-To") != "" {
		t.Fatal("Reply-To set without being configured")
	}
}

func TestRenderIsMultipartWithTextAndHTML(t *testing.T) {
	msg := renderOK(t, sample())
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("content type %q, %v", mt, err)
	}
	r := multipart.NewReader(msg.Body, params["boundary"])
	var types []string
	var text string
	for {
		p, err := r.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		ct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		types = append(types, ct)
		if p.Header.Get("Content-Transfer-Encoding") != "quoted-printable" {
			t.Fatalf("part %s not quoted-printable", ct)
		}
		body, _ := io.ReadAll(quotedprintable.NewReader(p))
		if ct == "text/plain" {
			text = string(body)
		}
	}
	if strings.Join(types, ",") != "text/plain,text/html" {
		t.Fatalf("parts = %v, want text/plain then text/html", types)
	}
	for _, want := range []string{"Xin chào", "123456", "https://quizzzone.com/x"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text part lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "<") {
		t.Fatalf("text part still has markup:\n%s", text)
	}
}

func TestRenderRejectsHeaderInjection(t *testing.T) {
	m := sample()
	m.To = "a@b.vn\r\nBcc: victim@x.vn"
	if _, err := m.render(); err == nil {
		t.Fatal("CRLF in To was accepted")
	}
	m = sample()
	m.Subject = "hi\r\nBcc: victim@x.vn"
	if _, err := m.render(); err == nil {
		t.Fatal("CRLF in Subject was accepted")
	}
}

func TestRenderReplyTo(t *testing.T) {
	m := sample()
	m.ReplyTo = "support@quizzzone.com"
	if got := renderOK(t, m).Header.Get("Reply-To"); got != "<support@quizzzone.com>" {
		t.Fatalf("Reply-To = %q", got)
	}
}

// Domain check ----------------------------------------------------------------

func stubDNS(t *testing.T, mx map[string][]*net.MX, hosts map[string]bool, tempFail map[string]bool) {
	t.Helper()
	oldMX, oldHost := lookupMX, lookupHost
	t.Cleanup(func() { lookupMX, lookupHost = oldMX, oldHost })
	domainMu.Lock()
	domainCache = map[string]domainVerdict{}
	domainMu.Unlock()
	notFound := &net.DNSError{Err: "no such host", IsNotFound: true}
	lookupMX = func(_ context.Context, d string) ([]*net.MX, error) {
		if tempFail[d] {
			return nil, &net.DNSError{Err: "server misbehaving", IsTemporary: true}
		}
		if r, ok := mx[d]; ok {
			return r, nil
		}
		return nil, notFound
	}
	lookupHost = func(_ context.Context, d string) ([]string, error) {
		if hosts[d] {
			return []string{"192.0.2.1"}, nil
		}
		return nil, notFound
	}
}

func TestCheckDomain(t *testing.T) {
	stubDNS(t,
		map[string][]*net.MX{
			"gmail.com":   {{Host: "gmail-smtp-in.l.google.com.", Pref: 5}},
			"nullmx.test": {{Host: ".", Pref: 0}},
		},
		map[string]bool{"aonly.test": true},
		map[string]bool{"flaky.test": true},
	)
	cases := map[string]bool{
		"a@gmail.com":   true,  // MX
		"a@GMAIL.com":   true,  // case-insensitive
		"a@aonly.test":  true,  // implicit MX via A record
		"a@flaky.test":  true,  // DNS trouble fails open
		"a@gmial.com":   false, // typo, NXDOMAIN
		"a@nullmx.test": false, // RFC 7505 null MX
		"no-at-sign":    false,
		"trailing@":     false,
	}
	for addr, want := range cases {
		err := CheckDomain(context.Background(), addr)
		if (err == nil) != want {
			t.Fatalf("CheckDomain(%q) = %v, want deliverable=%v", addr, err, want)
		}
	}
}

func TestCheckDomainCachesDefiniteAnswersOnly(t *testing.T) {
	stubDNS(t, nil, nil, map[string]bool{"flaky.test": true})
	calls := 0
	inner := lookupMX
	lookupMX = func(ctx context.Context, d string) ([]*net.MX, error) {
		calls++
		return inner(ctx, d)
	}
	_ = CheckDomain(context.Background(), "a@gone.test")
	_ = CheckDomain(context.Background(), "b@gone.test")
	if calls != 1 {
		t.Fatalf("NXDOMAIN not cached: %d lookups", calls)
	}
	calls = 0
	_ = CheckDomain(context.Background(), "a@flaky.test")
	_ = CheckDomain(context.Background(), "a@flaky.test")
	if calls != 2 {
		t.Fatalf("a temporary failure was cached: %d lookups", calls)
	}
}

// Recipient caps ----------------------------------------------------------------

func TestAllowRecipientCapsPerHour(t *testing.T) {
	counts := map[string]int64{}
	old := recipientIncr
	t.Cleanup(func() { recipientIncr = old })
	recipientIncr = func(_ context.Context, key string, _ time.Duration) (int64, error) {
		counts[key]++
		return counts[key], nil
	}
	allowed := 0
	for i := 0; i < 20; i++ {
		if allowRecipient(context.Background(), " Victim@X.vn ") {
			allowed++
		}
	}
	if allowed != 5 {
		t.Fatalf("allowed %d mails in an hour, want the default cap of 5", allowed)
	}
	if counts["mail_rcpt_h:victim@x.vn"] == 0 {
		t.Fatalf("address not normalised in the key: %v", counts)
	}
	if !allowRecipient(context.Background(), "other@x.vn") {
		t.Fatal("one address's cap blocked another")
	}
}

func TestAllowRecipientFailsOpenWithoutRedis(t *testing.T) {
	old := recipientIncr
	t.Cleanup(func() { recipientIncr = old })
	recipientIncr = func(context.Context, string, time.Duration) (int64, error) {
		return 0, errors.New("down")
	}
	if !allowRecipient(context.Background(), "a@x.vn") {
		t.Fatal("a Redis outage blocked mail")
	}
}

// Pacing ----------------------------------------------------------------------------

func TestTokenBucketBurstThenRate(t *testing.T) {
	b := newTokenBucket(60, 3) // 1/s steady, burst of 3
	now := b.last
	for i := 0; i < 3; i++ {
		if ok, _ := b.take(now); !ok {
			t.Fatalf("burst token %d refused", i)
		}
	}
	ok, wait := b.take(now)
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("4th immediate send: ok=%v wait=%v, want refused with ≤1s wait", ok, wait)
	}
	if ok, _ := b.take(now.Add(1100 * time.Millisecond)); !ok {
		t.Fatal("no token after the refill interval")
	}
}
