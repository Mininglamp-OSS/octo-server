package user

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// TestValidatePasswordStrength_MaxLengthMatchesBcryptLimit 锁死上限与 bcrypt 实现
// 的一致性：如果 x/crypto 未来放宽/收紧 72 字节，这个断言会先失败，而不是让线上
// 出现"校验通过但 HashPassword 报内部错误"的组合。
func TestValidatePasswordStrength_MaxLengthMatchesBcryptLimit(t *testing.T) {
	atLimit := strings.Repeat("a", maxPasswordLength-1) + "1"
	if err := ValidatePasswordStrength(atLimit); err != nil {
		t.Fatalf("password at max length must validate, got %v", err)
	}
	if _, err := HashPassword(atLimit); err != nil {
		t.Fatalf("password accepted by validation must be hashable, got %v", err)
	}

	overLimit := strings.Repeat("a", maxPasswordLength) + "1"
	if _, err := bcrypt.GenerateFromPassword([]byte(overLimit), bcryptCost); !errors.Is(err, bcrypt.ErrPasswordTooLong) {
		t.Fatalf("expected bcrypt to reject %d bytes, got %v", len(overLimit), err)
	}
	if err := ValidatePasswordStrength(overLimit); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("validation must reject what bcrypt would reject, got %v", err)
	}
}

func TestValidatePasswordStrength(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  error // nil means valid
	}{
		{"too short at 7 chars", "Ab1@xyz", ErrPasswordTooShort},
		{"exactly 8 chars but only one class", "aaaaaaaa", ErrPasswordNotComplexEnough},
		{"pure digits", "12345678", ErrPasswordNotComplexEnough},
		{"pure lowercase", "abcdefgh", ErrPasswordNotComplexEnough},
		{"lowercase+digit at boundary length", "abcdefg1", nil},
		{"uppercase+lowercase", "AbcdefGh", nil},
		{"lowercase+special", "abcdef!g", nil},
		{"three classes", "Abcdefg1", nil},
		{"four classes", "Ab1@cdef", nil},
		{"long weak password still fails without a second class", "aaaaaaaaaaaaaaaaaaaa", ErrPasswordNotComplexEnough},
		{"empty password", "", ErrPasswordTooShort},
		// 长度下限按 rune 计：4 个中文字符是 12 字节，若按 byte 计会被误判为达标。
		{"4 CJK chars are 12 bytes but only 4 characters", "密码密码", ErrPasswordTooShort},
		{"8 CJK chars plus a digit meets both length and class rules", "密码密码密码密码1", nil},
		// 长度上限按 byte 计，对齐 bcrypt 的 72 字节硬限制。
		{"72 bytes is allowed", strings.Repeat("a", 71) + "1", nil},
		{"73 bytes is rejected before bcrypt sees it", strings.Repeat("a", 72) + "1", ErrPasswordTooLong},
		{"25 CJK chars is 75 bytes and rejected", strings.Repeat("密", 24) + "码1", ErrPasswordTooLong},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePasswordStrength(tt.password)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidatePasswordStrength(%q) = %v, want nil", tt.password, err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidatePasswordStrength(%q) = %v, want %v", tt.password, err, tt.wantErr)
			}
		})
	}
}
