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
func approvedOwnerPredicate(column string) (string, []any) {
	owners := approvedRuntimeOwnerList()
	placeholders := make([]string, 0, len(owners))
	args := make([]any, 0, len(owners))
	for _, owner := range owners {
		placeholders = append(placeholders, "?")
		args = append(args, owner)
	}
	return column + " IN (" + strings.Join(placeholders, ",") + ")", args
}
