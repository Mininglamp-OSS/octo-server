package i18n

import (
	"testing"

	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
)

func TestBundle_LoadsSuccessfully(t *testing.T) {
	resetBundle()
	t.Cleanup(resetBundle)

	b, err := Bundle()
	if err != nil {
		t.Fatalf("Bundle() err = %v", err)
	}
	if b == nil {
		t.Fatal("Bundle() returned nil")
	}
}

func TestBundle_IsSingleton(t *testing.T) {
	resetBundle()
	t.Cleanup(resetBundle)

	b1, err := Bundle()
	if err != nil {
		t.Fatalf("Bundle() err = %v", err)
	}
	b2, err := Bundle()
	if err != nil {
		t.Fatalf("Bundle() err = %v", err)
	}
	if b1 != b2 {
		t.Fatal("Bundle() returned different instances; expected singleton")
	}
}

// TestBundle_InjectsSourceFromCodes 验证 bundle 初始化把 codes.Register
// 的 DefaultMessage 注入为 source 语言消息——即使 active.en-US.toml 为空
// 也能解析出 source 文案。
func TestBundle_InjectsSourceFromCodes(t *testing.T) {
	resetBundle()
	t.Cleanup(resetBundle)

	b, err := Bundle()
	if err != nil {
		t.Fatalf("Bundle() err = %v", err)
	}

	loc := i18n.NewLocalizer(b, SourceLanguage)
	got, err := loc.Localize(&i18n.LocalizeConfig{
		MessageID: "err.shared.auth.required",
	})
	if err != nil {
		t.Fatalf("Localize(en-US) err = %v", err)
	}
	want := "Please log in to continue."
	if got != want {
		t.Errorf("Localize(en-US, err.shared.auth.required) = %q, want %q", got, want)
	}
}

// TestBundle_LoadsZhTOML 验证 translate.zh-CN.toml 被加载且能渲染 zh 文案。
func TestBundle_LoadsZhTOML(t *testing.T) {
	resetBundle()
	t.Cleanup(resetBundle)

	b, err := Bundle()
	if err != nil {
		t.Fatalf("Bundle() err = %v", err)
	}

	loc := i18n.NewLocalizer(b, "zh-CN")
	got, err := loc.Localize(&i18n.LocalizeConfig{
		MessageID: "err.shared.auth.required",
	})
	if err != nil {
		t.Fatalf("Localize(zh-CN) err = %v", err)
	}
	want := "请先登录！"
	if got != want {
		t.Errorf("Localize(zh-CN) = %q, want %q", got, want)
	}
}

// TestBundle_TagsParse 防御性测试：所有 SDK 用到的 lang 标签必须是合法 BCP-47，
// 否则 language.MustParse 会 panic。
func TestBundle_TagsParse(t *testing.T) {
	for _, tag := range []string{SourceLanguage, "zh-CN", "en-US"} {
		if _, err := language.Parse(tag); err != nil {
			t.Errorf("Parse(%q) err = %v", tag, err)
		}
	}
}
