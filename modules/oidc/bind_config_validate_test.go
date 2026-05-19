package oidc

import (
	"strings"
	"testing"
)

// TestValidateBindConfigAgainstProvider 锁定启动期硬约束:
//   - Bind.Enabled=true && Provider.AllowNewUser=true 必须报错。
//     原因:用户首次 OIDC 登录 autolink 三种全失败时,系统只有两条路 ——
//     "新建空账号"(AllowNewUser=true) 或 "走自助绑定"(Bind.Enabled=true)。
//     两者同时开,用户会被静默兜底到新建空账号,绑定流程根本进不来,
//     运维和用户都察觉不到。FR-1.1 明确要求绑定触发条件是
//     AllowNewUser=false,这里在启动期做硬校验,迫使 ops 显式取舍。
//   - Bind.Enabled=true 时 RedirectBase 必须为合法 https URL。漏配 / scheme
//     非 https 都直接 fail Init,避免 callback 路径才发现降级。
//   - Bind.Enabled=false 不校验(老行为不动)。
//   - Bind.Enabled=true && AllowNewUser=false && 合法 RedirectBase 通过。
func TestValidateBindConfigAgainstProvider(t *testing.T) {
	const validBase = "https://app.example.com/oidc/bind"
	cases := []struct {
		name             string
		bindEnabled      bool
		allowNewUser     bool
		redirectBase     string
		wantErr          bool
		wantErrSubstring string
	}{
		{"both off — old behaviour preserved", false, false, "", false, ""},
		{"bind off, allow_new_user on — old behaviour preserved", false, true, "", false, ""},
		{"bind on, allow_new_user off, valid https base", true, false, validBase, false, ""},
		{
			"bind on AND allow_new_user on — must fail-fast at startup",
			true, true, validBase, true, "AllowNewUser",
		},
		{
			"bind on, redirect base empty — fail Init",
			true, false, "", true, "OCTO_OIDC_BIND_REDIRECT_BASE",
		},
		{
			"bind on, redirect base http — rejected without insecure override",
			true, false, "http://app.example.com/bind", true, "https scheme",
		},
		{
			"bind on, redirect base relative — rejected",
			true, false, "/bind", true, "absolute",
		},
		{
			// javascript: 缺 Host,先被 "absolute" 校验拦下;两层校验都拒,
			// 实际语义是 "scheme + host 任一非法都 fail",无需区分先后。
			"bind on, redirect base javascript scheme — rejected",
			true, false, "javascript:alert(1)", true, "absolute",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				Enabled:  true,
				Provider: ProviderConfig{AllowNewUser: tc.allowNewUser},
				Bind: BindConfig{
					Enabled:      tc.bindEnabled,
					RedirectBase: tc.redirectBase,
					// 主表用例默认填合法 Methods,空 Methods 校验由独立子用例覆盖。
					Methods: []BindMethod{BindMethodPassword, BindMethodSMSOTP},
				},
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

	// Methods 必须非空(防 loadBindMethods 在 env 全 invalid 时静默回退默认):
	// 显式拒,匹配 "Methods 是真实策略不是 UI hint" 的安全契约。
	t.Run("bind on, methods empty (all-invalid env) — fail Init", func(t *testing.T) {
		cfg := &Config{
			Enabled:  true,
			Provider: ProviderConfig{AllowNewUser: false},
			Bind: BindConfig{
				Enabled:      true,
				RedirectBase: validBase,
				Methods:      nil,
			},
		}
		err := validateBindConfigAgainstProvider(cfg)
		if err == nil {
			t.Fatal("expected error for empty Methods, got nil")
		}
		if !strings.Contains(err.Error(), "OCTO_OIDC_BIND_METHODS") {
			t.Fatalf("error %q must mention OCTO_OIDC_BIND_METHODS", err.Error())
		}
	})

	// T5: Bind.Enabled=true + AllowCreate=true + AllowNewUser=true → must fail
	// 同时开 AllowCreate 和 AllowNewUser 等价于"autolink 失败后直接建号",
	// bind 流程的建号路径(create)与 callback 自动建号路径同时存在,运维会误
	// 以为 create 被 bind 护栏保护但实际已经被 AllowNewUser 旁路。
	t.Run("T5: AllowCreate=true + AllowNewUser=true — must panic/fail", func(t *testing.T) {
		cfg := &Config{
			Enabled:  true,
			Provider: ProviderConfig{AllowNewUser: true},
			Bind: BindConfig{
				Enabled:      true,
				AllowCreate:  true,
				RedirectBase: validBase,
				Methods:      []BindMethod{BindMethodPassword, BindMethodSMSOTP},
			},
		}
		err := validateBindConfigAgainstProvider(cfg)
		if err == nil {
			t.Fatal("T5: AllowCreate=true + AllowNewUser=true must be rejected at startup")
		}
		if !strings.Contains(err.Error(), "AllowNewUser") {
			t.Fatalf("T5: error %q must mention AllowNewUser", err.Error())
		}
	})

	// T6: AllowCreate=true + AllowNewUser=false → pass
	t.Run("T6: AllowCreate=true + AllowNewUser=false — passes", func(t *testing.T) {
		cfg := &Config{
			Enabled:  true,
			Provider: ProviderConfig{AllowNewUser: false},
			Bind: BindConfig{
				Enabled:      true,
				AllowCreate:  true,
				RedirectBase: validBase,
				Methods:      []BindMethod{BindMethodPassword, BindMethodSMSOTP},
			},
		}
		if err := validateBindConfigAgainstProvider(cfg); err != nil {
			t.Fatalf("T6: valid AllowCreate config must pass validation, got: %v", err)
		}
	})
}
