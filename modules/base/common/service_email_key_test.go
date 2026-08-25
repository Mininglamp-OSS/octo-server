package common

import (
	"testing"
)

func TestEmailVerificationKeysAreIsolatedByCodeType(t *testing.T) {
	email := "admin@example.com"
	userKey := EmailCodeKey(email, CodeTypeEmailLogin)
	managerKey := EmailCodeKey(email, CodeTypeManagerLogin)
	if userKey == managerKey {
		t.Fatalf("email code keys must differ: %q", userKey)
	}
	if EmailRateLimitKey(email, CodeTypeEmailLogin) == EmailRateLimitKey(email, CodeTypeManagerLogin) {
		t.Fatal("email rate-limit keys must differ by CodeType")
	}
	if EmailVerifyFailKey(email, CodeTypeEmailLogin) == EmailVerifyFailKey(email, CodeTypeManagerLogin) {
		t.Fatal("email failure keys must differ by CodeType")
	}
	if EmailVerifyLockKey(email, CodeTypeEmailLogin) == EmailVerifyLockKey(email, CodeTypeManagerLogin) {
		t.Fatal("email lock keys must differ by CodeType")
	}
}

func TestManagerEmailCodeRequiresSentStatus(t *testing.T) {
	if !emailCodeRequiresSentStatus(CodeTypeManagerLogin) {
		t.Fatal("manager OTP must require the sent status")
	}
	if emailCodeRequiresSentStatus(CodeTypeEmailLogin) {
		t.Fatal("ordinary email login must retain missing-status compatibility")
	}
}

func TestValidateSMTPConfiguration(t *testing.T) {
	valid := []struct {
		addr string
		from string
	}{
		{"smtp.example.com:587", "admin@example.com"},
		{"[::1]:2525", "admin+ops@example.com"},
	}
	for _, tc := range valid {
		if err := ValidateSMTPConfiguration(tc.addr, tc.from, "secret"); err != nil {
			t.Errorf("valid SMTP config rejected: %s: %v", tc.addr, err)
		}
	}
	if err := ValidateSMTPConfiguration("smtp.example.com:25", "relay@example.com", ""); err != nil {
		t.Fatalf("passwordless SMTP relay must be accepted: %v", err)
	}
	invalid := []struct {
		addr string
		from string
	}{
		{"smtp.example.com:587", "invalid"},
		{"smtp.example.com:587", "Name <admin@example.com>"},
		{"smtp.example.com", "admin@example.com"},
		{"smtp.example.com:0", "admin@example.com"},
	}
	for _, tc := range invalid {
		if err := ValidateSMTPConfiguration(tc.addr, tc.from, "secret"); err == nil {
			t.Errorf("invalid SMTP config accepted: %q / %q", tc.addr, tc.from)
		}
	}
}
