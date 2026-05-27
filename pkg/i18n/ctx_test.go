package i18n

import (
	"context"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
)

func TestContextLanguageRoundTrip(t *testing.T) {
	ctx := WithLanguage(context.Background(), LanguageDecision{
		Language: "zh-CN",
		Source:   LanguageSourceCookie,
	})

	got, ok := LanguageFromContext(ctx)
	if !ok {
		t.Fatal("LanguageFromContext ok=false")
	}
	if got.Language != "zh-CN" || got.Source != LanguageSourceCookie {
		t.Fatalf("LanguageFromContext = %#v", got)
	}
}

func TestWithLanguageIfHigherPriority(t *testing.T) {
	ctx := WithLanguage(context.Background(), LanguageDecision{
		Language: "en-US",
		Source:   LanguageSourceAccept,
	})

	ctx, changed := WithLanguageIfHigherPriority(ctx, LanguageDecision{
		Language: "zh-CN",
		Source:   LanguageSourceUser,
	})
	if !changed {
		t.Fatal("user language should override Accept-Language")
	}
	got, _ := LanguageFromContext(ctx)
	if got.Language != "zh-CN" || got.Source != LanguageSourceUser {
		t.Fatalf("after user override = %#v", got)
	}

	ctx, changed = WithLanguageIfHigherPriority(ctx, LanguageDecision{
		Language: "en-US",
		Source:   LanguageSourceCookie,
	})
	if !changed {
		t.Fatal("cookie language should override user language")
	}
	got, _ = LanguageFromContext(ctx)
	if got.Language != "en-US" || got.Source != LanguageSourceCookie {
		t.Fatalf("after cookie override = %#v", got)
	}

	ctx, changed = WithLanguageIfHigherPriority(ctx, LanguageDecision{
		Language: "zh-CN",
		Source:   LanguageSourceUser,
	})
	if changed {
		t.Fatal("user language must not override explicit cookie language")
	}
	got, _ = LanguageFromContext(ctx)
	if got.Language != "en-US" || got.Source != LanguageSourceCookie {
		t.Fatalf("after lower-priority override = %#v", got)
	}
}

// TestLanguageFromContextPromotesUserLanguage 验证 D9 两段式协商的读侧合并：
// 当 ctx 中早期协商结果优先级低于 LanguageSourceUser 时，UserInfo.Language
// 应在读取时被提升为最终决策。
func TestLanguageFromContextPromotesUserLanguage(t *testing.T) {
	cases := []struct {
		name        string
		existing    *LanguageDecision
		userLang    string
		wantLang    string
		wantSource  LanguageSource
		wantPresent bool
	}{
		{
			name:        "promotes_over_accept",
			existing:    &LanguageDecision{Language: "en-US", Source: LanguageSourceAccept},
			userLang:    "zh-CN",
			wantLang:    "zh-CN",
			wantSource:  LanguageSourceUser,
			wantPresent: true,
		},
		{
			name:        "promotes_over_default",
			existing:    &LanguageDecision{Language: "en-US", Source: LanguageSourceDefault},
			userLang:    "zh-CN",
			wantLang:    "zh-CN",
			wantSource:  LanguageSourceUser,
			wantPresent: true,
		},
		{
			name:        "cookie_wins_over_user",
			existing:    &LanguageDecision{Language: "en-US", Source: LanguageSourceCookie},
			userLang:    "zh-CN",
			wantLang:    "en-US",
			wantSource:  LanguageSourceCookie,
			wantPresent: true,
		},
		{
			name:        "trusted_header_wins_over_user",
			existing:    &LanguageDecision{Language: "en-US", Source: LanguageSourceTrustedHeader},
			userLang:    "zh-CN",
			wantLang:    "en-US",
			wantSource:  LanguageSourceTrustedHeader,
			wantPresent: true,
		},
		{
			name:        "empty_user_language_no_promotion",
			existing:    &LanguageDecision{Language: "en-US", Source: LanguageSourceAccept},
			userLang:    "",
			wantLang:    "en-US",
			wantSource:  LanguageSourceAccept,
			wantPresent: true,
		},
		{
			name:        "no_existing_decision_user_promotes",
			existing:    nil,
			userLang:    "zh-CN",
			wantLang:    "zh-CN",
			wantSource:  LanguageSourceUser,
			wantPresent: true,
		},
		{
			name:        "no_existing_no_user",
			existing:    nil,
			userLang:    "",
			wantPresent: false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.existing != nil {
				ctx = WithLanguage(ctx, *tc.existing)
			}
			ctx = wkhttp.WithUser(ctx, wkhttp.UserInfo{UID: "u1", Language: tc.userLang})

			got, ok := LanguageFromContext(ctx)
			if ok != tc.wantPresent {
				t.Fatalf("ok = %v, want %v", ok, tc.wantPresent)
			}
			if !tc.wantPresent {
				return
			}
			if got.Language != tc.wantLang || got.Source != tc.wantSource {
				t.Fatalf("got %+v, want lang=%q source=%q", got, tc.wantLang, tc.wantSource)
			}
		})
	}
}

func TestLanguageOrDefault(t *testing.T) {
	if got := LanguageOrDefault(context.Background(), "zh-CN"); got != "zh-CN" {
		t.Fatalf("empty context fallback = %q, want zh-CN", got)
	}

	ctx := WithLanguage(context.Background(), LanguageDecision{
		Language: "en-US",
		Source:   LanguageSourceDefault,
	})
	if got := LanguageOrDefault(ctx, "zh-CN"); got != "en-US" {
		t.Fatalf("context language = %q, want en-US", got)
	}
}
