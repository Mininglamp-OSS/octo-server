package i18n

import (
	"reflect"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

func TestNewLocalizer_DefaultsFallbackLang(t *testing.T) {
	l := NewLocalizer("").(*defaultLocalizer)
	if l.fallbackLang != SourceLanguage {
		t.Errorf("empty fallback should default to %q, got %q", SourceLanguage, l.fallbackLang)
	}
}

func TestTranslate_PrefersRequestedLang(t *testing.T) {
	resetBundle()
	t.Cleanup(resetBundle)

	l := NewLocalizer("en-US")

	tests := []struct {
		name string
		code string
		lang string
		want string
	}{
		{"zh-CN", "err.shared.auth.required", "zh-CN", "请先登录！"},
		{"en-US", "err.shared.auth.required", "en-US", "Please log in to continue."},
		{"rate.limited zh", "err.shared.rate.limited", "zh-CN", "请求过于频繁，请稍后再试。"},
		{"rate.limited en", "err.shared.rate.limited", "en-US", "Too many requests, please try again later."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := l.Translate(tt.code, tt.lang, nil)
			if got != tt.want {
				t.Errorf("Translate(%q, %q) = %q, want %q", tt.code, tt.lang, got, tt.want)
			}
		})
	}
}

// TestTranslate_UnknownLangFallsBack: 未知 lang 走 fallback (en-US source)
func TestTranslate_UnknownLangFallsBack(t *testing.T) {
	resetBundle()
	t.Cleanup(resetBundle)

	l := NewLocalizer("en-US")
	got := l.Translate("err.shared.auth.required", "fr-FR", nil)
	want := "Please log in to continue."
	if got != want {
		t.Errorf("unknown lang fallback = %q, want %q", got, want)
	}
}

// TestTranslate_EmptyLangFallsBack: 空 lang → 直接走 fallback chain。
func TestTranslate_EmptyLangFallsBack(t *testing.T) {
	resetBundle()
	t.Cleanup(resetBundle)

	l := NewLocalizer("zh-CN")
	got := l.Translate("err.shared.auth.required", "", nil)
	want := "请先登录！"
	if got != want {
		t.Errorf("empty lang with zh-CN fallback = %q, want %q", got, want)
	}
}

// TestTranslate_UnknownCodeReturnsID: 未注册 code → 返回 code ID 本身
// （让调用方可记 i18n_unknown_code_total）。
func TestTranslate_UnknownCodeReturnsID(t *testing.T) {
	resetBundle()
	t.Cleanup(resetBundle)

	l := NewLocalizer("en-US")
	got := l.Translate("err.shared.does_not_exist", "zh-CN", nil)
	if got != "err.shared.does_not_exist" {
		t.Errorf("unknown code = %q, want code id itself", got)
	}
}

// TestFallbackMessage_LangPrecedence: 直接测兜底链路（绕过 bundle）。
// 注册一个 bundle 未含的 code，验证 DefaultMessages[lang] → [fallback] → DefaultMessage。
//
// 注：Register 在 -count>1 时会 panic（重复 ID），故先 Lookup 跳过已注册情形。
// codes 包未暴露 testing-only 的 Unregister，跨包测试只能这样规避。
func TestFallbackMessage_LangPrecedence(t *testing.T) {
	if _, ok := codes.Lookup("err.shared.fb.test"); !ok {
		codes.Register(codes.Code{
			ID:             "err.shared.fb.test",
			HTTPStatus:     400,
			DefaultMessage: "default en source",
			DefaultMessages: map[string]string{
				"zh-CN": "中文兜底",
				"ja-JP": "日本語",
			},
		})
	}

	tests := []struct {
		name     string
		lang     string
		fallback string
		want     string
	}{
		{"exact zh", "zh-CN", "en-US", "中文兜底"},
		{"exact ja", "ja-JP", "en-US", "日本語"},
		{"fallback hits zh", "fr-FR", "zh-CN", "中文兜底"},
		{"no match → source", "fr-FR", "en-US", "default en source"},
		{"lang and fallback both miss", "ko-KR", "fr-FR", "default en source"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fallbackMessage("err.shared.fb.test", tt.lang, tt.fallback)
			if got != tt.want {
				t.Errorf("fallbackMessage(%q, fb=%q) = %q, want %q",
					tt.lang, tt.fallback, got, tt.want)
			}
		})
	}
}

func TestFallbackMessage_UnknownCode(t *testing.T) {
	got := fallbackMessage("err.does.not.exist", "zh-CN", "en-US")
	if got != "err.does.not.exist" {
		t.Errorf("unknown code = %q, want code id", got)
	}
}

func TestBuildLangTags(t *testing.T) {
	tests := []struct {
		name      string
		requested string
		fallback  string
		want      []string
	}{
		{"all different", "zh-CN", "ja-JP", []string{"zh-CN", "ja-JP", "en-US"}},
		{"requested == source", "en-US", "zh-CN", []string{"en-US", "zh-CN"}},
		{"fallback == source", "zh-CN", "en-US", []string{"zh-CN", "en-US"}},
		{"all same", "en-US", "en-US", []string{"en-US"}},
		{"empty requested", "", "zh-CN", []string{"zh-CN", "en-US"}},
		{"empty fallback", "zh-CN", "", []string{"zh-CN", "en-US"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildLangTags(tt.requested, tt.fallback)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildLangTags(%q, %q) = %v, want %v",
					tt.requested, tt.fallback, got, tt.want)
			}
		})
	}
}

