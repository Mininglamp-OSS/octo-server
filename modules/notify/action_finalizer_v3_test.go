package notify

import (
	"context"
	"encoding/json"
	"strings"
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
	if decision["operatorName"] != "审批人" || decision["rejectionReason"] != "scope mismatch" {
		t.Fatalf("decision fields = %+v", decision)
	}
	if strings.Contains(string(updater.fields), event.OperatorUID) {
		t.Fatalf("result fields exposed raw operator uid: %s", updater.fields)
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

// okMutator 是 cardtmpl.mutationGateway 的最小实现:ReplaceView 只调 Mutate,
// 让真实 updater 走完 RenderCard(触发 0.3.0 InputSchema 校验)。
type okMutator struct{ mutated bool }

func (m *okMutator) Snapshot(context.Context, carddispatch.CardMutationTarget) (carddispatch.CardMutationSnapshot, error) {
	return carddispatch.CardMutationSnapshot{}, nil
}

func (m *okMutator) Mutate(context.Context, carddispatch.CardMutationRequest) (carddispatch.CardMutationResult, error) {
	m.mutated = true
	return carddispatch.CardMutationResult{}, nil
}

// P1(PR#641 review):回调 Display / event.Data 的合法长值(≤500 runes)必须在渲染
// 前截断到 0.3.0 schema cap。用真实 updater 驱动 RenderCard —— 若不截断,超长字段
// 会命中 InputSchema.Validate 的 maxLength → ErrFieldsInvalid,ReplaceView 确定性
// 失败(审批卡卡在 pending、申请人漏通知)。截断后必须渲染成功并写入 mutator。
func TestDocsActionFinalizerV3TruncatesOversizedDisplayFields(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"

	registry := cardtmpl.NewRegistry()
	registry.Register(docsaccessrequest.New(), docsaccessrequest.Assets, docsaccessrequest.HandoffRoot)
	registry.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	registry.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3)
	registry.Freeze()
	mut := &okMutator{}
	catalog, err := cardtmpl.NewStaticCatalog(registry)
	if err != nil {
		t.Fatalf("NewStaticCatalog: %v", err)
	}
	realUpdater, err := cardtmpl.NewCardUpdater(catalog, mut)
	if err != nil {
		t.Fatalf("NewCardUpdater: %v", err)
	}
	finalizer := &DocsActionFinalizer{ctx: ctx, updater: realUpdater}

	// 每个来源字段都超出对应 schema cap,但都在 ≤500 rune 的回调契约内。
	over := func(n int) string { return strings.Repeat("字", n) }
	event := cardactiondispatch.Event{
		EventID: 44, SenderUID: NotifyBotUIDValue, MessageID: "1003", ChannelID: NotifyBotUIDValue,
		ChannelType: 1, SpaceID: "space-1", OperatorUID: "reviewer-1",
		Data: map[string]any{
			"doc_id": "doc-3", "request_id": "request-3", "actor": over(200),
			"actor_avatar_url": "https://cdn.example.com/a.png", "source_name": over(300),
			"request_reason": over(400), "requested_at_display": over(200), "message_time_display": over(200),
			"permission_label": over(200), "permission_role_label": over(200),
		},
	}
	result := cardactiondispatch.DecisionResult{
		State: cardactiondispatch.StateDenied,
		Display: map[string]string{
			"operator_name": over(200), "decided_at": over(200), "title": over(400),
		},
	}
	if err := finalizer.replaceWithRegistryResult(context.Background(), event, result,
		NotifyBotUIDValue, "en", over(400), over(400)); err != nil {
		t.Fatalf("replaceWithRegistryResult with oversized display fields must succeed after truncation: %v", err)
	}
	if !mut.mutated {
		t.Fatal("mutator was never called — terminal update did not persist")
	}
}
