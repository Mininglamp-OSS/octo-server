package card_template_catalog

import (
	"strings"
	"testing"
)

// An emptied allowlist must fail closed, not fail to parse.
//
// approvedRuntimeOwners is a code constant, so this guards a future edit rather
// than a live defect — but the two outcomes are not equivalent. `owner IN ()` is
// a MySQL syntax error, so B1 and B2 would answer 5xx and read like an outage,
// while emptying an owner allowlist during an incident is supposed to mean
// "nothing is discoverable" (review P2, yujiawei).
func TestApprovedOwnerPredicateFailsClosedOnAnEmptyAllowlist(t *testing.T) {
	original := approvedRuntimeOwners
	approvedRuntimeOwners = map[string]struct{}{}
	t.Cleanup(func() { approvedRuntimeOwners = original })

	predicate, args := approvedOwnerPredicate("a.owner")
	if strings.Contains(predicate, "IN ()") {
		t.Fatalf("predicate = %q, which MySQL cannot parse", predicate)
	}
	if predicate != "1 = 0" || len(args) != 0 {
		t.Fatalf("predicate = %q args = %v, want a literal false with no arguments", predicate, args)
	}
}

// And the populated case still renders a real IN list, so the guard above
// cannot be satisfied by making the predicate constant.
func TestApprovedOwnerPredicateRendersTheAllowlist(t *testing.T) {
	predicate, args := approvedOwnerPredicate("a.owner")
	if len(args) == 0 {
		t.Fatal("the production allowlist rendered no arguments")
	}
	if !strings.HasPrefix(predicate, "a.owner IN (?") {
		t.Fatalf("predicate = %q, want an IN list over the approved owners", predicate)
	}
	if strings.Count(predicate, "?") != len(args) {
		t.Fatalf("predicate %q has %d placeholders for %d arguments",
			predicate, strings.Count(predicate, "?"), len(args))
	}
}
