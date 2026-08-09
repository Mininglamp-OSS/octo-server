package card_template_catalog

import (
	"sort"
	"strings"
)

// approvedRuntimeOwners is the E3 v1 server-authored owner policy. Adding an
// owner is a reviewed code/config capability change; a manifest cannot grant
// itself authority by declaring an arbitrary owner or an ext.* namespace.
var approvedRuntimeOwners = map[string]struct{}{
	"ai":   {},
	"docs": {},
}

func isApprovedRuntimeOwner(owner string) bool {
	_, ok := approvedRuntimeOwners[owner]
	return ok
}

// approvedRuntimeOwnerList is the same policy in the shape SQL needs, sorted so
// the generated predicate is stable across processes.
//
// B1 and B2 have to agree on this or the allowlist stops being a kill switch:
// B2 resolves through the runtime authorizer, which rejects an unapproved
// owner, while B1's predicate had no owner clause at all. Narrow the list
// during an incident and B1 would keep listing every affected template with
// full metadata — and keep spending LIMIT slots on rows B2 answers not-found
// for. Deriving both from this one source is what stops them drifting.
func approvedRuntimeOwnerList() []string {
	owners := make([]string, 0, len(approvedRuntimeOwners))
	for owner := range approvedRuntimeOwners {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	return owners
}

// approvedOwnerPredicate renders `a.owner IN (?,?)` plus its arguments.
//
// An empty allowlist renders a literal false rather than `owner IN ()`, which
// is a MySQL syntax error (review P2, yujiawei). The allowlist is a code
// constant today, so this is a guard against a future edit rather than a live
// defect — but the two failure shapes are very different: `1 = 0` means B1 and
// B2 answer "nothing is discoverable", which is what emptying an owner
// allowlist during an incident is *for*, while a syntax error takes the
// endpoints down with a 5xx and reads like an outage.
func approvedOwnerPredicate(column string) (string, []any) {
	owners := approvedRuntimeOwnerList()
	if len(owners) == 0 {
		return "1 = 0", nil
	}
	placeholders := make([]string, 0, len(owners))
	args := make([]any, 0, len(owners))
	for _, owner := range owners {
		placeholders = append(placeholders, "?")
		args = append(args, owner)
	}
	return column + " IN (" + strings.Join(placeholders, ",") + ")", args
}
