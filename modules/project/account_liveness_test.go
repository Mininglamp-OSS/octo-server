package project

import (
	"context"
	"errors"
	"testing"

	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	userpkg "github.com/Mininglamp-OSS/octo-server/pkg/user"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Engine coverage for the ACCOUNT-liveness axis of the peer answer.
//
// Round 8 found this: `_verify` conjoined the Space half but nothing about the
// account, so a globally banned or fully destroyed uid was served to the peer as an
// active project member WITH its real role. Verified at the source before fixing:
// a super-admin ban writes only the `user` row (modules/user.liftBanUser — session
// revocation, device kick and the IM channel ban, no membership write), and
// modules/user/api_destroy.go references none of space_member,
// octo_project_member or member_epoch. So both membership rows stay status = 1.
//
// Why the ban's own session revocation does not cover this endpoint: its subject
// presents NOTHING. That is the endpoint's whole reason to exist — a peer control
// plane asking about a third party it holds no token for. The same argument this
// package already accepted for conjoining the Space half transfers verbatim, and
// the repository has fixed this identical class twice on other tokenless paths
// (modules/user.authVerifyAPIKey, modules/bot_provision.assertSpaceMember), both
// of which say so in their comments.

// setUserLiveness sets the two columns that define account liveness.
//
// status 0 = disabled/banned (what liftBanUser writes); is_destroy 2 = terminal
// destroy. is_destroy 1 is the reversible cooling-off window and is deliberately
// NOT a liveness failure — see the cooling-off case below.
func setUserLiveness(t *testing.T, uid string, status, isDestroy int) {
	t.Helper()
	res, err := testCtx.DB().UpdateBySql(
		"UPDATE `user` SET status = ?, is_destroy = ? WHERE uid = ?", status, isDestroy, uid,
	).Exec()
	require.NoError(t, err)
	affected, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, affected,
		"the fixture must actually have updated %s — a silently-missing row would make "+
			"every assertion below pass for the wrong reason", uid)
}

// TestVerifyDeniesAGloballyBannedAccount is the round-8 P1, reproduced against the
// engine and then closed.
//
// The state is exactly what a ban leaves behind: user.status = 0 with BOTH
// membership rows untouched at status = 1. Before the fix, ProjectMemberships
// answered member:true with the role.
func TestVerifyDeniesAGloballyBannedAccount(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "livOwner")
	seedSpaceMember(t, spaceA, "livOwner", 2, 1)
	seedUser(t, "livBanned")
	seedSpaceMember(t, spaceA, "livBanned", 0, 1)

	created := createProjectVia(t, srv, spaceA, ownerToken, "liveness-ban")
	admitted, err := p.addOneMember(created.ProjectID, spaceA, "livOwner", "livBanned")
	require.NoError(t, err)
	require.True(t, admitted)

	// Baseline: a live account is a member.
	_, roles, err := projectpkg.ProjectMemberships(context.Background(),
		testCtx.DB(), spaceA, created.ProjectID, []string{"livOwner", "livBanned"})
	require.NoError(t, err)
	require.Contains(t, roles, projectpkg.FoldID("livBanned"))

	// The ban, modelled as liftBanUser writes it: `user` only.
	setUserLiveness(t, "livBanned", 0, 0)

	// Both membership rows must still be intact — that IS the window. If a fixture
	// or a future cascade closed them, this test would pass without exercising the
	// account gate at all.
	seat, err := testDB.queryMember(created.ProjectID, "livBanned")
	require.NoError(t, err)
	require.NotNil(t, seat)
	require.Equal(t, MemberStatusActive, seat.Status)
	inSpace, err := testCtx.DB().SelectBySql(
		"SELECT status FROM space_member WHERE space_id = ? AND uid = ?", spaceA, "livBanned").
		ReturnInt64s()
	require.NoError(t, err)
	require.Equal(t, []int64{1}, inSpace,
		"the ban must not have touched space_member — otherwise the Space conjunction "+
			"would already deny this uid and the account gate is untested")

	_, roles, err = projectpkg.ProjectMemberships(context.Background(),
		testCtx.DB(), spaceA, created.ProjectID, []string{"livOwner", "livBanned"})
	require.NoError(t, err)
	assert.NotContains(t, roles, projectpkg.FoldID("livBanned"),
		"a globally banned account must not be served to a peer as a project member. Its "+
			"subject presents no token, so the ban's session revocation cannot reach this "+
			"answer, and no endpoint here lets the peer read user.status for itself")
	assert.Contains(t, roles, projectpkg.FoldID("livOwner"),
		"the account gate must narrow only the banned uid, not empty the answer")
}

// TestVerifyDeniesADestroyedAccountButKeepsCoolingOff pins the half of the
// predicate that is easy to get wrong in the UNSAFE direction.
//
// is_destroy 2 (terminal) is gone; is_destroy 1 (a reversible destroy request) can
// still send and receive and is therefore a live member. Gating on status alone
// would miss the first — teardown can leave status = 1 — and gating on
// `is_destroy <> 0` would wrongly deny the second.
func TestVerifyDeniesADestroyedAccountButKeepsCoolingOff(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "destOwner")
	seedSpaceMember(t, spaceA, "destOwner", 2, 1)
	for _, uid := range []string{"destGone", "destCooling"} {
		seedUser(t, uid)
		seedSpaceMember(t, spaceA, uid, 0, 1)
	}

	created := createProjectVia(t, srv, spaceA, ownerToken, "liveness-destroy")
	for _, uid := range []string{"destGone", "destCooling"} {
		admitted, err := p.addOneMember(created.ProjectID, spaceA, "destOwner", uid)
		require.NoError(t, err)
		require.True(t, admitted)
	}

	// Terminal destroy, with status still 1 — the shape that defeats a status-only gate.
	setUserLiveness(t, "destGone", 1, 2)
	// Cooling-off: a destroy REQUEST inside its reversible window.
	setUserLiveness(t, "destCooling", 1, 1)

	_, roles, err := projectpkg.ProjectMemberships(context.Background(),
		testCtx.DB(), spaceA, created.ProjectID, []string{"destGone", "destCooling"})
	require.NoError(t, err)
	assert.NotContains(t, roles, projectpkg.FoldID("destGone"),
		"a fully destroyed account (is_destroy = 2) must be denied even though teardown "+
			"left status = 1 — that is why the predicate needs both columns")
	assert.Contains(t, roles, projectpkg.FoldID("destCooling"),
		"a cooling-off account (is_destroy = 1) can still send and receive, so it is a live "+
			"member. Denying it would be a false ghost — the failure the batch-user liveness "+
			"gate was built to avoid")
}

// TestActiveAccountsUsesTheCanonicalPredicate covers pkg/user directly against the
// real `user` table, including the case-insensitivity that forces callers to fold.
func TestActiveAccountsUsesTheCanonicalPredicate(t *testing.T) {
	setup(t)
	for _, uid := range []string{"paLive", "paBanned", "paGone", "paCooling"} {
		seedUser(t, uid)
	}
	setUserLiveness(t, "paBanned", 0, 0)
	setUserLiveness(t, "paGone", 1, 2)
	setUserLiveness(t, "paCooling", 1, 1)

	live, err := userpkg.ActiveAccounts(testCtx.DB(),
		[]string{"paLive", "paBanned", "paGone", "paCooling", "paMissing"})
	require.NoError(t, err)
	assert.True(t, live["paLive"], "an enabled account is live")
	assert.False(t, live["paBanned"], "status = 0 is not live")
	assert.False(t, live["paGone"], "is_destroy = 2 is not live")
	assert.True(t, live["paCooling"], "is_destroy = 1 (cooling off) is still live")
	assert.False(t, live["paMissing"], "a uid with no row is not live")

	// The answer is keyed by the DATABASE's spelling, because uid compares
	// case-insensitively. This is why every caller folds both sides.
	live, err = userpkg.ActiveAccounts(testCtx.DB(), []string{"PALIVE"})
	require.NoError(t, err)
	assert.False(t, live["PALIVE"],
		"the map is keyed by what the database returned, not by what was asked — a caller "+
			"matching its own spelling must fold, see pkg/project.FoldID")
	assert.True(t, live["paLive"],
		"and the row itself did match in SQL under the case-insensitive collation")
}

// ---------- the WRITE path ----------

// TestBannedAccountCannotBeAdmittedToAProject covers the other direction of the
// same axis, which is not merely symmetry: without it a peer's answer is correct
// only until someone re-adds the banned uid, and the add itself bumps the epoch,
// so the peer would re-verify and be told the fresh (wrong) answer with confidence.
//
// Both admission entry points are covered because they take DIFFERENT seat locks:
// addMembers goes through lockSpaceSeatsTx, which also joins `space`, while
// createProject goes through lockSpaceSeatRowsTx, which does not — a table outside
// the FOR SHARE list would open the read view before the quota counts. The account
// predicate is the same `user` STRAIGHT_JOIN in both, so a ban has to be refused on
// both paths or neither.
func TestBannedAccountCannotBeAdmittedToAProject(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "wrOwner")
	seedSpaceMember(t, spaceA, "wrOwner", 2, 1)
	seedUser(t, "wrBanned")
	seedSpaceMember(t, spaceA, "wrBanned", 0, 1)
	setUserLiveness(t, "wrBanned", 0, 0)

	created := createProjectVia(t, srv, spaceA, ownerToken, "write-gate")

	admitted, err := p.addOneMember(created.ProjectID, spaceA, "wrOwner", "wrBanned")
	assert.Error(t, err,
		"admitting a globally banned account must be refused. Otherwise the read gate only "+
			"holds until someone re-adds them — and the add bumps member_epoch, so the peer "+
			"re-verifies and is served the wrong answer as a FRESH one")
	assert.False(t, admitted)

	seat, err := testDB.queryMember(created.ProjectID, "wrBanned")
	require.NoError(t, err)
	assert.Nil(t, seat, "and no seat row may be left behind")
}

// TestBannedAccountCannotCreateAProject covers createProject, whose seat check is
// the helper that does NOT join `space`.
//
// The distinction that matters is not "JOIN or no JOIN" but which side of
// `FOR SHARE OF` a joined table sits on. A table OUTSIDE that list is read as a
// consistency read, which OPENS the read view, and all three creation quotas are
// counted after it — measured, six concurrent creates all passed MaxPerSpace=1 when
// that regressed. `user` is inside the list, so the liveness gate costs no read view;
// `space` would be outside it, so lockSpaceSeatRowsTx does not join it at all and the
// Space's activeness is rechecked under the exclusive lock instead.
// TestCreateQuotaStillHoldsUnderConcurrency below is what keeps that property.
func TestBannedAccountCannotCreateAProject(t *testing.T) {
	_, p := setup(t)
	seedSpace(t, spaceA, 1)
	seedUser(t, "crBanned")
	seedSpaceMember(t, spaceA, "crBanned", 2, 1)
	setUserLiveness(t, "crBanned", 0, 0)

	created, err := p.createProject(createInput{
		SpaceID: spaceA, Creator: "crBanned", Name: "banned-create",
	})
	assert.Error(t, err, "a globally banned account must not be able to create a project")
	assert.Nil(t, created)
}

// TestCreateQuotaStillHoldsUnderConcurrency is the REGRESSION GUARD for the fix
// above, and it is the reason the write gate is shaped the way it is.
//
// The obvious implementation of the account gate is to add `INNER JOIN user` to the
// seat lock. On createProject's path that silently destroys every creation quota:
// the JOIN is a consistency read, it assigns the transaction's read view before the
// `space` row lock, and the three quota COUNTs then run against a snapshot older
// than the lock. The failure is invisible in the code and the locks look perfectly
// correct — it was reproduced as six concurrent creates all succeeding against
// MaxPerSpace = 1.
//
// So: MaxPerSpace = 1, N concurrent creates, exactly one may win.
func TestCreateQuotaStillHoldsUnderConcurrency(t *testing.T) {
	_, p := setup(t)
	seedSpace(t, spaceA, 1)
	seedUser(t, "qOwner")
	seedSpaceMember(t, spaceA, "qOwner", 2, 1)

	p.cfg.MaxPerSpace = 1

	const attempts = 6
	type result struct {
		ok  bool
		err error
	}
	results := make(chan result, attempts)
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		go func(n int) {
			<-start
			created, err := p.createProject(createInput{
				SpaceID: spaceA, Creator: "qOwner", Name: "quota-race-" + itoa(n),
			})
			results <- result{ok: created != nil && err == nil, err: err}
		}(i)
	}
	close(start)

	won := 0
	for i := 0; i < attempts; i++ {
		r := <-results
		if r.ok {
			won++
		}
	}
	assert.Equal(t, 1, won,
		"exactly one of %d concurrent creates may pass MaxPerSpace=1. More than one means the "+
			"transaction's read view is being opened before the `space` lock — the classic "+
			"cause is a joined table OUTSIDE the seat-lock statement's FOR SHARE list, which "+
			"makes it a consistency read. `user` is inside that list and `space` is not "+
			"joined at all on createProject's path — TestSeatLockStatementPinsItsPlan is the "+
			"structural half of this",
		attempts)

	var total int
	_, err := testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project` WHERE space_id = ? AND status = ?",
		spaceA, StatusNormal).Load(&total)
	require.NoError(t, err)
	assert.Equal(t, 1, total, "and the database must agree with the API's answer")
}

// ---------- the sentinel fast path ----------

// TestSentinelAnomalyNamesTheProjectAndRepairsInOneStatement covers the P2 raised in
// round 8: the refusal's recovery bound was a full cursor rotation, not "minutes".
//
// scanEpochSanity walks a bounded page budget per tick behind a PERSISTED cursor, and
// a row an old pod inserts during a rollout gets the HIGHEST id — so it is reached
// only when the cursor completes its pass. On a large table that is hours, and every
// request naming that id 500s for the whole window, because one anomalous row
// deliberately poisons the batch. So the endpoint that hit the anomaly needs to be
// able to fix that one row.
//
// Two properties, and the first is what makes the second reachable at all: the error
// must NAME the project, and the repair must be a single indexed, CAS-guarded
// statement.
func TestSentinelAnomalyNamesTheProjectAndRepairsInOneStatement(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "fastPathOwner")
	seedSpaceMember(t, spaceA, "fastPathOwner", 0, 1)

	created := createProjectVia(t, srv, spaceA, token, "sentinel-fast-path")
	forceAbsentSentinelEpoch(t, created.ProjectID)

	_, err := projectpkg.ProjectEpochsInSpace(testCtx.DB(), spaceA, []string{created.ProjectID})
	require.Error(t, err)
	require.True(t, errors.Is(err, projectpkg.ErrLiveProjectOnAbsentSentinel),
		"errors.Is must keep working — every existing caller branches on the sentinel value")

	var anomaly *projectpkg.SentinelAnomalyError
	require.True(t, errors.As(err, &anomaly),
		"the refusal must carry WHICH project tripped it, or the handler has nothing to "+
			"repair and the operator waits for the reconcile cursor")
	assert.Equal(t, created.ProjectID, anomaly.ProjectID)

	// The repair itself: one statement, and its predicate is its own CAS.
	repaired, err := projectpkg.RepairAbsentSentinelEpoch(context.Background(), testCtx.DB(), created.ProjectID)
	require.NoError(t, err)
	assert.True(t, repaired, "the anomalous row must have been lifted off the sentinel")

	// Idempotent under a race: a second attempt (or another replica) changes nothing.
	repaired, err = projectpkg.RepairAbsentSentinelEpoch(context.Background(), testCtx.DB(), created.ProjectID)
	require.NoError(t, err)
	assert.False(t, repaired,
		"the member_epoch = 0 predicate is the CAS, so N replicas racing produce exactly "+
			"one increment")

	// And service resumes — the refusal was a window, not a latch.
	epochs, err := projectpkg.ProjectEpochsInSpace(testCtx.DB(), spaceA, []string{created.ProjectID})
	require.NoError(t, err)
	assert.EqualValues(t, 1, epochs[projectpkg.FoldID(created.ProjectID)])
}

// TestRepairAbsentSentinelEpochLeavesDisbandedRowsAlone pins the predicate's other
// half. A disbanded project's 0 is the CORRECT answer (it reads as absent), so
// repairing it would invent an epoch for a project that is gone.
func TestRepairAbsentSentinelEpochLeavesDisbandedRowsAlone(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "disbOwner")
	seedSpaceMember(t, spaceA, "disbOwner", 0, 1)

	created := createProjectVia(t, srv, spaceA, token, "sentinel-disbanded")
	forceAbsentSentinelEpoch(t, created.ProjectID)
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project` SET status = ? WHERE project_id = ?", StatusDisbanded, created.ProjectID).Exec()
	require.NoError(t, err)

	repaired, err := projectpkg.RepairAbsentSentinelEpoch(context.Background(), testCtx.DB(), created.ProjectID)
	require.NoError(t, err)
	assert.False(t, repaired,
		"a DISBANDED project on 0 is not an anomaly — 0 is what it must answer. Repairing it "+
			"would hand a consumer a live-looking epoch for a project that is gone")
}

// TestAdmissionPathsFoldTheLivenessLookup covers the P2 round 10 raised: the three
// places where the admission path intersects two maps keyed by two databases'
// spellings, all of which were exact-match.
//
// pkg/user.ActiveAccounts' own comment prescribes folding ("a row can come back
// spelled differently from the uid that was asked for … fold both sides — see
// pkg/project.FoldID"), the two batch READERS do it, and these three write-path sites
// did not. The direction is fail-closed, so the cost is a live, seated caller being
// REFUSED — the same shape this branch treated as a blocker when it appeared on the
// read path.
//
// Driven through the real entry points with a case-variant uid, so it fails if any of
// the three reverts to an exact lookup.
func TestAdmissionPathsFoldTheLivenessLookup(t *testing.T) {
	_, p := setup(t)
	seedSpace(t, spaceA, 1)

	// Stored lower-case; every call below uses the UPPER-case spelling, which the
	// collation matches in SQL and an exact Go lookup does not.
	seedUser(t, "foldowner")
	seedSpaceMember(t, spaceA, "foldowner", 2, 1)
	seedUser(t, "foldtarget")
	seedSpaceMember(t, spaceA, "foldtarget", 0, 1)

	// 1) createProjectOnce's creator check.
	created, err := p.createProject(createInput{
		SpaceID: spaceA, Creator: "FOLDOWNER", Name: "fold-liveness",
	})
	require.NoError(t, err,
		"a re-cased creator must be able to create: space_member and user both match it "+
			"under the case-insensitive collation, so refusing it means the Go-side liveness "+
			"lookup is exact-match")
	require.NotNil(t, created)

	// 2) lockSeatsTx's actor check and 3) addOneMemberOnce's target check, both in one
	// call: the actor is re-cased AND the target is re-cased.
	admitted, err := p.addOneMember(created.ProjectID, spaceA, "FOLDOWNER", "FOLDTARGET")
	require.NoError(t, err,
		"a re-cased actor and target must both be accepted; a refusal here means either the "+
			"actor intersection in lockSeatsTx or the target check in addOneMemberOnce is "+
			"exact-match against a map keyed by the database's spelling")
	assert.True(t, admitted)

	// The seat really exists, keyed however the database stored it.
	_, roles, err := projectpkg.ProjectMemberships(context.Background(),
		testCtx.DB(), spaceA, created.ProjectID, []string{"FOLDTARGET"})
	require.NoError(t, err)
	assert.Contains(t, roles, projectpkg.FoldID("FOLDTARGET"))

	// And the gate still REFUSES a banned account when the spelling varies — folding must
	// not have turned the check into a no-op, which is the way this fix could go wrong.
	seedUser(t, "foldbanned")
	seedSpaceMember(t, spaceA, "foldbanned", 0, 1)
	setUserLiveness(t, "foldbanned", 0, 0)
	admitted, err = p.addOneMember(created.ProjectID, spaceA, "FOLDOWNER", "FOLDBANNED")
	assert.Error(t, err,
		"a banned account must still be refused through a re-cased spelling: folding is "+
			"about finding the row, not about accepting it")
	assert.False(t, admitted)

	// Unit-level: FoldedHas must not match a key that is merely a prefix or a different
	// uid, which a sloppy fold would.
	set := map[string]bool{"abc": true, "zzz": false}
	assert.True(t, projectpkg.FoldedHas(set, "ABC"))
	assert.True(t, projectpkg.FoldedHas(set, "abc"))
	assert.False(t, projectpkg.FoldedHas(set, "ab"), "a prefix is not a match")
	assert.False(t, projectpkg.FoldedHas(set, "abcd"), "a longer uid is not a match")
	assert.False(t, projectpkg.FoldedHas(set, "ZZZ"),
		"a key present but false must not count as live")
}
