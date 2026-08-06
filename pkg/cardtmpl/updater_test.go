package cardtmpl_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
)

type captureMutationGateway struct {
	snapshot carddispatch.CardMutationSnapshot
	requests []carddispatch.CardMutationRequest
	result   carddispatch.CardMutationResult
	err      error
}

func (g *captureMutationGateway) Snapshot(_ context.Context, _ carddispatch.CardMutationTarget) (carddispatch.CardMutationSnapshot, error) {
	return g.snapshot, g.err
}

func (g *captureMutationGateway) Mutate(_ context.Context, req carddispatch.CardMutationRequest) (carddispatch.CardMutationResult, error) {
	g.requests = append(g.requests, req)
	return g.result, g.err
}

func TestCardUpdaterReplaceViewRendersV3AndPreservesMessageIdentity(t *testing.T) {
	r := cardtmpl.NewRegistry()
	r.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3)
	r.Freeze()
	gateway := &captureMutationGateway{result: carddispatch.CardMutationResult{Applied: true}}
	updater, err := cardtmpl.NewCardUpdater(staticCatalog(t, r), gateway)
	if err != nil {
		t.Fatalf("NewCardUpdater: %v", err)
	}
	target := cardtmpl.UpdateTarget{
		Target:    carddispatch.Target{SpaceID: "space-1", ChannelID: "user-b", ChannelType: 1},
		SenderUID: "notification", MessageID: "1001", MessageSeq: 7, CardSeq: 42,
	}
	fields := json.RawMessage(`{
		"requestId":"request-1","state":"approved",
		"document":{"docId":"doc-1","title":"Roadmap"},
		"requester":{"name":"Alice"},"decision":{"operatorName":"Bob"}
	}`)
	if err := updater.ReplaceView(context.Background(), target, docsaccessrequest.TemplateID,
		docsaccessrequest.TemplateVersionV3, docsaccessrequest.StateApproved, fields,
		cardtmpl.BuildEnv{WebLoginURL: "https://web.example.com", Lang: "en", SpaceID: "space-1"}); err != nil {
		t.Fatalf("ReplaceView: %v", err)
	}
	if len(gateway.requests) != 1 {
		t.Fatalf("mutation count = %d, want 1", len(gateway.requests))
	}
	req := gateway.requests[0]
	if req.SenderUID != target.SenderUID || req.MessageID != target.MessageID || req.MessageSeq != target.MessageSeq ||
		req.ChannelID != target.Target.ChannelID || req.ChannelType != target.Target.ChannelType {
		t.Fatalf("mutation target = %+v, want %+v", req, target)
	}
	assertUpdateEnvelope(t, req.ContentEdit, 42, cardmsg.ProfileV1, cardmsg.RenderProfileOctoChatV1, docsaccessrequest.TemplateVersionV3)
}

func TestCardUpdaterAppendUsesEffectiveFrameAndValidatesWholeCard(t *testing.T) {
	original := map[string]any{
		"type": 17, "card_version": cardmsg.CardVersion, "profile": cardmsg.ProfileV1,
		"space_id": "space-1", "card_seq": 4,
		"card": map[string]any{
			"type": "AdaptiveCard", "version": cardmsg.CardVersion,
			"body": []any{map[string]any{"type": "TextBlock", "text": "before"}},
			"metadata": map[string]any{"webUrl": "https://web.example.com/d/1", "octo": map[string]any{
				"protocol": cardtmpl.Protocol, "template": map[string]any{"id": "docs.access-request", "version": "0.3.0"},
			}},
		},
	}
	raw, _ := json.Marshal(original)
	gateway := &captureMutationGateway{
		snapshot: carddispatch.CardMutationSnapshot{Envelope: raw, CardSeq: 4},
		result:   carddispatch.CardMutationResult{Applied: true},
	}
	r := cardtmpl.NewRegistry()
	r.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3)
	r.Freeze()
	updater, err := cardtmpl.NewCardUpdater(staticCatalog(t, r), gateway)
	if err != nil {
		t.Fatalf("NewCardUpdater: %v", err)
	}
	target := cardtmpl.UpdateTarget{
		Target:    carddispatch.Target{SpaceID: "space-1", ChannelID: "user-b", ChannelType: 1},
		SenderUID: "notification", MessageID: "1001", CardSeq: 5,
	}
	element := json.RawMessage(`{"type":"TextBlock","text":"after"}`)
	if err := updater.Append(context.Background(), target, element); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if len(gateway.requests) != 1 {
		t.Fatalf("mutation count = %d, want 1", len(gateway.requests))
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(gateway.requests[0].ContentEdit), &envelope); err != nil {
		t.Fatalf("decode content_edit: %v", err)
	}
	card := envelope["card"].(map[string]any)
	body := card["body"].([]any)
	if len(body) != 2 || body[1].(map[string]any)["text"] != "after" {
		t.Fatalf("appended body = %#v", body)
	}
	assertUpdateEnvelope(t, gateway.requests[0].ContentEdit, 5, cardmsg.ProfileV1, "", "0.3.0")
}

func TestCardUpdaterAppendRejectsNonContiguousCardSequence(t *testing.T) {
	original := map[string]any{
		"type": 17, "card_version": cardmsg.CardVersion, "profile": cardmsg.ProfileV1,
		"space_id": "space-1", "card_seq": 4,
		"card": map[string]any{
			"type": "AdaptiveCard", "version": cardmsg.CardVersion,
			"body": []any{map[string]any{"type": "TextBlock", "text": "current"}},
		},
	}
	raw, _ := json.Marshal(original)
	gateway := &captureMutationGateway{
		snapshot: carddispatch.CardMutationSnapshot{Envelope: raw, CardSeq: 4},
		result:   carddispatch.CardMutationResult{Applied: true},
	}
	updater, err := cardtmpl.NewCardUpdater(staticCatalog(t, cardtmpl.NewRegistry()), gateway)
	if err != nil {
		t.Fatalf("NewCardUpdater: %v", err)
	}
	target := cardtmpl.UpdateTarget{
		Target:    carddispatch.Target{SpaceID: "space-1", ChannelID: "user-b", ChannelType: 1},
		SenderUID: "notification", MessageID: "1001", CardSeq: 6,
	}
	err = updater.Append(context.Background(), target, json.RawMessage(`{"type":"TextBlock","text":"stale append"}`))
	if !errors.Is(err, carddispatch.ErrCardMutationConflict) {
		t.Fatalf("Append(non-contiguous card_seq) error = %v, want %v", err, carddispatch.ErrCardMutationConflict)
	}
	if len(gateway.requests) != 0 {
		t.Fatalf("stale append wrote mutations: %+v", gateway.requests)
	}
}

func TestCardUpdaterFailsClosedOnInvalidInputsAndDependencies(t *testing.T) {
	gateway := &captureMutationGateway{}
	if _, err := cardtmpl.NewCardUpdater(nil, gateway); err == nil {
		t.Fatal("NewCardUpdater(nil catalog) error = nil")
	}
	if _, err := cardtmpl.NewCardUpdater(staticCatalog(t, cardtmpl.NewRegistry()), nil); err == nil {
		t.Fatal("NewCardUpdater(nil mutator) error = nil")
	}

	r := cardtmpl.NewRegistry()
	r.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3)
	r.Freeze()
	updater, err := cardtmpl.NewCardUpdater(staticCatalog(t, r), gateway)
	if err != nil {
		t.Fatal(err)
	}
	target := cardtmpl.UpdateTarget{
		Target:    carddispatch.Target{SpaceID: "space-1", ChannelID: "user-b", ChannelType: 1},
		SenderUID: "notification", MessageID: "1001", CardSeq: 1,
	}
	validFields := json.RawMessage(`{
		"requestId":"request-1","state":"approved",
		"document":{"docId":"doc-1","title":"Roadmap"},
		"decision":{"operatorName":"reviewer-1"}
	}`)

	if err := updater.ReplaceView(context.Background(), target, docsaccessrequest.TemplateID,
		docsaccessrequest.TemplateVersionV3, docsaccessrequest.StateApproved, validFields,
		cardtmpl.BuildEnv{WebLoginURL: "https://web.example.com", SpaceID: "other"}); !errors.Is(err, cardtmpl.ErrUpdateInvalid) {
		t.Fatalf("ReplaceView(space mismatch) error = %v, want %v", err, cardtmpl.ErrUpdateInvalid)
	}
	if err := updater.ReplaceView(context.Background(), target, docsaccessrequest.TemplateID,
		docsaccessrequest.TemplateVersionV3, docsaccessrequest.StateApproved, json.RawMessage(`{}`),
		cardtmpl.BuildEnv{WebLoginURL: "https://web.example.com", SpaceID: "space-1"}); !errors.Is(err, cardtmpl.ErrFieldsInvalid) {
		t.Fatalf("ReplaceView(invalid fields) error = %v, want %v", err, cardtmpl.ErrFieldsInvalid)
	}

	if err := updater.Append(context.Background(), target, json.RawMessage(`[]`)); !errors.Is(err, cardtmpl.ErrUpdateInvalid) {
		t.Fatalf("Append(non-object) error = %v, want %v", err, cardtmpl.ErrUpdateInvalid)
	}
	snapshotErr := errors.New("snapshot failed")
	gateway.err = snapshotErr
	if err := updater.Append(context.Background(), target, json.RawMessage(`{"type":"TextBlock","text":"after"}`)); !errors.Is(err, snapshotErr) {
		t.Fatalf("Append(snapshot error) = %v, want %v", err, snapshotErr)
	}
	gateway.err = nil
	gateway.snapshot = carddispatch.CardMutationSnapshot{Envelope: json.RawMessage(`{"type":17}`)}
	if err := updater.Append(context.Background(), target, json.RawMessage(`{"type":"TextBlock","text":"after"}`)); !errors.Is(err, cardtmpl.ErrUpdateInvalid) {
		t.Fatalf("Append(invalid effective frame) = %v, want %v", err, cardtmpl.ErrUpdateInvalid)
	}
}

func TestCardUpdaterAppendRejectsBlockedStoredTemplate(t *testing.T) {
	original := map[string]any{
		"type": 17, "card_version": cardmsg.CardVersion, "profile": cardmsg.ProfileV1,
		"space_id": "space-1", "card_seq": 4,
		"card": map[string]any{
			"type": "AdaptiveCard", "version": cardmsg.CardVersion,
			"body": []any{map[string]any{"type": "TextBlock", "text": "before"}},
			"metadata": map[string]any{"octo": map[string]any{
				"protocol": cardtmpl.Protocol, "template": map[string]any{"id": "test.dynamic", "version": "1.0.0"},
			}},
		},
	}
	raw, _ := json.Marshal(original)
	gateway := &captureMutationGateway{
		snapshot: carddispatch.CardMutationSnapshot{Envelope: raw, CardSeq: 4},
		result:   carddispatch.CardMutationResult{Applied: true},
	}
	updater, err := cardtmpl.NewCardUpdater(blockedCatalog{}, gateway)
	if err != nil {
		t.Fatal(err)
	}
	target := cardtmpl.UpdateTarget{
		Target:    carddispatch.Target{SpaceID: "space-1", ChannelID: "user-b", ChannelType: 1},
		SenderUID: "notification", MessageID: "1001", CardSeq: 5,
	}
	err = updater.Append(context.Background(), target, json.RawMessage(`{"type":"TextBlock","text":"after"}`))
	if !errors.Is(err, cardtmpl.ErrRuntimeCatalogBlocked) {
		t.Fatalf("Append(blocked template) error = %v, want ErrRuntimeCatalogBlocked", err)
	}
	if len(gateway.requests) != 0 {
		t.Fatalf("blocked append wrote mutations: %+v", gateway.requests)
	}
}

func staticCatalog(t *testing.T, registry *cardtmpl.Registry) cardtmpl.Catalog {
	t.Helper()
	catalog, err := cardtmpl.NewStaticCatalog(registry)
	if err != nil {
		t.Fatalf("NewStaticCatalog: %v", err)
	}
	return catalog
}

type blockedCatalog struct{}

func (blockedCatalog) Render(context.Context, cardtmpl.CatalogRenderRequest) (map[string]any, error) {
	return nil, cardtmpl.ErrRuntimeCatalogBlocked
}
func (blockedCatalog) MetaExact(context.Context, cardtmpl.CatalogExactRequest) (cardtmpl.TemplateMeta, error) {
	return cardtmpl.TemplateMeta{}, cardtmpl.ErrRuntimeCatalogBlocked
}
func (blockedCatalog) MetaDefault(context.Context, cardtmpl.CatalogDefaultRequest) (cardtmpl.TemplateMeta, error) {
	return cardtmpl.TemplateMeta{}, cardtmpl.ErrRuntimeCatalogBlocked
}
func (blockedCatalog) FallbackText(context.Context, cardtmpl.CatalogFallbackRequest) (string, error) {
	return "", cardtmpl.ErrRuntimeCatalogBlocked
}
func (blockedCatalog) ActionContext(context.Context, cardtmpl.CatalogActionRequest) (cardtmpl.CatalogActionContext, error) {
	return cardtmpl.CatalogActionContext{}, cardtmpl.ErrRuntimeCatalogBlocked
}

func assertUpdateEnvelope(t *testing.T, raw string, seq int64, profile, renderProfile, version string) {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("decode content_edit: %v", err)
	}
	if envelope["card_seq"] != float64(seq) || envelope["profile"] != profile || envelope["space_id"] != "space-1" {
		t.Fatalf("envelope = %+v", envelope)
	}
	if got, _ := envelope["render_profile"].(string); got != renderProfile {
		t.Fatalf("render_profile = %q, want %q", got, renderProfile)
	}
	card := envelope["card"].(map[string]any)
	metadata := card["metadata"].(map[string]any)
	octo := metadata["octo"].(map[string]any)
	template := octo["template"].(map[string]any)
	if template["version"] != version {
		t.Fatalf("template.version = %v, want %q", template["version"], version)
	}
}

// ---- PR-C Slice 1 (D3/G13): stored catalog markers pin identity, principal
// and Space through every mutation. ----

type accessCaptureCatalog struct {
	cardtmpl.Catalog
	metaAccess   []cardtmpl.CatalogAccess
	renderAccess []cardtmpl.CatalogAccess
}

func (c *accessCaptureCatalog) MetaExact(ctx context.Context, request cardtmpl.CatalogExactRequest) (cardtmpl.TemplateMeta, error) {
	c.metaAccess = append(c.metaAccess, request.Access)
	return c.Catalog.MetaExact(ctx, request)
}

func (c *accessCaptureCatalog) Render(ctx context.Context, request cardtmpl.CatalogRenderRequest) (map[string]any, error) {
	c.renderAccess = append(c.renderAccess, request.Access)
	return c.Catalog.Render(ctx, request)
}

func markedV3Snapshot(t *testing.T, mutate func(map[string]any)) json.RawMessage {
	t.Helper()
	frame := map[string]any{
		"type": 17, "card_version": cardmsg.CardVersion, "profile": cardmsg.ProfileV2,
		"space_id": "space-1", "card_seq": 4,
		"template_ref": map[string]any{
			"id": string(docsaccessrequest.TemplateID), "version": docsaccessrequest.TemplateVersionV3,
		},
		"catalog_provenance": map[string]any{
			"version": 1, "principal_type": "internal_producer",
			"principal_id": "docs-notify", "space_id": "space-1",
		},
		"card": map[string]any{
			"type": "AdaptiveCard", "version": cardmsg.CardVersion,
			"body": []any{map[string]any{"type": "TextBlock", "text": "pending"}},
			"metadata": map[string]any{"octo": map[string]any{
				"protocol": cardtmpl.Protocol,
				"template": map[string]any{
					"id": string(docsaccessrequest.TemplateID), "version": docsaccessrequest.TemplateVersionV3,
				},
			}},
		},
	}
	if mutate != nil {
		mutate(frame)
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func v3UpdaterFixture(t *testing.T, snapshot json.RawMessage) (*accessCaptureCatalog, *captureMutationGateway, cardtmpl.CardUpdater) {
	t.Helper()
	r := cardtmpl.NewRegistry()
	r.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3)
	r.Freeze()
	catalog := &accessCaptureCatalog{Catalog: staticCatalog(t, r)}
	gateway := &captureMutationGateway{
		snapshot: carddispatch.CardMutationSnapshot{Envelope: snapshot, CardSeq: 4},
		result:   carddispatch.CardMutationResult{Applied: true},
	}
	updater, err := cardtmpl.NewCardUpdater(catalog, gateway)
	if err != nil {
		t.Fatalf("NewCardUpdater: %v", err)
	}
	return catalog, gateway, updater
}

func approvedV3Fields() json.RawMessage {
	return json.RawMessage(`{
		"requestId":"request-1","state":"approved",
		"document":{"docId":"doc-1","title":"Roadmap"},
		"requester":{"name":"Alice"},"decision":{"operatorName":"Bob"}
	}`)
}

func v3UpdateTarget() cardtmpl.UpdateTarget {
	return cardtmpl.UpdateTarget{
		Target:    carddispatch.Target{SpaceID: "space-1", ChannelID: "user-b", ChannelType: 1},
		SenderUID: "notification", MessageID: "1001", MessageSeq: 7, CardSeq: 42,
	}
}

func TestCardUpdaterReplaceViewPreservesStoredCatalogMarkers(t *testing.T) {
	catalog, gateway, updater := v3UpdaterFixture(t, markedV3Snapshot(t, nil))
	if err := updater.ReplaceView(context.Background(), v3UpdateTarget(), docsaccessrequest.TemplateID,
		docsaccessrequest.TemplateVersionV3, docsaccessrequest.StateApproved, approvedV3Fields(),
		cardtmpl.BuildEnv{WebLoginURL: "https://web.example.com", Lang: "en", SpaceID: "space-1"}); err != nil {
		t.Fatalf("ReplaceView: %v", err)
	}
	if len(gateway.requests) != 1 {
		t.Fatalf("mutation count = %d", len(gateway.requests))
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(gateway.requests[0].ContentEdit), &envelope); err != nil {
		t.Fatal(err)
	}
	ref, _ := envelope["template_ref"].(map[string]any)
	if ref == nil || ref["id"] != string(docsaccessrequest.TemplateID) || ref["version"] != docsaccessrequest.TemplateVersionV3 {
		t.Fatalf("replacement frame lost template_ref: %#v", envelope["template_ref"])
	}
	provenance, _ := envelope["catalog_provenance"].(map[string]any)
	if provenance == nil || provenance["principal_type"] != "internal_producer" ||
		provenance["principal_id"] != "docs-notify" || provenance["space_id"] != "space-1" {
		t.Fatalf("replacement frame lost catalog_provenance: %#v", envelope["catalog_provenance"])
	}
	// The stored provenance — not the SenderUID guess — is the edit principal.
	want := cardtmpl.CatalogPrincipal{
		Kind: cardtmpl.CatalogPrincipalInternalProducer, ID: "docs-notify", SpaceID: "space-1",
	}
	if len(catalog.renderAccess) == 0 || catalog.renderAccess[0].Principal != want {
		t.Fatalf("render access = %+v, want principal %+v", catalog.renderAccess, want)
	}
	if catalog.renderAccess[0].Purpose != cardtmpl.CatalogPurposeHistoricalEdit {
		t.Fatalf("render purpose = %v", catalog.renderAccess[0].Purpose)
	}
}

func TestCardUpdaterReplaceViewRejectsStoredIdentityMismatch(t *testing.T) {
	_, gateway, updater := v3UpdaterFixture(t, markedV3Snapshot(t, nil))
	err := updater.ReplaceView(context.Background(), v3UpdateTarget(), docsaccessrequest.TemplateID,
		"9.9.9", docsaccessrequest.StateApproved, approvedV3Fields(),
		cardtmpl.BuildEnv{WebLoginURL: "https://web.example.com", Lang: "en", SpaceID: "space-1"})
	if !errors.Is(err, cardtmpl.ErrUpdateInvalid) {
		t.Fatalf("ReplaceView(version drift) error = %v, want ErrUpdateInvalid", err)
	}
	if len(gateway.requests) != 0 {
		t.Fatalf("identity drift still mutated: %+v", gateway.requests)
	}
}

func TestCardUpdaterReplaceViewRejectsMalformedStoredMarkers(t *testing.T) {
	snapshot := markedV3Snapshot(t, func(frame map[string]any) {
		delete(frame, "template_ref") // provenance without ref is never a server shape
	})
	_, gateway, updater := v3UpdaterFixture(t, snapshot)
	err := updater.ReplaceView(context.Background(), v3UpdateTarget(), docsaccessrequest.TemplateID,
		docsaccessrequest.TemplateVersionV3, docsaccessrequest.StateApproved, approvedV3Fields(),
		cardtmpl.BuildEnv{WebLoginURL: "https://web.example.com", Lang: "en", SpaceID: "space-1"})
	if !errors.Is(err, cardtmpl.ErrUpdateInvalid) {
		t.Fatalf("ReplaceView(malformed markers) error = %v, want ErrUpdateInvalid", err)
	}
	if len(gateway.requests) != 0 {
		t.Fatalf("malformed markers still mutated: %+v", gateway.requests)
	}
}

func TestCardUpdaterReplaceViewRejectsCrossSpaceStoredProvenance(t *testing.T) {
	snapshot := markedV3Snapshot(t, func(frame map[string]any) {
		frame["catalog_provenance"].(map[string]any)["space_id"] = "space-2"
	})
	_, gateway, updater := v3UpdaterFixture(t, snapshot)
	err := updater.ReplaceView(context.Background(), v3UpdateTarget(), docsaccessrequest.TemplateID,
		docsaccessrequest.TemplateVersionV3, docsaccessrequest.StateApproved, approvedV3Fields(),
		cardtmpl.BuildEnv{WebLoginURL: "https://web.example.com", Lang: "en", SpaceID: "space-1"})
	if !errors.Is(err, cardtmpl.ErrUpdateInvalid) {
		t.Fatalf("ReplaceView(cross-space provenance) error = %v, want ErrUpdateInvalid", err)
	}
	if len(gateway.requests) != 0 {
		t.Fatalf("cross-space provenance still mutated: %+v", gateway.requests)
	}
}

func TestCardUpdaterReplaceViewLegacyFrameStaysUnmarked(t *testing.T) {
	snapshot := markedV3Snapshot(t, func(frame map[string]any) {
		delete(frame, "template_ref")
		delete(frame, "catalog_provenance")
	})
	catalog, gateway, updater := v3UpdaterFixture(t, snapshot)
	if err := updater.ReplaceView(context.Background(), v3UpdateTarget(), docsaccessrequest.TemplateID,
		docsaccessrequest.TemplateVersionV3, docsaccessrequest.StateApproved, approvedV3Fields(),
		cardtmpl.BuildEnv{WebLoginURL: "https://web.example.com", Lang: "en", SpaceID: "space-1"}); err != nil {
		t.Fatalf("ReplaceView(legacy frame): %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(gateway.requests[0].ContentEdit), &envelope); err != nil {
		t.Fatal(err)
	}
	if _, forged := envelope["template_ref"]; forged {
		t.Fatalf("legacy replacement grew template_ref: %#v", envelope["template_ref"])
	}
	if _, forged := envelope["catalog_provenance"]; forged {
		t.Fatalf("legacy replacement grew catalog_provenance: %#v", envelope["catalog_provenance"])
	}
	// Without stored provenance the legacy sender-derived principal remains.
	want := cardtmpl.CatalogPrincipal{
		Kind: cardtmpl.CatalogPrincipalInternalProducer, ID: "notification", SpaceID: "space-1",
	}
	if len(catalog.renderAccess) == 0 || catalog.renderAccess[0].Principal != want {
		t.Fatalf("legacy render access = %+v, want principal %+v", catalog.renderAccess, want)
	}
}

func TestCardUpdaterAppendValidatesStoredMarkers(t *testing.T) {
	t.Run("markers preserved and provenance principal used", func(t *testing.T) {
		catalog, gateway, updater := v3UpdaterFixture(t, markedV3Snapshot(t, nil))
		target := v3UpdateTarget()
		target.CardSeq = 5
		if err := updater.Append(context.Background(), target, json.RawMessage(`{"type":"TextBlock","text":"after"}`)); err != nil {
			t.Fatalf("Append: %v", err)
		}
		var envelope map[string]any
		if err := json.Unmarshal([]byte(gateway.requests[0].ContentEdit), &envelope); err != nil {
			t.Fatal(err)
		}
		provenance, _ := envelope["catalog_provenance"].(map[string]any)
		if provenance == nil || provenance["principal_id"] != "docs-notify" {
			t.Fatalf("append frame lost catalog_provenance: %#v", envelope["catalog_provenance"])
		}
		want := cardtmpl.CatalogPrincipal{
			Kind: cardtmpl.CatalogPrincipalInternalProducer, ID: "docs-notify", SpaceID: "space-1",
		}
		if len(catalog.metaAccess) == 0 || catalog.metaAccess[0].Principal != want {
			t.Fatalf("append meta access = %+v, want principal %+v", catalog.metaAccess, want)
		}
	})
	t.Run("malformed markers fail closed", func(t *testing.T) {
		snapshot := markedV3Snapshot(t, func(frame map[string]any) {
			frame["catalog_provenance"] = "forged"
		})
		_, gateway, updater := v3UpdaterFixture(t, snapshot)
		target := v3UpdateTarget()
		target.CardSeq = 5
		err := updater.Append(context.Background(), target, json.RawMessage(`{"type":"TextBlock","text":"after"}`))
		if !errors.Is(err, cardtmpl.ErrUpdateInvalid) {
			t.Fatalf("Append(malformed markers) error = %v, want ErrUpdateInvalid", err)
		}
		if len(gateway.requests) != 0 {
			t.Fatalf("malformed markers still mutated: %+v", gateway.requests)
		}
	})
}
