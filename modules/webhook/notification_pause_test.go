package webhook

import "testing"

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
	if len(got) != len(uids) || &got[0] != &uids[0] {
		t.Fatalf("no-op filtering should preserve the input slice")
	}
}
