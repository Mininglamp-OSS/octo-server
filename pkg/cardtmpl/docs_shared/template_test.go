package docsshared_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docsshared "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_shared"
)

func freshRegistry(t *testing.T) *cardtmpl.Registry {
	t.Helper()
	r := cardtmpl.NewRegistry()
	r.Register(docsshared.New(), docsshared.Assets, docsshared.HandoffRoot)
	r.Freeze()
	return r
}

func TestRegisterAndRenderSample(t *testing.T) {
	r := freshRegistry(t)
	sample, err := docsshared.Assets.ReadFile(docsshared.HandoffRoot + "/samples/shown.json")
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	env := cardtmpl.BuildEnv{WebLoginURL: "https://web.example.com", Lang: "zh-CN", SpaceID: "space"}
	payload, err := r.Render(context.Background(), docsshared.TemplateID,
		docsshared.TemplateVersion, docsshared.StateShown, sample, env)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	card := payload["card"].(map[string]any)
	octo := card["metadata"].(map[string]any)["octo"].(map[string]any)
	if octo["variant"] != docsshared.Variant {
		t.Errorf("variant = %v", octo["variant"])
	}
	tpl := octo["template"].(map[string]any)
	if tpl["id"] != string(docsshared.TemplateID) {
		t.Errorf("template.id = %v", tpl["id"])
	}
	if payload["profile"] != "octo/v1" {
		t.Errorf("profile = %v", payload["profile"])
	}
}

func TestFallbackTextZH(t *testing.T) {
	tmpl := docsshared.New()
	tmpl.SetMeta(cardtmpl.TemplateMeta{})
	sample := json.RawMessage(`{"docId":"d","title":"OKR","actorName":"李四","excerpt":"评审用","updatedAt":"2026-07-16 10:30"}`)
	text, err := tmpl.FallbackText(docsshared.StateShown, sample, "zh-CN")
	if err != nil {
		t.Fatalf("FallbackText: %v", err)
	}
	want := "李四 分享了文档\n文档：OKR\n评审用\n时间：2026-07-16 10:30"
	if text != want {
		t.Errorf("fallback zh:\n got: %q\nwant: %q", text, want)
	}
}

func TestFallbackTextAnon(t *testing.T) {
	tmpl := docsshared.New()
	tmpl.SetMeta(cardtmpl.TemplateMeta{})
	sample := json.RawMessage(`{"docId":"d","title":"OKR","actorName":"","excerpt":"","updatedAt":""}`)
	text, err := tmpl.FallbackText(docsshared.StateShown, sample, "zh-CN")
	if err != nil {
		t.Fatalf("FallbackText: %v", err)
	}
	if !strings.HasPrefix(text, "有人分享了文档") {
		t.Errorf("anon banner missing: %q", text)
	}
}

func TestSchemaC1RejectsMissingDocID(t *testing.T) {
	r := freshRegistry(t)
	env := cardtmpl.BuildEnv{WebLoginURL: "https://x", Lang: "zh-CN", SpaceID: "s"}
	bad := json.RawMessage(`{"title":"only title"}`)
	_, err := r.Render(context.Background(), docsshared.TemplateID, docsshared.TemplateVersion,
		docsshared.StateShown, bad, env)
	if err == nil {
		t.Fatal("expected ErrFieldsInvalid")
	}
}
