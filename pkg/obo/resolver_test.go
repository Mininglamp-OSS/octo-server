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
	f := &fakeSnapshotReader{state: Snapshot{BotUID: "bot-1", OwnerUID: "human-1", GrantID: 7}}
	r := Resolver{Reader: f}
	_, err := r.Resolve(context.Background(), Request{BotToken: "bf_valid", SpaceID: "S", Mode: ModeOBO, Action: "project.read", Local: true})
	if DecisionCode(err) != "delegation_denied" {
		t.Fatalf("expected delegation denial, got %v", err)
	}
	f.state.BoundScopes = []string{"ALL"}
	p, err := r.Resolve(context.Background(), Request{BotToken: "bf_valid", SpaceID: "S", Mode: ModeOBO, Action: "project.read", Local: true})
	if err != nil || p.Subject.UID != "human-1" || p.Delegation.MatchedScope != "ALL" {
		t.Fatalf("principal=%+v err=%v", p, err)
	}
}

func TestResolverRequiresSpaceAndRegisteredAction(t *testing.T) {
	r := Resolver{Reader: &fakeSnapshotReader{state: Snapshot{BotUID: "bot-1"}}}
	if _, err := r.Resolve(context.Background(), Request{BotToken: "bf_valid", Mode: ModeAsBot}); DecisionCode(err) != "missing_space_id" {
		t.Fatalf("expected missing Space, got %v", err)
	}
	if _, err := r.Resolve(context.Background(), Request{BotToken: "bf_valid", SpaceID: "S", Mode: ModeOBO, Action: "unknown", Local: true}); DecisionCode(err) != "action_not_allowed" {
		t.Fatalf("expected Action denial, got %v", err)
	}
}
