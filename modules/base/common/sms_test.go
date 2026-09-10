package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGenerateSecureVerifyCode(t *testing.T) {
	tests := []struct {
		name   string
		length int
	}{
		{"4 digits", 4},
		{"6 digits", 6},
		{"1 digit", 1},
		{"8 digits", 8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, err := generateSecureVerifyCode(tt.length)
			assert.NoError(t, err)
			assert.Len(t, code, tt.length, "code length should be %d", tt.length)

			// 验证只包含数字
			for _, c := range code {
				assert.True(t, c >= '0' && c <= '9',
					"code should only contain digits, got %c", c)
			}
		})
	}
}

func TestGenerateSecureVerifyCode_Uniqueness(t *testing.T) {
	// 生成多个验证码，验证不全是同一个值（概率极小但做基本检查）
	codes := make(map[string]bool)
	for i := 0; i < 100; i++ {
		code, err := generateSecureVerifyCode(4)
		assert.NoError(t, err)
		codes[code] = true
	}
	// 100次生成的4位验证码不可能全部相同
	assert.Greater(t, len(codes), 1, "should generate different codes")
}

func TestGenerateSecureVerifyCode_OnlyDigits(t *testing.T) {
	// 大量生成验证所有字符都是数字
	for i := 0; i < 1000; i++ {
		code, err := generateSecureVerifyCode(6)
		assert.NoError(t, err)
		for j, c := range code {
			assert.True(t, c >= '0' && c <= '9',
				"iteration %d, position %d: expected digit, got %c", i, j, c)
		}
	}
}

func TestCodeTypeConstants(t *testing.T) {
	// 验证 CodeType 常量存在且不重复
	types := []CodeType{
		CodeTypeRegister,
		CodeTypePayPWD,
		CodeTypeForgetLoginPWD,
		CodeTypeCheckMobile,
		CodeTypeDestroyAccount,
		CodeTypeEmailLogin,
		CodeTypeOIDCBind,
		CodeTypeManagerLogin,
	}

	seen := make(map[CodeType]bool)
	for _, ct := range types {
		assert.False(t, seen[ct], "CodeType %v should be unique", ct)
		seen[ct] = true
	}
	assert.Equal(t, CodeType(0), CodeTypeRegister)
	assert.Equal(t, CodeType(5), CodeTypeEmailLogin)
	assert.Equal(t, CodeType(6), CodeTypeOIDCBind)
	assert.Equal(t, CodeType(7), CodeTypeManagerLogin)
}

func TestCacheKeySMSCode(t *testing.T) {
	assert.Equal(t, "smscode:", CacheKeySMSCode)
}
