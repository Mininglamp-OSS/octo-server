package oidc

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// hashDomainSeparator HKDF 风格的域分离串,确保 token-hash 用的密钥与 AES-GCM 主密钥
// 在密码学层面相互独立(HMAC 输出作为派生子密钥),避免同密钥跨原语复用。
const hashDomainSeparator = "dmwork-oidc-token-hash-v1"

// Encryptor 提供 refresh_token 的对称加密 + 抗预映射哈希
//
// 加密:AES-256-GCM,密文格式 nonce(12B) || ciphertext || tag(16B)
// 哈希:HMAC-SHA256,密钥从主密钥派生(域分离),输出 64 字符小写 hex
type Encryptor struct {
	gcm     cipher.AEAD
	hashKey []byte
}

// NewEncryptor 用 32 字节主密钥构造 Encryptor
func NewEncryptor(key []byte) (*Encryptor, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("oidc: encryption key must be 32 bytes (AES-256), got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("oidc: aes.NewCipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("oidc: cipher.NewGCM: %w", err)
	}
	return &Encryptor{gcm: gcm, hashKey: deriveHashKey(key)}, nil
}

// Encrypt 加密明文,每次返回带随机 nonce 的密文
func (e *Encryptor) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, e.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("oidc: rand nonce: %w", err)
	}
	return e.gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt 解密 Encrypt 产生的密文,认证失败返回错误
//
// 显式拒绝 < nonce + GCM tag 的密文(payload 至少 0 字节,但 tag 占 16B 不能少),
// 避免对 GCM 内部行为的隐式依赖,错误信息也更明确。
func (e *Encryptor) Decrypt(ciphertext []byte) ([]byte, error) {
	ns := e.gcm.NonceSize()
	if len(ciphertext) < ns+e.gcm.Overhead() {
		return nil, errors.New("oidc: ciphertext too short")
	}
	nonce, payload := ciphertext[:ns], ciphertext[ns:]
	plaintext, err := e.gcm.Open(nil, nonce, payload, nil)
	if err != nil {
		return nil, fmt.Errorf("oidc: gcm open: %w", err)
	}
	return plaintext, nil
}

// HashToken 用派生密钥对 token 做 HMAC-SHA256,作为 user_oidc_refresh.token_hash
//
// 相比裸 SHA256,HMAC 让 DB 行单独泄漏时无法离线穷举 token —— 攻击者还需要拿到主密钥。
func (e *Encryptor) HashToken(token string) string {
	mac := hmac.New(sha256.New, e.hashKey)
	mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

// deriveHashKey 用主密钥 + 域分离串导出 32 字节子密钥
func deriveHashKey(masterKey []byte) []byte {
	mac := hmac.New(sha256.New, masterKey)
	mac.Write([]byte(hashDomainSeparator))
	return mac.Sum(nil)
}
