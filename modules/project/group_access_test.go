package project

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

func TestUniqueGroupProjectIDsSortsAndDeduplicatesLockSet(t *testing.T) {
	got := uniqueGroupProjectIDs([]string{"p2", " p1 ", "p2", "", "p3", "p1"})
	want := []string{"p1", "p2", "p3"}
	if len(got) != len(want) {
		t.Fatalf("uniqueGroupProjectIDs length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("uniqueGroupProjectIDs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestGroupProjectSpaceSeatLockUsesPreparedPrimaryKeyOrder exercises the
// actual lock acquisition path. The first inserted seat has the lower
// clustered id but sorts after the second seat by UID. A blocker holds the
// lower id; while the prepared lock waits there, an exclusive writer for the
// higher id must still proceed. The old (space_id, uid) lookup acquired the
// higher UID-index row first and made that writer time out.
func TestGroupProjectSpaceSeatLockUsesPreparedPrimaryKeyOrder(t *testing.T) {
	_, _ = setup(t)
	spaceID := "space-lock-order-" + util.GenerUUID()[:8]
	lowUID := "z-seat-" + util.GenerUUID()[:8]
	highUID := "a-seat-" + util.GenerUUID()[:8]
	seedSpace(t, spaceID, 1)
	seedUser(t, lowUID)
	seedUser(t, highUID)
	seedSpaceMember(t, spaceID, lowUID, 0, 1)
	seedSpaceMember(t, spaceID, highUID, 0, 1)

	var rows []struct {
		ID  int64  `db:"id"`
		UID string `db:"uid"`
	}
	_, err := testCtx.DB().SelectBySql(
		"SELECT id, uid FROM space_member WHERE space_id=? ORDER BY id",
		spaceID,
	).Load(&rows)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, lowUID, rows[0].UID)
	require.Equal(t, highUID, rows[1].UID)

	refs, err := PrepareGroupProjectSpaceSeatRefs(
		testCtx.DB(), spaceID, []string{highUID, lowUID},
	)
	require.NoError(t, err)

	blocker, err := testCtx.DB().Begin()
	require.NoError(t, err)
	defer blocker.Rollback()
	_, err = blocker.UpdateBySql(
		"UPDATE space_member SET version=version+1 WHERE id=?", rows[0].ID,
	).Exec()
	require.NoError(t, err)

	lockTx, err := testCtx.DB().Begin()
	require.NoError(t, err)
	defer lockTx.Rollback()
	lockDone := make(chan error, 1)
	go func() {
		_, lockErr := lockGroupProjectSpaceSeatsTx(lockTx, []groupProjectSeatKey{
			{SpaceID: spaceID, UID: highUID},
			{SpaceID: spaceID, UID: lowUID},
		}, refs)
		lockDone <- lockErr
	}()

	// Give the lock query time to reach the lower-id blocker before probing the
	// higher-id row. This is a real lock interaction, not a source-text guard.
	time.Sleep(250 * time.Millisecond)
	writer, err := sql.Open("mysql", projectTestMySQLAddr())
	require.NoError(t, err)
	defer writer.Close()
	_, err = writer.Exec("SET SESSION innodb_lock_wait_timeout = 1")
	require.NoError(t, err)
	_, writerErr := writer.Exec(
		"UPDATE space_member SET version=version+1 WHERE id=?", rows[1].ID,
	)

	require.NoError(t, blocker.Rollback())
	select {
	case lockErr := <-lockDone:
		require.NoError(t, lockErr)
	case <-time.After(3 * time.Second):
		t.Fatal("prepared seat lock did not finish after releasing the blocker")
	}
	require.NoError(t, lockTx.Rollback())
	if writerErr != nil {
		var myErr *mysql.MySQLError
		if !errors.As(writerErr, &myErr) || myErr.Number != 1205 {
			t.Fatalf("higher-id writer failed unexpectedly: %v", writerErr)
		}
		t.Fatalf("higher-id writer waited on a row that should not have been locked first: %v", writerErr)
	}
}

func TestGroupProjectCandidateExpandedDetectsOnlyNewMembers(t *testing.T) {
	if groupProjectCandidateExpanded([]string{"u1", "u2"}, []string{"u2", "u1"}) {
		t.Fatal("same membership in a different order must not force a retry")
	}
	if !groupProjectCandidateExpanded([]string{"u1"}, []string{"u1", "u2"}) {
		t.Fatal("a newly active Project member must force preparation retry")
	}
}
