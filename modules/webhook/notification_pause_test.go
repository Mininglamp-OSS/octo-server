package webhook

import (
	"context"
	"testing"
)

func TestExcludePausedUIDs(t *testing.T) {
	uids := []string{"u1", "u2", "u3", "u2"}
	got := excludePausedUIDs(uids, map[string]struct{}{"u2": {}})
	want := []string{"u1", "u3"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
}

func TestExcludePausedUIDsKeepsInputWhenNoPausedUID(t *testing.T) {
	uids := []string{"u1", "u2"}
	got := excludePausedUIDs(uids, nil)
	if len(got) != len(uids) || got[0] != uids[0] || got[1] != uids[1] {
		t.Fatalf("no-op filtering should preserve the recipients, got %v", got)
	}
	if &got[0] == &uids[0] {
		t.Fatalf("no-op filtering should not alias the input slice")
	}
}

func TestFilterPausedUIDsFailsOpenWhenLookupFails(t *testing.T) {
	uids := []string{"u1", "u2"}
	got := filterPausedUIDs(uids, map[string]struct{}{"u1": {}}, context.DeadlineExceeded)
	if len(got) != len(uids) || got[0] != "u1" || got[1] != "u2" {
		t.Fatalf("lookup failure must preserve the original recipients, got %v", got)
	}
}

func TestFilterPausedUIDsExcludesOnlyKnownPausedUIDs(t *testing.T) {
	got := filterPausedUIDs([]string{"u1", "u2"}, map[string]struct{}{"u1": {}}, nil)
	if len(got) != 1 || got[0] != "u2" {
		t.Fatalf("expected only paused uid to be removed, got %v", got)
	}
}
