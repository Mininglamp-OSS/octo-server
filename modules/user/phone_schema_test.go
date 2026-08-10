package user

import (
	"strings"
	"testing"
)

func TestPhoneEncryptedWidthMigrationCoversUTF8MB4ColumnBounds(t *testing.T) {
	raw, err := sqlFS.ReadFile("sql/20260810000004_user_phone_encrypted_width.sql")
	if err != nil {
		t.Fatalf("read phone encrypted width migration: %v", err)
	}
	if !strings.Contains(string(raw), "VARBINARY(1024)") {
		t.Fatal("phone_encrypted must be widened to VARBINARY(1024)")
	}
}

func TestMaximumSchemaSizedPhoneCiphertextExceeds512Bytes(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	enc, err := newPhoneEncryptor()
	if err != nil {
		t.Fatalf("newPhoneEncryptor: %v", err)
	}
	zone := strings.Repeat("😀", 20)   // zone VARCHAR(20) under utf8mb4
	phone := strings.Repeat("😀", 100) // phone VARCHAR(100) under utf8mb4
	ciphertext, _, _, err := enc.encryptPhone(zone, phone)
	if err != nil {
		t.Fatalf("encryptPhone: %v", err)
	}
	if len(ciphertext) <= 512 {
		t.Fatalf("fixture ciphertext = %d bytes, want >512 to guard against undersized migration", len(ciphertext))
	}
	if len(ciphertext) > 1024 {
		t.Fatalf("ciphertext = %d bytes, exceeds proposed VARBINARY(1024)", len(ciphertext))
	}
}
