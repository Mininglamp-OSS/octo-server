package project

// PR #841 review round 3 (yujiawei P1 / Jerry-Xin P1, both reproduced on real MySQL). The B-3
// fix ordered two TABLES (space_member before space). It says nothing about the ROWS of
// space_member, and four paths now lock several of them in sequence:
//
//	addOneMember      actor, then target
//	leaveProject      uid, then successor
//	changeMemberRole  actor, then target, then successor
//
// modules/space's disband takes `space_member WHERE space_id=? AND status=1 FOR UPDATE`
// (lockActiveMemberUIDsTx), a range lock acquired ROW BY ROW in index order — clustered-key
// (id) order, i.e. seat-creation order. So whenever the second uid a project write locks comes
// EARLIER in id order than the first, the cycle closes again:
//
//	project write: holds S(high-id row), waits for S(low-id row)
//	disband:       holds X(low-id row),  waits for X(high-id row)
//
// Error 1213, no retry anywhere in this module, and the loser answers store_failed (500). When
// the loser is the disband, a step of the member-removal security cascade has failed.

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAddDoesNotDeadlockWhenSeatRowsAreLockedAgainstTheDisbandScanOrder seeds the TARGET's seat
// before the ACTOR's, so the actor's row has the higher id and the two orders oppose.
func TestAddDoesNotDeadlockWhenSeatRowsAreLockedAgainstTheDisbandScanOrder(t *testing.T) {
	_, p := setup(t)
	seedSpace(t, spaceA, 1)

	// Seat order matters and is the whole setup: target first (low id), actor second (high id).
	seedUser(t, "rl_target")
	seedSpaceMember(t, spaceA, "rl_target", 0, 1)
	ownerTok := seedUser(t, "rl_actor")
	seedSpaceMember(t, spaceA, "rl_actor", 0, 1)
	_ = ownerTok

	var targetID, actorID int64
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT id FROM space_member WHERE space_id=? AND uid=?", spaceA, "rl_target").LoadOne(&targetID))
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT id FROM space_member WHERE space_id=? AND uid=?", spaceA, "rl_actor").LoadOne(&actorID))
	require.Less(t, targetID, actorID,
		"precondition: the target's seat row must come FIRST in id order, so the add's lock "+
			"order (actor then target) opposes the disband scan's (id ascending)")

	created, err := p.createProject(createInput{
		SpaceID: spaceA, Creator: "rl_actor", Name: "rowlock-probe",
		Discoverability: DiscoverabilitySpaceListed,
	})
	require.NoError(t, err)

	// The disband side: hold X on the LOW-id row only, the state its row-by-row range scan is
	// in when it reaches a row the project write already holds.
	txD, err := testCtx.DB().Begin()
	require.NoError(t, err)
	defer txD.RollbackUnlessCommitted()
	var locked []int
	_, err = txD.SelectBySql(
		"SELECT 1 FROM space_member WHERE space_id=? AND uid=? AND status=1 FOR UPDATE",
		spaceA, "rl_target").Load(&locked)
	require.NoError(t, err)
	require.NotEmpty(t, locked)

	// The real add, in flight: it takes S on the actor's (high-id) row, then wants the target's.
	type outcome struct {
		admitted bool
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		ok, aErr := p.addOneMember(created.ProjectID, spaceA, "rl_actor", "rl_target")
		done <- outcome{admitted: ok, err: aErr}
	}()
	time.Sleep(700 * time.Millisecond)

	// The disband side now continues its scan onto the actor's row — closing the cycle if the
	// add is holding it while waiting for the target's.
	_, scanErr := txD.SelectBySql(
		"SELECT 1 FROM space_member WHERE space_id=? AND uid=? AND status=1 FOR UPDATE",
		spaceA, "rl_actor").Load(&locked)
	if scanErr == nil {
		require.NoError(t, txD.Commit())
	}

	var got outcome
	select {
	case got = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("addOneMember 未在 15s 内返回：行级加锁顺序把它挂死了")
	}

	assert.False(t, isDeadlockErr(scanErr),
		"the Space-disband scan must not be deadlocked by this module's row-level lock order: %v", scanErr)
	assert.False(t, isDeadlockErr(got.err),
		"addOneMember must not deadlock against the disband scan's row order: %v", got.err)
}

// TestEachWritePathTakesItsSeatLocksInOneStatement is the structural half, and the one that ends
// the class rather than this instance.
//
// Sorting the uids does NOT suffice: the disband scan orders by id, not by uid, so any order
// this module picks can still oppose it. One statement per path lets InnoDB acquire the rows in
// ITS scan order, which is the same order the disband scan uses — so there is no second row to
// be waiting for while holding the first.
func TestEachWritePathTakesItsSeatLocksInOneStatement(t *testing.T) {
	src := readLinesWithoutComments(t, "service.go")
	for _, fn := range []string{
		"func (p *Project) addOneMember",
		"func (p *Project) leaveProject",
		"func (p *Project) changeMemberRole",
		"func (p *Project) removeMember",
		"func (p *Project) updateProject",
		"func (p *Project) disbandProject",
	} {
		// implBody, not funcBody: these all have a retry wrapper now, and inspecting the wrapper
		// would make this guard vacuously green.
		body := implBody(t, src, fn)
		n := countOccurrences(body, "requireSpaceSeatsTx(") +
			countOccurrences(body, "lockSpaceSeatsTx(") +
			countOccurrences(body, "checkSpaceMembershipForWriteTx(") +
			countOccurrences(body, "lockSpaceSeatRowTx(")
		assert.LessOrEqual(t, n, 1,
			"%s takes %d separate space_member seat locks. Two or more sequential row locks on "+
				"that table reopen the Error 1213 cycle with modules/space's disband scan, which "+
				"acquires its range lock row by row in id order — reproduced, with the disband as "+
				"InnoDB's victim. Take them in ONE statement (requireSpaceSeatsTx) so InnoDB picks "+
				"a scan-consistent order.", fn, n)
	}
}

func countOccurrences(s, sub string) int {
	return strings.Count(s, sub)
}
