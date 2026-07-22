package message

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
)

func TestResolveRegistryCardContextUsesEffectiveMetadataAndReport(t *testing.T) {
	registry := cardtmpl.NewRegistry()
	registry.Register(docsaccessrequest.New(), docsaccessrequest.Assets, docsaccessrequest.HandoffRoot)
	registry.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersion)
	registry.Freeze()
	cardtmpl.SetDefaultRegistry(registry)
	t.Cleanup(func() { cardtmpl.SetDefaultRegistry(nil) })
	sample, err := docsaccessrequest.Assets.ReadFile(docsaccessrequest.HandoffRoot + "/samples/pending.json")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := registry.Render(context.Background(), docsaccessrequest.TemplateID, "",
		docsaccessrequest.StatePending, sample,
		cardtmpl.BuildEnv{WebLoginURL: "https://web.example.com", Lang: "en", SpaceID: "space-1"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(payload)
	got, err := resolveRegistryCardContext(raw, cardtmpl.DocsApproveActionID)
	if err != nil {
		t.Fatalf("resolveRegistryCardContext: %v", err)
	}
	want := cardactiondispatch.CardContext{TemplateID: "docs.access-request", TemplateVersion: "0.2.0", View: "pending"}
	if got != want {
		t.Fatalf("card context = %+v, want %+v", got, want)
	}
	if _, err := resolveRegistryCardContext(raw, "missing"); err == nil {
		t.Fatal("undeclared action resolved without error")
	}
	card := payload["card"].(map[string]any)
	metadata := card["metadata"].(map[string]any)
	octo := metadata["octo"].(map[string]any)
	template := octo["template"].(map[string]any)
	delete(template, "version")
	partialRaw, _ := json.Marshal(payload)
	if _, err := resolveRegistryCardContext(partialRaw, cardtmpl.DocsApproveActionID); err == nil {
		t.Fatal("partial registry metadata resolved as a legacy card")
	}
	legacy, err := resolveRegistryCardContext([]byte(`{"type":17,"card":{"body":[]}}`), "legacy")
	if err != nil || legacy != (cardactiondispatch.CardContext{}) {
		t.Fatalf("legacy context = (%+v, %v)", legacy, err)
	}
}
