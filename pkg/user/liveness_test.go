package user

import "testing"

// Unit-level guards for the argument handling. The predicate itself is covered
// against a real engine from modules/project, where the `user` table and its
// fixtures live — this package has no database harness of its own, exactly like
// pkg/space.
func TestActiveAccountsEmptyArgs(t *testing.T) {
	live, err := ActiveAccounts(nil, nil)
	if err != nil {
		t.Fatalf("nil uids must not error: %v", err)
	}
	if live == nil {
		t.Error("the map must never be nil on success — callers index it directly")
	}
	if len(live) != 0 {
		t.Errorf("expected an empty map, got %d entries", len(live))
	}

	live, err = ActiveAccounts(nil, []string{})
	if err != nil {
		t.Fatalf("empty uids must not error: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("expected an empty map, got %d entries", len(live))
	}

	// All-empty input must short-circuit BEFORE the query: an empty string in an
	// IN list matches nothing but still costs a bind parameter, and callers reach
	// this straight off the wire.
	live, err = ActiveAccounts(nil, []string{"", ""})
	if err != nil {
		t.Fatalf("all-empty uids must short-circuit rather than query: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("expected an empty map, got %d entries", len(live))
	}
}
