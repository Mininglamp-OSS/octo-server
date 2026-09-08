package space

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
)

// errNotATestBinary is returned rather than panicking: a caller that reaches these
// in production gets an error it must handle, not a crashed process.
var errNotATestBinary = errors.New(
	"space: the *ForTest removal seams are only callable from a test binary")

func refuseOutsideTests() error {
	if !testing.Testing() {
		return errNotATestBinary
	}
	return nil
}

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
//
// # They REFUSE to run outside a test binary
//
// These functions enter the removal and reactivation transactions directly, past
// every handler-level check the real routes apply — operator identity, super-admin,
// role hierarchy. Nothing calls them in production today, but "nothing calls it" is
// a property of the current tree, not a boundary: any package in this binary could,
// and the writer guard cannot see a call that adds no statement.
//
// So the boundary is enforced at run time by testing.Testing(), which the stdlib
// sets only in a binary built by `go test`. A build tag was the alternative and is
// worse here: it silently excludes the file from `go vet ./...` and from any build
// that forgets the tag, so a typo inside would go unnoticed until someone ran the
// tests with it — whereas this compiles and is vetted everywhere, and simply cannot
// fire in the shipped binary.

// RemoveMemberForTest runs the single-member removal transaction — the kick and
// self-leave path — including its registered synchronous tx steps.
func RemoveMemberForTest(
	ctx *config.Context, spaceID, uid string, rejectRoleAtOrAbove int, operatorUID, reason string,
) (bool, error) {
	if err := refuseOutsideTests(); err != nil {
		return false, err
	}
	return removeMemberLocked(ctx.DB(), spaceID, uid, rejectRoleAtOrAbove, operatorUID, reason)
}

// RemoveMembersForceForTest runs the admin batch removal transaction — one
// transaction for the whole batch — including its registered synchronous tx steps.
func RemoveMembersForceForTest(
	ctx *config.Context, spaceID string, uids []string, operatorUID string,
) ([]string, error) {
	if err := refuseOutsideTests(); err != nil {
		return nil, err
	}
	return newManagerDB(ctx.DB()).removeMembersForce(spaceID, uids, operatorUID)
}

// ReactivateMemberForTest runs the single-member reactivation transaction — the
// invite/add-back path — including its registered synchronous tx steps.
func ReactivateMemberForTest(ctx *config.Context, spaceID, uid string, role int) error {
	if err := refuseOutsideTests(); err != nil {
		return err
	}
	return NewDB(ctx).reactivateMember(spaceID, uid, role)
}

// ReactivateMemberIfNotFullForTest runs the capacity-checked reactivation
// transaction — the join path — including its registered synchronous tx steps.
func ReactivateMemberIfNotFullForTest(ctx *config.Context, spaceID, uid string, maxUsers int) error {
	if err := refuseOutsideTests(); err != nil {
		return err
	}
	return NewDB(ctx).atomicReactivateMemberIfNotFull(spaceID, uid, maxUsers)
}
