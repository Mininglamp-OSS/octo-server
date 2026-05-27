package i18n

import (
	"strings"
	"testing"

	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
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

// TestBundle_OnlyEmbedsActiveFiles 验证 embed pattern 只收 active.*.toml，
// 不会把 translate.*.toml 一起带进 runtime bundle。防止 P2 reviewer 提的
// 「字典序覆盖」陷阱回归：translate.* 进 bundle 会让客户端看到 WIP/未译文案。
func TestBundle_OnlyEmbedsActiveFiles(t *testing.T) {
	entries, err := localesFS.ReadDir("locales")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".toml") {
			continue
		}
		if !strings.HasPrefix(name, "active.") {
			t.Errorf("locales FS contains non-active file %q; "+
				"only active.*.toml should be embedded (translate.* is goi18n merge WIP)",
				name)
		}
	}
}

// TestBundle_DefaultMessagesMatchTOML 防止 codes.DefaultMessages["zh-CN"]
// 与 active.zh-CN.toml 漂移——两者必须一致，否则 bundle 失败兜底链路输出
// 与正常路径不一致，行为难以排查。
//
// 仅校验 zh-CN（首期 source 之外的唯一翻译）。后续语言扩展时按需追加。
func TestBundle_DefaultMessagesMatchTOML(t *testing.T) {
	resetBundle()
	t.Cleanup(resetBundle)

	b, err := Bundle()
	if err != nil {
		t.Fatalf("Bundle err = %v", err)
	}
	loc := i18n.NewLocalizer(b, "zh-CN")

	for _, c := range codes.All() {
		want, ok := c.DefaultMessages["zh-CN"]
		if !ok {
			continue // 该 code 未配 zh-CN 兜底，跳过
		}
		got, err := loc.Localize(&i18n.LocalizeConfig{MessageID: c.ID})
		if err != nil {
			// TOML 没有该 code 的 zh-CN 条目——属于「仅 DefaultMessages 兜底」
			// 设计，不算漂移。跳过即可。
			//
			// 注意：这一分支也覆盖了其他测试在 init 后注册的代码
			// （如 localizer_test 的 err.shared.fb.test）漏到 bundle 测试的情形。
			continue
		}
		if got != want {
			t.Errorf("%s: TOML zh-CN = %q, DefaultMessages[zh-CN] = %q (drift!)",
				c.ID, got, want)
		}
	}
}
