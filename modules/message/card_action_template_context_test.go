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
	origin := cardActionFrameOrigin{SenderUID: "notification", SpaceID: "space-1", SpaceKnown: true}
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
	// Install the Registry too: the markerless-frame guard consults it, and
	// production always has one (DefaultCatalog itself falls back to it).
	previousRegistry := cardtmpl.DefaultRegistry()
	cardtmpl.SetDefaultRegistry(registry)
	t.Cleanup(func() {
		cardtmpl.SetDefaultCatalog(previousCatalog)
		cardtmpl.SetDefaultRegistry(previousRegistry)
	})

	origin := cardActionFrameOrigin{SenderUID: "bot-reasoning", SpaceID: "space-1", SpaceKnown: true}
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
	// The markerless-frame guard asks the frozen Registry whether it knows the
	// version a frame names, so the fixture has to install one the way the
	// composition root does. Without it the guard sees no Registry, answers
	// "unknown" for everything, and every legacy frame is refused — fail-closed,
	// but it would make this fixture disagree with production.
	previousRegistry := cardtmpl.DefaultRegistry()
	cardtmpl.SetDefaultRegistry(registry)
	t.Cleanup(func() {
		cardtmpl.SetDefaultCatalog(previousCatalog)
		cardtmpl.SetDefaultRegistry(previousRegistry)
	})
	return registry, spy
}

func TestResolveRegistryCardContextUsesStoredProvenancePrincipal(t *testing.T) {
	registry, spy := provenanceActionFixture(t)
	raw, actionData := markedActionFrame(t, registry, map[string]interface{}{
		"version": 1, "principal_type": "internal_producer",
		"principal_id": "docs-notify", "space_id": "space-1",
	})
	origin := cardActionFrameOrigin{
		SenderUID: "notification", SpaceID: "space-1", SpaceKnown: true,
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
			origin: cardActionFrameOrigin{SenderUID: "other-bot", SpaceID: "space-1", SpaceKnown: true, ProducerBinding: binding},
		},
		{
			name: "unregistered producer",
			provenance: map[string]interface{}{
				"version": 1, "principal_type": "internal_producer",
				"principal_id": "rogue-producer", "space_id": "space-1",
			},
			origin: cardActionFrameOrigin{SenderUID: "notification", SpaceID: "space-1", SpaceKnown: true, ProducerBinding: binding},
		},
		{
			name: "bot provenance names a different bot",
			provenance: map[string]interface{}{
				"version": 1, "principal_type": "bot",
				"principal_id": "bot-a", "space_id": "space-1",
			},
			origin: cardActionFrameOrigin{SenderUID: "bot-b", SpaceID: "space-1", SpaceKnown: true, ProducerBinding: binding},
		},
		{
			name: "cross-space provenance",
			provenance: map[string]interface{}{
				"version": 1, "principal_type": "internal_producer",
				"principal_id": "docs-notify", "space_id": "space-2",
			},
			origin: cardActionFrameOrigin{SenderUID: "notification", SpaceID: "space-1", SpaceKnown: true, ProducerBinding: binding},
		},
		{
			name: "malformed provenance",
			provenance: map[string]interface{}{
				"version": 1, "principal_type": "internal_producer",
				"principal_id": "docs-notify", "space_id": "space-1", "extra": true,
			},
			origin: cardActionFrameOrigin{SenderUID: "notification", SpaceID: "space-1", SpaceKnown: true, ProducerBinding: binding},
		},
		{
			name: "no binding resolver fails closed",
			provenance: map[string]interface{}{
				"version": 1, "principal_type": "internal_producer",
				"principal_id": "docs-notify", "space_id": "space-1",
			},
			origin: cardActionFrameOrigin{SenderUID: "notification", SpaceID: "space-1", SpaceKnown: true},
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
	origin := cardActionFrameOrigin{SenderUID: "bot-a", SpaceID: "space-1", SpaceKnown: true}
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
	origin := cardActionFrameOrigin{SenderUID: "notification", SpaceID: "space-1", SpaceKnown: true}
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

// A frame that names a Space is only as trustworthy as the check against the
// server's own answer. When that answer is unavailable the click must be
// refused — and refused as an outage, because the frame may be perfectly valid
// and the caller has no way to fix a group-table read from its side.
func TestValidatedFramePrincipalRefusesAFrameSpaceItCannotCheck(t *testing.T) {
	registry, _ := provenanceActionFixture(t)
	raw, actionData := markedActionFrame(t, registry, map[string]interface{}{
		"version": 1, "principal_type": "internal_producer",
		"principal_id": "docs-notify", "space_id": "space-1",
	})
	binding := func(producerID string) (string, bool) {
		if producerID == "docs-notify" {
			return "notification", true
		}
		return "", false
	}
	blind := cardActionFrameOrigin{
		SenderUID: "notification", ProducerBinding: binding,
		// SpaceKnown deliberately false: the group row would not load.
	}
	_, err := resolveRegistryCardContext(context.Background(), blind, raw,
		cardtmpl.DocsApproveActionID, actionData)
	if !errors.Is(err, errCardOriginSpaceUnavailable) {
		t.Fatalf("err = %v, want errCardOriginSpaceUnavailable", err)
	}

	// A determined "this card is in no Space" is a different answer, and a
	// frame claiming one then genuinely disagrees with the server.
	determined := cardActionFrameOrigin{
		SenderUID: "notification", SpaceKnown: true, ProducerBinding: binding,
	}
	_, err = resolveRegistryCardContext(context.Background(), determined, raw,
		cardtmpl.DocsApproveActionID, actionData)
	if err == nil || errors.Is(err, errCardOriginSpaceUnavailable) {
		t.Fatalf("err = %v, want a plain mismatch rejection", err)
	}

	// An unmarked frame carries no Space claim, so an unavailable origin is
	// nothing to verify and the legacy path stays open.
	plain, plainData := markedActionFrame(t, registry, nil)
	if _, err := resolveRegistryCardContext(context.Background(), blind, plain,
		cardtmpl.DocsApproveActionID, plainData); errors.Is(err, errCardOriginSpaceUnavailable) {
		t.Fatalf("an unmarked frame was refused for a Space it never claimed: %v", err)
	}
}

// Review P1-1: the escalation this closes. A markerless frame's template
// identity comes from `card.metadata.octo.template` — inside the card body a
// raw caller controls — while raw ingress only rejects the two *top-level*
// server-only keys. With no marker the principal fell back to the sending
// Bot's own identity, and `Allows(action_context)` reads `edit`. So a Bot with
// only `discover+edit` could fabricate a frame naming an active dynamic version
// and drive its action route, with `edit` standing in for the `send` grant the
// frame never had.
//
// Markerless compatibility exists for cards delivered before PR-C, every one of
// which names a version the frozen Registry knows. Scoping the fallback to
// exactly that population keeps invariant 7 and closes the substitution.
func TestMarkerlessFrameNamingAnUnknownTemplateIsRefused(t *testing.T) {
	registry, _ := provenanceActionFixture(t)
	binding := func(string) (string, bool) { return "", false }
	origin := cardActionFrameOrigin{
		SenderUID: "notification", SpaceID: "space-1", SpaceKnown: true, ProducerBinding: binding,
	}

	// A markerless frame naming the frozen Registry's own version still works —
	// that is the entire legacy population and it must not regress.
	legacy, legacyData := markedActionFrame(t, registry, nil)
	if _, err := resolveRegistryCardContext(context.Background(), origin, legacy,
		cardtmpl.DocsApproveActionID, legacyData); err != nil {
		t.Fatalf("a legacy markerless frame was refused: %v", err)
	}

	// The raw-forgery shape: no top-level marker at all (ingress rejects those
	// by key presence), only the nested metadata a caller controls, pointed at
	// a version the Registry does not know. Refused before any authorization.
	forged := retargetFrameTemplateVersion(t, legacy, "9.9.9-dyn")
	_, err := resolveRegistryCardContext(context.Background(), origin, forged,
		cardtmpl.DocsApproveActionID, legacyData)
	if !errors.Is(err, errMarkerlessDynamicFrame) {
		t.Fatalf("err = %v, want errMarkerlessDynamicFrame", err)
	}
}

// retargetFrameTemplateVersion rewrites metadata.octo.template.version, which is
// the field a raw caller controls, leaving everything else intact.
func retargetFrameTemplateVersion(t *testing.T, frame []byte, version string) []byte {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(frame, &envelope); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	// A raw caller cannot set the top-level marker — ingress rejects it by key
	// presence — so the forgery has to work without one.
	delete(envelope, "template_ref")
	card, _ := envelope["card"].(map[string]any)
	metadata, _ := card["metadata"].(map[string]any)
	octo, _ := metadata["octo"].(map[string]any)
	template, _ := octo["template"].(map[string]any)
	if template == nil {
		t.Fatal("frame carries no metadata.octo.template")
	}
	template["version"] = version
	retargeted, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	return retargeted
}

// Review P2-3: an unknown origin Space had to fail closed on *both* branches.
// The empty-Space branch looks harmless and is not — assigning an unknown
// origin leaves the principal's Space empty, which resolves against the global
// grant row alone, so a principal holding an active global grant plus an exact
// tombstone for the card's real Space would be allowed. That defeats invariant
// 11, whose whole purpose is letting an exact tombstone shadow a live global
// grant, and a transient group-row read failure is enough to reach it.
func TestUnknownOriginSpaceIsRefusedWhateverTheFrameClaims(t *testing.T) {
	registry, _ := provenanceActionFixture(t)
	binding := func(producerID string) (string, bool) {
		if producerID == "docs-notify" {
			return "notification", true
		}
		return "", false
	}
	blind := cardActionFrameOrigin{SenderUID: "notification", ProducerBinding: binding}

	for _, frameSpace := range []string{"space-1", ""} {
		raw, actionData := markedActionFrame(t, registry, map[string]interface{}{
			"version": 1, "principal_type": "internal_producer",
			"principal_id": "docs-notify", "space_id": frameSpace,
		})
		_, err := resolveRegistryCardContext(context.Background(), blind, raw,
			cardtmpl.DocsApproveActionID, actionData)
		if !errors.Is(err, errCardOriginSpaceUnavailable) {
			t.Fatalf("frame space %q: err = %v, want errCardOriginSpaceUnavailable", frameSpace, err)
		}
	}

	// A determined origin still resolves, so the refusal is about not knowing
	// rather than about the Space being absent.
	known := blind
	known.SpaceKnown = true
	known.SpaceID = "space-1"
	raw, actionData := markedActionFrame(t, registry, map[string]interface{}{
		"version": 1, "principal_type": "internal_producer",
		"principal_id": "docs-notify", "space_id": "space-1",
	})
	if _, err := resolveRegistryCardContext(context.Background(), known, raw,
		cardtmpl.DocsApproveActionID, actionData); err != nil {
		t.Fatalf("a determined origin Space was refused: %v", err)
	}
}
