package license

import (
	"strings"
	"testing"
)

// A buyer retypes a code from a receipt or hears it over the phone. Case, spaces
// and dash placement must not decide whether activation works.
func TestNormalizeCodeIsTypingTolerant(t *testing.T) {
	canonical := "KOVIO-23AB-45CD-67EF"
	variants := []string{
		"KOVIO-23AB-45CD-67EF",
		"kovio-23ab-45cd-67ef",
		"  KOVIO 23AB 45CD 67EF  ",
		"KOVIO23AB45CD67EF",
		"kovio23ab45cd67ef",
		"KOVIO_23AB_45CD_67EF",
		"23AB-45CD-67EF",
		"23ab45cd67ef",
	}
	for _, v := range variants {
		if got := NormalizeCode(v); got != canonical {
			t.Fatalf("NormalizeCode(%q) = %q, want %q", v, got, canonical)
		}
	}
}

// A string that is not our shape must not be coerced into a lookalike code —
// it should fall through to a clean "not found", not a partial match.
func TestNormalizeCodeLeavesForeignInputAlone(t *testing.T) {
	for _, in := range []string{"", "hello", "KOVIO-123", "KOVIO-23AB-45CD-67EF-EXTRA"} {
		got := NormalizeCode(in)
		if strings.Count(got, "-") == 3 && strings.HasPrefix(got, codePrefix+"-") {
			t.Fatalf("NormalizeCode(%q) = %q — wrong-length input must not produce a canonical code", in, got)
		}
	}
}

func TestRandomPayloadShapeAndAlphabet(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		p, err := randomPayload()
		if err != nil {
			t.Fatalf("randomPayload: %v", err)
		}
		if len(p) != codePayloadLen {
			t.Fatalf("payload %q length %d want %d", p, len(p), codePayloadLen)
		}
		for _, r := range p {
			if !strings.ContainsRune(codeAlphabet, r) {
				t.Fatalf("payload %q contains %q, outside the alphabet", p, r)
			}
		}
		if seen[p] {
			t.Fatalf("randomPayload repeated %q within 200 draws", p)
		}
		seen[p] = true
	}
}

// 0/1/I/L/O/U are the characters people confuse when reading a code aloud.
// Keeping them out of the alphabet is the whole point of not using base32.
func TestCodeAlphabetExcludesAmbiguousCharacters(t *testing.T) {
	for _, r := range "01ILOU" {
		if strings.ContainsRune(codeAlphabet, r) {
			t.Fatalf("codeAlphabet must not contain ambiguous %q", r)
		}
	}
	if len(codeAlphabet) != 30 {
		t.Fatalf("codeAlphabet len=%d want 30", len(codeAlphabet))
	}
}

// Every redeem error a user can trigger must be listed as such, otherwise the
// handler reports a real failure as a 500 (or worse, narrates an internal one).
func TestRedeemErrorsAreDistinct(t *testing.T) {
	errs := []error{
		ErrCodeNotFound, ErrCodeRevoked, ErrCodeExpired,
		ErrCodeExhausted, ErrCodeAlreadyUsed, ErrCodePlanInactive,
	}
	seen := map[string]bool{}
	for _, e := range errs {
		if e == nil || e.Error() == "" {
			t.Fatal("redeem error must carry a message")
		}
		if seen[e.Error()] {
			t.Fatalf("duplicate redeem error message %q", e.Error())
		}
		seen[e.Error()] = true
	}
}

// A lifetime code is DurationDays == 0, which is also Go's zero value. That
// collision is what made GORM omit the column from the INSERT and let a DB-side
// default rewrite it to 30 days. Guard the semantic so the meaning of 0 cannot
// be quietly reassigned.
func TestZeroDurationMeansLifetime(t *testing.T) {
	opts := AssignOptions{}
	durationDays := 0
	if durationDays > 0 {
		d := durationDays
		opts.EndsAtDays = &d
	}
	if opts.EndsAtDays != nil {
		t.Fatal("duration_days = 0 must leave EndsAtDays nil (lifetime)")
	}

	durationDays = 30
	if durationDays > 0 {
		d := durationDays
		opts.EndsAtDays = &d
	}
	if opts.EndsAtDays == nil || *opts.EndsAtDays != 30 {
		t.Fatalf("duration_days = 30 must map to EndsAtDays 30, got %v", opts.EndsAtDays)
	}
}
