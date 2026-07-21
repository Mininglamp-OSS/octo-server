package docsaccessrequest_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
)

// TestRegisterAndRender_Smoke 端到端跑一遍:
//   NewRegistry → Register(pilot) → SetDefault → Freeze → Render(pending sample)
// 断言产物含 metadata.octo.{protocol,template} + webUrl,且 body/actions 非空。
func TestRegisterAndRender_Smoke(t *testing.T) {
	r := cardtmpl.NewRegistry()
	tmpl := docsaccessrequest.New()
	r.Register(tmpl, docsaccessrequest.Assets, docsaccessrequest.HandoffRoot)
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersion)
	r.Freeze()

	// 加载 pending sample 直接作 fields
	sample, err := docsaccessrequest.Assets.ReadFile(
		docsaccessrequest.HandoffRoot + "/samples/pending.json",
	)
	if err != nil {
		t.Fatalf("read pending sample: %v", err)
	}

	env := cardtmpl.BuildEnv{
		WebLoginURL: "https://web.example.com",
		Lang:        "zh-CN",
		SpaceID:     "space-1",
	}
	payload, err := r.Render(context.Background(),
		docsaccessrequest.TemplateID, "",
		docsaccessrequest.StatePending, sample, env)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// envelope
	if got, ok := payload["type"].(int); !ok || got != 17 {
		t.Fatalf("type != 17: %v (%T)", payload["type"], payload["type"])
	}
	if got, _ := payload["profile"].(string); got != "octo/v2" {
		t.Fatalf("profile != octo/v2: %v", payload["profile"])
	}

	// card + metadata
	card, ok := payload["card"].(map[string]any)
	if !ok {
		t.Fatalf("card not a map: %T", payload["card"])
	}
	metadata, _ := card["metadata"].(map[string]any)
	if metadata == nil {
		t.Fatalf("card.metadata missing")
	}
	webURL, _ := metadata["webUrl"].(string)
	if !strings.HasPrefix(webURL, "https://web.example.com/d/") {
		t.Fatalf("webUrl not on WebLoginURL host: %q", webURL)
	}
	octo, _ := metadata["octo"].(map[string]any)
	if octo == nil {
		t.Fatalf("metadata.octo missing")
	}
	if got, _ := octo["protocol"].(string); got != cardtmpl.Protocol {
		t.Fatalf("metadata.octo.protocol != %q: %v", cardtmpl.Protocol, got)
	}
	tplMeta, _ := octo["template"].(map[string]any)
	if tplMeta == nil {
		t.Fatalf("metadata.octo.template missing")
	}
	if got, _ := tplMeta["id"].(string); got != string(docsaccessrequest.TemplateID) {
		t.Fatalf("template.id: %v", got)
	}
	if got, _ := tplMeta["version"].(string); got != docsaccessrequest.TemplateVersion {
		t.Fatalf("template.version: %v", got)
	}

	// body & actions non-empty
	body, _ := card["body"].([]any)
	if len(body) == 0 {
		t.Fatalf("body empty")
	}
	actions, _ := card["actions"].([]any)
	if len(actions) == 0 {
		t.Fatalf("actions empty")
	}
	// re-marshal 保序 (调试用)
	raw, _ := json.MarshalIndent(payload, "", "  ")
	t.Logf("payload size = %d bytes", len(raw))
}
