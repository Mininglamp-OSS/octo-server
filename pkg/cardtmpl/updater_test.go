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
