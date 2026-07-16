package cardactiondispatch

import (
	"context"
	"testing"
)

type recordingFinalizer struct {
	events []Event
}

func (f *recordingFinalizer) Finalize(_ context.Context, event Event, _ DecisionResult) error {
	f.events = append(f.events, event)
	return nil
}

func TestFinalizerRegistryUsesSpecializedBindingAndStandardFallback(t *testing.T) {
	standard := &recordingFinalizer{}
	docs := &recordingFinalizer{}
	registry, err := NewFinalizerRegistry(standard, map[FinalizerKey]Finalizer{
		{Owner: "docs", ActionType: "access_request.decision"}: docs,
	})
	if err != nil {
		t.Fatalf("NewFinalizerRegistry() error = %v", err)
	}

	result := DecisionResult{Disposition: DispositionApplied, State: StateApproved, RequesterUID: "user-a"}
	if err := registry.Finalize(context.Background(), Event{Owner: "docs", ActionType: "access_request.decision"}, result); err != nil {
		t.Fatalf("Finalize(docs) error = %v", err)
	}
	if err := registry.Finalize(context.Background(), Event{Owner: "tasks", ActionType: "task.decision"}, result); err != nil {
		t.Fatalf("Finalize(tasks) error = %v", err)
	}
	if len(docs.events) != 1 || docs.events[0].Owner != "docs" {
		t.Fatalf("docs events = %+v", docs.events)
	}
	if len(standard.events) != 1 || standard.events[0].Owner != "tasks" {
		t.Fatalf("standard events = %+v", standard.events)
	}
}

func TestFinalizerRegistryRejectsInvalidBindings(t *testing.T) {
	standard := &recordingFinalizer{}
	tests := []struct {
		name     string
		fallback Finalizer
		bindings map[FinalizerKey]Finalizer
	}{
		{name: "missing fallback", bindings: map[FinalizerKey]Finalizer{}},
		{name: "invalid owner", fallback: standard, bindings: map[FinalizerKey]Finalizer{{Owner: "Bad Owner", ActionType: "x"}: standard}},
		{name: "invalid action", fallback: standard, bindings: map[FinalizerKey]Finalizer{{Owner: "docs", ActionType: ""}: standard}},
		{name: "nil specialized finalizer", fallback: standard, bindings: map[FinalizerKey]Finalizer{{Owner: "docs", ActionType: "x"}: nil}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewFinalizerRegistry(tt.fallback, tt.bindings); err == nil {
				t.Fatal("NewFinalizerRegistry() error = nil")
			}
		})
	}
}
