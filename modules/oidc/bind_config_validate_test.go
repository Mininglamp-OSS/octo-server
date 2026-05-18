package oidc

import (
	"strings"
	"testing"
)

// TestValidateBindConfigAgainstProvider 锁定 PR4 引入的硬约束:
//   - Bind.Enabled=true && Provider.AllowNewUser=true 必须报错。
//     原因:用户首次 OIDC 登录 autolink 三种全失败时,系统只有两条路 ——
//     "新建空账号"(AllowNewUser=true) 或 "走自助绑定"(Bind.Enabled=true)。
//     两者同时开,用户会被静默兜底到新建空账号,绑定流程根本进不来,
//     运维和用户都察觉不到。FR-1.1 明确要求绑定触发条件是
//     AllowNewUser=false,这里在启动期做硬校验,迫使 ops 显式取舍。
//   - Bind.Enabled=false 不校验(老行为不动)。
//   - Bind.Enabled=true && AllowNewUser=false 通过。
func TestValidateBindConfigAgainstProvider(t *testing.T) {
	cases := []struct {
		name             string
		bindEnabled      bool
		allowNewUser     bool
		wantErr          bool
		wantErrSubstring string
	}{
		{"both off — old behaviour preserved", false, false, false, ""},
		{"bind off, allow_new_user on — old behaviour preserved", false, true, false, ""},
		{"bind on, allow_new_user off — proper binding configuration", true, false, false, ""},
		{
			"bind on AND allow_new_user on — must fail-fast at startup",
			true, true, true, "AllowNewUser",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				Enabled:  true,
				Provider: ProviderConfig{AllowNewUser: tc.allowNewUser},
				Bind:     BindConfig{Enabled: tc.bindEnabled},
			}
			err := validateBindConfigAgainstProvider(cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tc.wantErrSubstring) {
					t.Fatalf("error %q must mention %q", err.Error(), tc.wantErrSubstring)
				}
			} else if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}
