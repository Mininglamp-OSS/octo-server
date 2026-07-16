package notify

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	// Blank imports so NewTestServer runs the migrations for the tables the
	// welcome ledger JOINs against. notify already pulls space/user/group
	// transitively; robot owns the `robot` table the space migration JOINs.
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"
)

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------

func swTestServer(t *testing.T) *config.Context {
	t.Helper()
	// modules/common.Route generates + stores an encrypted key at setup; without
	// a master key it panics. This is imported transitively via our config path.
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	return ctx
}

func swInsertUser(t *testing.T, ctx *config.Context, uid string, robot int) {
	t.Helper()
	// short_no has a UNIQUE index; give each user a distinct value (empty '' would
	// collide across inserts).
	_, err := ctx.DB().InsertBySql("INSERT INTO `user` (uid, name, short_no, robot) VALUES (?, ?, ?, ?)", uid, uid, uid, robot).Exec()
	require.NoError(t, err)
}

func swInsertSpace(t *testing.T, ctx *config.Context, spaceID string, status int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql("INSERT INTO `space` (space_id, name, status) VALUES (?, ?, ?)", spaceID, spaceID, status).Exec()
	require.NoError(t, err)
}

func swInsertMember(t *testing.T, ctx *config.Context, spaceID, uid string, status int, createdAt time.Time) {
	t.Helper()
	// Store created_at via FROM_UNIXTIME so it lands in the DB session's wall
	// clock exactly as NOW() would in production, and UNIX_TIMESTAMP round-trips
	// back to createdAt.Unix() regardless of the test DB's session TZ.
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `space_member` (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, 0, ?, FROM_UNIXTIME(?), FROM_UNIXTIME(?))",
		spaceID, uid, status, createdAt.Unix(), createdAt.Unix(),
	).Exec()
	require.NoError(t, err)
}

// swInsertLedger inserts a ledger row directly in a chosen state (for sweep/CAS
// tests that bypass the claim path).
func swInsertLedger(t *testing.T, ctx *config.Context, spaceID, uid string, status int, owner string, claimExpire *time.Time, attempts int) int64 {
	t.Helper()
	now := time.Now().UTC()
	res, err := ctx.DB().InsertBySql(
		"INSERT INTO "+spaceWelcomeTable+" (space_id, uid, status, attempts, claim_owner, claim_expire_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		spaceID, uid, status, attempts, owner, claimExpire, now, now,
	).Exec()
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

func swRowStatus(t *testing.T, ctx *config.Context, id int64) (status, attempts int, errClass, owner string, messageID int64) {
	t.Helper()
	var row struct {
		Status    int    `db:"status"`
		Attempts  int    `db:"attempts"`
		ErrorCl   string `db:"error_class"`
		Owner     string `db:"claim_owner"`
		MessageID int64  `db:"message_id"`
	}
	err := ctx.DB().SelectBySql(
		"SELECT status, attempts, COALESCE(error_class,'') AS error_class, COALESCE(claim_owner,'') AS claim_owner, COALESCE(message_id,0) AS message_id FROM "+spaceWelcomeTable+" WHERE id=?",
		id,
	).LoadOne(&row)
	require.NoError(t, err)
	return row.Status, row.Attempts, row.ErrorCl, row.Owner, row.MessageID
}

func swCountRows(t *testing.T, ctx *config.Context, spaceID string) int {
	t.Helper()
	var n int
	err := ctx.DB().SelectBySql("SELECT COUNT(*) FROM "+spaceWelcomeTable+" WHERE space_id=?", spaceID).LoadOne(&n)
	require.NoError(t, err)
	return n
}

func bg() context.Context { return context.Background() }

// ---------------------------------------------------------------------------
// Ledger store: enqueue idempotency & unique constraint
// ---------------------------------------------------------------------------

func TestStore_UpsertPending_Idempotent(t *testing.T) {
	ctx := swTestServer(t)
	store := newSpaceWelcomeStore(ctx.DB(), "owner-1")
	now := time.Now().UTC()

	inserted, err := store.upsertPending(bg(), "spc_1", "u_1", now)
	require.NoError(t, err)
	assert.True(t, inserted, "first upsert must insert")

	inserted, err = store.upsertPending(bg(), "spc_1", "u_1", now)
	require.NoError(t, err)
	assert.False(t, inserted, "duplicate upsert must report not-inserted (dedup)")

	assert.Equal(t, 1, swCountRows(t, ctx, "spc_1"), "unique (space_id,uid) must keep exactly one row")
}

func TestStore_UpsertPending_ConcurrentSingleInsert(t *testing.T) {
	ctx := swTestServer(t)
	store := newSpaceWelcomeStore(ctx.DB(), "owner-1")
	now := time.Now().UTC()

	var wg sync.WaitGroup
	var insertedCount int32
	var mu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ins, err := store.upsertPending(bg(), "spc_c", "u_c", now); err == nil && ins {
				mu.Lock()
				insertedCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 1, insertedCount, "exactly one concurrent upsert reports inserted")
	assert.Equal(t, 1, swCountRows(t, ctx, "spc_c"))
}

// ---------------------------------------------------------------------------
// Claim: SELECT ... FOR UPDATE SKIP LOCKED
// ---------------------------------------------------------------------------

func TestStore_ClaimOne(t *testing.T) {
	ctx := swTestServer(t)
	store := newSpaceWelcomeStore(ctx.DB(), "owner-A")
	now := time.Now().UTC()
	_, err := store.upsertPending(bg(), "spc_1", "u_1", now)
	require.NoError(t, err)

	row, err := store.claimOne(bg(), "spc_1", now)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "u_1", row.UID)

	status, _, _, owner, _ := swRowStatus(t, ctx, row.ID)
	assert.Equal(t, swStatusClaimed, status)
	assert.Equal(t, "owner-A", owner)

	// Nothing left to claim.
	row2, err := store.claimOne(bg(), "spc_1", now)
	require.NoError(t, err)
	assert.Nil(t, row2)
}

func TestStore_ClaimOne_ConcurrentSingleWinner(t *testing.T) {
	ctx := swTestServer(t)
	now := time.Now().UTC()
	sA := newSpaceWelcomeStore(ctx.DB(), "owner-A")
	sB := newSpaceWelcomeStore(ctx.DB(), "owner-B")
	_, err := sA.upsertPending(bg(), "spc_1", "u_1", now)
	require.NoError(t, err)

	var wins int32
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, st := range []*spaceWelcomeStore{sA, sB} {
		wg.Add(1)
		go func(store *spaceWelcomeStore) {
			defer wg.Done()
			if row, err := store.claimOne(bg(), "spc_1", now); err == nil && row != nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(st)
	}
	wg.Wait()
	assert.EqualValues(t, 1, wins, "SKIP LOCKED must let exactly one replica claim the row")
}

// next_retry_at in the future must not be claimable yet.
func TestStore_ClaimOne_RespectsNextRetry(t *testing.T) {
	ctx := swTestServer(t)
	store := newSpaceWelcomeStore(ctx.DB(), "owner-A")
	now := time.Now().UTC()
	id := swInsertLedger(t, ctx, "spc_1", "u_1", swStatusPending, "", nil, 1)
	_, err := ctx.DB().UpdateBySql("UPDATE "+spaceWelcomeTable+" SET next_retry_at=? WHERE id=?", now.Add(time.Hour), id).Exec()
	require.NoError(t, err)

	row, err := store.claimOne(bg(), "spc_1", now)
	require.NoError(t, err)
	assert.Nil(t, row, "row with next_retry_at in the future is not yet claimable")

	row, err = store.claimOne(bg(), "spc_1", now.Add(2*time.Hour))
	require.NoError(t, err)
	assert.NotNil(t, row, "row becomes claimable once next_retry_at has passed")
}

// ---------------------------------------------------------------------------
// Sweep
// ---------------------------------------------------------------------------

func TestStore_SweepClaimed_BackToPending(t *testing.T) {
	ctx := swTestServer(t)
	store := newSpaceWelcomeStore(ctx.DB(), "owner-A")
	now := time.Now().UTC()
	past := now.Add(-time.Minute)
	id := swInsertLedger(t, ctx, "spc_1", "u_1", swStatusClaimed, "owner-dead", &past, 2)

	n, err := store.sweepClaimed(bg(), "spc_1", now)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)

	status, attempts, _, _, _ := swRowStatus(t, ctx, id)
	assert.Equal(t, swStatusPending, status)
	assert.Equal(t, 2, attempts, "sweep must not consume the retry budget")
}

func TestStore_SweepDispatching_ToUnknown(t *testing.T) {
	ctx := swTestServer(t)
	store := newSpaceWelcomeStore(ctx.DB(), "owner-A")
	now := time.Now().UTC()
	past := now.Add(-time.Minute)
	id := swInsertLedger(t, ctx, "spc_1", "u_1", swStatusDispatching, "owner-dead", &past, 0)

	n, err := store.sweepDispatching(bg(), "spc_1", now)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)

	status, _, errClass, owner, _ := swRowStatus(t, ctx, id)
	assert.Equal(t, swStatusUnknown, status)
	assert.Equal(t, swErrClaimExpired, errClass)
	assert.Empty(t, owner, "terminal sweep must clear claim_owner (no phantom-leased terminal rows)")
}

// A not-yet-expired lease must survive the sweep.
func TestStore_Sweep_RespectsLease(t *testing.T) {
	ctx := swTestServer(t)
	store := newSpaceWelcomeStore(ctx.DB(), "owner-A")
	now := time.Now().UTC()
	future := now.Add(time.Minute)
	id := swInsertLedger(t, ctx, "spc_1", "u_1", swStatusClaimed, "owner-live", &future, 0)

	n, err := store.sweepClaimed(bg(), "spc_1", now)
	require.NoError(t, err)
	assert.EqualValues(t, 0, n)
	status, _, _, _, _ := swRowStatus(t, ctx, id)
	assert.Equal(t, swStatusClaimed, status)
}

// ---------------------------------------------------------------------------
// CAS transitions & cross-owner protection
// ---------------------------------------------------------------------------

func TestStore_CAS_CrossOwnerBlocked(t *testing.T) {
	ctx := swTestServer(t)
	now := time.Now().UTC()
	future := now.Add(time.Minute)
	id := swInsertLedger(t, ctx, "spc_1", "u_1", swStatusClaimed, "owner-A", &future, 0)

	// A different replica must not be able to move a row it does not own.
	sB := newSpaceWelcomeStore(ctx.DB(), "owner-B")
	ok, err := sB.casToDispatching(bg(), id, "zh-CN", now)
	require.NoError(t, err)
	assert.False(t, ok, "cross-owner CAS must not match")

	status, _, _, _, _ := swRowStatus(t, ctx, id)
	assert.Equal(t, swStatusClaimed, status, "row untouched by non-owner")

	// The true owner succeeds.
	sA := newSpaceWelcomeStore(ctx.DB(), "owner-A")
	ok, err = sA.casToDispatching(bg(), id, "zh-CN", now)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestStore_CasPreIMFailure_BackoffThenFailed(t *testing.T) {
	ctx := swTestServer(t)
	store := newSpaceWelcomeStore(ctx.DB(), "owner-A")
	now := time.Now().UTC()

	// attempts 0->1, 1->2, 2->3 back off to pending; 3->4 fails terminally.
	for attempt := 0; attempt <= swMaxPreIMAttempts; attempt++ {
		id := swInsertLedger(t, ctx, "spc_p", "u_p"+string(rune('a'+attempt)), swStatusClaimed, "owner-A", nil, attempt)
		ok, err := store.casPreIMFailure(bg(), id, attempt, swErrBotNotReady, now)
		require.NoError(t, err)
		require.True(t, ok)
		status, attempts, _, _, _ := swRowStatus(t, ctx, id)
		assert.Equal(t, attempt+1, attempts, "attempts increments by one")
		if attempt+1 > swMaxPreIMAttempts {
			assert.Equal(t, swStatusFailed, status, "4th consecutive pre-IM failure is terminal-failed")
		} else {
			assert.Equal(t, swStatusPending, status, "pre-IM failure within budget backs off to pending")
		}
	}
}

// ---------------------------------------------------------------------------
// Pre-send recipient re-check
// ---------------------------------------------------------------------------

func TestStore_Precheck(t *testing.T) {
	ctx := swTestServer(t)
	store := newSpaceWelcomeStore(ctx.DB(), "owner-A")
	now := time.Now().UTC()

	// eligible human member
	swInsertUser(t, ctx, "human", 0)
	swInsertMember(t, ctx, "spc_1", "human", 1, now)
	// robot member
	swInsertUser(t, ctx, "bot", 1)
	swInsertMember(t, ctx, "spc_1", "bot", 1, now)
	// left member (user exists, no active membership)
	swInsertUser(t, ctx, "left", 0)
	// orphan: no user row, but ineligible on user lookup

	cases := []struct {
		uid      string
		eligible bool
		errClass string
	}{
		{"human", true, ""},
		{"bot", false, swErrHumanFilter},
		{"left", false, swErrMemberLeft},
		{"orphan", false, swErrOrphanMember},
		{"notification", false, swErrHumanFilter}, // system bot as recipient
	}
	for _, tc := range cases {
		t.Run(tc.uid, func(t *testing.T) {
			res, err := store.precheckRecipient(bg(), "spc_1", tc.uid, isSystemBotForTest)
			require.NoError(t, err)
			assert.Equal(t, tc.eligible, res.eligible)
			if !tc.eligible {
				assert.Equal(t, tc.errClass, res.errClass)
			}
		})
	}
}

// isSystemBotForTest mirrors pkg/space.IsSystemBot for the recipient-only scope.
func isSystemBotForTest(uid string) bool { return uid == "notification" }

// ---------------------------------------------------------------------------
// Reconcile scan & first-join eligibility
// ---------------------------------------------------------------------------

func TestStore_ReconcileScan(t *testing.T) {
	ctx := swTestServer(t)
	store := newSpaceWelcomeStore(ctx.DB(), "owner-A")
	activeFrom := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	before := activeFrom.Add(-time.Hour)
	after := activeFrom.Add(time.Hour)

	swInsertSpace(t, ctx, "spc_1", 1)
	// eligible: human, active, joined after active_from, no ledger row
	swInsertUser(t, ctx, "new_human", 0)
	swInsertMember(t, ctx, "spc_1", "new_human", 1, after)
	// excluded: joined before active_from
	swInsertUser(t, ctx, "old_human", 0)
	swInsertMember(t, ctx, "spc_1", "old_human", 1, before)
	// excluded: robot
	swInsertUser(t, ctx, "robot1", 1)
	swInsertMember(t, ctx, "spc_1", "robot1", 1, after)
	// excluded: already has a ledger row
	swInsertUser(t, ctx, "done_human", 0)
	swInsertMember(t, ctx, "spc_1", "done_human", 1, after)
	_, err := store.upsertPending(bg(), "spc_1", "done_human", time.Now().UTC())
	require.NoError(t, err)
	// excluded: left (status=0)
	swInsertUser(t, ctx, "left_human", 0)
	swInsertMember(t, ctx, "spc_1", "left_human", 0, after)
	// excluded: different space
	swInsertUser(t, ctx, "other_space_human", 0)
	swInsertMember(t, ctx, "spc_2", "other_space_human", 1, after)

	uids, err := store.reconcileScan(bg(), "spc_1", activeFrom, []string{"notification", "botfather"}, 500)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"new_human"}, uids)
}

func TestStore_FirstJoinHumanMember_Boundary(t *testing.T) {
	ctx := swTestServer(t)
	store := newSpaceWelcomeStore(ctx.DB(), "owner-A")
	activeFrom := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)

	swInsertUser(t, ctx, "at_boundary", 0)
	swInsertMember(t, ctx, "spc_1", "at_boundary", 1, activeFrom) // created_at == active_from → included
	swInsertUser(t, ctx, "before", 0)
	swInsertMember(t, ctx, "spc_1", "before", 1, activeFrom.Add(-time.Second))

	ok, err := store.firstJoinHumanMember(bg(), "spc_1", "at_boundary", activeFrom)
	require.NoError(t, err)
	assert.True(t, ok, "created_at == active_from must be included (>=)")

	ok, err = store.firstJoinHumanMember(bg(), "spc_1", "before", activeFrom)
	require.NoError(t, err)
	assert.False(t, ok, "created_at before active_from must be excluded")
}

// ---------------------------------------------------------------------------
// Service: dispatch state machine (stubbed sender)
// ---------------------------------------------------------------------------

func swSetConfig(t *testing.T, ctx *config.Context, settings *common.SystemSettings, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		vt := "string"
		if k == "space_welcome_enabled" {
			vt = "bool"
		}
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO system_setting (category, key_name, value, value_type, description) VALUES ('onboarding', ?, ?, ?, '') "+
				"ON DUPLICATE KEY UPDATE value=VALUES(value), value_type=VALUES(value_type)",
			k, v, vt,
		).Exec()
		require.NoError(t, err)
	}
	require.NoError(t, settings.Load())
}

func swEnabledConfig(spaceID string, activeFrom time.Time) map[string]string {
	return map[string]string{
		"space_welcome_enabled":       "1",
		"space_welcome_space_id":      spaceID,
		"space_welcome_active_from":   activeFrom.UTC().Format(time.RFC3339),
		"space_welcome_message_zh_cn": "欢迎加入本空间",
		"space_welcome_message_en_us": "Welcome to the space",
	}
}

func swNewService(ctx *config.Context, settings *common.SystemSettings) *spaceWelcomeService {
	svc := newSpaceWelcomeService(ctx, settings, nil, NotifyBotUID(), func() bool { return true })
	return svc
}

func TestService_Dispatch_Success(t *testing.T) {
	ctx := swTestServer(t)
	settings := common.EnsureSystemSettings(ctx)
	activeFrom := time.Now().UTC().Add(-time.Hour)
	swInsertSpace(t, ctx, "spc_1", 1)
	swInsertUser(t, ctx, "u_1", 0)
	swInsertMember(t, ctx, "spc_1", "u_1", 1, time.Now().UTC())
	swSetConfig(t, ctx, settings, swEnabledConfig("spc_1", activeFrom))

	svc := swNewService(ctx, settings)
	var gotReq *config.MsgSendReq
	svc.sendFn = func(_ context.Context, req *config.MsgSendReq) (*swSendResult, error) {
		gotReq = req
		return &swSendResult{messageID: 999, clientMsgNo: "cmn-1"}, nil
	}

	id := swInsertLedger(t, ctx, "spc_1", "u_1", swStatusClaimed, svc.store.claimOwner, ptrTime(time.Now().UTC().Add(time.Minute)), 0)
	svc.dispatch(bg(), settings.SpaceWelcomeConfig(), &spaceWelcomeRow{ID: id, SpaceID: "spc_1", UID: "u_1"})

	status, _, _, _, messageID := swRowStatus(t, ctx, id)
	assert.Equal(t, swStatusSent, status)
	assert.EqualValues(t, 999, messageID)
	assert.EqualValues(t, 1, svc.metrics.sendSuccess.Load())
	// Wire protocol: from notification, red_dot=1, personal channel.
	require.NotNil(t, gotReq)
	assert.Equal(t, "notification", gotReq.FromUID)
	assert.EqualValues(t, 1, gotReq.Header.RedDot)
}

func TestService_Dispatch_Unknown(t *testing.T) {
	ctx := swTestServer(t)
	settings := common.EnsureSystemSettings(ctx)
	activeFrom := time.Now().UTC().Add(-time.Hour)
	swInsertSpace(t, ctx, "spc_1", 1)
	swInsertUser(t, ctx, "u_1", 0)
	swInsertMember(t, ctx, "spc_1", "u_1", 1, time.Now().UTC())
	swSetConfig(t, ctx, settings, swEnabledConfig("spc_1", activeFrom))

	svc := swNewService(ctx, settings)
	svc.sendFn = func(_ context.Context, _ *config.MsgSendReq) (*swSendResult, error) {
		return nil, &swSendError{class: swErrIMTimeout}
	}
	id := swInsertLedger(t, ctx, "spc_1", "u_1", swStatusClaimed, svc.store.claimOwner, ptrTime(time.Now().UTC().Add(time.Minute)), 0)
	svc.dispatch(bg(), settings.SpaceWelcomeConfig(), &spaceWelcomeRow{ID: id, SpaceID: "spc_1", UID: "u_1"})

	status, _, errClass, _, _ := swRowStatus(t, ctx, id)
	assert.Equal(t, swStatusUnknown, status)
	assert.Equal(t, swErrIMTimeout, errClass)
	assert.EqualValues(t, 1, svc.metrics.sendUnknown.Load())
}

func TestService_Dispatch_SkipMemberLeft(t *testing.T) {
	ctx := swTestServer(t)
	settings := common.EnsureSystemSettings(ctx)
	activeFrom := time.Now().UTC().Add(-time.Hour)
	swInsertSpace(t, ctx, "spc_1", 1)
	swInsertUser(t, ctx, "u_gone", 0) // user exists but NOT a member
	swSetConfig(t, ctx, settings, swEnabledConfig("spc_1", activeFrom))

	svc := swNewService(ctx, settings)
	sendCalled := false
	svc.sendFn = func(_ context.Context, _ *config.MsgSendReq) (*swSendResult, error) {
		sendCalled = true
		return &swSendResult{messageID: 1}, nil
	}
	id := swInsertLedger(t, ctx, "spc_1", "u_gone", swStatusClaimed, svc.store.claimOwner, ptrTime(time.Now().UTC().Add(time.Minute)), 0)
	svc.dispatch(bg(), settings.SpaceWelcomeConfig(), &spaceWelcomeRow{ID: id, SpaceID: "spc_1", UID: "u_gone"})

	status, _, errClass, _, _ := swRowStatus(t, ctx, id)
	assert.Equal(t, swStatusSkipped, status)
	assert.Equal(t, swErrMemberLeft, errClass)
	assert.False(t, sendCalled, "must not deliver to an ineligible recipient")
	assert.EqualValues(t, 1, svc.metrics.skipNonMember.Load())
}

// ---------------------------------------------------------------------------
// Service: event enqueue & disable
// ---------------------------------------------------------------------------

func TestService_HandleMemberJoin_EnqueueAndDedup(t *testing.T) {
	ctx := swTestServer(t)
	settings := common.EnsureSystemSettings(ctx)
	activeFrom := time.Now().UTC().Add(-time.Hour)
	swInsertSpace(t, ctx, "spc_1", 1)
	swInsertUser(t, ctx, "u_1", 0)
	swInsertMember(t, ctx, "spc_1", "u_1", 1, time.Now().UTC())
	swSetConfig(t, ctx, settings, swEnabledConfig("spc_1", activeFrom))

	svc := swNewService(ctx, settings)
	svc.handleMemberJoin("spc_1", "u_1")
	assert.Equal(t, 1, swCountRows(t, ctx, "spc_1"))
	assert.EqualValues(t, 1, svc.metrics.enqueueEvent.Load())

	// Replayed event → dedup, no new row.
	svc.handleMemberJoin("spc_1", "u_1")
	assert.Equal(t, 1, swCountRows(t, ctx, "spc_1"))
	assert.EqualValues(t, 1, svc.metrics.enqueueEvent.Load())
	assert.EqualValues(t, 1, svc.metrics.dedupEvent.Load())
}

func TestService_HandleMemberJoin_Excludes(t *testing.T) {
	ctx := swTestServer(t)
	settings := common.EnsureSystemSettings(ctx)
	activeFrom := time.Now().UTC()
	swInsertSpace(t, ctx, "spc_1", 1)
	swSetConfig(t, ctx, settings, swEnabledConfig("spc_1", activeFrom))
	svc := swNewService(ctx, settings)

	// robot
	swInsertUser(t, ctx, "bot", 1)
	swInsertMember(t, ctx, "spc_1", "bot", 1, activeFrom.Add(time.Minute))
	// pre-active_from human
	swInsertUser(t, ctx, "old", 0)
	swInsertMember(t, ctx, "spc_1", "old", 1, activeFrom.Add(-time.Hour))

	svc.handleMemberJoin("spc_1", "bot")
	svc.handleMemberJoin("spc_1", "old")
	svc.handleMemberJoin("spc_2", "someone") // wrong space
	svc.handleMemberJoin("spc_1", "notification")
	assert.Equal(t, 0, swCountRows(t, ctx, "spc_1"))
}

func TestService_HandleMemberJoin_DisabledNoEnqueue(t *testing.T) {
	ctx := swTestServer(t)
	settings := common.EnsureSystemSettings(ctx)
	swInsertSpace(t, ctx, "spc_1", 1)
	swInsertUser(t, ctx, "u_1", 0)
	swInsertMember(t, ctx, "spc_1", "u_1", 1, time.Now().UTC())
	cfg := swEnabledConfig("spc_1", time.Now().UTC().Add(-time.Hour))
	cfg["space_welcome_enabled"] = "0" // disabled
	swSetConfig(t, ctx, settings, cfg)

	svc := swNewService(ctx, settings)
	svc.handleMemberJoin("spc_1", "u_1")
	assert.Equal(t, 0, swCountRows(t, ctx, "spc_1"), "disabled feature must not enqueue")
}

func TestService_ReconcileOnce_CatchUp(t *testing.T) {
	ctx := swTestServer(t)
	settings := common.EnsureSystemSettings(ctx)
	activeFrom := time.Now().UTC().Add(-time.Hour)
	swInsertSpace(t, ctx, "spc_1", 1)
	swInsertUser(t, ctx, "u_1", 0)
	swInsertMember(t, ctx, "spc_1", "u_1", 1, time.Now().UTC()) // joined, but event was "lost"
	swSetConfig(t, ctx, settings, swEnabledConfig("spc_1", activeFrom))

	svc := swNewService(ctx, settings)
	svc.runReconcileOnce(bg())
	assert.Equal(t, 1, swCountRows(t, ctx, "spc_1"), "reconciler must catch up a member with no ledger row")
	assert.EqualValues(t, 1, svc.metrics.enqueueReconciler.Load())
}

func ptrTime(t time.Time) *time.Time { return &t }

// ---------------------------------------------------------------------------
// Review follow-ups: worker drive-loop cap, immediate disable, config-error
// preflight (P1-2 / P2-4).
// ---------------------------------------------------------------------------

// TestService_Worker_WakeCap seeds more than the per-wake cap of pending rows
// and verifies one runWorkerWake claims at most swWorkerWakeCap of them.
func TestService_Worker_WakeCap(t *testing.T) {
	ctx := swTestServer(t)
	settings := common.EnsureSystemSettings(ctx)
	activeFrom := time.Now().UTC().Add(-time.Hour)
	swInsertSpace(t, ctx, "spc_1", 1)
	swSetConfig(t, ctx, settings, swEnabledConfig("spc_1", activeFrom))

	total := swWorkerWakeCap + 5
	for i := 0; i < total; i++ {
		uid := fmt.Sprintf("u_%02d", i)
		swInsertUser(t, ctx, uid, 0)
		swInsertMember(t, ctx, "spc_1", uid, 1, time.Now().UTC())
		swInsertLedger(t, ctx, "spc_1", uid, swStatusPending, "", nil, 0)
	}

	svc := swNewService(ctx, settings)
	svc.sendFn = func(_ context.Context, _ *config.MsgSendReq) (*swSendResult, error) {
		return &swSendResult{messageID: 1, clientMsgNo: "x"}, nil
	}
	svc.runWorkerWake(bg())

	var sent, pending int64
	sent, _ = svc.store.countByStatus(bg(), "spc_1", swStatusSent)
	pending, _ = svc.store.countByStatus(bg(), "spc_1", swStatusPending)
	assert.EqualValues(t, swWorkerWakeCap, sent, "one wake sends at most the per-wake cap")
	assert.EqualValues(t, 5, pending, "the remainder stays pending for the next wake")
}

// TestService_Worker_ImmediateDisableMidDrain flips the config to disabled after
// the first successful send; the worker must stop claiming the rest of this wake
// (not drain the full cap). Guards the P1-2 fix.
func TestService_Worker_ImmediateDisableMidDrain(t *testing.T) {
	ctx := swTestServer(t)
	settings := common.EnsureSystemSettings(ctx)
	activeFrom := time.Now().UTC().Add(-time.Hour)
	swInsertSpace(t, ctx, "spc_1", 1)
	swSetConfig(t, ctx, settings, swEnabledConfig("spc_1", activeFrom))

	for i := 0; i < 3; i++ {
		uid := fmt.Sprintf("u_%02d", i)
		swInsertUser(t, ctx, uid, 0)
		swInsertMember(t, ctx, "spc_1", uid, 1, time.Now().UTC())
		swInsertLedger(t, ctx, "spc_1", uid, swStatusPending, "", nil, 0)
	}

	svc := swNewService(ctx, settings)
	svc.sendFn = func(_ context.Context, _ *config.MsgSendReq) (*swSendResult, error) {
		// Disable the feature right after the first delivery, mimicking an admin
		// flip mid-wake, and reload the shared snapshot.
		_, _ = ctx.DB().InsertBySql(
			"INSERT INTO system_setting (category, key_name, value, value_type, description) VALUES ('onboarding','space_welcome_enabled','0','bool','') " +
				"ON DUPLICATE KEY UPDATE value='0'").Exec()
		_ = settings.Reload()
		return &swSendResult{messageID: 1, clientMsgNo: "x"}, nil
	}
	svc.runWorkerWake(bg())

	sent, _ := svc.store.countByStatus(bg(), "spc_1", swStatusSent)
	pending, _ := svc.store.countByStatus(bg(), "spc_1", swStatusPending)
	assert.EqualValues(t, 1, sent, "disable mid-wake stops further claims immediately")
	assert.EqualValues(t, 2, pending, "rows not yet claimed stay pending")
}

// TestService_Dispatch_EmptyAPIURL_PreIMNotUnknown verifies an unconfigured IM
// endpoint is a retryable pre-IM failure (attempts+1, back to pending), NOT a
// terminal unknown. Guards the P2-4 fix.
func TestService_Dispatch_EmptyAPIURL_PreIMNotUnknown(t *testing.T) {
	ctx := swTestServer(t)
	settings := common.EnsureSystemSettings(ctx)
	activeFrom := time.Now().UTC().Add(-time.Hour)
	swInsertSpace(t, ctx, "spc_1", 1)
	swInsertUser(t, ctx, "u_1", 0)
	swInsertMember(t, ctx, "spc_1", "u_1", 1, time.Now().UTC())
	swSetConfig(t, ctx, settings, swEnabledConfig("spc_1", activeFrom))

	svc := swNewService(ctx, settings)
	sendCalled := false
	svc.sendFn = func(_ context.Context, _ *config.MsgSendReq) (*swSendResult, error) {
		sendCalled = true
		return &swSendResult{messageID: 1}, nil
	}
	// Blank the IM endpoint and restore afterwards.
	cfg := ctx.GetConfig()
	orig := cfg.WuKongIM.APIURL
	cfg.WuKongIM.APIURL = ""
	defer func() { cfg.WuKongIM.APIURL = orig }()

	id := swInsertLedger(t, ctx, "spc_1", "u_1", swStatusClaimed, svc.store.claimOwner, ptrTime(time.Now().UTC().Add(time.Minute)), 0)
	svc.dispatch(bg(), settings.SpaceWelcomeConfig(), &spaceWelcomeRow{ID: id, SpaceID: "spc_1", UID: "u_1"})

	status, attempts, _, _, _ := swRowStatus(t, ctx, id)
	assert.Equal(t, swStatusPending, status, "config error must be pre-IM retryable, not unknown")
	assert.Equal(t, 1, attempts, "pre-IM failure increments attempts")
	assert.False(t, sendCalled, "no HTTP send should be attempted with an empty APIURL")
}
