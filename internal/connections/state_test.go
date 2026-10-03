package connections

import (
	"encoding/base64"
	"strings"
	"testing"
)

// The state is the only thing proving a signup round trip came back from Meta,
// so it has to be unguessable and must not collide between clicks.
func TestNewStateIsUnguessableAndUnique(t *testing.T) {
	seen := make(map[string]bool, 100)
	for range 100 {
		state, err := NewState()
		if err != nil {
			t.Fatalf("NewState: %v", err)
		}
		if seen[state] {
			t.Fatalf("NewState repeated %q", state)
		}
		seen[state] = true

		raw, err := base64.RawURLEncoding.DecodeString(state)
		if err != nil {
			t.Fatalf("the state is not raw URL encoding: %v", err)
		}
		if len(raw) != StateBytes {
			t.Fatalf("entropy = %d bytes, want %d", len(raw), StateBytes)
		}
	}
}

// Facebook echoes the state in a query parameter, so characters that would need
// escaping cannot appear.
func TestNewStateIsQueryParameterSafe(t *testing.T) {
	state, err := NewState()
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}
	for _, r := range state {
		if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", r) {
			t.Fatalf("the state contains %q, which needs escaping", r)
		}
	}
}

func TestValidState(t *testing.T) {
	const expected = "c3RhdGUtdmFsdWU"

	tests := []struct {
		name     string
		expected string
		got      string
		want     bool
	}{
		{name: "match", expected: expected, got: expected, want: true},
		{name: "mismatch", expected: expected, got: "other"},
		{name: "prefix of the expected value", expected: expected, got: expected[:len(expected)-1]},
		{name: "longer than the expected value", expected: expected, got: expected + "x"},
		// An absent pending state must never match an empty state, or a request
		// that sends nothing would pass the check.
		{name: "nothing issued", expected: "", got: ""},
		{name: "nothing issued, caller sends a value", expected: "", got: expected},
		{name: "something issued, caller sends nothing", expected: expected, got: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidState(tc.expected, tc.got); got != tc.want {
				t.Fatalf("ValidState(%q, %q) = %v, want %v", tc.expected, tc.got, got, tc.want)
			}
		})
	}
}
