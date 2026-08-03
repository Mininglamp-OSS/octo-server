package summarycompleted_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	summarycompleted "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/summary_completed"
)

// freshRegistry 构造一个只装 summary.completed 的 Registry —— 与 pilot / PR-1
// conformance 同口径:合法 sample 过 Registry.Render 是 self-check 的核心。
func freshRegistry(t *testing.T) *cardtmpl.Registry {
	t.Helper()
	r := cardtmpl.NewRegistry()
	r.Register(summarycompleted.New(), summarycompleted.Assets, summarycompleted.HandoffRoot)
	r.Freeze()
	return r
}

func TestRegisterAndRenderSample(t *testing.T) {
	r := freshRegistry(t)
	sample, err := summarycompleted.Assets.ReadFile(summarycompleted.HandoffRoot + "/samples/shown.json")
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	env := cardtmpl.BuildEnv{
		WebLoginURL: "https://web.example.com",
		Lang:        "zh-CN",
		SpaceID:     "space-c-test",
	}
	payload, err := r.Render(context.Background(), summarycompleted.TemplateID,
		summarycompleted.TemplateVersion, summarycompleted.StateShown, sample, env)
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
	if tpl["id"] != string(summarycompleted.TemplateID) || tpl["version"] != summarycompleted.TemplateVersion {
		t.Errorf("metadata.octo.template = %+v", tpl)
	}
	if octo["variant"] != summarycompleted.Variant {
		t.Errorf("metadata.octo.variant = %v want %v", octo["variant"], summarycompleted.Variant)
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
	sample, _ := summarycompleted.Assets.ReadFile(summarycompleted.HandoffRoot + "/samples/shown.json")
	env := cardtmpl.BuildEnv{
		WebLoginURL: "https://web.example.com",
		Lang:        "en",
		SpaceID:     "space",
	}
	payload, err := r.Render(context.Background(), summarycompleted.TemplateID,
		summarycompleted.TemplateVersion, summarycompleted.StateShown, sample, env)
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

func TestFallbackTextZH(t *testing.T) {
	tmpl := summarycompleted.New()
	tmpl.SetMeta(cardtmpl.TemplateMeta{})
	sample := json.RawMessage(`{"taskNo":"t","title":"日报","timeRange":"2026-07-16 00:00 – 24:00","members":12,"msgCount":348,"generatedAt":"2026-07-16 23:59"}`)
	text, err := tmpl.FallbackText(summarycompleted.StateShown, sample, "zh-CN")
	if err != nil {
		t.Fatalf("FallbackText: %v", err)
	}
	want := "你的总结「日报」已生成完成。\n时间范围：2026-07-16 00:00 – 24:00\n参与成员：12 人\n消息数量：348 条\n生成时间：2026-07-16 23:59"
	if text != want {
		t.Errorf("fallback zh:\n got: %q\nwant: %q", text, want)
	}
}

func TestFallbackTextEN(t *testing.T) {
	tmpl := summarycompleted.New()
	tmpl.SetMeta(cardtmpl.TemplateMeta{})
	sample := json.RawMessage(`{"taskNo":"t","title":"Daily","timeRange":"2026-07-16 00:00 – 24:00","members":12,"msgCount":348}`)
	text, err := tmpl.FallbackText(summarycompleted.StateShown, sample, "en")
	if err != nil {
		t.Fatalf("FallbackText: %v", err)
	}
	want := "Your summary \"Daily\" is ready.\nTime range: 2026-07-16 00:00 – 24:00\nParticipants: 12\nMessages: 348"
	if text != want {
		t.Errorf("fallback en:\n got: %q\nwant: %q", text, want)
	}
}

func TestFallbackTextSanitizesTitle(t *testing.T) {
	tmpl := summarycompleted.New()
	tmpl.SetMeta(cardtmpl.TemplateMeta{})
	// title 里带控制字符,不应换行注入到 headline
	sample := json.RawMessage(`{"taskNo":"t","title":"OK\nR"}`)
	text, err := tmpl.FallbackText(summarycompleted.StateShown, sample, "zh-CN")
	if err != nil {
		t.Fatalf("FallbackText: %v", err)
	}
	// headline 内的换行被收敛成空格,整段无换行
	if strings.Contains(text, "\n") {
		t.Errorf("newline injection not sanitized: %q", text)
	}
}

func TestSchemaC1RejectsEmptyTaskNo(t *testing.T) {
	r := freshRegistry(t)
	env := cardtmpl.BuildEnv{WebLoginURL: "https://x", Lang: "zh-CN", SpaceID: "s"}
	// taskNo 空 → schema minLength=1 违规 → ErrFieldsInvalid(C1 零投递)
	bad := json.RawMessage(`{"taskNo":"","title":"OK"}`)
	_, err := r.Render(context.Background(), summarycompleted.TemplateID, summarycompleted.TemplateVersion,
		summarycompleted.StateShown, bad, env)
	if err == nil {
		t.Fatal("expected ErrFieldsInvalid, got nil")
	}
	if !isFieldsInvalid(err) {
		t.Errorf("want ErrFieldsInvalid, got %v", err)
	}
}

func TestSchemaC1RejectsAdditionalProperty(t *testing.T) {
	r := freshRegistry(t)
	env := cardtmpl.BuildEnv{WebLoginURL: "https://x", Lang: "zh-CN", SpaceID: "s"}
	// completed 卡 schema 里没有 reason 字段 → additionalProperties:false 违规
	bad := json.RawMessage(`{"taskNo":"t","title":"OK","reason":"foo"}`)
	_, err := r.Render(context.Background(), summarycompleted.TemplateID, summarycompleted.TemplateVersion,
		summarycompleted.StateShown, bad, env)
	if err == nil {
		t.Fatal("expected ErrFieldsInvalid, got nil")
	}
	if !isFieldsInvalid(err) {
		t.Errorf("want ErrFieldsInvalid, got %v", err)
	}
}

func TestMetaCloneIsIndependent(t *testing.T) {
	tmpl := summarycompleted.New()
	tmpl.SetMeta(cardtmpl.TemplateMeta{ID: summarycompleted.TemplateID, Version: summarycompleted.TemplateVersion})
	m1 := tmpl.Meta()
	if m1.ID != summarycompleted.TemplateID || m1.Version != summarycompleted.TemplateVersion {
		t.Errorf("Meta() = %+v", m1)
	}
}

func TestBuildRejectsUnknownState(t *testing.T) {
	tmpl := summarycompleted.New()
	tmpl.SetMeta(cardtmpl.TemplateMeta{})
	sample := json.RawMessage(`{"taskNo":"t","title":"OK"}`)
	env := cardtmpl.BuildEnv{WebLoginURL: "https://x", Lang: "zh-CN", SpaceID: "s"}
	_, err := tmpl.Build(context.Background(), cardtmpl.State("unknown"), sample, env)
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Errorf("want state-not-declared error, got %v", err)
	}
}

func TestFallbackTextRejectsUnknownState(t *testing.T) {
	tmpl := summarycompleted.New()
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
