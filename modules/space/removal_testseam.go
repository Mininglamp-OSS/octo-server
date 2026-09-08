package space

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
)

// Test-only entry points into the two member-removal transactions.
//
// Why exported production code rather than an _test.go helper: the transactions
// live in THIS package (unexported), and the test that has to exercise them lives
// in modules/project — a `_test.go` file here is not visible from there, and
// modules/project cannot import modules/space's test binary.
//
// The alternative was driving the HTTP handlers from modules/project, which needs
// this module's auth fixtures, the Space middleware and a role-bearing operator
// token. That machinery is what made the epoch bump untested in the first place:
// modules/space's own tx-step tests register STUBS, and modules/project's tests
// hand-rolled their own transaction — so the real statement had never run inside
// the real removal transaction in any lane. A two-function seam is the cheaper
// price.
//
// `ForTest` in the name and nothing in production calling them; the source guard
// over space_member writers counts the statements in db_manager.go, so these
// wrappers cannot become a way to add an unaccounted writer.

// RemoveMemberForTest runs the single-member removal transaction — the kick and
// self-leave path — including its registered synchronous tx steps.
func RemoveMemberForTest(
	ctx *config.Context, spaceID, uid string, rejectRoleAtOrAbove int, operatorUID, reason string,
) (bool, error) {
	return removeMemberLocked(ctx.DB(), spaceID, uid, rejectRoleAtOrAbove, operatorUID, reason)
}

// RemoveMembersForceForTest runs the admin batch removal transaction — one
// transaction for the whole batch — including its registered synchronous tx steps.
func RemoveMembersForceForTest(
	ctx *config.Context, spaceID string, uids []string, operatorUID string,
) ([]string, error) {
	return newManagerDB(ctx.DB()).removeMembersForce(spaceID, uids, operatorUID)
}
