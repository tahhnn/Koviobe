// Package payment runs self-service checkout: orders, the QR a host pays, and
// turning a matched bank transfer into a plan grant.
//
// Buying through Zalo is not modelled here — it creates no order. An admin
// grants the plan directly (license.AdminSetPlanWithContext).
//
// See LICENSING.md in the license package, section "Thanh toán".
package payment

import (
	"crypto/rand"
	"math/big"
	"regexp"
	"strings"
	"unicode"
)

// OrderCodePrefix must match the prefix configured in SePay ("Cấu trúc mã
// thanh toán": prefix KV, 8-character alphanumeric suffix) so SePay can lift
// the code into the webhook's "code" field on its own.
const OrderCodePrefix = "KV"

// orderAlphabet is the license-code alphabet (no 0 1 I L O U): a code a buyer
// may type into a banking app by hand must survive being read off a screen.
const orderAlphabet = "23456789ABCDEFGHJKMNPQRSTVWXYZ"

const orderSuffixLen = 8

var orderCodeRe = regexp.MustCompile(OrderCodePrefix + `[` + orderAlphabet + `]{8}`)

// NewOrderCode returns KV + 8 random characters. No separators: banks strip
// punctuation from transfer content, and a dash that vanishes would break an
// exact match. 30^8 ≈ 6.6e11 codes; collisions are caught by the unique index.
func NewOrderCode() (string, error) {
	max := big.NewInt(int64(len(orderAlphabet)))
	b := make([]byte, orderSuffixLen)
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = orderAlphabet[n.Int64()]
	}
	return OrderCodePrefix + string(b), nil
}

// OrderCodeCandidates lists every order code a transfer could be paying, best
// guess first. The caller looks each up and takes the first that exists.
//
// Order of preference:
//  1. SePay's own detected code.
//  2. Codes standing in the content as typed (case-insensitive).
//  3. Codes found after removing spaces, dashes and dots — buyers type
//     "KV7K9 Q2MXA". This pass can also glue neighbouring words into a
//     false code ("K VAN THANH" → "KVANTHANH…"), which is why it comes last
//     and why the caller must confirm a candidate exists rather than trust it.
func OrderCodeCandidates(detected, content string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(cs ...string) {
		for _, c := range cs {
			if c != "" && !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	add(orderCodeRe.FindString(strings.ToUpper(strings.TrimSpace(detected))))
	upper := strings.ToUpper(content)
	add(orderCodeRe.FindAllString(upper, -1)...)
	norm := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '-' || r == '.' || r == '_' {
			return -1
		}
		return r
	}, upper)
	add(orderCodeRe.FindAllString(norm, -1)...)
	return out
}
