package docscommented_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docscommented "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_commented"
)

// freshRegistry 构造一个只装 docs.commented 的 Registry — 与 pilot conformance
// 同口径:合法 sample 过 Registry.Render 是 self-check 的核心。
func freshRegistry(t *testing.T) *cardtmpl.Registry {
	t.Helper()
	r := cardtmpl.NewRegistry()
	r.Register(docscommented.New(), docscommented.Assets, docscommented.HandoffRoot)
	r.Freeze()
	return r
}

func TestRegisterAndRenderSample(t *testing.T) {
	r := freshRegistry(t)
	sample, err := docscommented.Assets.ReadFile(docscommented.HandoffRoot + "/samples/shown.json")
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	env := cardtmpl.BuildEnv{
		WebLoginURL: "https://web.example.com",
		Lang:        "zh-CN",
		SpaceID:     "space-c-test",
	}
	payload, err := r.Render(context.Background(), docscommented.TemplateID,
		docscommented.TemplateVersion, docscommented.StateShown, sample, env)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	card, _ := payload["card"].(map[string]any)
	if card == nil {
		t.Fatalf("card node missing: %+v", payload)
	}
	meta, _ := card["metadata"].(map[string]any)
	octo, _ := meta["octo"].(map[string]any)
	if octo["protocol"] != "octo-card@1.0" {
		t.Errorf("metadata.octo.protocol = %v", octo["protocol"])
	}
	tpl, _ := octo["template"].(map[string]any)
	if tpl["id"] != string(docscommented.TemplateID) || tpl["version"] != docscommented.TemplateVersion {
		t.Errorf("metadata.octo.template = %+v", tpl)
	}
	if octo["variant"] != docscommented.Variant {
		t.Errorf("metadata.octo.variant = %v want %v", octo["variant"], docscommented.Variant)
	}
	src, _ := octo["source"].(map[string]any)
	if src["label"] != "文档" {
		t.Errorf("zh source.label = %v", src["label"])
	}
	if webURL, _ := meta["webUrl"].(string); !strings.HasPrefix(webURL, "https://web.example.com/d/d_2026-q3-okr") {
		t.Errorf("metadata.webUrl = %v", webURL)
	}
	if payload["profile"] != "octo/v1" {
		t.Errorf("wire profile = %v want octo/v1", payload["profile"])
	}
}

func TestRenderEnglishSourceLocalized(t *testing.T) {
	r := freshRegistry(t)
	sample, _ := docscommented.Assets.ReadFile(docscommented.HandoffRoot + "/samples/shown.json")
	env := cardtmpl.BuildEnv{
		WebLoginURL: "https://web.example.com",
		Lang:        "en",
		SpaceID:     "space",
	}
	payload, err := r.Render(context.Background(), docscommented.TemplateID,
		docscommented.TemplateVersion, docscommented.StateShown, sample, env)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	card := payload["card"].(map[string]any)
	octo := card["metadata"].(map[string]any)["octo"].(map[string]any)
	src := octo["source"].(map[string]any)
	if src["label"] != "Docs" {
		t.Errorf("en source.label = %v want Docs", src["label"])
	}
}

func TestFallbackTextZH(t *testing.T) {
	tmpl := docscommented.New()
	tmpl.SetMeta(cardtmpl.TemplateMeta{})
	sample := json.RawMessage(`{"docId":"d","title":"OKR","actorName":"张三","excerpt":"补数据","updatedAt":"2026-07-16 11:00"}`)
	text, err := tmpl.FallbackText(docscommented.StateShown, sample, "zh-CN")
	if err != nil {
		t.Fatalf("FallbackText: %v", err)
	}
	want := "张三 评论了文档\n文档：OKR\n补数据\n时间：2026-07-16 11:00"
	if text != want {
		t.Errorf("fallback zh:\n got: %q\nwant: %q", text, want)
	}
}

func TestFallbackTextAnonAndSanitize(t *testing.T) {
	tmpl := docscommented.New()
	tmpl.SetMeta(cardtmpl.TemplateMeta{})
	// actorName 空 + title 里带控制字符(不应换行注入)
	sample := json.RawMessage(`{"docId":"d","title":"OK\nR","actorName":"","excerpt":"","updatedAt":""}`)
	text, err := tmpl.FallbackText(docscommented.StateShown, sample, "zh-CN")
	if err != nil {
		t.Fatalf("FallbackText: %v", err)
	}
	// 换行被 SanitizeLine 收敛成空格,不会出现在 title 里
	if strings.Count(text, "\n") != 1 { // attribution + title 各占一行,共 1 个换行
		t.Errorf("newline injection not sanitized: %q", text)
	}
	if !strings.HasPrefix(text, "有新评论") {
		t.Errorf("anon banner missing: %q", text)
	}
}

func TestSchemaC1RejectsShortTitle(t *testing.T) {
	r := freshRegistry(t)
	env := cardtmpl.BuildEnv{WebLoginURL: "https://x", Lang: "zh-CN", SpaceID: "s"}
	// title 空 → schema minLength=1 违规 → ErrFieldsInvalid(C1 零投递)
	bad := json.RawMessage(`{"docId":"d","title":""}`)
	_, err := r.Render(context.Background(), docscommented.TemplateID, docscommented.TemplateVersion,
		docscommented.StateShown, bad, env)
	if err == nil {
		t.Fatal("expected ErrFieldsInvalid, got nil")
	}
	// 与 pilot 同错误分类
	if !isFieldsInvalid(err) {
		t.Errorf("want ErrFieldsInvalid, got %v", err)
	}
}

func isFieldsInvalid(err error) bool {
	return err != nil && strings.Contains(err.Error(), "fields did not pass input schema")
}
