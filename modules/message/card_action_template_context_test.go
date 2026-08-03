package message

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
)

func TestResolveRegistryCardContextUsesEffectiveMetadataAndReport(t *testing.T) {
	registry := cardtmpl.NewRegistry()
	registry.Register(docsaccessrequest.New(), docsaccessrequest.Assets, docsaccessrequest.HandoffRoot)
	registry.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersion)
	registry.Freeze()
	cardtmpl.SetDefaultRegistry(registry)
	t.Cleanup(func() { cardtmpl.SetDefaultRegistry(nil) })
	static, err := cardtmpl.NewStaticCatalog(registry)
	if err != nil {
		t.Fatal(err)
	}
	spy := &actionContextCatalogSpy{Catalog: static}
	cardtmpl.SetDefaultCatalog(spy)
	t.Cleanup(func() { cardtmpl.SetDefaultCatalog(nil) })
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
	actionData, ok := cardmsg.SubmitAction(raw, cardtmpl.DocsApproveActionID)
	if !ok {
		t.Fatal("approve action not found in rendered card")
	}
	access := cardtmpl.CatalogAccess{
		Purpose: cardtmpl.CatalogPurposeActionContext,
		Principal: cardtmpl.CatalogPrincipal{
			Kind: cardtmpl.CatalogPrincipalBot, ID: "notification", SpaceID: "space-1",
		},
	}
	got, err := resolveRegistryCardContext(context.Background(), access, raw, cardtmpl.DocsApproveActionID, actionData)
	if err != nil {
		t.Fatalf("resolveRegistryCardContext: %v", err)
	}
	want := cardactiondispatch.CardContext{TemplateID: "docs.access-request", TemplateVersion: "0.2.0", View: "pending"}
	if got != want {
		t.Fatalf("card context = %+v, want %+v", got, want)
	}
	if spy.lastRequest.Access != access {
		t.Fatalf("action catalog access = %+v, want %+v", spy.lastRequest.Access, access)
	}
	if _, err := resolveRegistryCardContext(context.Background(), access, raw, "missing", nil); err == nil {
		t.Fatal("undeclared action resolved without error")
	}

	// P2-b(PR#641 review):route owner(Action.data.owner)与 template owner 不一致
	// 必须拒绝 —— 防止带 docs 身份的信封投递到别的路由。
	if _, err := resolveRegistryCardContext(context.Background(), access, raw, cardtmpl.DocsApproveActionID,
		map[string]interface{}{"owner": "summary", "action_type": "access_request.decision"}); err == nil {
		t.Fatal("owner mismatch resolved without error")
	}

	// partial metadata(缺 version)→ 不能退化成 legacy。
	card := payload["card"].(map[string]any)
	metadata := card["metadata"].(map[string]any)
	octo := metadata["octo"].(map[string]any)
	template := octo["template"].(map[string]any)
	delete(template, "version")
	partialRaw, _ := json.Marshal(payload)
	if _, err := resolveRegistryCardContext(context.Background(), access, partialRaw, cardtmpl.DocsApproveActionID, actionData); err == nil {
		t.Fatal("partial registry metadata resolved as a legacy card")
	}

	// P2-a(PR#641 review):metadata.octo 是损坏的非对象 → fail closed,不当 legacy。
	corrupt := map[string]any{"type": 17, "card": map[string]any{
		"body":     []any{},
		"metadata": map[string]any{"octo": "corrupt"},
	}}
	corruptRaw, _ := json.Marshal(corrupt)
	if _, err := resolveRegistryCardContext(context.Background(), access, corruptRaw, "any", nil); err == nil {
		t.Fatal("corrupt octo metadata resolved as a legacy card")
	}

	legacy, err := resolveRegistryCardContext(context.Background(), access, []byte(`{"type":17,"card":{"body":[]}}`), "legacy", nil)
	if err != nil || legacy != (cardactiondispatch.CardContext{}) {
		t.Fatalf("legacy context = (%+v, %v)", legacy, err)
	}
}

func TestResolveRegistryCardContextRejectsV3LegacyControlIDs(t *testing.T) {
	registry := cardtmpl.NewRegistry()
	registry.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV3)
	registry.Freeze()
	static, err := cardtmpl.NewStaticCatalog(registry)
	if err != nil {
		t.Fatal(err)
	}
	previousCatalog := cardtmpl.DefaultCatalog()
	cardtmpl.SetDefaultCatalog(static)
	t.Cleanup(func() { cardtmpl.SetDefaultCatalog(previousCatalog) })

	access := cardtmpl.CatalogAccess{
		Purpose: cardtmpl.CatalogPurposeActionContext,
		Principal: cardtmpl.CatalogPrincipal{
			Kind: cardtmpl.CatalogPrincipalBot, ID: "bot-reasoning", SpaceID: "space-1",
		},
	}
	for _, tc := range []struct {
		state    cardtmpl.State
		actionID string
	}{
		{state: "reasoning", actionID: "reasoning_stop"},
		{state: "error", actionID: "reasoning_retry"},
	} {
		t.Run(string(tc.state)+"/"+tc.actionID, func(t *testing.T) {
			sample, err := aireasoningprocess.Assets.ReadFile(
				aireasoningprocess.HandoffRootV3 + "/samples/" + string(tc.state) + ".json")
			if err != nil {
				t.Fatal(err)
			}
			payload, err := registry.Render(context.Background(), aireasoningprocess.TemplateID,
				aireasoningprocess.TemplateVersionV3, tc.state, sample,
				cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-1"})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			_, err = resolveRegistryCardContext(context.Background(), access, raw, tc.actionID,
				map[string]interface{}{"owner": "ai", "action_type": "reasoning.control"})
			if !errors.Is(err, cardtmpl.ErrActionUnknown) {
				t.Fatalf("resolve legacy action error = %v, want ErrActionUnknown", err)
			}
		})
	}
}

type actionContextCatalogSpy struct {
	cardtmpl.Catalog
	lastRequest cardtmpl.CatalogActionRequest
}

func (s *actionContextCatalogSpy) ActionContext(
	ctx context.Context,
	request cardtmpl.CatalogActionRequest,
) (cardtmpl.CatalogActionContext, error) {
	s.lastRequest = request
	return s.Catalog.ActionContext(ctx, request)
}
