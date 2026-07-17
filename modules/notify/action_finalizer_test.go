package notify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

type captureCardMutator struct {
	requests []carddispatch.CardMutationRequest
}

func (m *captureCardMutator) Mutate(_ context.Context, request carddispatch.CardMutationRequest) (carddispatch.CardMutationResult, error) {
	m.requests = append(m.requests, request)
	return carddispatch.CardMutationResult{Applied: true}, nil
}

func TestDocsActionFinalizerUsesEventIDAsCardSeqAndNotifiesRequester(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
	mutator := &captureCardMutator{}
	sender := &capturingCardSender{}
	finalizer, err := NewDocsActionFinalizer(ctx, mutator, sender)
	if err != nil {
		t.Fatalf("NewDocsActionFinalizer() error = %v", err)
	}
	event := cardactiondispatch.Event{
		EventID: 42, SenderUID: NotifyBotUIDValue, Owner: "docs", ActionType: "access_request.decision",
		MessageID: "1001", ChannelID: NotifyBotUIDValue, ChannelType: 1, SpaceID: "space-1",
		OperatorUID: "user-b",
		Data:        map[string]interface{}{"doc_id": "doc-1", "request_id": "request-1"},
	}
	result := cardactiondispatch.DecisionResult{
		Disposition:  cardactiondispatch.DispositionApplied,
		State:        cardactiondispatch.StateApproved,
		RequesterUID: "user-a",
		Display:      map[string]string{"title": "Roadmap", "approver": "must-not-render", "reason": "must-not-render"},
	}
	if err := finalizer.Finalize(context.Background(), event, result); err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	if len(mutator.requests) != 1 {
		t.Fatalf("mutation count = %d, want 1", len(mutator.requests))
	}
	mutation := mutator.requests[0]
	if mutation.SenderUID != NotifyBotUIDValue || mutation.MessageID != "1001" || mutation.ChannelID != "user-b" {
		t.Fatalf("mutation target = %+v", mutation)
	}
	var terminal map[string]interface{}
	if err := json.Unmarshal([]byte(mutation.ContentEdit), &terminal); err != nil {
		t.Fatalf("decode terminal content_edit: %v", err)
	}
	if terminal["card_seq"] != float64(42) || terminal["profile"] != cardmsg.ProfileV2 {
		t.Fatalf("terminal envelope = %+v", terminal)
	}
	card, _ := terminal["card"].(map[string]interface{})
	if _, hasActions := card["actions"]; hasActions {
		t.Fatal("terminal card must remove approval actions")
	}
	if strings.Contains(mutation.ContentEdit, "must-not-render") {
		t.Fatal("terminal card leaked unreviewed callback display fields")
	}

	outcome := sender.last()
	if target := sender.lastTarget(); target.ChannelID != result.RequesterUID || target.SpaceID != event.SpaceID {
		t.Fatalf("applicant outcome target = %+v, want requester %q in space %q", target, result.RequesterUID, event.SpaceID)
	}
	if outcome.Profile != cardmsg.ProfileV1 {
		t.Fatalf("applicant outcome profile = %q, want octo/v1", outcome.Profile)
	}
	if strings.Contains(string(outcome.Document), "must-not-render") {
		t.Fatal("applicant outcome leaked approver/reason")
	}

	// Replayed callbacks render byte-identical content. The second applicant
	// notification is explicitly allowed by the at-least-once boundary.
	if err := finalizer.Finalize(context.Background(), event, result); err != nil {
		t.Fatalf("Finalize(replay) error = %v", err)
	}
	if len(mutator.requests) != 2 || mutator.requests[0].ContentEdit != mutator.requests[1].ContentEdit {
		t.Fatal("replay did not produce a byte-identical deterministic terminal frame")
	}
}

func TestDocsActionFinalizerEnrichedTerminalAndDenyReason(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"

	build := func(state cardactiondispatch.State, inputs map[string]interface{}) string {
		mutator := &captureCardMutator{}
		finalizer, err := NewDocsActionFinalizer(ctx, mutator, &capturingCardSender{})
		if err != nil {
			t.Fatalf("NewDocsActionFinalizer() error = %v", err)
		}
		event := cardactiondispatch.Event{
			EventID: 7, SenderUID: NotifyBotUIDValue, Owner: "docs", ActionType: "access_request.decision",
			MessageID: "1001", ChannelID: NotifyBotUIDValue, ChannelType: 1, SpaceID: "space-1",
			OperatorUID: "user-b",
			Data:        map[string]interface{}{"doc_id": "doc-1", "request_id": "request-1"},
			Inputs:      inputs,
		}
		result := cardactiondispatch.DecisionResult{
			Disposition: cardactiondispatch.DispositionApplied, State: state,
			RequesterUID: "user-a", Display: map[string]string{"title": "Roadmap"},
		}
		if err := finalizer.Finalize(context.Background(), event, result); err != nil {
			t.Fatalf("Finalize() error = %v", err)
		}
		if len(mutator.requests) != 1 {
			t.Fatalf("mutation count = %d, want 1", len(mutator.requests))
		}
		return mutator.requests[0].ContentEdit
	}

	approved := build(cardactiondispatch.StateApproved, nil)
	if !strings.Contains(approved, "已允许") || !strings.Contains(approved, "申请人已获得所申请的文档权限。") {
		t.Fatalf("approved terminal card missing enriched result box: %s", approved)
	}

	// The reviewer deny reason rides in via event.Inputs and is surfaced on the
	// denied terminal card.
	denied := build(cardactiondispatch.StateDenied, map[string]interface{}{
		cardtmpl.DocsDenyReasonInputID: "范围不符，请缩小到 Q3",
	})
	if !strings.Contains(denied, "已拒绝") || !strings.Contains(denied, "范围不符，请缩小到 Q3") {
		t.Fatalf("denied terminal card missing status or reason: %s", denied)
	}
}

func TestDocsActionFinalizerRejectsTerminalResultWithoutRequesterBeforeMutation(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
	mutator := &captureCardMutator{}
	finalizer, err := NewDocsActionFinalizer(ctx, mutator, &capturingCardSender{})
	if err != nil {
		t.Fatalf("NewDocsActionFinalizer() error = %v", err)
	}
	event := cardactiondispatch.Event{
		EventID: 42, SenderUID: NotifyBotUIDValue, Owner: "docs", ActionType: "access_request.decision",
		MessageID: "1001", ChannelID: "user-b", ChannelType: 1, SpaceID: "space-1",
		Data: map[string]interface{}{"doc_id": "doc-1", "request_id": "request-1"},
	}
	result := cardactiondispatch.DecisionResult{
		Disposition: cardactiondispatch.DispositionApplied,
		State:       cardactiondispatch.StateApproved,
		Display:     map[string]string{"title": "Roadmap"},
	}
	if err := finalizer.Finalize(context.Background(), event, result); err == nil {
		t.Fatal("Finalize() error = nil for terminal result without requester_uid")
	}
	if len(mutator.requests) != 0 {
		t.Fatalf("mutation count = %d, want 0 before requester validation", len(mutator.requests))
	}
}

func TestDocsActionFinalizerUsesAuthoritativeMutationChannel(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
	tests := []struct {
		name        string
		channelID   string
		channelType uint8
		operatorUID string
		wantChannel string
		wantErr     bool
	}{
		{
			name: "DM requires operator", channelID: NotifyBotUIDValue,
			channelType: common.ChannelTypePerson.Uint8(), wantErr: true,
		},
		{
			name: "group keeps event channel", channelID: "group-1",
			channelType: common.ChannelTypeGroup.Uint8(), operatorUID: "user-b", wantChannel: "group-1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutator := &captureCardMutator{}
			finalizer, err := NewDocsActionFinalizer(ctx, mutator, &capturingCardSender{})
			if err != nil {
				t.Fatalf("NewDocsActionFinalizer() error = %v", err)
			}
			event := cardactiondispatch.Event{
				EventID: 42, SenderUID: NotifyBotUIDValue, Owner: "docs", ActionType: "access_request.decision",
				MessageID: "1001", ChannelID: tt.channelID, ChannelType: tt.channelType, SpaceID: "space-1",
				OperatorUID: tt.operatorUID, Data: map[string]interface{}{"doc_id": "doc-1", "request_id": "request-1"},
			}
			result := cardactiondispatch.DecisionResult{
				Disposition: cardactiondispatch.DispositionApplied, State: cardactiondispatch.StateCancelled,
				Display: map[string]string{"title": "Roadmap"},
			}
			err = finalizer.Finalize(context.Background(), event, result)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Finalize() error = nil")
				}
				if len(mutator.requests) != 0 {
					t.Fatalf("mutation count = %d, want 0", len(mutator.requests))
				}
				return
			}
			if err != nil {
				t.Fatalf("Finalize() error = %v", err)
			}
			if len(mutator.requests) != 1 || mutator.requests[0].ChannelID != tt.wantChannel {
				t.Fatalf("mutation requests = %+v, want channel %q", mutator.requests, tt.wantChannel)
			}
		})
	}
}

func TestStandardActionFinalizerSupportsNewOwnerWithoutOwnerSpecificCode(t *testing.T) {
	mutator := &captureCardMutator{}
	sender := &capturingCardSender{}
	finalizer, err := NewStandardActionFinalizer(mutator, sender)
	if err != nil {
		t.Fatalf("NewStandardActionFinalizer() error = %v", err)
	}
	event := cardactiondispatch.Event{
		EventID: 84, SenderUID: NotifyBotUIDValue, Owner: "tasks", ActionType: "task.decision",
		MessageID: "2001", ChannelID: NotifyBotUIDValue, ChannelType: 1, SpaceID: "space-1",
		OperatorUID: "user-b",
	}
	result := cardactiondispatch.DecisionResult{
		Disposition: cardactiondispatch.DispositionApplied,
		State:       cardactiondispatch.StateApproved, RequesterUID: "user-a",
		Display: map[string]string{"title": "Deploy release"},
	}
	if err := finalizer.Finalize(context.Background(), event, result); err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	if len(mutator.requests) != 1 {
		t.Fatalf("mutation count = %d, want 1", len(mutator.requests))
	}
	mutation := mutator.requests[0]
	if mutation.ChannelID != event.OperatorUID || mutation.SenderUID != event.SenderUID {
		t.Fatalf("mutation target = %+v", mutation)
	}
	if _, err := cardmsg.NormalizeContentEdit(mutation.ContentEdit); err != nil {
		t.Fatalf("standard terminal content_edit is invalid: %v", err)
	}
	var terminal map[string]interface{}
	if err := json.Unmarshal([]byte(mutation.ContentEdit), &terminal); err != nil {
		t.Fatalf("decode terminal content_edit: %v", err)
	}
	if terminal["card_seq"] != float64(event.EventID) || terminal["profile"] != cardmsg.ProfileV2 {
		t.Fatalf("terminal envelope = %+v", terminal)
	}
	if !strings.Contains(mutation.ContentEdit, "Deploy release") || !strings.Contains(mutation.ContentEdit, "approval.approved") {
		t.Fatalf("standard terminal card = %s", mutation.ContentEdit)
	}
	card, _ := terminal["card"].(map[string]interface{})
	if _, hasActions := card["actions"]; hasActions {
		t.Fatal("standard terminal card must not retain actions")
	}

	if target := sender.lastTarget(); target.ChannelID != result.RequesterUID || target.SpaceID != event.SpaceID {
		t.Fatalf("requester outcome target = %+v", target)
	}
	if outcome := sender.last(); outcome.Profile != cardmsg.ProfileV1 || !strings.Contains(string(outcome.Document), "Deploy release") {
		t.Fatalf("requester outcome = %+v", outcome)
	}
}

func TestStandardActionFinalizerRejectsMissingTerminalContextBeforeMutation(t *testing.T) {
	mutator := &captureCardMutator{}
	finalizer, err := NewStandardActionFinalizer(mutator, &capturingCardSender{})
	if err != nil {
		t.Fatalf("NewStandardActionFinalizer() error = %v", err)
	}
	base := cardactiondispatch.Event{
		EventID: 1, SenderUID: NotifyBotUIDValue, Owner: "tasks", ActionType: "task.decision",
		MessageID: "1", ChannelID: NotifyBotUIDValue, ChannelType: common.ChannelTypePerson.Uint8(),
		SpaceID: "space-1", OperatorUID: "user-b",
	}
	result := cardactiondispatch.DecisionResult{Disposition: cardactiondispatch.DispositionApplied, State: cardactiondispatch.StateApproved}
	if err := finalizer.Finalize(context.Background(), base, result); err == nil {
		t.Fatal("Finalize(missing requester_uid) error = nil")
	}
	result.RequesterUID = "user-a"
	base.SpaceID = ""
	if err := finalizer.Finalize(context.Background(), base, result); err == nil {
		t.Fatal("Finalize(missing space_id) error = nil")
	}
	if len(mutator.requests) != 0 {
		t.Fatalf("mutation count = %d, want 0", len(mutator.requests))
	}
}

func TestStandardActionFinalizerUsesAuthoritativeGroupChannel(t *testing.T) {
	mutator := &captureCardMutator{}
	sender := &capturingCardSender{}
	finalizer, err := NewStandardActionFinalizer(mutator, sender)
	if err != nil {
		t.Fatalf("NewStandardActionFinalizer() error = %v", err)
	}
	event := cardactiondispatch.Event{
		EventID: 2, SenderUID: "tasks-bot", Owner: "tasks", ActionType: "task.decision",
		MessageID: "2", ChannelID: "group-1", ChannelType: common.ChannelTypeGroup.Uint8(),
		SpaceID: "space-1",
	}
	result := cardactiondispatch.DecisionResult{Disposition: cardactiondispatch.DispositionApplied, State: cardactiondispatch.StateCancelled}
	if err := finalizer.Finalize(context.Background(), event, result); err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	if len(mutator.requests) != 1 || mutator.requests[0].ChannelID != event.ChannelID {
		t.Fatalf("mutation requests = %+v", mutator.requests)
	}
	if len(sender.cards) != 0 {
		t.Fatalf("non-terminal requester notification count = %d, want 0", len(sender.cards))
	}
}
