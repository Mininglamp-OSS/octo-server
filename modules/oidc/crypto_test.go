package oidc

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
)

func newTestEncryptor(t *testing.T) *Encryptor {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	enc, err := NewEncryptor(key)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func TestNewEncryptor(t *testing.T) {
	tests := []struct {
		name    string
		keyLen  int
		wantErr bool
	}{
		{"valid 32-byte AES-256 key", 32, false},
		{"too short (16 bytes)", 16, true},
		{"too short (24 bytes)", 24, true},
		{"too long (64 bytes)", 64, true},
		{"empty key", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key := make([]byte, tc.keyLen)
			_, _ = rand.Read(key)
			_, err := NewEncryptor(key)
			if (err != nil) != tc.wantErr {
				t.Fatalf("NewEncryptor(len=%d) err=%v, wantErr=%v", tc.keyLen, err, tc.wantErr)
			}
		})
	}

	t.Run("nil key", func(t *testing.T) {
		if _, err := NewEncryptor(nil); err == nil {
			t.Fatal("NewEncryptor(nil) should reject nil key")
		}
	})
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	enc := newTestEncryptor(t)

	cases := [][]byte{
		[]byte("refresh-token-sample-abcdef0123456789"),
		[]byte(""),
		bytes.Repeat([]byte("x"), 1024),
		[]byte{0x00, 0x01, 0x02, 0xff},
	}
	for _, plaintext := range cases {
		ciphertext, err := enc.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		if bytes.Equal(plaintext, ciphertext) && len(plaintext) > 0 {
			t.Fatalf("ciphertext equals plaintext")
		}
		got, err := enc.Decrypt(ciphertext)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Fatalf("round trip mismatch: got=%x want=%x", got, plaintext)
		}
	}
}

func TestEncryptIsNonDeterministic(t *testing.T) {
	enc := newTestEncryptor(t)

	plaintext := []byte("same-input")
	c1, _ := enc.Encrypt(plaintext)
	c2, _ := enc.Encrypt(plaintext)
	if bytes.Equal(c1, c2) {
		t.Fatal("two encryptions of identical plaintext should differ (random nonce)")
	}
}

func TestDecryptTamperedCiphertextFails(t *testing.T) {
	enc := newTestEncryptor(t)

	c, _ := enc.Encrypt([]byte("secret"))
	c[len(c)-1] ^= 0xff
	if _, err := enc.Decrypt(c); err == nil {
		t.Fatal("expected GCM auth failure on tampered ciphertext")
	}
}

func TestDecryptWrongKeyFails(t *testing.T) {
	k1 := make([]byte, 32)
	k2 := make([]byte, 32)
	_, _ = rand.Read(k1)
	_, _ = rand.Read(k2)
	e1, _ := NewEncryptor(k1)
	e2, _ := NewEncryptor(k2)

	c, _ := e1.Encrypt([]byte("secret"))
	if _, err := e2.Decrypt(c); err == nil {
		t.Fatal("expected decrypt failure with wrong key")
	}
}

func TestHashTokenDeterministicWithSameKey(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	e1, _ := NewEncryptor(key)
	e2, _ := NewEncryptor(key)

	h1 := e1.HashToken("abc")
	h2 := e2.HashToken("abc")
	if h1 != h2 {
		t.Fatal("same key should produce same HMAC for same input")
	}
	if len(h1) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(h1))
	}
	if strings.ContainsAny(h1, "GHIJKLMNOPQRSTUVWXYZ") {
		t.Fatal("hash should be lowercase hex")
	}
	if e1.HashToken("abc") == e1.HashToken("abd") {
		t.Fatal("different inputs must produce different hashes")
	}
}

func TestHashTokenKeyedDifferentiation(t *testing.T) {
	k1 := make([]byte, 32)
	k2 := make([]byte, 32)
	_, _ = rand.Read(k1)
	_, _ = rand.Read(k2)
	e1, _ := NewEncryptor(k1)
	e2, _ := NewEncryptor(k2)

	if e1.HashToken("abc") == e2.HashToken("abc") {
		t.Fatal("different keys should produce different hashes (HMAC keyed)")
	}
}
