package botfather

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	userAPIKeyCipherPrefix  = "enc:v1:"
	userAPIKeyStoragePrefix = "hash:"
	userAPIKeySecretEnv     = "OCTO_USER_API_KEY_SECRET"
	userAPIKeyHashDomain    = "dmwork-user-api-key-verifier-v2"
	userAPIKeyCipherDomain  = "dmwork-user-api-key-cipher-v1"
)

func hashUserAPIKey(plaintext string) (string, error) {
	if strings.TrimSpace(plaintext) == "" {
		return "", errors.New("empty user api key")
	}
	secret, err := userAPIKeySecret()
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(userAPIKeyHashDomain))
	mac.Write([]byte{0})
	mac.Write([]byte(plaintext))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func storedUserAPIKeyValue(hash string) string {
	if hash == "" {
		return ""
	}
	return userAPIKeyStoragePrefix + hash
}

func buildUserAPIKeyStorage(plaintext string) (stored, hash, cipherText string, err error) {
	if strings.TrimSpace(plaintext) == "" {
		return "", "", "", errors.New("empty user api key")
	}
	hash, err = hashUserAPIKey(plaintext)
	if err != nil {
		return "", "", "", err
	}
	cipherText, err = encryptUserAPIKey(plaintext)
	if err != nil {
		return "", "", "", err
	}
	return storedUserAPIKeyValue(hash), hash, cipherText, nil
}

func encryptUserAPIKey(plaintext string) (string, error) {
	aead, err := newUserAPIKeyAEAD()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("user api key nonce: %w", err)
	}
	ciphertext := aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return userAPIKeyCipherPrefix + base64.StdEncoding.EncodeToString(ciphertext), nil
}

func decryptUserAPIKey(cipherText string) (string, error) {
	if cipherText == "" {
		return "", errors.New("empty user api key cipher")
	}
	if !strings.HasPrefix(cipherText, userAPIKeyCipherPrefix) {
		return "", errors.New("unknown user api key cipher prefix")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(cipherText, userAPIKeyCipherPrefix))
	if err != nil {
		return "", fmt.Errorf("decode user api key cipher: %w", err)
	}
	aead, err := newUserAPIKeyAEAD()
	if err != nil {
		return "", err
	}
	ns := aead.NonceSize()
	if len(raw) < ns+aead.Overhead() {
		return "", errors.New("user api key cipher too short")
	}
	plaintext, err := aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt user api key: %w", err)
	}
	return string(plaintext), nil
}

func newUserAPIKeyAEAD() (cipher.AEAD, error) {
	secret, err := userAPIKeySecret()
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(userAPIKeyCipherDomain))
	key := mac.Sum(nil)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("user api key aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("user api key gcm: %w", err)
	}
	return aead, nil
}

func userAPIKeySecret() ([]byte, error) {
	secret := []byte(os.Getenv(userAPIKeySecretEnv))
	if len(secret) != 32 {
		return nil, fmt.Errorf("%s must be exactly 32 bytes", userAPIKeySecretEnv)
	}
	return secret, nil
}

// ValidateUserAPIKeySecret is used by integration enable/issuance paths. The
// BotFather legacy client deliberately does not require this secret so existing
// /quickstart deployments keep working until uk_ storage hardening is enabled
// for that client in a dedicated rollout.
func ValidateUserAPIKeySecret() error {
	_, err := userAPIKeySecret()
	return err
}
