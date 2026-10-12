package imreconcile

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/require"
)

func TestMySQLActiveSnapshotFinishesWhileEveryPageHasNewMutations(t *testing.T) {
	db, session := queueMySQL(t)
	require.NoError(t, Mutate(session, "g", func(tx *dbr.Tx) error {
		for i := 0; i < 260; i++ {
			if _, err := tx.Exec("INSERT INTO group_member(group_no,uid) VALUES ('g',?)", fmt.Sprintf("u%03d", i)); err != nil {
				return err
			}
		}
		for i := 1; i <= 17; i++ {
			if _, err := tx.Exec("INSERT INTO thread(id,group_no,short_id) VALUES (?,'g',?)", i, fmt.Sprint(i)); err != nil {
				return err
			}
		}
		return nil
	}))
	var mu sync.Mutex
	seen := map[string][]Snapshot{}
	w := &Worker{db: db, send: func(_ context.Context, s Snapshot) error {
		mu.Lock()
		defer mu.Unlock()
		seen[s.ChannelID] = append(seen[s.ChannelID], s)
		return nil
	}}
	var completed, desired uint64
	for step := 0; step < 20; step++ {
		progress, err := w.step(context.Background(), "g")
		require.NoError(t, err)
		require.True(t, progress)
		require.NoError(t, db.QueryRow("SELECT completed_revision FROM im_group_reconcile WHERE group_no='g'").Scan(&completed))
		if completed >= 1 {
			break
		}
		require.NoError(t, Mutate(session, "g", func(tx *dbr.Tx) error {
			_, err := tx.Exec("UPDATE group_member SET is_deleted=1-is_deleted WHERE group_no='g' AND uid='b'")
			return err
		}))
		// Simulate a restart between every scheduling quantum: no in-memory
		// copy of authority or cursor may be needed for the next page.
		w = &Worker{db: db, send: w.send}
	}
	require.EqualValues(t, 1, completed, "new writes must not starve the active snapshot")
	require.Len(t, seen, 18)
	for channel, pages := range seen {
		require.Len(t, pages, 3, channel)
		members := map[string]bool{}
		for i, p := range pages {
			require.EqualValues(t, 1, p.Revision)
			require.Equal(t, pages[0].SnapshotID, p.SnapshotID)
			require.EqualValues(t, i, p.PageIndex)
			for _, uid := range p.Subscribers {
				require.False(t, members[uid])
				members[uid] = true
			}
		}
		require.Len(t, members, 262)
		require.True(t, members["b"], "all pages must retain the captured version")
	}
	require.NoError(t, db.QueryRow("SELECT revision FROM im_group_reconcile WHERE group_no='g'").Scan(&desired))
	require.Greater(t, desired, completed)
	seen = map[string][]Snapshot{}
	for i := 0; i < 20; i++ {
		_, err := w.step(context.Background(), "g")
		require.NoError(t, err)
		var pending int
		require.NoError(t, db.QueryRow("SELECT pending FROM im_group_reconcile WHERE group_no='g'").Scan(&pending))
		if pending == 0 {
			break
		}
	}
	require.NoError(t, db.QueryRow("SELECT completed_revision FROM im_group_reconcile WHERE group_no='g'").Scan(&completed))
	require.Equal(t, desired, completed)
	for _, pages := range seen {
		for _, p := range pages {
			require.Equal(t, desired, p.Revision)
		}
	}
	for _, table := range []string{"im_reconcile_snapshot_page", "im_reconcile_snapshot_channel"} {
		var n int
		require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM "+table).Scan(&n))
		require.Zero(t, n)
	}
}

func TestMySQLSnapshotPartialSuccessDoesNotReplaySuccessfulSibling(t *testing.T) {
	db, session := queueMySQL(t)
	require.NoError(t, Mutate(session, "g", func(tx *dbr.Tx) error {
		_, err := tx.Exec("INSERT INTO thread(id,group_no,short_id) VALUES (1,'g','1'),(2,'g','2')")
		return err
	}))
	var mu sync.Mutex
	calls := map[string]int{}
	fail := true
	w := &Worker{db: db, send: func(_ context.Context, s Snapshot) error {
		mu.Lock()
		defer mu.Unlock()
		calls[s.ChannelID]++
		if s.ChannelID == "g" && fail {
			return errors.New("lost reply")
		}
		return nil
	}}
	progress, err := w.step(context.Background(), "g")
	require.Error(t, err)
	require.True(t, progress)
	fail = false
	_, err = db.Exec("UPDATE im_group_reconcile SET next_attempt_at=UTC_TIMESTAMP(6)")
	require.NoError(t, err)
	progress, err = w.step(context.Background(), "g")
	require.NoError(t, err)
	require.True(t, progress)
	require.Equal(t, map[string]int{"g": 2, "g____1": 1, "g____2": 1}, calls)
}

func TestMySQLSnapshotClaimFailureBacksOffAndReportsError(t *testing.T) {
	db, session := queueMySQL(t)
	require.NoError(t, Mutate(session, "g", func(*dbr.Tx) error { return nil }))
	w := &Worker{db: db, send: func(context.Context, Snapshot) error { t.Error("corrupt claim must not send"); return nil }}
	p, err := w.claim(context.Background(), "g")
	require.NoError(t, err)
	require.NotNil(t, p)
	_, err = db.Exec("DELETE FROM im_reconcile_snapshot_page")
	require.NoError(t, err)
	_, err = db.Exec("UPDATE im_group_reconcile SET lease_until=NULL,lease_owner=''")
	require.NoError(t, err)
	_, err = w.step(context.Background(), "g")
	require.Error(t, err)
	var attempts int
	var message string
	var future bool
	require.NoError(t, db.QueryRow("SELECT attempts,last_error,next_attempt_at>UTC_TIMESTAMP(6) FROM im_group_reconcile").Scan(&attempts, &message, &future))
	require.Equal(t, 1, attempts)
	require.NotEmpty(t, message)
	require.True(t, future)
	progress, err := w.step(context.Background(), "g")
	require.NoError(t, err)
	require.False(t, progress)
}

func TestMySQLSnapshotUpgradeResetsOnceWithoutLosingNewerDesiredState(t *testing.T) {
	db, session := queueMySQL(t)
	require.NoError(t, Mutate(session, "g", func(*dbr.Tx) error { return nil }))
	w := &Worker{db: db}
	p, err := w.claim(context.Background(), "g")
	require.NoError(t, err)
	require.NoError(t, Mutate(session, "g", func(tx *dbr.Tx) error {
		_, err := tx.Exec("UPDATE group_member SET is_deleted=1 WHERE uid='b'")
		return err
	}))
	upgrade := &snapshotHTTPError{Status: http.StatusConflict, Code: "snapshot_upgrade_required"}
	progress, err := w.checkpoint(p, []error{upgrade})
	require.NoError(t, err)
	require.True(t, progress)
	progress, err = w.checkpoint(p, []error{upgrade})
	require.NoError(t, err)
	require.False(t, progress)
	var revision uint64
	require.NoError(t, db.QueryRow("SELECT revision FROM im_group_reconcile").Scan(&revision))
	require.EqualValues(t, 3, revision)
	w.send = func(_ context.Context, s Snapshot) error {
		require.EqualValues(t, 3, s.Revision)
		require.Equal(t, []string{"a"}, s.Subscribers)
		return nil
	}
	progress, err = w.step(context.Background(), "g")
	require.NoError(t, err)
	require.True(t, progress)
}

func TestMySQLSnapshotOrdinaryConflictDoesNotInventNewRevision(t *testing.T) {
	db, session := queueMySQL(t)
	require.NoError(t, Mutate(session, "g", func(*dbr.Tx) error { return nil }))
	w := &Worker{db: db, send: func(context.Context, Snapshot) error {
		return &snapshotHTTPError{Status: http.StatusConflict, Code: "revision_conflict"}
	}}
	_, err := w.step(context.Background(), "g")
	require.Error(t, err)
	var revision, active uint64
	var pending int
	require.NoError(t, db.QueryRow("SELECT revision,active_revision,pending FROM im_group_reconcile").Scan(&revision, &active, &pending))
	require.EqualValues(t, 1, revision)
	require.EqualValues(t, 1, active)
	require.Equal(t, 1, pending)
}

func TestMySQLConfirmedDeletedChildIsNotResentOnUnrelatedMutation(t *testing.T) {
	db, session := queueMySQL(t)
	tx, err := session.Begin()
	require.NoError(t, err)
	require.NoError(t, RetireChildTx(tx, 1, "G", "removed"))
	require.NoError(t, tx.Commit())
	var mu sync.Mutex
	sent := map[string]int{}
	w := &Worker{db: db, send: func(_ context.Context, s Snapshot) error {
		mu.Lock()
		defer mu.Unlock()
		sent[s.ChannelID]++
		return nil
	}}
	_, err = w.step(context.Background(), "g")
	require.NoError(t, err)
	require.Equal(t, 1, sent["g____removed"])
	require.NoError(t, Mutate(session, "g", func(*dbr.Tx) error { return nil }))
	_, err = w.step(context.Background(), "g")
	require.NoError(t, err)
	require.Equal(t, 1, sent["g____removed"])
	var revision uint64
	require.NoError(t, db.QueryRow("SELECT completed_revision FROM im_reconcile_deleted_channel").Scan(&revision))
	require.EqualValues(t, 1, revision)
}

func TestMySQLRepeatedlyFailedObsoleteSnapshotYieldsToCompensation(t *testing.T) {
	db, session := queueMySQL(t)
	require.NoError(t, Mutate(session, "g", func(*dbr.Tx) error { return nil }))
	w := &Worker{db: db, send: func(_ context.Context, s Snapshot) error {
		if s.Revision == 1 {
			return errors.New("old create reply is permanently lost")
		}
		require.Empty(t, s.Subscribers)
		require.Equal(t, 1, s.Disband)
		return nil
	}}
	_, err := w.step(context.Background(), "g")
	require.Error(t, err)
	require.NoError(t, Mutate(session, "g", func(tx *dbr.Tx) error {
		_, err := tx.Exec("DELETE FROM `group` WHERE group_no='g'")
		return err
	}))
	for i := 0; i < 2; i++ {
		_, err = db.Exec("UPDATE im_group_reconcile SET next_attempt_at=UTC_TIMESTAMP(6)")
		require.NoError(t, err)
		_, err = w.step(context.Background(), "g")
		require.Error(t, err)
	}
	_, err = db.Exec("UPDATE im_group_reconcile SET next_attempt_at=UTC_TIMESTAMP(6)")
	require.NoError(t, err)
	_, err = w.step(context.Background(), "g")
	require.NoError(t, err)
	var pending int
	var completed uint64
	require.NoError(t, db.QueryRow("SELECT pending,completed_revision FROM im_group_reconcile").Scan(&pending, &completed))
	require.Zero(t, pending)
	require.EqualValues(t, 2, completed)
}

func TestMySQLFlushAdoptsExistingGroupButDoesNotInventMissingGroupSuccess(t *testing.T) {
	db, _ := queueMySQL(t)
	called := false
	w := &Worker{db: db, send: func(_ context.Context, s Snapshot) error {
		called = true
		require.Equal(t, []string{"a", "b"}, s.Subscribers)
		return nil
	}}
	require.NoError(t, w.flush(context.Background(), "g"))
	require.True(t, called)
	require.Error(t, w.flush(context.Background(), "missing"))
}

func TestMySQLSnapshotPreservesParentAndChildBanScopes(t *testing.T) {
	db, session := queueMySQL(t)
	require.NoError(t, Mutate(session, "g", func(tx *dbr.Tx) error {
		if _, err := tx.Exec("UPDATE `group` SET status=0 WHERE group_no='g'"); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO thread(id,group_no,short_id,status) VALUES (1,'g','live',1),(2,'g','deleted',3)")
		return err
	}))
	var mu sync.Mutex
	seen := map[string]Snapshot{}
	w := &Worker{db: db, send: func(_ context.Context, s Snapshot) error {
		mu.Lock()
		defer mu.Unlock()
		seen[s.ChannelID] = s
		return nil
	}}
	_, err := w.step(context.Background(), "g")
	require.NoError(t, err)
	require.Equal(t, 1, seen["g"].Ban)
	require.Zero(t, seen["g____live"].Ban)
	require.Equal(t, 1, seen["g____deleted"].Ban)
	for _, s := range seen {
		require.Equal(t, []string{"a", "b"}, s.Subscribers)
	}
}
