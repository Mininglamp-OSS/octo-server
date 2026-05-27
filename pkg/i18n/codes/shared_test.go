package codes

import (
	"net/http"
	"strings"
	"testing"
)

// TestSharedCodes_AllRegistered 验证首期 err.shared.* 全部成功注册，
// 且 HTTPStatus 与主方案 §3.1 表一致。失败说明 init() 注册被破坏。
func TestSharedCodes_AllRegistered(t *testing.T) {
	cases := []struct {
		id     string
		status int
	}{
		{"err.shared.auth.required", http.StatusUnauthorized},
		{"err.shared.auth.token_missing", http.StatusUnauthorized},
		{"err.shared.auth.token_invalid", http.StatusUnauthorized},
		{"err.shared.auth.token_expired", http.StatusUnauthorized},
		{"err.shared.auth.forbidden", http.StatusForbidden},
		{"err.shared.rate.limited", http.StatusTooManyRequests},
		{"err.shared.param.invalid", http.StatusBadRequest},
		{"err.shared.not_found", http.StatusNotFound},
		{"err.shared.internal", http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			got, ok := Lookup(c.id)
			if !ok {
				t.Fatalf("code %q not registered", c.id)
			}
			if got.HTTPStatus != c.status {
				t.Errorf("HTTPStatus = %d, want %d", got.HTTPStatus, c.status)
			}
			if got.DefaultMessage == "" {
				t.Error("DefaultMessage is empty")
			}
		})
	}
}

// TestSharedCodes_DefaultMessageEnglish 强制 source 文案为 ASCII 英文（D4）。
// 中文 / 非 ASCII 应只出现在 DefaultMessages["zh-CN"] 或 locale 文件里。
func TestSharedCodes_DefaultMessageEnglish(t *testing.T) {
	for _, c := range All() {
		if !strings.HasPrefix(c.ID, "err.shared.") {
			continue
		}
		for _, r := range c.DefaultMessage {
			if r > 127 {
				t.Errorf("%s: DefaultMessage contains non-ASCII rune %q (source must be en-US)",
					c.ID, r)
				break
			}
		}
	}
}

// TestSharedCodes_InternalFlag 验证 5xx code 必须带 Internal=true，
// 否则 renderer 会把 raw message 漏给客户端（D11/D13）。
func TestSharedCodes_InternalFlag(t *testing.T) {
	for _, c := range All() {
		if !strings.HasPrefix(c.ID, "err.shared.") {
			continue
		}
		is5xx := c.HTTPStatus >= 500 && c.HTTPStatus < 600
		if is5xx && !c.Internal {
			t.Errorf("%s: HTTPStatus=%d but Internal=false; 5xx must be Internal=true",
				c.ID, c.HTTPStatus)
		}
		if !is5xx && c.Internal {
			t.Errorf("%s: HTTPStatus=%d but Internal=true; only 5xx may be Internal",
				c.ID, c.HTTPStatus)
		}
	}
}

// TestSharedCodes_SafeDetailKeys 防止 SafeDetailKeys 列入敏感 key。
// 与 CI lint「Params 敏感 key 检查」（TODOS 0.10）双保险。
func TestSharedCodes_SafeDetailKeys(t *testing.T) {
	forbidden := []string{
		"uid", "token", "sql", "secret", "internal_id", "password", "raw_err",
	}
	for _, c := range All() {
		for _, k := range c.SafeDetailKeys {
			lower := strings.ToLower(k)
			for _, bad := range forbidden {
				if strings.Contains(lower, bad) {
					t.Errorf("%s: SafeDetailKeys contains forbidden key %q (matches %q)",
						c.ID, k, bad)
				}
			}
		}
	}
}

// TestSharedCodes_DefaultMessageInjectedIntoRegistry 验证 DefaultMessage 进入
// registry 后未被截断或丢失；TOML override 路径在 i18n 包测试中覆盖。
func TestSharedCodes_DefaultMessageInjectedIntoRegistry(t *testing.T) {
	for _, c := range All() {
		if c.DefaultMessage == "" {
			t.Errorf("%s: DefaultMessage empty after Register", c.ID)
		}
		// SafeDetailKeys 应是相同身份（地址不一定相同因为深拷贝）但内容相同。
		// 这里只断言「不为 nil 时长度合理」，避免误依赖 slice identity。
		if c.SafeDetailKeys != nil && len(c.SafeDetailKeys) == 0 {
			t.Errorf("%s: SafeDetailKeys is non-nil but empty (use nil for none)", c.ID)
		}
	}
}
