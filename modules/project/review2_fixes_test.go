package project

import (
	"errors"
	"net/http"
	"sync"
	"testing"

	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression cases for the nine findings of the second review round. Each fails against the
// code as it stood before the corresponding fix.

// --- P1 #1: createProject did not check the creator's Space seat inside the transaction ---

func TestCreateChecksCreatorSpaceSeatInsideTransaction(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "gone1")
	seedSpaceMember(t, spaceA, "gone1", 0, 1)

	// Warm the middleware's positive membership cache, then remove the seat in the database
	// only. This is exactly the state the old code acted on: the middleware passed from cache
	// and createProject never re-checked, leaving a permanent owner seat with no Space seat on
	// a project nobody could clean up (the cascade closes seats; an ownerless project cannot be
	// disbanded).
	w := doJSON(t, srv, http.MethodGet, "/v1/space/"+spaceA+"/projects", token, nil)
	require.Equal(t, http.StatusOK, w.Code)
	removeSpaceMember(t, spaceA, "gone1")

	w = doJSON(t, srv, http.MethodPost, "/v1/space/"+spaceA+"/projects", token,
		map[string]any{"name": "should-not-exist"})
	assertProjectErrorCode(t, w, "err.server.project.member_not_space_member")

	var n int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project` WHERE space_id = ?", spaceA).LoadOne(&n))
	assert.Equal(t, 0, n, "no project may be created by a non-member")
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member` WHERE space_id = ? AND uid = ?",
		spaceA, "gone1").LoadOne(&n))
	assert.Equal(t, 0, n, "no owner seat may survive the rejected create")
	_ = p
}

func TestCreateRejectedWhenSpaceWentInactive(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	w := doJSON(t, srv, http.MethodGet, "/v1/space/"+spaceA+"/projects", token, nil)
	require.Equal(t, http.StatusOK, w.Code)

	setSpaceStatus(t, spaceA, 0) // disbanded
	w = doJSON(t, srv, http.MethodPost, "/v1/space/"+spaceA+"/projects", token,
		map[string]any{"name": "in-dead-space"})
	assert.NotEqual(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	setSpaceStatus(t, spaceA, 1)
}

// --- P1 #2: the creation quotas were countable but not serialised ---

// TestConcurrentCreatesCannotExceedPerSpaceQuota is the case the old code failed. Counting
// inside a transaction is not enough: a plain SELECT COUNT(*) is a non-locking consistent read,
// so N concurrent creates all saw the same count and all inserted.
//
// Driven at the service layer rather than over HTTP, for a reason worth recording: the routes
// are served by the *Project instance module.Setup built at TestMain time, not by the one
// setup() returns, so lowering a quota on the latter has no effect on an HTTP request. An
// earlier version of this test did exactly that and "passed" six concurrent creates against
// MaxPerSpace=1 while asserting nothing. The lock under test lives in createProject, which is
// what this calls.
func TestConcurrentCreatesCannotExceedPerSpaceQuota(t *testing.T) {
	_, p := setup(t)
	seedSpace(t, spaceA, 1)
	uids := make([]string, 6)
	for i := range uids {
		uids[i] = "racer" + string(rune('a'+i))
		seedUser(t, uids[i])
		seedSpaceMember(t, spaceA, uids[i], 0, 1)
	}

	p.cfg.MaxPerSpace = 1

	var wg sync.WaitGroup
	errs := make([]error, len(uids))
	for i, uid := range uids {
		wg.Add(1)
		go func(i int, uid string) {
			defer wg.Done()
			_, errs[i] = p.createProject(createInput{
				SpaceID: spaceA, Creator: uid, Name: "race-" + string(rune('a'+i)),
				Discoverability: DiscoverabilitySpaceListed, JoinMode: JoinModeInviteOnly,
			})
		}(i, uid)
	}
	wg.Wait()

	var created int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project` WHERE space_id = ? AND status = ?",
		spaceA, StatusNormal).LoadOne(&created))
	assert.Equal(t, 1, created,
		"exactly one create may win with MaxPerSpace=1; got %d (errs=%v)", created, errs)

	quotaRejections := 0
	for _, err := range errs {
		if errors.Is(err, errQuotaPerSpace) {
			quotaRejections++
		}
	}
	assert.Equal(t, len(uids)-1, quotaRejections,
		"every loser must be rejected with the per-space quota error, not some other failure")
}

// TestConcurrentCreatesCannotExceedPerCreatorQuota covers the second hard cap, which the Space
// row lock also serialises because it is scoped to one Space.
func TestConcurrentCreatesCannotExceedPerCreatorQuota(t *testing.T) {
	_, p := setup(t)
	seedSpace(t, spaceA, 1)
	seedUser(t, "solo")
	seedSpaceMember(t, spaceA, "solo", 0, 1)

	p.cfg.MaxPerCreator, p.cfg.MaxPerSpace = 2, 100

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = p.createProject(createInput{
				SpaceID: spaceA, Creator: "solo", Name: "mine-" + string(rune('a'+i)),
				Discoverability: DiscoverabilitySpaceListed, JoinMode: JoinModeInviteOnly,
			})
		}(i)
	}
	wg.Wait()

	var created int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project` WHERE space_id = ? AND creator = ? AND status = ?",
		spaceA, "solo", StatusNormal).LoadOne(&created))
	assert.Equal(t, 2, created,
		"per-creator quota must hold exactly under concurrency; got %d", created)
}

// --- P1 #3: update / disband ran on the middleware's cached role ---

func TestUpdateAndDisbandReReadActorRoleUnderLock(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, tokens, created := projectWithMembers(t, srv, "admin1")
	w := doJSON(t, srv, http.MethodPut, "/v1/projects/"+created.ProjectID+"/members/admin1/role",
		ownerTok, map[string]any{"role": RoleAdmin})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Warm admin1's cached role, then demote in the database only.
	w = doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID, tokens["admin1"], nil)
	require.Equal(t, http.StatusOK, w.Code)
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE octo_project_member SET role = ? WHERE project_id = ? AND uid = ?",
		RoleCommon, created.ProjectID, "admin1").Exec()
	require.NoError(t, err)

	w = doJSON(t, srv, http.MethodPut, "/v1/projects/"+created.ProjectID, tokens["admin1"],
		map[string]any{"name": "renamed-by-ex-admin"})
	assertProjectErrorCode(t, w, "err.server.project.permission_denied")

	// And the owner, demoted the same way, may no longer disband.
	w = doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID, ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code)
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE octo_project_member SET role = ? WHERE project_id = ? AND uid = ?",
		RoleCommon, created.ProjectID, "owner1").Exec()
	require.NoError(t, err)
	w = doJSON(t, srv, http.MethodDelete, "/v1/projects/"+created.ProjectID, ownerTok, nil)
	assertProjectErrorCode(t, w, "err.server.project.permission_denied")

	var status int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT status FROM `octo_project` WHERE project_id = ?", created.ProjectID).LoadOne(&status))
	assert.Equal(t, StatusNormal, status, "a demoted ex-owner must not have disbanded the project")
}

// --- P1 #4: ownership could be transferred to someone already out of the Space ---

func TestOwnershipCannotTransferToExSpaceMember(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "heir1")

	// heir1 leaves the Space; the cascade has NOT run, so their project seat is still active —
	// precisely the state in which the old code happily promoted them.
	removeSpaceMember(t, spaceA, "heir1")
	member, err := NewDB(testCtx).queryMember(created.ProjectID, "heir1")
	require.NoError(t, err)
	require.NotNil(t, member)
	require.Equal(t, MemberStatusActive, member.Status, "precondition: seat not yet cascaded")

	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/leave", ownerTok,
		map[string]any{"transfer_to": "heir1"})
	assertProjectErrorCode(t, w, "err.server.project.member_not_space_member")

	// Same guard on the role-change path.
	w = doJSON(t, srv, http.MethodPut, "/v1/projects/"+created.ProjectID+"/members/owner1/role",
		ownerTok, map[string]any{"role": RoleCommon, "transfer_to": "heir1"})
	assertProjectErrorCode(t, w, "err.server.project.member_not_space_member")

	// The owner still owns it, so the project stayed manageable.
	owner, err := NewDB(testCtx).queryMember(created.ProjectID, "owner1")
	require.NoError(t, err)
	assert.Equal(t, RoleOwner, owner.Role)
}

// --- P1 #5: a stale cascade job could close a rejoined member's seat ---

func TestCascadeSkipsSeatWhenMemberRejoinedBeforeTheShortTransaction(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "rejoiner")
	_ = ownerTok

	// Removal happened, so a job exists; then the user rejoined. The outer gate is bypassed
	// here on purpose — this pins the per-transaction re-check, which is the layer that was
	// missing.
	removeSpaceMember(t, spaceA, "rejoiner")
	rejoinSpaceMember(t, spaceA, "rejoiner") // rejoined

	changed, err := p.deactivateSeatForCascade(
		created.ProjectID, spaceA, "rejoiner", "owner1", spacemod.MemberRemoveReasonKicked)
	require.NoError(t, err)
	assert.False(t, changed, "a rejoined member's seat must not be closed by a stale job")

	member, err := p.db.queryMember(created.ProjectID, "rejoiner")
	require.NoError(t, err)
	require.NotNil(t, member)
	assert.Equal(t, MemberStatusActive, member.Status)
}

// --- P1 #6: reported, and deliberately NOT resolved in P0 ---
//
// The review asked for the cascade to hand ownership over or disband. Both are product
// decisions — one silently changes who controls a project, the other destroys data — and the
// brief scopes this step to "deactivate every active row for (space_id, uid) and bump
// member_epoch when rows were affected". So P0 keeps the documented end state and makes it
// visible; the resolution is an Open question in the brief.
//
// These cases pin the scope rather than the (absent) behaviour, so that a later change which
// quietly adds auto-promotion or auto-disband has to update them.

// TestCascadeClosesSoleOwnerSeatWithoutPromotingOrDisbanding pins the P0 contract: the seat is
// closed, the project stays as it was, and nobody is promoted behind the operator's back.
func TestCascadeClosesSoleOwnerSeatWithoutPromotingOrDisbanding(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "member1", "member2")

	rolesBefore := memberRoles(t, created.ProjectID)

	removeSpaceMember(t, spaceA, "owner1")
	require.NoError(t, runCascade(t, p, spaceA, "owner1", "admin", spacemod.MemberRemoveReasonKicked))

	// The departing owner's seat is closed — that part IS required.
	gone, err := p.db.queryMember(created.ProjectID, "owner1")
	require.NoError(t, err)
	assert.Equal(t, MemberStatusRemoved, gone.Status)

	// The project is NOT disbanded: the cascade must not destroy data.
	var status int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT status FROM `octo_project` WHERE project_id = ?", created.ProjectID).LoadOne(&status))
	assert.Equal(t, StatusNormal, status, "the cascade must not disband the project")

	// And nobody was promoted: every remaining member keeps the role they had.
	rolesAfter := memberRoles(t, created.ProjectID)
	for uid, before := range rolesBefore {
		if uid == "owner1" {
			continue
		}
		assert.Equal(t, before, rolesAfter[uid],
			"member %s must keep role %d; the cascade must not promote anyone", uid, before)
	}

	// The documented consequence: the project now has no owner. Asserted so the end state is
	// pinned rather than merely described in a comment.
	var owners int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member` WHERE project_id = ? AND status = ? AND role = ?",
		created.ProjectID, MemberStatusActive, RoleOwner).LoadOne(&owners))
	assert.Equal(t, 0, owners,
		"P0 end state: an ownerless project. Resolution is an Open question, not a silent fix")
}

// memberRoles snapshots uid -> role for the active roster.
func memberRoles(t *testing.T, projectID string) map[string]int {
	t.Helper()
	type row struct {
		UID  string `db:"uid"`
		Role int    `db:"role"`
	}
	var rows []*row
	_, err := testCtx.DB().SelectBySql(
		"SELECT uid, role FROM `octo_project_member` WHERE project_id = ? AND status = ?",
		projectID, MemberStatusActive).Load(&rows)
	require.NoError(t, err)
	out := map[string]int{}
	for _, r := range rows {
		out[r.UID] = r.Role
	}
	return out
}

// --- P2 #8: an empty update faked updated_at and wrote an audit entry ---

func TestEmptyUpdateIsRejected(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv)

	before := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID, ownerTok, nil)
	require.Equal(t, http.StatusOK, before.Code)
	beforeResp := decodeResp(t, before)

	w := doJSON(t, srv, http.MethodPut, "/v1/projects/"+created.ProjectID, ownerTok, map[string]any{})
	assertProjectErrorCode(t, w, "err.server.project.request_invalid")

	after := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID, ownerTok, nil)
	require.Equal(t, http.StatusOK, after.Code)
	afterResp := decodeResp(t, after)
	assert.Equal(t, beforeResp.UpdatedAt, afterResp.UpdatedAt,
		"a rejected empty update must not move updated_at")
}

// --- P1 #7 / P2 #9: the abandoned scan was unbounded and counted the wrong thing ---

// TestAbandonedLeakCountsSeatsNotJobs pins all three semantic defects at once: one job whose
// member sits in several projects must count once PER SEAT, a repeated (space, uid) removal must
// not double-count, and a rejoined user must not be counted at all.
func TestAbandonedLeakCountsSeatsNotJobs(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, first := projectWithMembers(t, srv, "leaker")
	second := createProjectVia(t, srv, spaceA, ownerTok, "second")
	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+second.ProjectID+"/members/add",
		ownerTok, map[string]any{"uids": []string{"leaker"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// One abandoned job, two leaked seats. The old COUNT-of-jobs reported 1.
	removeSpaceMember(t, spaceA, "leaker")
	enqueueCleanupJob(t, spaceA, "leaker", cleanupStatusAbandoned)

	leaked := countAbandonedLeak(t, p)
	assert.Equal(t, 2, leaked, "two leaked seats behind one abandoned job must count as 2")

	// A second abandoned job for the same pair must NOT double the count.
	enqueueCleanupJob(t, spaceA, "leaker", cleanupStatusAbandoned)
	assert.Equal(t, 2, countAbandonedLeak(t, p),
		"a second abandoned job for the same (space, uid) must not double-count the same seats")

	// Rejoining clears the alert even though the abandoned jobs remain.
	rejoinSpaceMember(t, spaceA, "leaker")
	assert.Equal(t, 0, countAbandonedLeak(t, p),
		"a rejoined member is not a leak, however many abandoned jobs remain")

	_ = first
}

// TestAbandonedLeakScanPagesWithoutRepeating drives more leaked seats than one page holds, so a
// cursor bug (repeating or skipping a page) shows up as a wrong total.
func TestAbandonedLeakScanPagesWithoutRepeating(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, _ := projectWithMembers(t, srv, "pager")
	for i := 0; i < 4; i++ {
		pr := createProjectVia(t, srv, spaceA, ownerTok, "paged-"+string(rune('a'+i)))
		w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+pr.ProjectID+"/members/add",
			ownerTok, map[string]any{"uids": []string{"pager"}})
		require.Equal(t, http.StatusOK, w.Code)
	}
	removeSpaceMember(t, spaceA, "pager")
	enqueueCleanupJob(t, spaceA, "pager", cleanupStatusAbandoned)

	orig := p.cfg.ReconcileLimit
	p.cfg.ReconcileLimit = 2 // force several pages
	t.Cleanup(func() { p.cfg.ReconcileLimit = orig })

	assert.Equal(t, 5, countAbandonedLeak(t, p),
		"paged walk must total every leaked seat exactly once (1 original + 4 extra)")
}

// TestAbandonedLeakIgnoresBannedSpace pins the cleanup-semantics half: a banned Space keeps its
// members' seats, so it is not a leak.
func TestAbandonedLeakIgnoresBannedSpace(t *testing.T) {
	srv, p := setup(t)
	_, _, _ = projectWithMembers(t, srv, "banned1")
	enqueueCleanupJob(t, spaceA, "banned1", cleanupStatusAbandoned)
	setSpaceStatus(t, spaceA, 2)
	t.Cleanup(func() { setSpaceStatus(t, spaceA, 1) })

	assert.Equal(t, 0, countAbandonedLeak(t, p),
		"members of a banned Space keep their seats and are not a leak")
}

// --- the sentinel split from round one must survive these changes ---

func TestNotSpaceMemberSentinelIsDistinctFromNotFound(t *testing.T) {
	assert.False(t, errors.Is(errNotSpaceMember, errMemberNotFound))
	assert.False(t, errors.Is(errMemberNotFound, errNotSpaceMember))
}

// countAbandonedLeak runs the abandoned-leak scan and reads the resulting gauge, which is the
// number the alert is built on. Going through the real scan rather than a hand-written query is
// the point: the defect being pinned was in what the scan counted, not in the data.
func countAbandonedLeak(t *testing.T, p *Project) int {
	t.Helper()
	p.scanAbandonedCleanupLeak()
	return int(promtestutil.ToFloat64(i1AbandonedLeak))
}
