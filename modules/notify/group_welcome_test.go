package notify

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	libcommon "github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	// Ensure the group migrations (`group` / `group_member`) run in the test DB;
	// group does not import notify, so this blank import is cycle-free.
	_ "github.com/Mininglamp-OSS/octo-server/modules/group"
)

// ---------------------------------------------------------------------------
// Group welcome test harness (task group-welcome-message)
// ---------------------------------------------------------------------------

// gwNewService builds a group welcome service with the platform master switch
// turned ON (the feature ships dark by default, so every delivery test must enable
// it), reading the same SystemSettings singleton the production wiring uses.
func gwNewService(t *testing.T, ctx *config.Context) *groupWelcomeService {
	t.Helper()
	gwSetGlobalWelcomeEnabled(t, ctx, true)
	return newGroupWelcomeService(ctx, common.EnsureSystemSettings(ctx), NotifyBotUID(), func() bool { return true })
}

// gwSetGlobalWelcomeEnabled writes onboarding.group_welcome_enabled and reloads the
// SystemSettings snapshot the service reads. Each test sets it explicitly so the
// process-wide singleton is deterministic regardless of test order.
func gwSetGlobalWelcomeEnabled(t *testing.T, ctx *config.Context, enabled bool) {
	t.Helper()
	v := "0"
	if enabled {
		v = "1"
	}
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO system_setting (category, key_name, value, value_type, description) "+
			"VALUES ('onboarding','group_welcome_enabled',?,'bool','') "+
			"ON DUPLICATE KEY UPDATE value=VALUES(value), value_type=VALUES(value_type)", v,
	).Exec()
	require.NoError(t, err)
	require.NoError(t, common.EnsureSystemSettings(ctx).Reload())
}

func gwInsertGroup(t *testing.T, ctx *config.Context, groupNo string, status int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, status, creator) VALUES (?, ?, ?, ?)",
		groupNo, groupNo, status, "creator",
	).Exec()
	require.NoError(t, err)
}

func gwInsertUserNamed(t *testing.T, ctx *config.Context, uid, name string, robot int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, short_no, robot) VALUES (?, ?, ?, ?)", uid, name, uid, robot,
	).Exec()
	require.NoError(t, err)
}

func gwInsertGroupMember(t *testing.T, ctx *config.Context, groupNo, uid string, role, status int, createdAt time.Time) {
	t.Helper()
	// created_at via FROM_UNIXTIME so UNIX_TIMESTAMP round-trips regardless of the
	// DB session TZ (mirrors swInsertMember).
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group_member` (group_no, uid, role, status, is_deleted, robot, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, 0, 0, FROM_UNIXTIME(?), FROM_UNIXTIME(?))",
		groupNo, uid, role, status, createdAt.Unix(), createdAt.Unix(),
	).Exec()
	require.NoError(t, err)
}

func gwSetGroupConfig(t *testing.T, ctx *config.Context, groupNo string, enabled bool, activeFrom time.Time, msg string) {
	t.Helper()
	store := common.NewGroupWelcomeConfigStore(ctx.DB())
	require.NoError(t, store.Upsert(bg(), common.GroupWelcomeConfig{
		GroupNo:       groupNo,
		Enabled:       enabled,
		ActiveFromRaw: activeFrom.UTC().Format(time.RFC3339),
		Message:       msg,
		UpdatedBy:     "admin",
	}, time.Now().UTC()))
}

func gwInsertGroupLedger(t *testing.T, ctx *config.Context, groupNo, uid string, status int, owner string, claimExpire *time.Time, attempts int) int64 {
	t.Helper()
	now := time.Now().UTC()
	res, err := ctx.DB().InsertBySql(
		"INSERT INTO "+groupWelcomeTable+" (group_no, uid, status, attempts, claim_owner, claim_expire_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		groupNo, uid, status, attempts, owner, claimExpire, now, now,
	).Exec()
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

func gwRowStatus(t *testing.T, ctx *config.Context, id int64) (status int, messageID int64) {
	t.Helper()
	var row struct {
		Status    int   `db:"status"`
		MessageID int64 `db:"message_id"`
	}
	err := ctx.DB().SelectBySql(
		"SELECT status, COALESCE(message_id,0) AS message_id FROM "+groupWelcomeTable+" WHERE id=?", id,
	).LoadOne(&row)
	require.NoError(t, err)
	return row.Status, row.MessageID
}

func gwCountRows(t *testing.T, ctx *config.Context, groupNo string) int {
	t.Helper()
	var n int
	err := ctx.DB().SelectBySql("SELECT COUNT(*) FROM "+groupWelcomeTable+" WHERE group_no=?", groupNo).LoadOne(&n)
	require.NoError(t, err)
	return n
}

// gwSeedPending seeds n deliverable pending rows (user + active member + ledger)
// in groupNo, uids disambiguated by offset.
func gwSeedPending(t *testing.T, ctx *config.Context, groupNo string, n, offset int) {
	t.Helper()
	for i := 0; i < n; i++ {
		uid := fmt.Sprintf("%s_u%04d", groupNo, offset+i)
		gwInsertUserNamed(t, ctx, uid, uid, 0)
		gwInsertGroupMember(t, ctx, groupNo, uid, 0, 1, time.Now().UTC())
		gwInsertGroupLedger(t, ctx, groupNo, uid, swStatusPending, "", nil, 0)
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestGroupWelcome_Dispatch_RendersMemberAndPostsToGroupChannel: dispatch posts a
// PUBLIC group-channel message (channel_type=GROUP, channel_id=group_no) from the
// notification identity, with the {member} placeholder rendered to the joiner's
// display name.
func TestGroupWelcome_Dispatch_RendersMemberAndPostsToGroupChannel(t *testing.T) {
	ctx := swTestServer(t)
	af := time.Now().UTC().Add(-time.Hour)
	gwInsertGroup(t, ctx, "g_1", 1)
	gwInsertUserNamed(t, ctx, "u_1", "Alice", 0)
	gwInsertGroupMember(t, ctx, "g_1", "u_1", 0, 1, time.Now().UTC())
	gwSetGroupConfig(t, ctx, "g_1", true, af, "欢迎 {member} 入群!")

	svc := gwNewService(t, ctx)
	var gotReq *config.MsgSendReq
	svc.sendFn = func(_ context.Context, req *config.MsgSendReq) (*swSendResult, error) {
		gotReq = req
		return &swSendResult{messageID: 777, clientMsgNo: "c1"}, nil
	}

	cfg, err := svc.effectiveConfig(bg(), "g_1")
	require.NoError(t, err)
	id := gwInsertGroupLedger(t, ctx, "g_1", "u_1", swStatusClaimed, svc.store.claimOwner, ptrTime(time.Now().UTC().Add(time.Minute)), 0)
	svc.dispatch(bg(), cfg, &groupWelcomeRow{ID: id, GroupNo: "g_1", UID: "u_1"})

	st, messageID := gwRowStatus(t, ctx, id)
	assert.Equal(t, swStatusSent, st)
	assert.EqualValues(t, 777, messageID)
	assert.EqualValues(t, 1, svc.metrics.sendSuccess.Load())

	require.NotNil(t, gotReq)
	assert.Equal(t, "notification", gotReq.FromUID, "server-chosen sender")
	assert.Equal(t, "g_1", gotReq.ChannelID, "posts into the group channel")
	assert.EqualValues(t, libcommon.ChannelTypeGroup.Uint8(), gotReq.ChannelType, "channel_type=GROUP (public)")
	assert.EqualValues(t, 1, gotReq.Header.RedDot)
	assert.Empty(t, gotReq.Subscribers, "public — not scoped to a subscriber")
	assert.Contains(t, string(gotReq.Payload), "欢迎 Alice 入群!", "{member} rendered to the joiner's display name")
	assert.NotContains(t, string(gotReq.Payload), "{member}", "no unrendered placeholder leaks")
}

// TestGroupWelcome_MultiGroupDelivery_NoCrossing: two groups each with an enabled
// config welcome their own first-join member; each posts to ITS OWN channel with
// ITS OWN body + its own member's name — no cross-group leakage.
func TestGroupWelcome_MultiGroupDelivery_NoCrossing(t *testing.T) {
	ctx := swTestServer(t)
	af := time.Now().UTC().Add(-time.Hour)
	gwInsertGroup(t, ctx, "g_a", 1)
	gwInsertGroup(t, ctx, "g_b", 1)
	gwInsertUserNamed(t, ctx, "ua", "Aaa", 0)
	gwInsertUserNamed(t, ctx, "ub", "Bbb", 0)
	gwInsertGroupMember(t, ctx, "g_a", "ua", 0, 1, time.Now().UTC())
	gwInsertGroupMember(t, ctx, "g_b", "ub", 0, 1, time.Now().UTC())
	gwSetGroupConfig(t, ctx, "g_a", true, af, "welcome {member} to A")
	gwSetGroupConfig(t, ctx, "g_b", true, af, "welcome {member} to B")

	svc := gwNewService(t, ctx)
	// Drive an explicit clock: enqueue holds each row until now+coalesceWindow, so
	// advance time between reconcile (enqueue) and the worker so the rows are due.
	clock := time.Now().UTC()
	svc.now = func() time.Time { return clock }
	got := map[string]string{} // channel_id -> payload
	svc.sendFn = func(_ context.Context, req *config.MsgSendReq) (*swSendResult, error) {
		got[req.ChannelID] = string(req.Payload)
		return &swSendResult{messageID: 1, clientMsgNo: "x"}, nil
	}

	svc.runReconcileOnce(bg())     // enqueue rows due at clock + coalesceWindow
	clock = clock.Add(time.Minute) // time passes; the coalesce window has elapsed
	svc.runWorkerWake(bg())

	sentA, _ := svc.store.countByStatus(bg(), "g_a", swStatusSent)
	sentB, _ := svc.store.countByStatus(bg(), "g_b", swStatusSent)
	assert.EqualValues(t, 1, sentA, "g_a member welcomed exactly once")
	assert.EqualValues(t, 1, sentB, "g_b member welcomed exactly once")
	assert.Contains(t, got["g_a"], "welcome Aaa to A")
	assert.Contains(t, got["g_b"], "welcome Bbb to B")
	assert.NotContains(t, got["g_a"], "to B", "no cross-group body leakage")
	assert.NotContains(t, got["g_a"], "Bbb", "no cross-group member leakage")
}

// TestGroupWelcome_FirstJoin_AtMostOnce: the event path enqueues exactly one row
// for a first-join human member, and a repeated join (dedup / rejoin) does not add
// another — the ledger UNIQUE(group_no,uid) is the at-most-once authority even
// though group_member.created_at resets on rejoin.
func TestGroupWelcome_FirstJoin_AtMostOnce(t *testing.T) {
	ctx := swTestServer(t)
	af := time.Now().UTC().Add(-time.Hour)
	gwInsertGroup(t, ctx, "g_1", 1)
	gwInsertUserNamed(t, ctx, "u_1", "Alice", 0)
	gwInsertGroupMember(t, ctx, "g_1", "u_1", 0, 1, time.Now().UTC())
	gwSetGroupConfig(t, ctx, "g_1", true, af, "hi {member}")

	svc := gwNewService(t, ctx)
	svc.handleMemberJoin("g_1", "u_1")
	assert.Equal(t, 1, gwCountRows(t, ctx, "g_1"), "first join enqueues one row")
	svc.handleMemberJoin("g_1", "u_1")
	assert.Equal(t, 1, gwCountRows(t, ctx, "g_1"), "repeated join / rejoin does not re-enqueue (ledger dedup)")
}

// TestGroupWelcome_HandleMemberJoin_Filters: a robot joiner, a member of a group
// with no enabled config, and a member joined before active_from each enqueue
// nothing.
func TestGroupWelcome_HandleMemberJoin_Filters(t *testing.T) {
	ctx := swTestServer(t)
	af := time.Now().UTC()
	gwInsertGroup(t, ctx, "g_on", 1)
	gwInsertGroup(t, ctx, "g_off", 1)
	gwSetGroupConfig(t, ctx, "g_on", true, af, "hi {member}")
	// g_off has no config.

	// Robot joiner of the enabled group -> filtered.
	gwInsertUserNamed(t, ctx, "bot", "bot", 1)
	gwInsertGroupMember(t, ctx, "g_on", "bot", 0, 1, time.Now().UTC())
	// Human who joined BEFORE active_from -> filtered.
	gwInsertUserNamed(t, ctx, "early", "Early", 0)
	gwInsertGroupMember(t, ctx, "g_on", "early", 0, 1, af.Add(-time.Hour))
	// Human in the disabled/absent-config group -> filtered.
	gwInsertUserNamed(t, ctx, "uoff", "Off", 0)
	gwInsertGroupMember(t, ctx, "g_off", "uoff", 0, 1, time.Now().UTC())

	svc := gwNewService(t, ctx)
	svc.handleMemberJoin("g_on", "bot")
	svc.handleMemberJoin("g_on", "early")
	svc.handleMemberJoin("g_off", "uoff")

	assert.Equal(t, 0, gwCountRows(t, ctx, "g_on"), "robot + pre-active_from enqueue nothing")
	assert.Equal(t, 0, gwCountRows(t, ctx, "g_off"), "group without an enabled config enqueues nothing")
}

// TestGroupWelcome_Worker_CoalesceAndFairness: a single wake delivers ONE coalesced
// post per group. g_a's burst exceeds groupWelcomeCoalesceMax so it is capped to
// one batch (tail spills to a later wake), and — crucially — g_b is served in the
// SAME wake rather than starved behind g_a's larger backlog.
func TestGroupWelcome_Worker_CoalesceAndFairness(t *testing.T) {
	ctx := swTestServer(t)
	af := time.Now().UTC().Add(-time.Hour)
	gwInsertGroup(t, ctx, "g_a", 1)
	gwInsertGroup(t, ctx, "g_b", 1)
	gwSetGroupConfig(t, ctx, "g_a", true, af, "A {member}")
	gwSetGroupConfig(t, ctx, "g_b", true, af, "B {member}")

	svc := gwNewService(t, ctx)
	posts := map[string]int{} // channel_id -> number of posts
	svc.sendFn = func(_ context.Context, req *config.MsgSendReq) (*swSendResult, error) {
		posts[req.ChannelID]++
		return &swSendResult{messageID: 1, clientMsgNo: "x"}, nil
	}

	gwSeedPending(t, ctx, "g_a", groupWelcomeCoalesceMax+5, 0) // exceeds one coalesced batch
	gwSeedPending(t, ctx, "g_b", 3, 0)

	svc.runWorkerWake(bg())

	sentA, _ := svc.store.countByStatus(bg(), "g_a", swStatusSent)
	sentB, _ := svc.store.countByStatus(bg(), "g_b", swStatusSent)
	assert.EqualValues(t, groupWelcomeCoalesceMax, sentA, "g_a capped to one coalesced batch per wake")
	assert.EqualValues(t, 3, sentB, "g_b served in the same wake, not starved behind g_a's backlog")
	assert.Equal(t, 1, posts["g_a"], "g_a's burst is ONE public post, not many")
	assert.Equal(t, 1, posts["g_b"], "g_b's members are ONE public post")
}

// TestGroupWelcome_Coalesce_BurstBecomesOnePost: several members due at once for a
// group are delivered as a SINGLE post whose {member} renders to the joined name
// list — the anti-spam guarantee for a batch invite.
func TestGroupWelcome_Coalesce_BurstBecomesOnePost(t *testing.T) {
	ctx := swTestServer(t)
	af := time.Now().UTC().Add(-time.Hour)
	gwInsertGroup(t, ctx, "g_1", 1)
	gwSetGroupConfig(t, ctx, "g_1", true, af, "欢迎 {member} 入群")
	for _, name := range []string{"Alice", "Bob", "Carol"} {
		gwInsertUserNamed(t, ctx, name, name, 0)
		gwInsertGroupMember(t, ctx, "g_1", name, 0, 1, time.Now().UTC())
		gwInsertGroupLedger(t, ctx, "g_1", name, swStatusPending, "", nil, 0)
	}

	svc := gwNewService(t, ctx)
	var payloads []string
	svc.sendFn = func(_ context.Context, req *config.MsgSendReq) (*swSendResult, error) {
		payloads = append(payloads, string(req.Payload))
		return &swSendResult{messageID: 1, clientMsgNo: "x"}, nil
	}

	svc.runWorkerWake(bg())

	require.Len(t, payloads, 1, "a burst of joins is ONE coalesced post, not one per joiner")
	assert.Contains(t, payloads[0], "欢迎 Alice、Bob、Carol 入群", "{member} renders to the joined name list")
	sent, _ := svc.store.countByStatus(bg(), "g_1", swStatusSent)
	assert.EqualValues(t, 3, sent, "every joiner in the batch is marked sent (at-most-once each)")
}

// TestRenderGroupWelcomeNameList pins the {member} name-list rendering: small
// batches list every name; a large batch collapses the tail to "… 等 N 人".
func TestRenderGroupWelcomeNameList(t *testing.T) {
	assert.Equal(t, "", renderGroupWelcomeNameList(nil))
	assert.Equal(t, "Alice", renderGroupWelcomeNameList([]string{"Alice"}))
	assert.Equal(t, "Alice、Bob", renderGroupWelcomeNameList([]string{"Alice", "Bob"}))

	names := make([]string, groupWelcomeCoalesceNames+2)
	for i := range names {
		names[i] = fmt.Sprintf("u%d", i)
	}
	got := renderGroupWelcomeNameList(names)
	want := strings.Join(names[:groupWelcomeCoalesceNames], "、") + fmt.Sprintf(" 等 %d 人", len(names))
	assert.Equal(t, want, got, "overflow lists the first K names then the total count")
}

// TestGroupWelcome_GlobalSwitch_Off: with the platform master switch off the event
// path enqueues nothing and the worker posts nothing, even for an otherwise
// deliverable, enabled per-group config.
func TestGroupWelcome_GlobalSwitch_Off(t *testing.T) {
	ctx := swTestServer(t)
	af := time.Now().UTC().Add(-time.Hour)
	gwInsertGroup(t, ctx, "g_1", 1)
	gwInsertUserNamed(t, ctx, "u_1", "Alice", 0)
	gwInsertGroupMember(t, ctx, "g_1", "u_1", 0, 1, time.Now().UTC())
	gwSetGroupConfig(t, ctx, "g_1", true, af, "hi {member}")

	svc := gwNewService(t, ctx)              // switch ON
	gwSetGlobalWelcomeEnabled(t, ctx, false) // now flip the master switch OFF
	sent := false
	svc.sendFn = func(_ context.Context, _ *config.MsgSendReq) (*swSendResult, error) {
		sent = true
		return &swSendResult{messageID: 1, clientMsgNo: "x"}, nil
	}

	// Event path: no enqueue while dark.
	svc.handleMemberJoin("g_1", "u_1")
	assert.Equal(t, 0, gwCountRows(t, ctx, "g_1"), "master off -> event path enqueues nothing")

	// Worker: a pre-seeded pending row is not posted while dark.
	id := gwInsertGroupLedger(t, ctx, "g_1", "u_1", swStatusPending, "", nil, 0)
	svc.runWorkerWake(bg())
	assert.False(t, sent, "master off -> worker posts nothing")
	st, _ := gwRowStatus(t, ctx, id)
	assert.Equal(t, swStatusPending, st, "the pending row is left untouched (delivered when re-enabled)")
}

// TestGroupWelcome_SweepAll_CrossGroup: lease-expired claimed rows in different
// groups are all reclaimed by one cross-group sweep.
func TestGroupWelcome_SweepAll_CrossGroup(t *testing.T) {
	ctx := swTestServer(t)
	gwInsertGroup(t, ctx, "g_a", 1)
	gwInsertGroup(t, ctx, "g_b", 1)
	svc := gwNewService(t, ctx)

	expired := time.Now().UTC().Add(-time.Minute)
	idA := gwInsertGroupLedger(t, ctx, "g_a", "ua", swStatusClaimed, "dead-owner", &expired, 0)
	idB := gwInsertGroupLedger(t, ctx, "g_b", "ub", swStatusClaimed, "dead-owner", &expired, 0)

	svc.sweepAll(bg())

	stA, _ := gwRowStatus(t, ctx, idA)
	stB, _ := gwRowStatus(t, ctx, idB)
	assert.Equal(t, swStatusPending, stA, "lease-expired claimed row in g_a reclaimed")
	assert.Equal(t, swStatusPending, stB, "lease-expired claimed row in g_b reclaimed")
}
