package common

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func setenvForTest(t *testing.T, key, value string) {
	t.Helper()
	previous, ok := os.LookupEnv(key)
	if err := os.Setenv(key, value); err != nil {
		t.Fatalf("set %s: %v", key, err)
	}
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(key, previous)
			return
		}
		_ = os.Unsetenv(key)
	})
}

func unsetenvForTest(t *testing.T, key string) {
	t.Helper()
	previous, ok := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(key, previous)
		}
	})
}

func TestEncryptKey_NoMasterKey_ReturnsError(t *testing.T) {
	// Ensure OCTO_MASTER_KEY is not set
	unsetenvForTest(t, masterKeyEnv)

	plaintext := "test-private-key-content"
	result, err := encryptKey(plaintext)

	assert.Error(t, err)
	assert.Empty(t, result)
	assert.Contains(t, err.Error(), "OCTO_MASTER_KEY not configured")
}

func TestEncryptKey_InvalidKeyLength_ReturnsError(t *testing.T) {
	// Set an invalid key (not 32 bytes)
	setenvForTest(t, masterKeyEnv, "short-key")

	plaintext := "test-private-key-content"
	result, err := encryptKey(plaintext)

	assert.Error(t, err)
	assert.Empty(t, result)
	assert.Contains(t, err.Error(), "32 bytes")
}

func TestEncryptKey_ValidKey_ReturnsEncrypted(t *testing.T) {
	// Set a valid 32-byte key
	setenvForTest(t, masterKeyEnv, "12345678901234567890123456789012")

	plaintext := "test-private-key-content"
	result, err := encryptKey(plaintext)

	assert.NoError(t, err)
	assert.NotEmpty(t, result)
	assert.True(t, len(result) > len(encryptedKeyPrefix))
	assert.Equal(t, encryptedKeyPrefix, result[:len(encryptedKeyPrefix)])
}

func TestDecryptKey_LegacyPlaintext_ReturnsAsIs(t *testing.T) {
	// Legacy plaintext without enc: prefix should be returned as-is
	unsetenvForTest(t, masterKeyEnv)

	plaintext := "legacy-plaintext-key"
	result, err := decryptKey(plaintext)

	assert.NoError(t, err)
	assert.Equal(t, plaintext, result)
}

func TestDecryptKey_EncryptedWithoutMasterKey_ReturnsError(t *testing.T) {
	// Encrypted key but no master key configured
	unsetenvForTest(t, masterKeyEnv)

	encrypted := encryptedKeyPrefix + "some-base64-data"
	result, err := decryptKey(encrypted)

	assert.Error(t, err)
	assert.Empty(t, result)
	assert.Contains(t, err.Error(), "OCTO_MASTER_KEY is not set")
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	// Set a valid 32-byte key
	setenvForTest(t, masterKeyEnv, "12345678901234567890123456789012")

	original := "test-private-key-content-12345"
	encrypted, err := encryptKey(original)
	assert.NoError(t, err)
	assert.NotEqual(t, original, encrypted)

	decrypted, err := decryptKey(encrypted)
	assert.NoError(t, err)
	assert.Equal(t, original, decrypted)
}
