package connections

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
)

// StateBytes is the entropy in a signup state value.
//
// 32 bytes is what an OAuth provider is expected to use, and it is what makes
// the value unguessable rather than merely unique: the value is the only thing
// proving the signup round trip came back from Meta.
const StateBytes = 32

// NewState returns a fresh, unguessable state value.
func NewState() (string, error) {
	buf := make([]byte, StateBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", errors.New("connections: read random bytes for signup state")
	}
	// Raw URL encoding: this travels in a query parameter Facebook echoes back.
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ValidState reports whether the returned state matches the issued one.
//
// The comparison is constant time so a timing signal cannot reveal the expected
// value, and both sides must be non-empty: an empty issued state means the
// session had no pending signup, which must never match a caller sending an
// empty state.
func ValidState(expected, got string) bool {
	if expected == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(got)) == 1
}
