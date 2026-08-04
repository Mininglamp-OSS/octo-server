package space

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGenerateEmailInviteToken_FormatAndHash(t *testing.T) {
	raw, hash, err := generateEmailInviteToken()
	assert.NoError(t, err)

	// base64url 无填充解码应得到 32 字节
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	assert.NoError(t, err)
	assert.Len(t, decoded, emailInviteTokenBytes)

	// SHA-256 十六进制长度固定 64
	assert.Len(t, hash, 64)
	assert.Equal(t, hashEmailInviteToken(raw), hash)
}

func TestGenerateEmailInviteToken_UniquePerCall(t *testing.T) {
	seen := make(map[string]struct{}, 20)
	for i := 0; i < 20; i++ {
		raw, hash, err := generateEmailInviteToken()
		assert.NoError(t, err)
		_, dupRaw := seen[raw]
		_, dupHash := seen[hash]
		assert.False(t, dupRaw, "raw token collided")
		assert.False(t, dupHash, "hash collided")
		seen[raw] = struct{}{}
		seen[hash] = struct{}{}
	}
}

func TestMaskInviteEmail(t *testing.T) {
	cases := []struct{ in, want string }{
		{"jerry@example.com", "je***@e***.com"},
		{"a@x.io", "a***@x***.io"},
		{"ab@y.cn", "ab***@y***.cn"},
		{"foo@a.b.example.com", "fo***@a***.com"},
		{"", "***"},
		{"@", "***"},
		{"a@b", "***"},     // 域无 . 视为非法
		{"a@", "***"},
		{"@b.com", "***"},
	}
	for _, tc := range cases {
		got := maskInviteEmail(tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
}

func TestValidateInviteEmail(t *testing.T) {
	cases := []struct {
		in    string
		valid bool
	}{
		{"a@b.com", true},
		{"user.name+tag@sub.example.com", true},
		{"", false},
		{"@", false},
		{"a@", false},
		{"@b", false},
		{"a@b", false}, // 缺 TLD 的点
		{"abc", false},
		{"a @b.com", false}, // 含空格
		{"a@b.com extra", false},
	}
	for _, tc := range cases {
		err := validateInviteEmail(tc.in)
		if tc.valid {
			assert.NoError(t, err, tc.in)
		} else {
			assert.Error(t, err, tc.in)
		}
	}
}

func TestHashEmailInviteToken_Deterministic(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, hashEmailInviteToken(tc.in))
	}
}
