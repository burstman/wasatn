// Package phone validates and masks E.164 phone numbers.
//
// Every phone number that enters the system is validated here and masked before
// it reaches a log line (PROMPT.md: "E.164 validation everywhere; mask phone
// numbers in logs").
package phone

import (
	"regexp"
	"strings"
)

// e164Pattern matches E.164: a leading '+', a non-zero country digit, and 7 to
// 14 further digits (15 digits total maximum).
var e164Pattern = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

// IsE164 reports whether s is a syntactically valid E.164 number.
//
// This is a syntax check only; it does not verify the number is assigned.
func IsE164(s string) bool {
	return e164Pattern.MatchString(s)
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
