package summaryfailed_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	summaryfailed "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/summary_failed"
)

func freshRegistry(t *testing.T) *cardtmpl.Registry {
	t.Helper()
	r := cardtmpl.NewRegistry()
	r.Register(summaryfailed.New(), summaryfailed.Assets, summaryfailed.HandoffRoot)
	r.Freeze()
	return r
}

func TestRegisterAndRenderSample(t *testing.T) {
	r := freshRegistry(t)
	sample, err := summaryfailed.Assets.ReadFile(summaryfailed.HandoffRoot + "/samples/shown.json")
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	env := cardtmpl.BuildEnv{
		WebLoginURL: "https://web.example.com",
		Lang:        "zh-CN",
		SpaceID:     "space-c-test",
	}
	payload, err := r.Render(context.Background(), summaryfailed.TemplateID,
		summaryfailed.TemplateVersion, summaryfailed.StateShown, sample, env)
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
	if tpl["id"] != string(summaryfailed.TemplateID) || tpl["version"] != summaryfailed.TemplateVersion {
		t.Errorf("metadata.octo.template = %+v", tpl)
	}
	if octo["variant"] != summaryfailed.Variant {
		t.Errorf("metadata.octo.variant = %v want %v", octo["variant"], summaryfailed.Variant)
	}
	src, _ := octo["source"].(map[string]any)
	if src["label"] != "智能总结" {
		t.Errorf("zh source.label = %v", src["label"])
	}
	if webURL, _ := meta["webUrl"].(string); !strings.HasPrefix(webURL, "https://web.example.com/s/sum_2026-07-16-daily") {
		t.Errorf("metadata.webUrl = %v", webURL)
	}
	if payload["profile"] != "octo/v1" {
		t.Errorf("wire profile = %v want octo/v1", payload["profile"])
	}
	if payload["render_profile"] != cardmsg.RenderProfileOctoChatV1 {
		t.Errorf("render profile = %v want %s", payload["render_profile"], cardmsg.RenderProfileOctoChatV1)
	}
}

func TestRenderEnglishSourceLocalized(t *testing.T) {
	r := freshRegistry(t)
	sample, _ := summaryfailed.Assets.ReadFile(summaryfailed.HandoffRoot + "/samples/shown.json")
	env := cardtmpl.BuildEnv{
		WebLoginURL: "https://web.example.com",
		Lang:        "en",
		SpaceID:     "space",
	}
	payload, err := r.Render(context.Background(), summaryfailed.TemplateID,
		summaryfailed.TemplateVersion, summaryfailed.StateShown, sample, env)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	card := payload["card"].(map[string]any)
	octo := card["metadata"].(map[string]any)["octo"].(map[string]any)
	src := octo["source"].(map[string]any)
	if src["label"] != "Smart Summary" {
		t.Errorf("en source.label = %v want Smart Summary", src["label"])
	}
}

func TestFallbackTextZHWithReason(t *testing.T) {
	tmpl := summaryfailed.New()
	tmpl.SetMeta(cardtmpl.TemplateMeta{})
	sample := json.RawMessage(`{"taskNo":"t","title":"日报","reason":"上游模型 429"}`)
	text, err := tmpl.FallbackText(summaryfailed.StateShown, sample, "zh-CN")
	if err != nil {
		t.Fatalf("FallbackText: %v", err)
	}
	want := "你的总结「日报」生成失败。\n失败原因：上游模型 429"
	if text != want {
		t.Errorf("fallback zh:\n got: %q\nwant: %q", text, want)
	}
}

func TestFallbackTextENNoReason(t *testing.T) {
	tmpl := summaryfailed.New()
	tmpl.SetMeta(cardtmpl.TemplateMeta{})
	sample := json.RawMessage(`{"taskNo":"t","title":"Daily"}`)
	text, err := tmpl.FallbackText(summaryfailed.StateShown, sample, "en")
	if err != nil {
		t.Fatalf("FallbackText: %v", err)
	}
	want := "Your summary \"Daily\" failed to generate."
	if text != want {
		t.Errorf("fallback en:\n got: %q\nwant: %q", text, want)
	}
}

func TestFallbackTextSanitizesReason(t *testing.T) {
	tmpl := summaryfailed.New()
	tmpl.SetMeta(cardtmpl.TemplateMeta{})
	// reason 里带换行,不应注入到 fallback 文本的行结构
	sample := json.RawMessage(`{"taskNo":"t","title":"日报","reason":"line1\nline2"}`)
	text, err := tmpl.FallbackText(summaryfailed.StateShown, sample, "zh-CN")
	if err != nil {
		t.Fatalf("FallbackText: %v", err)
	}
	// headline (1 换行) + reason 行 (1 换行) = 恰好 1 个换行,reason 内部被收敛为空格
	if strings.Count(text, "\n") != 1 {
		t.Errorf("newline injection not sanitized: %q", text)
	}
	if !strings.Contains(text, "失败原因：line1 line2") {
		t.Errorf("reason not sanitized to space: %q", text)
	}
}

func TestSchemaC1RejectsEmptyTaskNo(t *testing.T) {
	r := freshRegistry(t)
	env := cardtmpl.BuildEnv{WebLoginURL: "https://x", Lang: "zh-CN", SpaceID: "s"}
	// taskNo 空 → schema minLength=1 违规 → ErrFieldsInvalid(C1 零投递)
	bad := json.RawMessage(`{"taskNo":"","title":"OK"}`)
	_, err := r.Render(context.Background(), summaryfailed.TemplateID, summaryfailed.TemplateVersion,
		summaryfailed.StateShown, bad, env)
	if err == nil {
		t.Fatal("expected ErrFieldsInvalid, got nil")
	}
	if !isFieldsInvalid(err) {
		t.Errorf("want ErrFieldsInvalid, got %v", err)
	}
}

func TestSchemaC1RejectsReasonTooLong(t *testing.T) {
	r := freshRegistry(t)
	env := cardtmpl.BuildEnv{WebLoginURL: "https://x", Lang: "zh-CN", SpaceID: "s"}
	// reason 超过 300 rune (schema maxLength) → ErrFieldsInvalid;
	// G9 单一真源:cap 与 schema.maxLength 对齐
	longReason := strings.Repeat("a", 301)
	bad := json.RawMessage(`{"taskNo":"t","title":"OK","reason":"` + longReason + `"}`)
	_, err := r.Render(context.Background(), summaryfailed.TemplateID, summaryfailed.TemplateVersion,
		summaryfailed.StateShown, bad, env)
	if err == nil {
		t.Fatal("expected ErrFieldsInvalid for oversize reason, got nil")
	}
	if !isFieldsInvalid(err) {
		t.Errorf("want ErrFieldsInvalid, got %v", err)
	}
}

func TestMetaCloneIsIndependent(t *testing.T) {
	tmpl := summaryfailed.New()
	tmpl.SetMeta(cardtmpl.TemplateMeta{ID: summaryfailed.TemplateID, Version: summaryfailed.TemplateVersion})
	m1 := tmpl.Meta()
	if m1.ID != summaryfailed.TemplateID || m1.Version != summaryfailed.TemplateVersion {
		t.Errorf("Meta() = %+v", m1)
	}
}

func TestBuildRejectsUnknownState(t *testing.T) {
	tmpl := summaryfailed.New()
	tmpl.SetMeta(cardtmpl.TemplateMeta{})
	sample := json.RawMessage(`{"taskNo":"t","title":"OK"}`)
	env := cardtmpl.BuildEnv{WebLoginURL: "https://x", Lang: "zh-CN", SpaceID: "s"}
	_, err := tmpl.Build(context.Background(), cardtmpl.State("unknown"), sample, env)
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Errorf("want state-not-declared error, got %v", err)
	}
}

func TestBuildFacts(t *testing.T) {
	// 补 Build 的 facts 分支覆盖:failed 卡也允许携带 timeRange/members/msgCount/generatedAt
	tmpl := summaryfailed.New()
	tmpl.SetMeta(cardtmpl.TemplateMeta{})
	sample := json.RawMessage(`{"taskNo":"t","title":"日报","reason":"上游 429","timeRange":"2026-07-16 00:00 – 24:00","members":5,"msgCount":128,"generatedAt":"2026-07-16 12:00"}`)
	env := cardtmpl.BuildEnv{WebLoginURL: "https://web.example.com", Lang: "zh-CN", SpaceID: "s"}
	res, err := tmpl.Build(context.Background(), summaryfailed.StateShown, sample, env)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Variant != summaryfailed.Variant {
		t.Errorf("variant = %v", res.Variant)
	}
	if res.DeepLink == "" || !strings.HasPrefix(res.DeepLink, "https://") {
		t.Errorf("DeepLink = %q", res.DeepLink)
	}
}

func TestFallbackTextRejectsUnknownState(t *testing.T) {
	tmpl := summaryfailed.New()
	tmpl.SetMeta(cardtmpl.TemplateMeta{})
	sample := json.RawMessage(`{"taskNo":"t","title":"OK"}`)
	_, err := tmpl.FallbackText(cardtmpl.State("unknown"), sample, "zh-CN")
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Errorf("want state-not-declared error, got %v", err)
	}
}

func isFieldsInvalid(err error) bool {
	return err != nil && strings.Contains(err.Error(), "fields did not pass input schema")
}
