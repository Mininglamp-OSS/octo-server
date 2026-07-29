package card_template_catalog

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
