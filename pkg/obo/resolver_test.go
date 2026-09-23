package obo

import (
	"context"
	"testing"
)

type fakeSnapshotReader struct {
	state Snapshot
	err   error
	mode  Mode
}

func (f *fakeSnapshotReader) Read(_ context.Context, _, _ string, mode Mode) (Snapshot, error) {
	f.mode = mode
	return f.state, f.err
}

func TestResolverAsBotDoesNotReadDelegation(t *testing.T) {
	f := &fakeSnapshotReader{state: Snapshot{BotUID: "bot-1"}}
	r := Resolver{Reader: f}
	p, err := r.Resolve(context.Background(), Request{BotToken: "bf_valid", SpaceID: "S", Mode: ModeAsBot})
	if err != nil || p.Actor.UID != "bot-1" || p.Subject.UID != "bot-1" || p.Delegation != nil || f.mode != ModeAsBot {
		t.Fatalf("principal=%+v err=%v mode=%s", p, err, f.mode)
	}
}

func TestResolverOBONeverFallsBack(t *testing.T) {
	registry, err := ParseActionRegistry(`{"all":["ALL"]}`)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSnapshotReader{state: Snapshot{BotUID: "bot-1", OwnerUID: "human-1", GrantID: 7}}
	r := Resolver{Reader: f, Registry: registry}
	_, err = r.Resolve(context.Background(), Request{BotToken: "bf_valid", SpaceID: "S", Mode: ModeOBO, Action: "all"})
	if DecisionCode(err) != "delegation_denied" {
		t.Fatalf("expected delegation denial, got %v", err)
	}
	f.state.BoundScopes = []string{"ALL"}
	p, err := r.Resolve(context.Background(), Request{BotToken: "bf_valid", SpaceID: "S", Mode: ModeOBO, Action: "all"})
	if err != nil || p.Subject.UID != "human-1" || p.Delegation.MatchedScope != "ALL" {
		t.Fatalf("principal=%+v err=%v", p, err)
	}
}

func TestResolverRequiresSpaceAndRegisteredAction(t *testing.T) {
	r := Resolver{Reader: &fakeSnapshotReader{state: Snapshot{BotUID: "bot-1"}}}
	if _, err := r.Resolve(context.Background(), Request{BotToken: "bf_valid", Mode: ModeAsBot}); DecisionCode(err) != "missing_space_id" {
		t.Fatalf("expected missing Space, got %v", err)
	}
	if _, err := r.Resolve(context.Background(), Request{BotToken: "bf_valid", SpaceID: "S", Mode: ModeOBO, Action: "unknown"}); DecisionCode(err) != "action_not_allowed" {
		t.Fatalf("expected Action denial, got %v", err)
	}
}

func TestResolverExternalActionRequiresRegistryAndALL(t *testing.T) {
	registry, err := ParseActionRegistry(`{"task.read":["ALL"]}`)
	if err != nil {
		t.Fatal(err)
	}
	reader := &fakeSnapshotReader{state: Snapshot{BotUID: "bot-1", OwnerUID: "human-1", GrantID: 7}}
	resolver := Resolver{Reader: reader, Registry: registry}
	req := Request{BotToken: "bf_valid", SpaceID: "S", Mode: ModeOBO, Action: "task.read"}
	if _, err := resolver.Resolve(context.Background(), req); DecisionCode(err) != "delegation_denied" {
		t.Fatalf("missing ALL binding must deny external Action, got %v", err)
	}
	reader.state.BoundScopes = []string{"ALL"}
	principal, err := resolver.Resolve(context.Background(), req)
	if err != nil || principal.Subject.UID != "human-1" || principal.Delegation.Action != "task.read" {
		t.Fatalf("external Action resolution failed: principal=%+v err=%v", principal, err)
	}
	req.Action = "task.delete"
	if _, err := resolver.Resolve(context.Background(), req); DecisionCode(err) != "action_not_allowed" {
		t.Fatalf("unregistered external Action must deny, got %v", err)
	}
}
