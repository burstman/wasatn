// Package cryptox provides authenticated encryption for secrets at rest.
//
// WhatsApp access tokens are encrypted with AES-256-GCM before they are stored,
// and are only decrypted in memory at the moment a Graph API call needs them
// (PROMPT.md: "AES-256-GCM encrypt access tokens").
package cryptox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// KeySize is the AES-256 key length in bytes.
const KeySize = 32

// ErrInvalidCiphertext is returned when ciphertext is too short to contain an
// authentication tag, i.e. it was never produced by Seal.
var ErrInvalidCiphertext = errors.New("cryptox: invalid ciphertext")

// Cipher performs authenticated encryption with AES-256-GCM.
//
// A fresh random nonce is generated per Seal call and prefixed to the
// ciphertext, so encrypting the same plaintext twice never yields the same
// output.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher builds a Cipher from a 32-byte key.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("cryptox: key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("cryptox: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cryptox: new gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt seals plaintext, returning nonce||ciphertext.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("cryptox: read nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// EncryptString is a convenience wrapper around Encrypt.
func (c *Cipher) EncryptString(plaintext string) ([]byte, error) {
	return c.Encrypt([]byte(plaintext))
}

// Decrypt opens nonce||ciphertext produced by Encrypt.
func (c *Cipher) Decrypt(ciphertext []byte) ([]byte, error) {
	nonceSize := c.aead.NonceSize()
	if len(ciphertext) < nonceSize+c.aead.Overhead() {
		return nil, ErrInvalidCiphertext
	}
	nonce, sealed := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("cryptox: decrypt: %w", err)
	}
	return plaintext, nil
}

// DecryptString is a convenience wrapper around Decrypt.
func (c *Cipher) DecryptString(ciphertext []byte) (string, error) {
	plaintext, err := c.Decrypt(ciphertext)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}
