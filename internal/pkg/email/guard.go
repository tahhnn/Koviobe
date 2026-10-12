package email

import (
	"context"
	"errors"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/quizzzone/backend/internal/cache"
	"github.com/quizzzone/backend/internal/config"
	"github.com/redis/go-redis/v9"
)

// ErrUndeliverableDomain means the address's domain cannot receive mail at all:
// it does not exist, or it publishes a null MX (RFC 7505). The English text is
// what internal/pkg/i18n/catalog.go keys on.
var ErrUndeliverableDomain = errors.New("This email address cannot receive mail")

// Whether a *mailbox* exists cannot be checked honestly. Probing with SMTP
// RCPT TO is answered "yes" by Gmail and every catch-all domain, is refused or
// tarpitted by the rest, and is itself the behaviour that gets a sending IP
// listed. What can be checked is the domain: one DNS lookup, cached, that
// rejects typos ("gmial.com") and made-up domains before anything is sent.

const (
	domainLookupTimeout = 3 * time.Second
	domainCacheOK       = 6 * time.Hour
	domainCacheBad      = time.Hour
	domainCacheMax      = 10000
)

// Swappable in tests.
var (
	lookupMX   = net.DefaultResolver.LookupMX
	lookupHost = net.DefaultResolver.LookupHost
)

type domainVerdict struct {
	ok      bool
	expires time.Time
}

var (
	domainMu    sync.Mutex
	domainCache = map[string]domainVerdict{}
)

// CheckDomain reports ErrUndeliverableDomain when addr's domain certainly
// cannot receive mail. A DNS failure that is not a definite "does not exist"
// (timeout, SERVFAIL) is allowed through: a flaky resolver must not lock real
// people out of signing up.
func CheckDomain(ctx context.Context, addr string) error {
	at := strings.LastIndex(addr, "@")
	if at < 0 || at == len(addr)-1 {
		return ErrUndeliverableDomain
	}
	domain := strings.ToLower(strings.TrimSuffix(addr[at+1:], "."))

	now := time.Now()
	domainMu.Lock()
	if v, hit := domainCache[domain]; hit && now.Before(v.expires) {
		domainMu.Unlock()
		if v.ok {
			return nil
		}
		return ErrUndeliverableDomain
	}
	domainMu.Unlock()

	ok, definite := resolveDomain(ctx, domain)
	if definite {
		ttl := domainCacheOK
		if !ok {
			ttl = domainCacheBad
		}
		domainMu.Lock()
		if len(domainCache) >= domainCacheMax {
			domainCache = map[string]domainVerdict{}
		}
		domainCache[domain] = domainVerdict{ok: ok, expires: now.Add(ttl)}
		domainMu.Unlock()
	}
	if !ok {
		return ErrUndeliverableDomain
	}
	return nil
}

// resolveDomain returns (deliverable, definite). definite=false means the
// lookup failed in a way that says nothing about the domain.
func resolveDomain(ctx context.Context, domain string) (bool, bool) {
	ctx, cancel := context.WithTimeout(ctx, domainLookupTimeout)
	defer cancel()

	mxs, err := lookupMX(ctx, domain)
	if err == nil && len(mxs) > 0 {
		// Null MX: "this domain accepts no mail".
		if len(mxs) == 1 && (mxs[0].Host == "." || mxs[0].Host == "") {
			return false, true
		}
		return true, true
	}
	if err != nil && !isNotFound(err) {
		return true, false
	}
	// No MX records: RFC 5321 §5.1 falls back to the A/AAAA record.
	if _, err := lookupHost(ctx, domain); err != nil {
		if isNotFound(err) {
			return false, true
		}
		return true, false
	}
	return true, true
}

func isNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

// Per-recipient caps ----------------------------------------------------------

// ErrRecipientLimited means this address has had its share of mail for now.
var ErrRecipientLimited = errors.New("too much mail to this address")

// recipientIncr bumps a counter that expires after ttl and returns its value.
// Swappable in tests.
var recipientIncr = func(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	if cache.RDB == nil {
		return 0, errors.New("redis not initialised")
	}
	return incrWithTTL.Run(ctx, cache.RDB, []string{key}, int64(ttl.Seconds())).Int64()
}

// INCR and EXPIRE in one step, re-arming a TTL that went missing — the same
// shape as the API rate limiter, for the same reason: a key without a TTL never
// resets.
var incrWithTTL = redis.NewScript(`
local n = redis.call("INCR", KEYS[1])
if redis.call("TTL", KEYS[1]) < 0 then redis.call("EXPIRE", KEYS[1], ARGV[1]) end
return n
`)

// allowRecipient counts one more mail to addr and reports whether it is within
// both the hourly and daily cap. Shared by every user-triggered mail type, so
// a victim's inbox has one budget no matter which form an attacker abuses.
//
// Fails open on Redis errors: the caps defend against bombing, and losing them
// for the length of an outage is better than losing every signup code.
func allowRecipient(ctx context.Context, addr string) bool {
	cfg := config.AppConfig
	perHour, perDay := 5, 15
	if cfg != nil {
		if cfg.EmailPerRecipientHour > 0 {
			perHour = cfg.EmailPerRecipientHour
		}
		if cfg.EmailPerRecipientDay > 0 {
			perDay = cfg.EmailPerRecipientDay
		}
	}
	addr = strings.ToLower(strings.TrimSpace(addr))

	h, err := recipientIncr(ctx, "mail_rcpt_h:"+addr, time.Hour)
	if err != nil {
		log.Printf("[EMAIL] recipient cap unavailable: %v", err)
		return true
	}
	d, err := recipientIncr(ctx, "mail_rcpt_d:"+addr, 24*time.Hour)
	if err != nil {
		log.Printf("[EMAIL] recipient cap unavailable: %v", err)
		return true
	}
	return h <= int64(perHour) && d <= int64(perDay)
}
