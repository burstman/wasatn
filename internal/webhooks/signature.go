// Package webhooks receives Meta's WhatsApp Cloud API webhooks: it verifies the
// signature, answers the subscription challenge, and routes each change by its
// field.
package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// SignatureHeader is the header Meta signs each delivery with.
const SignatureHeader = "X-Hub-Signature-256"

// signaturePrefix is the algorithm marker Meta puts in front of the digest.
const signaturePrefix = "sha256="

// ErrSignatureMissing means Meta did not send a signature header at all.
var ErrSignatureMissing = errors.New("webhooks: missing " + SignatureHeader)

// ErrSignatureMalformed means the header was present but not "sha256=<hex>".
var ErrSignatureMalformed = errors.New("webhooks: malformed " + SignatureHeader)

// ErrSignatureMismatch means the digest did not match the body. The body has
// been tampered with, or the app secret is wrong.
var ErrSignatureMismatch = errors.New("webhooks: signature does not match body")

// Sign returns the value Meta would send in SignatureHeader for body. Tests use
// it to build valid deliveries; production only ever verifies.
func Sign(appSecret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature checks Meta's signature over the raw request body.
//
// The body must be the exact bytes received, before any JSON decoding: Meta
// signs the serialised payload, so re-encoding it would change the digest. The
// comparison is constant time so a wrong signature cannot be recovered by
// timing the endpoint.
func VerifySignature(appSecret string, body []byte, header string) error {
	if strings.TrimSpace(header) == "" {
		return ErrSignatureMissing
	}
	// An unconfigured app secret must never verify anything: the HMAC key would
	// be empty, and anyone could compute the matching digest.
	if appSecret == "" {
		return ErrSignatureMismatch
	}
	if !strings.HasPrefix(header, signaturePrefix) {
		return ErrSignatureMalformed
	}

	// Meta sends a lowercase hex sha256 digest. Checking the shape first turns a
	// truncated or wrongly cased header into a clear "malformed" instead of a
	// puzzling signature mismatch.
	digest := strings.TrimPrefix(header, signaturePrefix)
	if len(digest) != hex.EncodedLen(sha256.Size) || strings.ToLower(digest) != digest {
		return ErrSignatureMalformed
	}

	want, err := hex.DecodeString(digest)
	if err != nil {
		return ErrSignatureMalformed
	}

	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	if !hmac.Equal(want, mac.Sum(nil)) {
		return ErrSignatureMismatch
	}
	return nil
}

// VerifyChallengeParams holds the query parameters Meta sends when you click
// "Verify and Save" in the dashboard.
type VerifyChallengeParams struct {
	Mode      string
	Token     string
	Challenge string
}

// Validate reports whether the subscription request is legitimate: Meta must
// ask to subscribe and echo the verify token we configured.
func (p VerifyChallengeParams) Validate(expectedToken string) error {
	if p.Mode != "subscribe" {
		return fmt.Errorf("webhooks: hub.mode = %q, want \"subscribe\"", p.Mode)
	}
	// An unconfigured verify token must not accept anything, and hmac.Equal
	// treats two empty strings as equal, so the emptiness is checked explicitly.
	if expectedToken == "" || p.Token == "" {
		return errors.New("webhooks: hub.verify_token is not configured")
	}
	// Compare the tokens in constant time as well: this endpoint is public and
	// the token is a shared secret.
	if !hmac.Equal([]byte(p.Token), []byte(expectedToken)) {
		return errors.New("webhooks: hub.verify_token does not match")
	}
	if p.Challenge == "" {
		return errors.New("webhooks: hub.challenge is empty")
	}
	return nil
}
