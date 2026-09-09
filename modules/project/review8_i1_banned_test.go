package project

// PR #841 review round 5 (yujiawei S-1/P0-2, independently confirmed by Jerry-Xin as B-2).
//
// The spec exempts MEMBERS of a banned Space from the I1 scan, matching
// CheckMembershipForCleanup (`space_member.status = 1 AND space.status <> 0`). The implemented
// predicate exempted every active project seat whose Space is banned, member or not — because its
// own fourth clause used `space.status = 1` (the AUTHORIZATION predicate), which flags a banned
// Space's members, which is why a blanket third clause was added to suppress them.
//
// The two clauses cancelled each other out into something broader than either: inside a banned
// Space, a seat whose Space seat is already closed and whose cleanup never completed — a genuine
// leak — was invisible to project_i1_violations forever.
//
// The sibling scan already had this right: queryAbandonedLeakPage uses `s.status <> 0` and needs
// no blanket clause. Two scans disagreeing about the predicate they both claim to mirror is the
// tell.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestI1ScanUsesCleanupSemanticsForBannedSpaces pins BOTH directions, because either one alone is
// satisfiable by a predicate that is wrong in the other direction.
func TestI1ScanUsesCleanupSemanticsForBannedSpaces(t *testing.T) {
	t.Run("a banned Space MEMBER is exempt", func(t *testing.T) {
		srv, p := setup(t)
		projectWithMembers(t, srv, "keeps_seat")
		require.Equal(t, 0, violationCount(t, p), "precondition: a healthy Space flags nothing")

		// The seat stays ACTIVE (status = 1) — this is what "member of a banned Space" means
		// under CheckMembershipForCleanup, and it is the case the cascade genuinely skips.
		setSpaceStatus(t, spaceA, 2)
		assert.Equal(t, 0, violationCount(t, p),
			"a member of a banned Space is still a member: the cascade skips them, so the scan "+
				"must too, or a ban would flag every seat in the Space forever")
	})

	t.Run("a banned Space NON-member IS flagged", func(t *testing.T) {
		srv, p := setup(t)
		projectWithMembers(t, srv, "lost_seat")

		// Space seat CLOSED, then the Space banned, and no cleanup job on record. The cascade
		// does NOT skip this one — deactivateSeatForCascade proceeds when stillMember is false —
		// so the seat is a real leak and the scan has to see it.
		removeSpaceMember(t, spaceA, "lost_seat")
		require.Equal(t, 1, violationCount(t, p), "precondition: flagged while the Space is active")

		setSpaceStatus(t, spaceA, 2)
		assert.Equal(t, 1, violationCount(t, p),
			"banning the Space must not hide a seat whose Space seat is already closed — that is "+
				"exactly the cleanup leak this scan exists to find, and nothing else reports it")
	})

	t.Run("the two scans agree on the predicate they both mirror", func(t *testing.T) {
		// Same fixture, both scans. They claim to mirror CheckMembershipForCleanup; if they
		// disagree, at least one is wrong regardless of which reading of the spec is preferred.
		srv, p := setup(t)
		projectWithMembers(t, srv, "agree1")
		removeSpaceMember(t, spaceA, "agree1")
		setSpaceStatus(t, spaceA, 2)

		i1 := violationCount(t, p)
		enqueueCleanupJob(t, spaceA, "agree1", cleanupStatusAbandoned)
		leak := countAbandonedLeak(t, p)

		assert.Equal(t, 1, leak,
			"the abandoned-leak scan counts this seat (cleanup semantics: status <> 0)")
		assert.Equal(t, 1, i1,
			"so the I1 scan must count it too before a job existed — the two scans cannot "+
				"disagree about whether a banned Space's non-member holds a leaked seat")
	})
}
