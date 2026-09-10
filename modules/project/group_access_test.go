package project

import "testing"

func TestUniqueGroupProjectIDsSortsAndDeduplicatesLockSet(t *testing.T) {
	got := uniqueGroupProjectIDs([]string{"p2", " p1 ", "p2", "", "p3", "p1"})
	want := []string{"p1", "p2", "p3"}
	if len(got) != len(want) {
		t.Fatalf("uniqueGroupProjectIDs length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("uniqueGroupProjectIDs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestGroupProjectCandidateExpandedDetectsOnlyNewMembers(t *testing.T) {
	if groupProjectCandidateExpanded([]string{"u1", "u2"}, []string{"u2", "u1"}) {
		t.Fatal("same membership in a different order must not force a retry")
	}
	if !groupProjectCandidateExpanded([]string{"u1"}, []string{"u1", "u2"}) {
		t.Fatal("a newly active Project member must force preparation retry")
	}
}
