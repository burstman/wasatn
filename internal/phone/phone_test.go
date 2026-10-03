package phone

import (
	"strings"
	"testing"
)

func TestIsE164(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "valid tunisia", input: "+21651234567", want: true},
		{name: "valid minimum length", input: "+12345678", want: true},
		{name: "valid maximum length", input: "+123456789012345", want: true},
		{name: "valid uk", input: "+447911123456", want: true},

		{name: "missing plus", input: "21651234567", want: false},
		{name: "leading zero country code", input: "+051234567", want: false},
		{name: "too short", input: "+123456", want: false},
		{name: "too long", input: "+1234567890123456", want: false},
		{name: "contains spaces", input: "+216 51 234 567", want: false},
		{name: "contains dashes", input: "+216-51-234-567", want: false},
		{name: "contains letters", input: "+21651234abc", want: false},
		{name: "empty", input: "", want: false},
		{name: "plus only", input: "+", want: false},
		{name: "national format with trunk zero", input: "+2160512345", want: true},
		{name: "trailing newline", input: "+21651234567\n", want: false},
		{name: "leading newline", input: "\n+21651234567", want: false},
		{name: "unicode digits", input: "+٢١٦٥١٢٣٤٥٦٧", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsE164(tc.input); got != tc.want {
				t.Errorf("IsE164(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestMask(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty", input: "", want: ""},
		{name: "valid e164", input: "+21651234567", want: "+*******4567"},
		{name: "valid e164 short", input: "+447911123456", want: "+********3456"},
		{name: "exactly four digits", input: "+1234", want: "+****"},
		{name: "fewer than four digits", input: "+123", want: "+***"},
		{name: "single digit", input: "+1", want: "+*"},
		{name: "no plus prefix", input: "21651234567", want: "*******4567"},
		{name: "malformed with letters still masked", input: "+216CALL5678", want: "+*******5678"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Mask(tc.input); got != tc.want {
				t.Errorf("Mask(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestMaskNeverLeaksMoreThanFourDigits(t *testing.T) {
	t.Parallel()

	const secret = "+21651234567"
	masked := Mask(secret)

	if want := "+*******4567"; masked != want {
		t.Errorf("Mask(%q) = %q, want %q", secret, masked, want)
	}
	if strings.Contains(masked, "5123") {
		t.Errorf("Mask(%q) = %q, leaked digits beyond the last four", secret, masked)
	}
	if !strings.HasSuffix(masked, "4567") {
		t.Errorf("Mask(%q) = %q, want the last four digits visible", secret, masked)
	}
}
