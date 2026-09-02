package space

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedInitialSpace 建一个正常状态的 Space 并放进一个 owner。
//
// owner 行不是装饰:它占掉一个名额,所以 maxUsers=1 的用例天然就是"满员",不必再
// 造第二个成员;也让 max_users=0(不限)与 >0 两条分支用同一个 fixture。
func seedInitialSpace(t *testing.T, f *Space, spaceID string, maxUsers, joinMode int) {
	t.Helper()
	err := f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId:  spaceID,
		Name:     "初始空间",
		Creator:  "owner-uid",
		MaxUsers: maxUsers,
		JoinMode: joinMode,
		Status:   SpaceStatusNormal,
	})
	assert.NoError(t, err)
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID, UID: "owner-uid", Role: 2, Status: 1,
	})
	assert.NoError(t, err)
}

func countMemberRows(t *testing.T, spaceID, uid string) int {
	t.Helper()
	var n int
	_, err := testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=?", spaceID, uid).Load(&n)
	assert.NoError(t, err)
	return n
}

// TestAutoJoinInitialSpace_JoinsAsOrdinaryMember covers acceptance 1/2 at the
// unit the two OIDC entry points share: role must be 0, not the 2 that
// createSpaceCoreTx writes for an owner — an auto-joined SSO user is a member,
// never an administrator of the Space they landed in.
func TestAutoJoinInitialSpace_JoinsAsOrdinaryMember(t *testing.T) {
	_, f, _ := setup(t)
	const spaceID = "sp-initial-join"
	seedInitialSpace(t, f, spaceID, 0, JoinModeDirect)

	outcome, err := AutoJoinInitialSpace(testCtx, "u-sso-1", spaceID)
	assert.NoError(t, err)
	assert.Equal(t, InitialSpaceJoined, outcome)

	m, err := f.db.queryMemberIncludeRemoved(spaceID, "u-sso-1")
	assert.NoError(t, err)
	if assert.NotNil(t, m) {
		assert.Equal(t, 0, m.Role, "auto-joined SSO user must be an ordinary member")
		assert.Equal(t, 1, m.Status)
	}
}

// TestAutoJoinInitialSpace_IsIdempotent covers acceptance 4. The second call must
// be a no-op that still reads as success: the OIDC hook has no way to retry
// selectively, so a duplicate call has to be free rather than producing an error
// counter that would page someone.
func TestAutoJoinInitialSpace_IsIdempotent(t *testing.T) {
	_, f, _ := setup(t)
	const spaceID = "sp-initial-idem"
	seedInitialSpace(t, f, spaceID, 0, JoinModeDirect)

	first, err := AutoJoinInitialSpace(testCtx, "u-sso-2", spaceID)
	assert.NoError(t, err)
	assert.Equal(t, InitialSpaceJoined, first)

	second, err := AutoJoinInitialSpace(testCtx, "u-sso-2", spaceID)
	assert.NoError(t, err)
	assert.Equal(t, InitialSpaceAlreadyMember, second)

	assert.Equal(t, 1, countMemberRows(t, spaceID, "u-sso-2"), "exactly one member row")
}

// TestAutoJoinInitialSpace_DoesNotReactivateRemovedMember is the load-bearing
// difference from executeJoinSpace, which reactivates a status=0 row.
//
// Acceptance 5 reaches this state through "admin removes the user, user logs in
// again", and that path is already closed upstream because the hook only fires on
// account creation. This asserts the property at the function itself, so a later
// caller on a login path cannot resurrect a membership an administrator revoked.
// Asserting only through the OIDC callback would leave that open.
func TestAutoJoinInitialSpace_DoesNotReactivateRemovedMember(t *testing.T) {
	_, f, _ := setup(t)
	const spaceID = "sp-initial-removed"
	seedInitialSpace(t, f, spaceID, 0, JoinModeDirect)

	err := f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID, UID: "u-removed", Role: 0, Status: 0,
	})
	assert.NoError(t, err)

	outcome, err := AutoJoinInitialSpace(testCtx, "u-removed", spaceID)
	assert.NoError(t, err)
	assert.Equal(t, InitialSpaceAlreadyMember, outcome)

	m, err := f.db.queryMemberIncludeRemoved(spaceID, "u-removed")
	assert.NoError(t, err)
	if assert.NotNil(t, m) {
		assert.Equal(t, 0, m.Status, "a member an admin removed must stay removed")
	}
}

// TestAutoJoinInitialSpace_RefusesDisbandedSpace covers acceptance 11: the Space
// was valid when configured and disbanded afterwards. Write-time validation
// cannot cover this, so the consumer has to, and it must refuse rather than
// insert a member row into a Space that no longer exists.
func TestAutoJoinInitialSpace_RefusesDisbandedSpace(t *testing.T) {
	_, f, _ := setup(t)
	const spaceID = "sp-initial-disbanded"
	err := f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceID, Name: "已解散", Creator: "owner-uid", Status: SpaceStatusDisbanded,
	})
	assert.NoError(t, err)

	outcome, err := AutoJoinInitialSpace(testCtx, "u-sso-3", spaceID)
	assert.NoError(t, err, "an inactive Space is a configuration state, not an infrastructure failure")
	assert.Equal(t, InitialSpaceInactive, outcome)
	assert.Equal(t, 0, countMemberRows(t, spaceID, "u-sso-3"))
}

// TestAutoJoinInitialSpace_RefusesBannedSpace pins that status=2 is refused too.
// querySpaceByID filters on `status=1` while the model also defines a banned
// state; a gate written as "not disbanded" instead of "is normal" would let SSO
// users pour into a Space an administrator had frozen.
func TestAutoJoinInitialSpace_RefusesBannedSpace(t *testing.T) {
	_, f, _ := setup(t)
	const spaceID = "sp-initial-banned"
	err := f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceID, Name: "已封禁", Creator: "owner-uid", Status: SpaceStatusBanned,
	})
	assert.NoError(t, err)

	outcome, err := AutoJoinInitialSpace(testCtx, "u-sso-4", spaceID)
	assert.NoError(t, err)
	assert.Equal(t, InitialSpaceInactive, outcome)
	assert.Equal(t, 0, countMemberRows(t, spaceID, "u-sso-4"))
}

// TestAutoJoinInitialSpace_RefusesMissingSpace pins that a typo'd space_id that
// was never valid behaves the same as one that went inactive. Both are "the
// configured target is unusable", and the operator reads the same counter.
func TestAutoJoinInitialSpace_RefusesMissingSpace(t *testing.T) {
	_, _, _ = setup(t)

	outcome, err := AutoJoinInitialSpace(testCtx, "u-sso-5", "sp-does-not-exist")
	assert.NoError(t, err)
	assert.Equal(t, InitialSpaceInactive, outcome)
	assert.Equal(t, 0, countMemberRows(t, "sp-does-not-exist", "u-sso-5"))
}

// TestAutoJoinInitialSpace_RespectsCapacity covers acceptance 10. The capacity
// limit is the whole reason the ghost-user exclusion in the callback matters, so
// it has to be honoured here exactly as the normal join path honours it.
func TestAutoJoinInitialSpace_RespectsCapacity(t *testing.T) {
	_, f, _ := setup(t)
	const spaceID = "sp-initial-full"
	seedInitialSpace(t, f, spaceID, 1, JoinModeDirect) // owner already fills it

	outcome, err := AutoJoinInitialSpace(testCtx, "u-sso-6", spaceID)
	assert.NoError(t, err, "a full Space is an operational state, not an error to page on")
	assert.Equal(t, InitialSpaceFull, outcome)
	assert.Equal(t, 0, countMemberRows(t, spaceID, "u-sso-6"))
}

// TestAutoJoinInitialSpace_BypassesApprovalMode covers acceptance 9. An
// admin-configured initial Space is equivalent to an admin force-add, so
// join_mode=1 must not divert the user into the approval queue — that would
// leave every SSO account waiting on a human, which is the exact problem this
// feature exists to remove.
func TestAutoJoinInitialSpace_BypassesApprovalMode(t *testing.T) {
	_, f, _ := setup(t)
	const spaceID = "sp-initial-approval"
	seedInitialSpace(t, f, spaceID, 0, JoinModeApproval)

	outcome, err := AutoJoinInitialSpace(testCtx, "u-sso-7", spaceID)
	assert.NoError(t, err)
	assert.Equal(t, InitialSpaceJoined, outcome)
	assert.Equal(t, 1, countMemberRows(t, spaceID, "u-sso-7"))

	var applies int
	_, err = testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM space_join_apply WHERE space_id=? AND uid=?", spaceID, "u-sso-7").Load(&applies)
	assert.NoError(t, err)
	assert.Equal(t, 0, applies, "auto-join must not create an approval request")
}

// TestAutoJoinInitialSpace_FiresMemberJoinEvent covers acceptance 8's event half.
//
// SpaceMemberJoin is what botfather's welcome and notify's space-welcome hang
// off, so an auto-joined user who never produces one is silently onboarded
// differently from everyone else — the exact divergence that made the manager
// addMembers path need a reconciler. Going through afterJoinSpace rather than
// writing the member row directly is what buys this, and only an assertion on
// the persisted row can tell the two apart.
//
// Follows the member-removal event test's setup: a dedicated ctx (config.Context
// .Event is an unsynchronised field that background goroutines from earlier tests
// may still be reading), a hand-created `event` table (this package does not pull
// in modules/base migrations), and a registered listener (fireSpaceMemberJoinEvent
// writes nothing when nobody is listening).
func TestAutoJoinInitialSpace_FiresMemberJoinEvent(t *testing.T) {
	_, _, err := setup(t)
	require.NoError(t, err)

	evtCtx := newEventTestContext(t)
	f := New(evtCtx)

	_, err = evtCtx.DB().Exec("CREATE TABLE IF NOT EXISTS `event` (" +
		"id INTEGER NOT NULL PRIMARY KEY AUTO_INCREMENT, " +
		"event VARCHAR(40) NOT NULL DEFAULT '', `type` SMALLINT NOT NULL DEFAULT 0, " +
		"data VARCHAR(10000) NOT NULL DEFAULT '', status SMALLINT NOT NULL DEFAULT 0, " +
		"reason VARCHAR(1000) NOT NULL DEFAULT '', version_lock INTEGER NOT NULL DEFAULT 0, " +
		"created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, " +
		"updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)")
	require.NoError(t, err)
	_, err = evtCtx.DB().Exec("DELETE FROM `event`")
	require.NoError(t, err)

	evtCtx.AddEventListener(event.SpaceMemberJoin, func(data []byte, commit config.EventCommit) {
		commit(nil)
	})

	const spaceID = "sp-initial-event"
	seedInitialSpace(t, f, spaceID, 0, JoinModeDirect)

	outcome, err := AutoJoinInitialSpace(evtCtx, "u-sso-8", spaceID)
	require.NoError(t, err)
	require.Equal(t, InitialSpaceJoined, outcome)

	// afterJoinSpace dispatches the event from a goroutine in its own
	// transaction, so the row lands shortly after the call returns.
	assert.Eventually(t, func() bool {
		var n int
		_, qerr := evtCtx.DB().SelectBySql(
			"SELECT COUNT(*) FROM `event` WHERE event=? AND data LIKE ?",
			event.SpaceMemberJoin, "%u-sso-8%").Load(&n)
		return qerr == nil && n > 0
	}, 5*time.Second, 50*time.Millisecond, "SpaceMemberJoin event row must be written")
}

// TestAutoJoinInitialSpace_RejectsEmptyArguments pins that a caller bug surfaces
// as an error instead of being absorbed. "Feature off" is the empty configuration
// value, which the caller checks before reaching this function; an empty uid or
// space_id here means something upstream went wrong and must be visible.
//
// Runs without touching the database — both cases return on the guard.
func TestAutoJoinInitialSpace_RejectsEmptyArguments(t *testing.T) {
	outcome, err := AutoJoinInitialSpace(testCtx, "", "sp-x")
	assert.Equal(t, InitialSpaceFailed, outcome)
	assert.Error(t, err)

	outcome, err = AutoJoinInitialSpace(testCtx, "u-1", "")
	assert.Equal(t, InitialSpaceFailed, outcome)
	assert.Error(t, err)
}

// TestAutoJoinInitialSpace_UnknownSpaceIsNotConfusedWithQueryFailure pins the
// reason queryActiveSpaceForAutoJoin exists next to querySpaceByID: the latter
// returns (nil, nil) before inspecting the error, collapsing "the id is wrong"
// into "the database is unavailable". The consumer counts those under different
// labels and only one of them is worth waking someone for.
func TestAutoJoinInitialSpace_UnknownSpaceIsNotConfusedWithQueryFailure(t *testing.T) {
	_, f, _ := setup(t)

	sp, err := f.db.queryActiveSpaceForAutoJoin("sp-absent")
	assert.NoError(t, err, "a missing row is not an error")
	assert.Nil(t, sp)
}
