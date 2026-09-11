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
	"net/http"
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

func activeMemberCount(t *testing.T, projectID string) int {
	t.Helper()
	var n int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member` WHERE project_id = ? AND status = ?",
		projectID, MemberStatusActive).LoadOne(&n))
	return n
}

// activeOwnerCount reads the authoritative sole-owner invariant outside any transaction.
func activeOwnerCount(t *testing.T, projectID string) int {
	t.Helper()
	var n int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member` "+
			"WHERE project_id = ? AND status = ? AND removing = 0 AND role = ?",
		projectID, MemberStatusActive, RoleOwner).LoadOne(&n))
	return n
}

// TestConcurrentOwnerTransfersKeepOneOwner starts from the normal sole-Owner
// state and races two valid successor choices. The dedicated transfer operation
// must serialize the handoff: exactly one request succeeds, the winner is the
// only Owner, and the original Owner becomes an Admin.
func TestConcurrentOwnerTransfersKeepOneOwner(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "successor1", "successor2")
	pid := created.ProjectID
	r := mountProject(t, p)

	require.Equal(t, 1, activeOwnerCount(t, pid), "precondition: one Owner")

	type result struct {
		code int
		body string
	}
	start := make(chan struct{})
	done := make(chan result, 2)
	for _, successor := range []string{"successor1", "successor2"} {
		successor := successor
		go func() {
			<-start
			w := doOn(t, r, http.MethodPut, "/v1/projects/"+pid+"/owner", ownerTok,
				map[string]any{"uid": successor})
			done <- result{code: w.Code, body: w.Body.String()}
		}()
	}
	close(start)

	results := []result{<-done, <-done}
	successes := 0
	for _, got := range results {
		if got.code == http.StatusOK {
			successes++
		}
	}
	require.Equal(t, 1, successes,
		"two simultaneous valid handoffs must commit exactly one transfer: %+v", results)

	assert.Equal(t, 1, activeOwnerCount(t, pid),
		"concurrent transfers must never create multiple active Owners")
	former, err := p.db.queryMember(pid, "owner1")
	require.NoError(t, err)
	require.NotNil(t, former)
	assert.Zero(t, former.Removing)
	assert.Equal(t, MemberStatusActive, former.Status)
	assert.Equal(t, RoleAdmin, former.Role,
		"the original sole Owner must become Admin after the winning handoff")

	winners := 0
	for _, uid := range []string{"successor1", "successor2"} {
		member, err := p.db.queryMember(pid, uid)
		require.NoError(t, err)
		require.NotNil(t, member)
		if member.Status == MemberStatusActive && member.Removing == 0 && member.Role == RoleOwner {
			winners++
		}
	}
	assert.Equal(t, 1, winners, "exactly one valid successor must hold Owner")
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
			"created_at, joined_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		pid, "q1", spaceA, RoleCommon, MemberStatusActive, "owner1", now, now, now).Exec()
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
			"created_at, joined_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		pid, "late1", spaceA, RoleCommon, MemberStatusActive, "owner1", now, now, now).Exec()
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
