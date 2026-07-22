package notify

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
)

type captureViewUpdater struct {
	target  cardtmpl.UpdateTarget
	id      cardtmpl.ID
	version string
	state   cardtmpl.State
	fields  json.RawMessage
	env     cardtmpl.BuildEnv
}

func (u *captureViewUpdater) ReplaceView(_ context.Context, target cardtmpl.UpdateTarget, id cardtmpl.ID,
	version string, state cardtmpl.State, fields json.RawMessage, env cardtmpl.BuildEnv) error {
	u.target, u.id, u.version, u.state, u.fields, u.env = target, id, version, state, fields, env
	return nil
}

func (u *captureViewUpdater) Append(context.Context, cardtmpl.UpdateTarget, json.RawMessage) error {
	return nil
}

func TestDocsActionFinalizerUsesV3RegistryResultForTerminalStates(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
	updater := &captureViewUpdater{}
	legacyMutator := &captureCardMutator{}
	finalizer, err := NewDocsActionFinalizerWithUpdater(ctx, updater, legacyMutator, &capturingCardSender{})
	if err != nil {
		t.Fatalf("NewDocsActionFinalizerWithUpdater: %v", err)
	}
	event := cardactiondispatch.Event{
		EventID: 42, SenderUID: NotifyBotUIDValue, Owner: "docs", ActionType: "access_request.decision",
		MessageID: "1001", ChannelID: NotifyBotUIDValue, ChannelType: 1, SpaceID: "space-1", OperatorUID: "reviewer-1",
		Inputs: map[string]any{cardtmpl.DocsDenyReasonInputID: "scope mismatch"},
		Data: map[string]any{
			"doc_id": "doc-1", "request_id": "request-1", "doc_title": "Roadmap", "actor": "Alice",
			"actor_avatar_url": "https://cdn.example.com/alice.png", "request_reason": "Need quarterly access",
			"requested_at_display": "2026-07-22 10:00", "message_time_display": "10:03",
			"permission_label": "Access", "permission_role_label": "Editor", "source_name": "Docs",
		},
	}
	result := cardactiondispatch.DecisionResult{
		Disposition: cardactiondispatch.DispositionApplied, State: cardactiondispatch.StateDenied,
		RequesterUID: "user-a", Display: map[string]string{"title": "Roadmap"},
	}
	if err := finalizer.Finalize(context.Background(), event, result); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if len(legacyMutator.requests) != 0 {
		t.Fatalf("terminal V3 path used legacy mutator: %+v", legacyMutator.requests)
	}
	if updater.target.MessageID != event.MessageID || updater.target.CardSeq != event.EventID || updater.target.SenderUID != event.SenderUID {
		t.Fatalf("update target = %+v", updater.target)
	}
	if updater.id != docsaccessrequest.TemplateID || updater.version != docsaccessrequest.TemplateVersionV3 || updater.state != docsaccessrequest.StateRejected {
		t.Fatalf("update selection = %s@%s/%s", updater.id, updater.version, updater.state)
	}
	var fields map[string]any
	if err := json.Unmarshal(updater.fields, &fields); err != nil {
		t.Fatalf("decode fields: %v", err)
	}
	if fields["state"] != "rejected" || fields["requestId"] != "request-1" {
		t.Fatalf("result fields = %+v", fields)
	}
	decision := fields["decision"].(map[string]any)
	if decision["operatorName"] != event.OperatorUID || decision["rejectionReason"] != "scope mismatch" {
		t.Fatalf("decision fields = %+v", decision)
	}
	document := fields["document"].(map[string]any)
	requester := fields["requester"].(map[string]any)
	permission := fields["permission"].(map[string]any)
	if document["sourceName"] != "Docs" || requester["avatarUrl"] != "https://cdn.example.com/alice.png" ||
		permission["label"] != "Access" || permission["roleLabel"] != "Editor" ||
		fields["requestReason"] != "Need quarterly access" || fields["requestedAtDisplay"] != "2026-07-22 10:00" ||
		fields["messageTimeDisplay"] != "10:03" {
		t.Fatalf("result context fields = %+v", fields)
	}
	if updater.env.SpaceID != event.SpaceID {
		t.Fatalf("BuildEnv = %+v", updater.env)
	}
	_ = carddispatch.CardMutationResult{}
}

func TestDocsActionFinalizerV3OmitsUnavailableV2Decoration(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	updater := &captureViewUpdater{}
	finalizer := &DocsActionFinalizer{ctx: ctx, updater: updater}
	event := cardactiondispatch.Event{
		EventID: 43, SenderUID: NotifyBotUIDValue, MessageID: "1002", ChannelID: NotifyBotUIDValue,
		ChannelType: 1, SpaceID: "space-1", OperatorUID: "reviewer-1",
		Data: map[string]any{
			"doc_id": "doc-2", "request_id": "request-2", "doc_title": "Legacy Roadmap", "actor": "Bob",
		},
	}
	result := cardactiondispatch.DecisionResult{State: cardactiondispatch.StateApproved}
	if err := finalizer.replaceWithRegistryResult(context.Background(), event, result,
		NotifyBotUIDValue, "en", "Legacy Roadmap", ""); err != nil {
		t.Fatalf("replaceWithRegistryResult(v2 minimum): %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(updater.fields, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"permission", "requestReason", "requestedAtDisplay", "messageTimeDisplay"} {
		if _, exists := fields[key]; exists {
			t.Errorf("legacy 0.2 data fabricated optional %q: %+v", key, fields)
		}
	}
	document := fields["document"].(map[string]any)
	requester := fields["requester"].(map[string]any)
	if _, exists := document["sourceName"]; exists {
		t.Errorf("legacy 0.2 data fabricated sourceName: %+v", document)
	}
	if _, exists := requester["avatarUrl"]; exists {
		t.Errorf("legacy 0.2 data fabricated avatarUrl: %+v", requester)
	}
}
