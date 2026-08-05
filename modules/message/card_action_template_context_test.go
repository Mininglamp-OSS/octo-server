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
	origin := cardActionFrameOrigin{SenderUID: "notification", SpaceID: "space-1"}
	got, err := resolveRegistryCardContext(context.Background(), origin, raw, cardtmpl.DocsApproveActionID, actionData)
	if err != nil {
		t.Fatalf("resolveRegistryCardContext: %v", err)
	}
	want := cardactiondispatch.CardContext{TemplateID: "docs.access-request", TemplateVersion: "0.2.0", View: "pending"}
	if got != want {
		t.Fatalf("card context = %+v, want %+v", got, want)
	}
	wantAccess := cardtmpl.CatalogAccess{
		Purpose: cardtmpl.CatalogPurposeActionContext,
		Principal: cardtmpl.CatalogPrincipal{
			Kind: cardtmpl.CatalogPrincipalBot, ID: "notification", SpaceID: "space-1",
		},
	}
	if spy.lastRequest.Access != wantAccess {
		t.Fatalf("action catalog access = %+v, want %+v", spy.lastRequest.Access, wantAccess)
	}
	if _, err := resolveRegistryCardContext(context.Background(), origin, raw, "missing", nil); err == nil {
		t.Fatal("undeclared action resolved without error")
	}

	// P2-b(PR#641 review):route owner(Action.data.owner)与 template owner 不一致
	// 必须拒绝 —— 防止带 docs 身份的信封投递到别的路由。
	if _, err := resolveRegistryCardContext(context.Background(), origin, raw, cardtmpl.DocsApproveActionID,
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
	if _, err := resolveRegistryCardContext(context.Background(), origin, partialRaw, cardtmpl.DocsApproveActionID, actionData); err == nil {
		t.Fatal("partial registry metadata resolved as a legacy card")
	}

	// P2-a(PR#641 review):metadata.octo 是损坏的非对象 → fail closed,不当 legacy。
	corrupt := map[string]any{"type": 17, "card": map[string]any{
		"body":     []any{},
		"metadata": map[string]any{"octo": "corrupt"},
	}}
	corruptRaw, _ := json.Marshal(corrupt)
	if _, err := resolveRegistryCardContext(context.Background(), origin, corruptRaw, "any", nil); err == nil {
		t.Fatal("corrupt octo metadata resolved as a legacy card")
	}

	legacy, err := resolveRegistryCardContext(context.Background(), origin, []byte(`{"type":17,"card":{"body":[]}}`), "legacy", nil)
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

	origin := cardActionFrameOrigin{SenderUID: "bot-reasoning", SpaceID: "space-1"}
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
			_, err = resolveRegistryCardContext(context.Background(), origin, raw, tc.actionID,
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

// ---- PR-C Slice 1 (D3): the action ingress derives the catalog principal
// from the frame's stored catalog_provenance after proving it consistent with
// the stored sender and authoritative Space — never from raw guesses. ----

func markedActionFrame(t *testing.T, registry *cardtmpl.Registry, provenance map[string]interface{}) ([]byte, map[string]interface{}) {
	t.Helper()
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
	payload["template_ref"] = map[string]interface{}{
		"id": string(docsaccessrequest.TemplateID), "version": docsaccessrequest.TemplateVersion,
	}
	if provenance != nil {
		payload["catalog_provenance"] = provenance
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	actionData, ok := cardmsg.SubmitAction(raw, cardtmpl.DocsApproveActionID)
	if !ok {
		t.Fatal("approve action not found")
	}
	return raw, actionData
}

func provenanceActionFixture(t *testing.T) (*cardtmpl.Registry, *actionContextCatalogSpy) {
	t.Helper()
	registry := cardtmpl.NewRegistry()
	registry.Register(docsaccessrequest.New(), docsaccessrequest.Assets, docsaccessrequest.HandoffRoot)
	registry.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersion)
	registry.Freeze()
	static, err := cardtmpl.NewStaticCatalog(registry)
	if err != nil {
		t.Fatal(err)
	}
	spy := &actionContextCatalogSpy{Catalog: static}
	previousCatalog := cardtmpl.DefaultCatalog()
	cardtmpl.SetDefaultCatalog(spy)
	t.Cleanup(func() { cardtmpl.SetDefaultCatalog(previousCatalog) })
	return registry, spy
}

func TestResolveRegistryCardContextUsesStoredProvenancePrincipal(t *testing.T) {
	registry, spy := provenanceActionFixture(t)
	raw, actionData := markedActionFrame(t, registry, map[string]interface{}{
		"version": 1, "principal_type": "internal_producer",
		"principal_id": "docs-notify", "space_id": "space-1",
	})
	origin := cardActionFrameOrigin{
		SenderUID: "notification", SpaceID: "space-1",
		ProducerBinding: func(producerID string) (string, bool) {
			if producerID == "docs-notify" {
				return "notification", true
			}
			return "", false
		},
	}
	got, err := resolveRegistryCardContext(context.Background(), origin, raw, cardtmpl.DocsApproveActionID, actionData)
	if err != nil {
		t.Fatalf("resolveRegistryCardContext: %v", err)
	}
	want := cardactiondispatch.CardContext{
		TemplateID: "docs.access-request", TemplateVersion: "0.2.0", View: "pending",
		PrincipalType: "internal_producer", PrincipalID: "docs-notify", SpaceID: "space-1",
	}
	if got != want {
		t.Fatalf("card context = %+v, want %+v", got, want)
	}
	wantPrincipal := cardtmpl.CatalogPrincipal{
		Kind: cardtmpl.CatalogPrincipalInternalProducer, ID: "docs-notify", SpaceID: "space-1",
	}
	if spy.lastRequest.Access.Principal != wantPrincipal {
		t.Fatalf("catalog access principal = %+v, want %+v", spy.lastRequest.Access.Principal, wantPrincipal)
	}
	if spy.lastRequest.Access.Purpose != cardtmpl.CatalogPurposeActionContext {
		t.Fatalf("catalog access purpose = %v", spy.lastRequest.Access.Purpose)
	}
}

func TestResolveRegistryCardContextRejectsInconsistentProvenance(t *testing.T) {
	registry, _ := provenanceActionFixture(t)
	binding := func(producerID string) (string, bool) {
		if producerID == "docs-notify" {
			return "notification", true
		}
		return "", false
	}
	cases := []struct {
		name       string
		provenance map[string]interface{}
		origin     cardActionFrameOrigin
	}{
		{
			name: "producer binding does not match stored sender",
			provenance: map[string]interface{}{
				"version": 1, "principal_type": "internal_producer",
				"principal_id": "docs-notify", "space_id": "space-1",
			},
			origin: cardActionFrameOrigin{SenderUID: "other-bot", SpaceID: "space-1", ProducerBinding: binding},
		},
		{
			name: "unregistered producer",
			provenance: map[string]interface{}{
				"version": 1, "principal_type": "internal_producer",
				"principal_id": "rogue-producer", "space_id": "space-1",
			},
			origin: cardActionFrameOrigin{SenderUID: "notification", SpaceID: "space-1", ProducerBinding: binding},
		},
		{
			name: "bot provenance names a different bot",
			provenance: map[string]interface{}{
				"version": 1, "principal_type": "bot",
				"principal_id": "bot-a", "space_id": "space-1",
			},
			origin: cardActionFrameOrigin{SenderUID: "bot-b", SpaceID: "space-1", ProducerBinding: binding},
		},
		{
			name: "cross-space provenance",
			provenance: map[string]interface{}{
				"version": 1, "principal_type": "internal_producer",
				"principal_id": "docs-notify", "space_id": "space-2",
			},
			origin: cardActionFrameOrigin{SenderUID: "notification", SpaceID: "space-1", ProducerBinding: binding},
		},
		{
			name: "malformed provenance",
			provenance: map[string]interface{}{
				"version": 1, "principal_type": "internal_producer",
				"principal_id": "docs-notify", "space_id": "space-1", "extra": true,
			},
			origin: cardActionFrameOrigin{SenderUID: "notification", SpaceID: "space-1", ProducerBinding: binding},
		},
		{
			name: "no binding resolver fails closed",
			provenance: map[string]interface{}{
				"version": 1, "principal_type": "internal_producer",
				"principal_id": "docs-notify", "space_id": "space-1",
			},
			origin: cardActionFrameOrigin{SenderUID: "notification", SpaceID: "space-1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, actionData := markedActionFrame(t, registry, tc.provenance)
			if _, err := resolveRegistryCardContext(context.Background(), tc.origin, raw, cardtmpl.DocsApproveActionID, actionData); err == nil {
				t.Fatal("inconsistent stored provenance accepted")
			}
		})
	}
}

func TestResolveRegistryCardContextBotProvenanceMatchesSender(t *testing.T) {
	registry, _ := provenanceActionFixture(t)
	raw, actionData := markedActionFrame(t, registry, map[string]interface{}{
		"version": 1, "principal_type": "bot", "principal_id": "bot-a", "space_id": "space-1",
	})
	origin := cardActionFrameOrigin{SenderUID: "bot-a", SpaceID: "space-1"}
	got, err := resolveRegistryCardContext(context.Background(), origin, raw, cardtmpl.DocsApproveActionID, actionData)
	if err != nil {
		t.Fatalf("resolveRegistryCardContext: %v", err)
	}
	if got.PrincipalType != "bot" || got.PrincipalID != "bot-a" || got.SpaceID != "space-1" {
		t.Fatalf("card context principal = %+v", got)
	}
}

func TestResolveRegistryCardContextLegacyFrameKeepsSenderPrincipal(t *testing.T) {
	registry, spy := provenanceActionFixture(t)
	// Pre-PR-C Bot Registry frame: template_ref only, no provenance.
	raw, actionData := markedActionFrame(t, registry, nil)
	origin := cardActionFrameOrigin{SenderUID: "notification", SpaceID: "space-1"}
	got, err := resolveRegistryCardContext(context.Background(), origin, raw, cardtmpl.DocsApproveActionID, actionData)
	if err != nil {
		t.Fatalf("resolveRegistryCardContext: %v", err)
	}
	if got.PrincipalType != "" || got.PrincipalID != "" {
		t.Fatalf("legacy frame invented a validated principal: %+v", got)
	}
	wantPrincipal := cardtmpl.CatalogPrincipal{
		Kind: cardtmpl.CatalogPrincipalBot, ID: "notification", SpaceID: "space-1",
	}
	if spy.lastRequest.Access.Principal != wantPrincipal {
		t.Fatalf("legacy catalog access = %+v, want %+v", spy.lastRequest.Access.Principal, wantPrincipal)
	}
}
