package webhooks

import (
	"errors"
	"strings"
	"testing"
)

const testSecret = "test-app-secret"

func TestVerifySignature(t *testing.T) {
	t.Parallel()

	body := []byte(`{"object":"whatsapp_business_account"}`)

	t.Run("valid", func(t *testing.T) {
		t.Parallel()

		if err := VerifySignature(testSecret, body, signature(testSecret, body)); err != nil {
			t.Fatalf("VerifySignature() = %v, want nil", err)
		}
	})

	t.Run("missing header", func(t *testing.T) {
		t.Parallel()

		err := VerifySignature(testSecret, body, "")
		if err == nil {
			t.Fatal("VerifySignature() = nil, want an error for a missing header")
		}
		if !errors.Is(err, ErrSignatureMissing) {
			t.Fatalf("VerifySignature() = %v, want ErrSignatureMissing", err)
		}
	})

	t.Run("malformed header", func(t *testing.T) {
		t.Parallel()

		// A header Meta would never send. Answering 401 rather than 403 tells us
		// the request was not from Meta at all, without confirming anything else.
		// deadbeef is valid hex but the wrong length for a sha256 digest.
		for _, header := range []string{"sha256=deadbeef", "=", "sha256=", "deadbeef", "sha1=" + signature(testSecret, body)[7:]} {
			if err := VerifySignature(testSecret, body, header); !errors.Is(err, ErrSignatureMalformed) {
				t.Errorf("VerifySignature(%q) = %v, want ErrSignatureMalformed", header, err)
			}
		}
	})

	t.Run("unconfigured app secret", func(t *testing.T) {
		t.Parallel()

		// Without META_APP_SECRET the key would be empty, so the "signature"
		// below would be computable by anyone.
		if err := VerifySignature("", body, Sign("", body)); !errors.Is(err, ErrSignatureMismatch) {
			t.Fatalf("VerifySignature() = %v, want ErrSignatureMismatch", err)
		}
	})

	t.Run("wrong secret", func(t *testing.T) {
		t.Parallel()

		err := VerifySignature("other-secret", body, signature(testSecret, body))
		if !errors.Is(err, ErrSignatureMismatch) {
			t.Fatalf("VerifySignature() = %v, want ErrSignatureMismatch", err)
		}
	})

	t.Run("tampered body", func(t *testing.T) {
		t.Parallel()

		header := signature(testSecret, body)
		tampered := []byte(`{"object":"tampered"}`)
		if err := VerifySignature(testSecret, tampered, header); !errors.Is(err, ErrSignatureMismatch) {
			t.Fatalf("VerifySignature() = %v, want ErrSignatureMismatch", err)
		}
	})

	t.Run("empty body", func(t *testing.T) {
		t.Parallel()

		// Signing an empty body must stay verifiable, since Meta does send
		// test deliveries with no entries.
		empty := []byte{}
		if err := VerifySignature(testSecret, empty, signature(testSecret, empty)); err != nil {
			t.Fatalf("VerifySignature() = %v, want nil", err)
		}
	})

	t.Run("signature prefix is case sensitive", func(t *testing.T) {
		t.Parallel()

		upper := strings.ToUpper(strings.TrimPrefix(Sign(testSecret, body), "sha256="))
		err := VerifySignature(testSecret, body, "sha256="+upper)
		if !errors.Is(err, ErrSignatureMalformed) {
			t.Fatalf("VerifySignature() = %v, want ErrSignatureMalformed", err)
		}
	})
}

func TestVerifyChallengeParams_Validate(t *testing.T) {
	t.Parallel()

	const token = "verify-me"

	tests := map[string]struct {
		params   VerifyChallengeParams
		wantErr  bool
		wantEcho string
	}{
		"valid": {
			params:   VerifyChallengeParams{Mode: "subscribe", Token: token, Challenge: "1158201444"},
			wantEcho: "1158201444",
		},
		"empty challenge": {
			params:  VerifyChallengeParams{Mode: "subscribe", Token: token},
			wantErr: true,
		},
		"wrong token": {
			params:  VerifyChallengeParams{Mode: "subscribe", Token: "nope", Challenge: "1"},
			wantErr: true,
		},
		"empty token": {
			// An unconfigured verify token must never accept a challenge.
			params:  VerifyChallengeParams{Mode: "subscribe", Challenge: "1"},
			wantErr: true,
		},
		"wrong mode": {
			params:  VerifyChallengeParams{Mode: "unsubscribe", Token: token, Challenge: "1"},
			wantErr: true,
		},
		"missing mode": {
			params:  VerifyChallengeParams{Token: token, Challenge: "1"},
			wantErr: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := tc.params.Validate(token)
			if tc.wantErr {
				if err == nil {
					t.Fatal("Validate() = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if tc.wantEcho != "" && tc.params.Challenge != tc.wantEcho {
				t.Errorf("Challenge = %q, want %q", tc.params.Challenge, tc.wantEcho)
			}
		})
	}
}

// signature builds the header Meta would send, so the tests read like a delivery
// rather than a copy of the production signing code.
func signature(appSecret string, body []byte) string {
	return Sign(appSecret, body)
}

func TestSign(t *testing.T) {
	t.Parallel()

	body := []byte("payload")

	if got, want := Sign(testSecret, body), signature(testSecret, body); got != want {
		t.Fatalf("Sign() = %q, want %q", got, want)
	}
}
