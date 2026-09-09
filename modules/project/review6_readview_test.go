package project

// PR #841 review round 3 (yujiawei P0). The round-2 fix identified the read-view trap
// correctly — a table outside a `FOR SHARE OF` list is read as a CONSISTENT read, and a
// consistent read OPENS the transaction's read view — and then guarded exactly one call site.
//
// The other five write paths still take the JOINing helper as their FIRST statement, so their
// snapshot is frozen at the top of the transaction, before lockActiveProjectTx. On those paths
// the stale reads are not a quota count; they are the guard protecting a state this module
// itself calls unrecoverable.
//
// The interleaving below is the one the reviewer executed against MySQL 8.0.33. It is
// reproduced here against the REAL service methods, with a hand-written transaction standing
// in for the concurrent writer so the timing is deterministic:
//
//	W: lock the project row ; make its change ; (hold)
//	A: real service call — opens its read view, then blocks on the project row lock
//	W: COMMIT  ->  A acquires the lock, and reads its aggregate from the STALE snapshot
//
// What makes this invisible in code review: the row the transaction is about to write is read
// FOR UPDATE and is therefore fresh. Only the aggregate that AUTHORISES the write is stale.

import (
	"testing"
	"time"

	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// holdProjectRow opens a transaction, takes the project row's exclusive lock, and returns it
// still open. The caller commits it to release the writer this test is racing.
func holdProjectRow(t *testing.T, projectID string) *dbr.Tx {
	t.Helper()
	tx, err := testCtx.DB().Begin()
	require.NoError(t, err)
	var found []int
	_, err = tx.SelectBySql(
		"SELECT 1 FROM `octo_project` WHERE project_id = ? AND status = 1 FOR UPDATE", projectID,
	).Load(&found)
	require.NoError(t, err)
	require.NotEmpty(t, found, "the project row must be lockable")
	return tx
}

// activeOwnerCount reads the authoritative owner count outside any transaction.
func activeOwnerCount(t *testing.T, projectID string) int {
	t.Helper()
	var n int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member` WHERE project_id = ? AND status = ? AND role = ?",
		projectID, MemberStatusActive, RoleOwner).LoadOne(&n))
	return n
}

func activeMemberCount(t *testing.T, projectID string) int {
	t.Helper()
	var n int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member` WHERE project_id = ? AND status = ?",
		projectID, MemberStatusActive).LoadOne(&n))
	return n
}

// TestConcurrentLastOwnerDeparturesCannotLeaveAProjectOwnerless is the P0 reproducer.
//
// Two owners leave concurrently. Each transaction re-reads its OWN membership row FOR UPDATE
// (fresh) but counts owners with a plain SELECT answered from a snapshot opened before the
// project row lock — so both see 2 owners, both pass "you are not the last owner", and the
// project ends with none. There is no path back in P0: role change and disband are owner-only,
// a Space admin has read access only, and no reconcile scan looks for this state.
func TestConcurrentLastOwnerDeparturesCannotLeaveAProjectOwnerless(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "co2")
	pid := created.ProjectID

	// Make co2 a second owner.
	w := doJSON(t, srv, "PUT", "/v1/projects/"+pid+"/members/co2/role", ownerTok,
		map[string]any{"role": RoleOwner})
	require.Equal(t, 200, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 2, activeOwnerCount(t, pid), "precondition: two owners")

	// W: the concurrent departure. Holds the project row and closes co2's seat.
	txW := holdProjectRow(t, pid)
	defer txW.RollbackUnlessCommitted()
	_, err := txW.UpdateBySql(
		"UPDATE `octo_project_member` SET status = ?, updated_at = ? "+
			"WHERE project_id = ? AND uid = ? AND status = ?",
		MemberStatusRemoved, time.Now().UTC(), pid, "co2", MemberStatusActive).Exec()
	require.NoError(t, err)
	_, err = txW.UpdateBySql(
		"UPDATE `octo_project` SET member_epoch = member_epoch + 1 WHERE project_id = ? AND status = 1",
		pid).Exec()
	require.NoError(t, err)

	// A: the real leave, in flight. Its first statement opens the read view; it then blocks
	// on the project row lock W is holding.
	type outcome struct{ err error }
	done := make(chan outcome, 1)
	go func() {
		_, lErr := p.leaveProject(pid, spaceA, "owner1", "")
		done <- outcome{err: lErr}
	}()
	time.Sleep(700 * time.Millisecond) // let A open its snapshot and park on the lock

	require.NoError(t, txW.Commit()) // now A proceeds, with a snapshot from before this commit

	var got outcome
	select {
	case got = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("leaveProject 未在 15s 内返回")
	}

	assert.ErrorIs(t, got.err, errLastOwnerMustTransfer,
		"owner1 IS the last owner once W committed; the guard must see that and refuse")
	assert.Equal(t, 1, activeOwnerCount(t, pid),
		"a project must never be left with zero owners — there is no path back in P0")
}

// TestConcurrentAddsCannotExceedTheMemberQuota is the same root cause on the add path.
func TestConcurrentAddsCannotExceedTheMemberQuota(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv)
	pid := created.ProjectID
	_ = ownerTok

	// Cap the project at 2 members. owner1 is already one of them.
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project` SET max_members = 2 WHERE project_id = ?", pid).Exec()
	require.NoError(t, err)
	require.Equal(t, 1, activeMemberCount(t, pid), "precondition: one member")

	for _, uid := range []string{"q1", "q2"} {
		seedUser(t, uid)
		seedSpaceMember(t, spaceA, uid, 0, 1)
	}

	// W: the concurrent add. Holds the project row and admits q1.
	txW := holdProjectRow(t, pid)
	defer txW.RollbackUnlessCommitted()
	now := time.Now().UTC()
	_, err = txW.InsertBySql(
		"INSERT INTO `octo_project_member` (project_id, uid, space_id, role, status, invite_uid, "+
			"created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		pid, "q1", spaceA, RoleCommon, MemberStatusActive, "owner1", now, now).Exec()
	require.NoError(t, err)

	type outcome struct {
		admitted bool
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		ok, aErr := p.addOneMember(pid, spaceA, "owner1", "q2")
		done <- outcome{admitted: ok, err: aErr}
	}()
	time.Sleep(700 * time.Millisecond)
	require.NoError(t, txW.Commit())

	var got outcome
	select {
	case got = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("addOneMember 未在 15s 内返回")
	}

	assert.ErrorIs(t, got.err, errQuotaMembers,
		"the cap is 2 and W's commit filled it; the quota must be counted freshly")
	assert.False(t, got.admitted, "q2 must not be admitted over the cap")
	assert.LessOrEqual(t, activeMemberCount(t, pid), 2,
		"the member quota must hold under concurrency, not just in isolation")
}

// TestDisbandInvalidatesConcurrentlyAdmittedMembers is the third consequence: disband reads the
// seats it is about to close with a plain SELECT, then closes them with an UPDATE (a current
// read). A member admitted in between is closed by the UPDATE and never reaches the returned
// list — so nothing invalidates their cached role, and they keep a positive entry for the full
// TTL on a DISBANDED project. The comment above that read says it exists to prevent exactly
// this.
func TestDisbandInvalidatesConcurrentlyAdmittedMembers(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv)
	pid := created.ProjectID
	seedUser(t, "late1")
	seedSpaceMember(t, spaceA, "late1", 0, 1)

	// W: admits late1 while holding the project row.
	txW := holdProjectRow(t, pid)
	defer txW.RollbackUnlessCommitted()
	now := time.Now().UTC()
	_, err := txW.InsertBySql(
		"INSERT INTO `octo_project_member` (project_id, uid, space_id, role, status, invite_uid, "+
			"created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		pid, "late1", spaceA, RoleCommon, MemberStatusActive, "owner1", now, now).Exec()
	require.NoError(t, err)

	type outcome struct {
		uids []string
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		uids, dErr := p.disbandProject(pid, "owner1", spaceA)
		done <- outcome{uids: uids, err: dErr}
	}()
	time.Sleep(700 * time.Millisecond)
	require.NoError(t, txW.Commit())

	var got outcome
	select {
	case got = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("disbandProject 未在 15s 内返回")
	}
	require.NoError(t, got.err)

	assert.Contains(t, got.uids, "late1",
		"a member the disband UPDATE closed must be in the list whose caches get invalidated, "+
			"or they keep a positive cached role on a disbanded project for the full TTL")
}
