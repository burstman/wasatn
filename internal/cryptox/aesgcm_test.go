package cryptox

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, KeySize)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

func TestNewCipherRejectsWrongKeySize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  []byte
	}{
		{name: "empty", key: nil},
		{name: "too short", key: make([]byte, 16)},
		{name: "aes-192 size", key: make([]byte, 24)},
		{name: "one byte too long", key: make([]byte, 33)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewCipher(tc.key); err == nil {
				t.Fatal("NewCipher() error = nil, want error for wrong key size")
			}
		})
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	t.Parallel()

	c, err := NewCipher(testKey(t))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}

	plaintexts := []string{
		"",
		"EAAGYzBUm...",
		"token with spaces and = signs",
		"\x00\x01\x02 binary-ish \xff",
		string(bytes.Repeat([]byte("a"), 4096)),
	}

	for _, plaintext := range plaintexts {
		got, err := c.EncryptString(plaintext)
		if err != nil {
			t.Fatalf("EncryptString(%q) error = %v", plaintext, err)
		}
		if bytes.Contains(got, []byte(plaintext)) && plaintext != "" {
			t.Errorf("EncryptString(%q) leaked plaintext into ciphertext", plaintext)
		}
		back, err := c.DecryptString(got)
		if err != nil {
			t.Fatalf("DecryptString() error = %v", err)
		}
		if back != plaintext {
			t.Errorf("round trip = %q, want %q", back, plaintext)
		}
	}
}

func TestEncryptIsNonDeterministic(t *testing.T) {
	t.Parallel()

	c, err := NewCipher(testKey(t))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}

	first, err := c.EncryptString("same-token")
	if err != nil {
		t.Fatalf("EncryptString() error = %v", err)
	}
	second, err := c.EncryptString("same-token")
	if err != nil {
		t.Fatalf("EncryptString() error = %v", err)
	}

	if bytes.Equal(first, second) {
		t.Error("EncryptString() produced identical ciphertext twice; nonce is not random")
	}

	// Both must still decrypt to the same plaintext.
	for i, ct := range [][]byte{first, second} {
		got, err := c.DecryptString(ct)
		if err != nil {
			t.Fatalf("DecryptString(ciphertext %d) error = %v", i, err)
		}
		if got != "same-token" {
			t.Errorf("DecryptString(ciphertext %d) = %q, want %q", i, got, "same-token")
		}
	}
}

func TestDecryptRejectsTamperedCiphertext(t *testing.T) {
	t.Parallel()

	c, err := NewCipher(testKey(t))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}

	sealed, err := c.EncryptString("token")
	if err != nil {
		t.Fatalf("EncryptString() error = %v", err)
	}

	tests := []struct {
		name       string
		ciphertext []byte
	}{
		{name: "nil", ciphertext: nil},
		{name: "empty", ciphertext: []byte{}},
		{name: "nonce only", ciphertext: sealed[:12]},
		{name: "flipped ciphertext bit", ciphertext: flipBit(sealed, len(sealed)-1)},
		{name: "flipped nonce bit", ciphertext: flipBit(sealed, 0)},
		{name: "truncated", ciphertext: sealed[:len(sealed)-1]},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := c.Decrypt(tc.ciphertext); err == nil {
				t.Error("Decrypt() error = nil, want error")
			}
		})
	}
}

func TestDecryptFailsWithDifferentKey(t *testing.T) {
	t.Parallel()

	c, err := NewCipher(testKey(t))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}
	other, err := NewCipher(bytes.Repeat([]byte{0xff}, KeySize))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}

	sealed, err := c.EncryptString("token")
	if err != nil {
		t.Fatalf("EncryptString() error = %v", err)
	}
	if _, err := other.Decrypt(sealed); err == nil {
		t.Error("Decrypt() with wrong key succeeded, want error")
	}
}

func TestCiphertextIsStoredAsBytesNotBase64(t *testing.T) {
	t.Parallel()

	c, err := NewCipher(testKey(t))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}

	sealed, err := c.EncryptString("token")
	if err != nil {
		t.Fatalf("EncryptString() error = %v", err)
	}

	// The bytea column stores raw bytes; base64 appears only if something
	// encodes it by mistake. Confirm raw bytes are directly usable.
	if _, err := c.Decrypt(sealed); err != nil {
		t.Fatalf("Decrypt(raw bytes) error = %v", err)
	}
	if _, err := base64.StdEncoding.DecodeString(string(sealed)); err == nil {
		t.Error("ciphertext is valid base64 text; it should be raw bytes")
	}
}

func flipBit(b []byte, i int) []byte {
	out := bytes.Clone(b)
	out[i] ^= 0x01
	return out
}
