package space

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/mail"
	"strings"
)

// emailInviteTokenBytes token 原文字节长度（256 bit 熵）。
const emailInviteTokenBytes = 32

// generateEmailInviteToken 生成邮件邀请 token；返回明文（base64url 无填充）与其 SHA-256 十六进制哈希。
// 明文用于拼邮件链接，哈希用于入库。明文一旦返回就不再保存。
func generateEmailInviteToken() (raw, hash string, err error) {
	buf := make([]byte, emailInviteTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("read random for email invite token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	return raw, hashEmailInviteToken(raw), nil
}

// hashEmailInviteToken 对明文 token 做 SHA-256 并返回十六进制字符串。
func hashEmailInviteToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// validateInviteEmail 校验邀请邮箱：使用标准库 net/mail.ParseAddress 拒绝 "@"、"a@"、"@b" 这类
// 表面通过 strings.Contains 但 RFC 上无效的格式（PR #1172 review）。
// 入参应为已 trim 的字符串。
func validateInviteEmail(email string) error {
	if email == "" {
		return fmt.Errorf("邮箱不能为空")
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return fmt.Errorf("邮箱格式错误")
	}
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return fmt.Errorf("邮箱格式错误")
	}
	if !strings.Contains(email[at+1:], ".") {
		return fmt.Errorf("邮箱格式错误")
	}
	return nil
}
