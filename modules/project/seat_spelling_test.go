package project

// What this file pins:
//
//	`octo_project_member.uid` must hold the spelling `space_member` STORES, not the
//	spelling the admitting caller sent.
//
// # Why this is load-bearing rather than tidiness
//
// The epoch channel's remedy — resolving a seat transition's identifier back out of
// `space_member` before handing it to the invalidation step (modules/space/seatref.go)
// — is only sound if the project seat carries a spelling that `octo_project_member`'s
// own collation can match against that resolved one. Both reviewers of round 12 assert
// this holds "by invariant I1, by construction". It does not follow from I1: I1 says
// every active project seat belongs to an active Space member, which is a statement
// about MEMBERSHIP, not about bytes. `admitMemberTx` writes whatever uid its caller
// passed.
//
// What actually holds today is narrower and accidental: the admission gate looks the
// target up with projectpkg.FoldedHas over a set keyed by `space_member`'s spelling,
// FoldID is deliberately ASCII-only, so every spelling that DIVERGES between the two
// production collations (compatibility characters — fullwidth forms, KELVIN SIGN, OHM
// SIGN, the ordinal letters) fails that fold and is REFUSED before it can be written.
// Only ASCII case drift gets through, and general_ci is case-insensitive, so the
// enumeration still matches.
//
// That is a correctness argument resting on two mechanisms in different packages that
// nothing connects, and it fails in the dangerous direction under a change that looks
// like an improvement: making FoldID width-aware — strictly closer to what the database
// does — would start admitting drifted spellings into `octo_project_member`, and the
// removal-side resolution would silently stop matching them. Writing the canonical
// spelling removes the dependency instead of documenting it.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAdmissionStoresTheSpaceMemberSpelling drives the real admission path with a
// case-drifted uid over a seat stored under a different casing and asserts the project
// seat records the stored one.
//
// ASCII case is the only drift that can reach this code (see the file comment), which
// is exactly why it is the case worth pinning: it is the one spelling that gets
// through the fold and can therefore be written non-canonically today.
func TestAdmissionStoresTheSpaceMemberSpelling(t *testing.T) {
	srv, p := setup(t)

	const (
		stored  = "ssTargetMixed"
		drifted = "SSTARGETMIXED"
	)

	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "ssOwner")
	seedSpaceMember(t, spaceA, "ssOwner", 2, 1)
	seedUser(t, stored)
	seedSpaceMember(t, spaceA, stored, 0, 1)

	proj := createProjectVia(t, srv, spaceA, ownerToken, "seat-spelling")

	admitted, err := p.addOneMember(proj.ProjectID, spaceA, "ssOwner", drifted)
	require.NoError(t, err)
	require.True(t, admitted,
		"the drifted spelling must still be ADMITTED — space_member's collation matches it, "+
			"and refusing here would be a behaviour change, not a fix")

	var got []string
	_, err = testCtx.DB().SelectBySql(
		"SELECT uid FROM `octo_project_member` WHERE project_id = ?", proj.ProjectID).Load(&got)
	require.NoError(t, err)
	require.Contains(t, got, "ssOwner", "the owner seat must be there")

	var seatUID string
	for _, uid := range got {
		if uid != "ssOwner" {
			seatUID = uid
		}
	}
	assert.Equal(t, stored, seatUID,
		"the project seat must record the spelling space_member stores. The removal and "+
			"reactivation paths resolve their identifier out of space_member before handing it "+
			"to the epoch step; if the project seat holds a different spelling, that resolved "+
			"identifier enumerates nothing and the seat is orphaned with no epoch movement. "+
			"Today general_ci's case-insensitivity happens to bridge this particular gap — the "+
			"assertion is that the bytes agree, not that a collation rescues them.")
}
