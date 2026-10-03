// Package phone validates, normalises and masks E.164 phone numbers.
//
// Every phone number that enters the system is validated here and masked before
// it reaches a log line (PROMPT.md: "E.164 validation everywhere; mask phone
// numbers in logs").
package phone

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
)

// e164Pattern matches E.164: a leading '+', a non-zero country digit, and 7 to
// 14 further digits (15 digits total maximum).
var e164Pattern = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

// ErrNotE164 reports a value that could not be normalised into E.164.
var ErrNotE164 = errors.New("phone: value is not E.164")

// IsE164 reports whether s is a syntactically valid E.164 number.
//
// This is a syntax check only; it does not verify the number is assigned.
func IsE164(s string) bool {
	return e164Pattern.MatchString(s)
}

// Normalize converts a human-formatted phone number into E.164, so a value
// received from Meta can satisfy the CHECK constraint on connections.
//
// Meta's display_phone_number arrives formatted for humans, e.g.
// "+1 555-010-9999", and occasionally without the leading '+'. Separators are
// dropped and the '+' is always restored, because the country code is present in
// what Meta returns and a number stored without a '+' could not be dialled.
//
// Anything else is left to the caller: Normalize does not check that a country
// code is plausible, so "(555) 010-9999" becomes "+5550109999", which passes
// E.164 syntax while being the wrong number. That is accepted deliberately,
// since the only sources for a real country code are Meta and the customer, and
// guessing one here would send messages to the wrong person. Callers that need
// certainty should use NormalizeE164, which at least rejects the shapes that
// cannot be a number at all.
func Normalize(raw string) string {
	trimmed := strings.TrimSpace(raw)

	var digits strings.Builder
	digits.Grow(len(trimmed))
	for _, r := range trimmed {
		if unicode.IsDigit(r) {
			digits.WriteRune(r)
		}
	}
	if digits.Len() == 0 {
		return ""
	}
	return "+" + digits.String()
}

// NormalizeE164 is Normalize plus a validity check, for callers that must reject
// a value rather than store it and fail later.
func NormalizeE164(raw string) (string, error) {
	normalized := Normalize(raw)
	if !IsE164(normalized) {
		return "", ErrNotE164
	}
	return normalized, nil
}

// visibleDigits is how many trailing digits Mask leaves readable.
const visibleDigits = 4

// Mask redacts an E.164 number for logging, keeping only the last four digits
// visible: "+216 51 234 567" becomes "+*********4567".
//
// Input that is not valid E.164 is fully masked except its last four
// characters, so a malformed value can still be correlated without leaking.
func Mask(s string) string {
	if s == "" {
		return ""
	}

	prefix := ""
	digits := s
	if strings.HasPrefix(s, "+") {
		prefix = "+"
		digits = s[1:]
	}

	if len(digits) <= visibleDigits {
		return prefix + strings.Repeat("*", len(digits))
	}
	return prefix + strings.Repeat("*", len(digits)-visibleDigits) + digits[len(digits)-visibleDigits:]
}
