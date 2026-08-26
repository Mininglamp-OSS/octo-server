package notify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
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
	calls   int
}

func (u *captureViewUpdater) ReplaceView(_ context.Context, target cardtmpl.UpdateTarget, id cardtmpl.ID,
	version string, state cardtmpl.State, fields json.RawMessage, env cardtmpl.BuildEnv) error {
	u.calls++
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
	finalizer, err := NewDocsActionFinalizerWithUpdater(ctx, updater, legacyMutator)
	if err != nil {
		t.Fatalf("NewDocsActionFinalizerWithUpdater: %v", err)
	}
	event := cardactiondispatch.Event{
		EventID: 42, SenderUID: NotifyBotUIDValue, Owner: "docs", ActionType: "access_request.decision",
		MessageID: "1001", ChannelID: NotifyBotUIDValue, ChannelType: 1, SpaceID: "card-space-a", OperatorUID: "reviewer-1",
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
		RequesterUID: "user-a", Display: map[string]string{
			"title": "Roadmap", "operator_name": "林澈", "operator_space_id": "operator-space-b",
			"operator_space_name": "Operator Space B", "decided_at": "2026-08-20 19:01",
		},
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
	if decision["operatorName"] != "林澈" || decision["operatorSpaceName"] != "Operator Space B" ||
		decision["decidedAtDisplay"] != "2026-08-20 19:01" || decision["rejectionReason"] != "scope mismatch" {
		t.Fatalf("decision fields = %+v", decision)
	}
	r := cardtmpl.NewRegistry()
	r.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3)
	r.Freeze()
	rendered, err := r.Render(context.Background(), updater.id, updater.version, updater.state, updater.fields, updater.env)
	if err != nil {
		t.Fatalf("render terminal fields: %v", err)
	}
	renderedJSON, err := json.Marshal(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(renderedJSON), "Operator Space B") || strings.Contains(string(renderedJSON), "card-space-a") {
		t.Fatalf("terminal display must identify operator Space B, never card Space A: %s", renderedJSON)
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

func TestDocsActionFinalizerAuthoritativeDeciderWithoutTimeDoesNotUseRequestTime(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
	updater := &captureViewUpdater{}
	finalizer := &DocsActionFinalizer{ctx: ctx, updater: updater}
	event := cardactiondispatch.Event{
		EventID: 42, SenderUID: NotifyBotUIDValue, MessageID: "1001", ChannelID: NotifyBotUIDValue,
		ChannelType: 1, SpaceID: "card-space-a",
		Data: map[string]any{
			"doc_id": "doc-1", "request_id": "request-1", "doc_title": "Roadmap", "actor": "Alice",
			"message_time_display": "REQUEST-TIME-MUST-NOT-BECOME-DECISION-TIME",
		},
	}
	result := cardactiondispatch.DecisionResult{
		State: cardactiondispatch.StateApproved, DeciderUID: "authoritative-decider",
		Display: map[string]string{"operator_name": "Reviewer"},
	}

	if err := finalizer.replaceWithRegistryResult(context.Background(), event, result,
		NotifyBotUIDValue, "en", "Roadmap", ""); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(updater.fields, &fields); err != nil {
		t.Fatal(err)
	}
	if _, found := fields["messageTimeDisplay"]; found {
		t.Fatalf("authoritative result retained request-time fallback: %+v", fields)
	}
	registry := cardtmpl.NewRegistry()
	registry.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	registry.Freeze()
	rendered, err := registry.Render(context.Background(), updater.id, updater.version, updater.state, updater.fields, updater.env)
	if err != nil {
		t.Fatalf("render terminal fields: %v", err)
	}
	raw, err := json.Marshal(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "REQUEST-TIME-MUST-NOT-BECOME-DECISION-TIME") {
		t.Fatalf("terminal card rendered request time as decision time: %s", raw)
	}
}

func TestDocsActionFinalizerLegacyResultKeepsRequestTimeFallback(t *testing.T) {
	input := docsResultRenderInput{
		Data: map[string]any{
			"doc_id": "doc-1", "request_id": "request-1", "doc_title": "Roadmap", "actor": "Alice",
			"message_time_display": "LEGACY-REQUEST-TIME",
		},
		Title: "Roadmap", OperatorName: "Reviewer",
	}
	fields, state, err := buildDocsAccessResultFields("en", docsaccessrequest.TemplateVersionV3, input)
	if err != nil {
		t.Fatal(err)
	}
	registry := cardtmpl.NewRegistry()
	registry.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	registry.Freeze()
	rendered, err := registry.Render(context.Background(), docsaccessrequest.TemplateID,
		docsaccessrequest.TemplateVersionV3, state, fields,
		cardtmpl.BuildEnv{Lang: "en", SpaceID: "space-1", WebLoginURL: "https://im.example.com/login"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "LEGACY-REQUEST-TIME") {
		t.Fatalf("legacy result lost request-time compatibility fallback: %s", raw)
	}
}

func TestDocsActionFinalizerUsesConsumerResolvedOperatorDisplay(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	updater := &captureViewUpdater{}
	finalizer := &DocsActionFinalizer{ctx: ctx, updater: updater}
	event := cardactiondispatch.Event{
		EventID: 42, SenderUID: NotifyBotUIDValue, Owner: "docs", ActionType: "access_request.decision",
		MessageID: "1001", ChannelID: NotifyBotUIDValue, ChannelType: 1, SpaceID: "card-space-a", OperatorUID: "reviewer-1",
		Data: map[string]any{"doc_id": "doc-1", "request_id": "request-1"},
	}
	result := cardactiondispatch.DecisionResult{
		State: cardactiondispatch.StateApproved,
		Display: map[string]string{
			"operator_name": "林澈", "operator_space_name": "产品中心", "operator_space_id": "untrusted-space-id",
		},
	}
	if err := finalizer.replaceWithRegistryResult(context.Background(), event, result,
		NotifyBotUIDValue, "zh-CN", "Roadmap", ""); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(updater.fields, &fields); err != nil {
		t.Fatal(err)
	}
	decision := fields["decision"].(map[string]any)
	if decision["operatorName"] != "林澈" || decision["operatorSpaceName"] != "产品中心" {
		t.Fatalf("decision display = %+v", decision)
	}
	if strings.Contains(string(updater.fields), "untrusted-space-id") {
		t.Fatalf("operator ids must not enter display fields: %s", updater.fields)
	}
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

// ---- PR-C Slice 1 (D3/D7): the finalizer's result edit pins the version the
// event's stored frame actually used — never a hardcoded constant, never a
// foreign template identity. ----

func storedVersionFinalizerFixture(t *testing.T) (*captureViewUpdater, *DocsActionFinalizer, cardactiondispatch.Event, cardactiondispatch.DecisionResult) {
	t.Helper()
	wk := newWuKongServer()
	t.Cleanup(wk.close)
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
	updater := &captureViewUpdater{}
	finalizer, err := NewDocsActionFinalizerWithUpdater(ctx, updater, &captureCardMutator{})
	if err != nil {
		t.Fatalf("NewDocsActionFinalizerWithUpdater: %v", err)
	}
	event := cardactiondispatch.Event{
		EventID: 42, SenderUID: NotifyBotUIDValue, Owner: "docs", ActionType: "access_request.decision",
		MessageID: "1001", ChannelID: NotifyBotUIDValue, ChannelType: 1, SpaceID: "space-1", OperatorUID: "reviewer-1",
		Data: map[string]any{"doc_id": "doc-1", "request_id": "request-1", "doc_title": "Roadmap"},
	}
	result := cardactiondispatch.DecisionResult{
		Disposition: cardactiondispatch.DispositionApplied, State: cardactiondispatch.StateApproved,
		RequesterUID: "user-a", Display: map[string]string{"title": "Roadmap"},
	}
	return updater, finalizer, event, result
}

func TestDocsActionFinalizerUsesEventStoredTemplateVersion(t *testing.T) {
	updater, finalizer, event, result := storedVersionFinalizerFixture(t)
	// A dynamic pilot frame stores its own exact version in the durable event.
	event.Card = cardactiondispatch.CardContext{
		TemplateID: string(docsaccessrequest.TemplateID), TemplateVersion: "0.4.0-test.1", View: "pending",
	}
	if err := finalizer.Finalize(context.Background(), event, result); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if updater.version != "0.4.0-test.1" {
		t.Fatalf("result edit version = %q, want the event's stored exact version", updater.version)
	}
	if updater.id != docsaccessrequest.TemplateID {
		t.Fatalf("result edit template = %q", updater.id)
	}
}

func TestDocsActionFinalizerLegacyEventKeepsStaticVersion(t *testing.T) {
	updater, finalizer, event, result := storedVersionFinalizerFixture(t)
	// Pre-registry durable events carry no card context.
	if err := finalizer.Finalize(context.Background(), event, result); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if updater.version != docsaccessrequest.TemplateVersionV3 {
		t.Fatalf("legacy result edit version = %q, want %q", updater.version, docsaccessrequest.TemplateVersionV3)
	}
}

func TestDocsActionFinalizerRejectsForeignStoredTemplate(t *testing.T) {
	updater, finalizer, event, result := storedVersionFinalizerFixture(t)
	event.Card = cardactiondispatch.CardContext{
		TemplateID: "summary.completed", TemplateVersion: "0.1.0", View: "default",
	}
	if err := finalizer.Finalize(context.Background(), event, result); err == nil {
		t.Fatal("foreign stored template identity finalized")
	}
	if updater.calls != 0 {
		t.Fatalf("foreign identity still edited the card: %d calls", updater.calls)
	}
}

// PR-C review rounds 5 and 7 (yujiawei): the replacement route must not be
// selected by state.
//
// The pending card is a Registry render and carries the server-authored
// catalog markers; the fallback rebuilds the frame from a six-key allowlist
// that holds neither, so routing a marked card there erased both on an ordinary
// user click and left the identity guards in pkg/cardtmpl/updater.go inert for
// that message. `cancelled` was the reported case and `pending` reaches the
// same branch, so an allowlist of "states that render through the Registry"
// would have gone stale the next time the contract grew a state — silently, in
// the direction of erasing markers. The route turns on whether an updater
// exists instead, and this table is what holds that shape.
//
// This test was written in round 7, reported as landed, and then removed by a
// later whole-section edit in the same session. Nothing went red, because a
// deleted test is not a failing test — which is the argument for the table
// rather than for one more single-state case.
func TestDocsActionFinalizerRoutesEveryStateThroughTheRegistry(t *testing.T) {
	for _, test := range []struct {
		name      string
		state     cardactiondispatch.State
		wantState cardtmpl.State
		wantWire  string
	}{
		{"approved", cardactiondispatch.StateApproved, docsaccessrequest.StateApproved, "approved"},
		{"denied", cardactiondispatch.StateDenied, docsaccessrequest.StateRejected, "rejected"},
		{"cancelled", cardactiondispatch.StateCancelled, docsaccessrequest.StateCancelled, "cancelled"},
		// The state that would have kept the defect alive after the cancelled
		// fix: valid, decodable, and previously routed to the legacy rebuild.
		{"pending", cardactiondispatch.StatePending, docsaccessrequest.StateUnavailable, "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			wk := newWuKongServer()
			defer wk.close()
			ctx := newTestContext(t, wk)
			ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
			updater := &captureViewUpdater{}
			legacyMutator := &captureCardMutator{}
			finalizer, err := NewDocsActionFinalizerWithUpdater(ctx, updater, legacyMutator)
			if err != nil {
				t.Fatalf("NewDocsActionFinalizerWithUpdater: %v", err)
			}
			event := cardactiondispatch.Event{
				EventID: 44, SenderUID: NotifyBotUIDValue, Owner: "docs", ActionType: "access_request.decision",
				MessageID: "1003", ChannelID: NotifyBotUIDValue, ChannelType: 1, SpaceID: "space-1",
				OperatorUID: "reviewer-1",
				Data: map[string]any{
					"doc_id": "doc-1", "request_id": "request-1", "doc_title": "Roadmap", "actor": "Alice",
				},
			}
			result := cardactiondispatch.DecisionResult{
				Disposition: cardactiondispatch.DispositionApplied, State: test.state,
				RequesterUID: "user-a", Display: map[string]string{"title": "Roadmap"},
			}
			if err := finalizer.Finalize(context.Background(), event, result); err != nil {
				t.Fatalf("Finalize(%s): %v", test.state, err)
			}
			if len(legacyMutator.requests) != 0 {
				t.Fatalf("%s took the marker-erasing legacy path", test.state)
			}
			if updater.state != test.wantState {
				t.Fatalf("registry state = %q, want %q", updater.state, test.wantState)
			}
			var fields map[string]any
			if err := json.Unmarshal(updater.fields, &fields); err != nil {
				t.Fatalf("decode fields: %v", err)
			}
			if fields["state"] != test.wantWire {
				t.Fatalf("wire state = %v, want %q", fields["state"], test.wantWire)
			}
			// Nobody decided cancelled or pending, so neither carries an
			// operator: emitting one would put the generic approver placeholder
			// on a card no one acted on.
			_, hasDecision := fields["decision"]
			wantDecision := test.state == cardactiondispatch.StateApproved ||
				test.state == cardactiondispatch.StateDenied
			if hasDecision != wantDecision {
				t.Fatalf("decision present = %v, want %v for %s", hasDecision, wantDecision, test.state)
			}
		})
	}
}

// The template must actually be able to render what the finalizer now asks of
// it — a state the manifest does not declare fails ViewFor with ErrStateUnknown,
// which would turn the routing fix into a runtime error instead of a card. The
// finalizer tests above drive a capturing fake and never reach ViewFor, so this
// is the only thing that witnesses the manifest declaration.
func TestBuildDocsAccessResultFieldsGatesV3OnlyFieldsByVersion(t *testing.T) {
	input := docsResultRenderInput{
		Data: map[string]any{
			"request_id": "request-1", "doc_id": "doc-1", "requester_space_name": "Space B",
			"requested_bot_names": []string{"Bot A"},
		},
		Title: "Roadmap", OperatorName: "Reviewer", OperatorSpaceName: "Operator Space",
	}
	for _, test := range []struct {
		version string
		wantV3  bool
	}{
		{version: docsaccessrequest.TemplateVersionV3, wantV3: true},
		{version: "0.4.0", wantV3: false},
	} {
		raw, _, err := buildDocsAccessResultFields("en", test.version, input)
		if err != nil {
			t.Fatalf("version %s: %v", test.version, err)
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		requester := fields["requester"].(map[string]any)
		decision := fields["decision"].(map[string]any)
		_, hasSpace := requester["sourceSpaceName"]
		_, hasOperatorSpace := decision["operatorSpaceName"]
		_, hasBots := fields["requestedBotNames"]
		if hasSpace != test.wantV3 || hasOperatorSpace != test.wantV3 || hasBots != test.wantV3 {
			t.Fatalf("version %s fields = %+v", test.version, fields)
		}
	}
}

func TestDocsAccessRequestV3RendersTheNonDecisionStates(t *testing.T) {
	registry := cardtmpl.NewRegistry()
	registry.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	registry.Freeze()

	for _, test := range []struct {
		name        string
		input       docsResultRenderInput
		wantState   cardtmpl.State
		wantVariant string
	}{
		{
			name:        "cancelled",
			input:       docsResultRenderInput{Cancelled: true},
			wantState:   docsaccessrequest.StateCancelled,
			wantVariant: docsaccessrequest.VariantCancelled,
		},
		{
			name:        "unavailable",
			input:       docsResultRenderInput{Unavailable: true},
			wantState:   docsaccessrequest.StateUnavailable,
			wantVariant: docsaccessrequest.VariantUnavailable,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := test.input
			input.Data = map[string]any{"request_id": "request-1", "doc_id": "doc-1"}
			input.Title = "Roadmap"
			fields, state, err := buildDocsAccessResultFields("zh-CN", docsaccessrequest.TemplateVersionV3, input)
			if err != nil {
				t.Fatalf("buildDocsAccessResultFields: %v", err)
			}
			if state != test.wantState {
				t.Fatalf("state = %q, want %q", state, test.wantState)
			}
			payload, err := registry.Render(context.Background(),
				docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3, state, fields,
				cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-1", WebLoginURL: "https://im.example.com/login"})
			if err != nil {
				t.Fatalf("render %s: %v", test.wantState, err)
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			// The render is what makes the markers legal on the replacement
			// frame: only a Registry render writes metadata.octo.template, which
			// template_ref must agree with.
			if tmplCtx, ok := cardmsg.CardTemplateContext(raw); !ok ||
				tmplCtx.ID != string(docsaccessrequest.TemplateID) ||
				tmplCtx.Version != docsaccessrequest.TemplateVersionV3 {
				t.Fatalf("%s render lost its template identity: %+v ok=%v", test.wantState, tmplCtx, ok)
			}
			if variant, _ := cardmsg.CardVariant(raw); variant != test.wantVariant {
				t.Fatalf("variant = %q, want %q", variant, test.wantVariant)
			}
			// Nobody decided these, so they carry no operator: emitting one
			// would put the generic approver placeholder on an untouched card.
			var decoded map[string]any
			if err := json.Unmarshal(fields, &decoded); err != nil {
				t.Fatal(err)
			}
			if _, present := decoded["decision"]; present {
				t.Fatalf("%s invented a decision: %+v", test.wantState, decoded["decision"])
			}
		})
	}
}

// markerEnforcingCardMutator applies the production preservation rule against a
// stored frame the test supplies. It exists because the notify package cannot
// construct a real CardMutator (its constructor takes an unexported backend),
// and the point of the test below is what StandardActionFinalizer's *output*
// would do at that boundary — not what the fake does.
//
// It calls cardmsg.CatalogMarkersPreserved rather than restating the
// comparison. An earlier version hand-copied the boolean logic and said it went
// "through the same exported helper" when no such helper existed; the copy
// inherited the original's presence-equality bug, so this fake reproduced the
// defect instead of catching it.
type markerEnforcingCardMutator struct {
	stored   []byte
	requests []carddispatch.CardMutationRequest
}

func (m *markerEnforcingCardMutator) Mutate(_ context.Context,
	request carddispatch.CardMutationRequest) (carddispatch.CardMutationResult, error) {
	stored, err := cardmsg.CatalogFrameMarkers(m.stored)
	if err != nil {
		return carddispatch.CardMutationResult{}, err
	}
	next, err := cardmsg.CatalogFrameMarkers([]byte(request.ContentEdit))
	if err != nil {
		return carddispatch.CardMutationResult{}, err
	}
	if err := cardmsg.CatalogMarkersPreserved(stored, next); err != nil {
		return carddispatch.CardMutationResult{}, carddispatch.ErrCardMutationInvalid
	}
	m.requests = append(m.requests, request)
	return carddispatch.CardMutationResult{Applied: true}, nil
}

// PR-C review round 7 (yujiawei): StandardActionFinalizer is the default
// finalizer for every owner except docs, it has no updater, and it routes every
// state — approved and denied included — through buildTerminalEnvelope.
//
// It is latent rather than live today, and the reason is worth pinning down
// rather than assuming: BuildApprovalRequestCard hand-builds its document
// without metadata.octo.protocol/template, so cardDocumentTemplateContext
// returns false and those cards are never marked in the first place. The moment
// any non-docs owner's card becomes a Registry render, it would be marked, and
// this finalizer would erase the markers on every finalize.
//
// The mutation boundary is what stops that becoming a silent regression, so
// this asserts the boundary refuses it — the finalizer fails loudly instead of
// downgrading a marked card. Today's unmarked cards keep working unchanged.
func TestStandardActionFinalizerCannotSilentlyDowngradeAMarkedCard(t *testing.T) {
	newEvent := func() (cardactiondispatch.Event, cardactiondispatch.DecisionResult) {
		return cardactiondispatch.Event{
				EventID: 85, SenderUID: NotifyBotUIDValue, Owner: "tasks", ActionType: "task.decision",
				MessageID: "2002", ChannelID: NotifyBotUIDValue, ChannelType: 1, SpaceID: "space-1",
				OperatorUID: "user-b",
			}, cardactiondispatch.DecisionResult{
				Disposition: cardactiondispatch.DispositionApplied,
				State:       cardactiondispatch.StateApproved, RequesterUID: "user-a",
				Display: map[string]string{"title": "Deploy release"},
			}
	}

	t.Run("a marked stored frame is refused, not erased", func(t *testing.T) {
		mutator := &markerEnforcingCardMutator{stored: markedNotifyFrame(t)}
		finalizer, err := NewStandardActionFinalizer(mutator, &capturingCardSender{})
		if err != nil {
			t.Fatalf("NewStandardActionFinalizer: %v", err)
		}
		event, result := newEvent()
		if err := finalizer.Finalize(context.Background(), event, result); err == nil {
			t.Fatal("a marked card was silently downgraded by the standard finalizer")
		}
		if len(mutator.requests) != 0 {
			t.Fatalf("a marker-erasing frame was persisted: %+v", mutator.requests)
		}
	})

	t.Run("today's unmarked cards are unaffected", func(t *testing.T) {
		mutator := &markerEnforcingCardMutator{stored: unmarkedNotifyFrame(t)}
		finalizer, err := NewStandardActionFinalizer(mutator, &capturingCardSender{})
		if err != nil {
			t.Fatalf("NewStandardActionFinalizer: %v", err)
		}
		event, result := newEvent()
		if err := finalizer.Finalize(context.Background(), event, result); err != nil {
			t.Fatalf("an unmarked card must still finalize: %v", err)
		}
		if len(mutator.requests) != 1 {
			t.Fatalf("mutation count = %d, want 1", len(mutator.requests))
		}
	})
}

func unmarkedNotifyFrame(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type": cardmsg.InteractiveCard.Int(), "card_version": cardmsg.CardVersion,
		"profile": cardmsg.ProfileV2, "space_id": "space-1", "card_seq": 1,
		"card": map[string]any{
			"type": "AdaptiveCard", "version": cardmsg.CardVersion,
			"body": []any{map[string]any{"type": "TextBlock", "text": "Deploy release"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// markedNotifyFrame is what a Registry-rendered card of this family would look
// like: the top-level markers plus the metadata.octo.template they are checked
// against.
func markedNotifyFrame(t *testing.T) []byte {
	t.Helper()
	var frame map[string]any
	if err := json.Unmarshal(unmarkedNotifyFrame(t), &frame); err != nil {
		t.Fatal(err)
	}
	frame["card"].(map[string]any)["metadata"] = map[string]any{
		"octo": map[string]any{
			"protocol": "octo-card@1.0",
			"template": map[string]any{"id": "tasks.decision", "version": "1.0.0"},
		},
	}
	frame[cardmsg.CatalogTemplateRefKey] = map[string]any{"id": "tasks.decision", "version": "1.0.0"}
	frame[cardmsg.CatalogProvenanceKey] = cardmsg.CatalogProvenance{
		Version:       cardmsg.CatalogProvenanceVersion,
		PrincipalType: cardmsg.CatalogPrincipalWireInternalProducer,
		PrincipalID:   "tasks-notify",
		SpaceID:       "space-1",
	}.MarshalMap()
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
