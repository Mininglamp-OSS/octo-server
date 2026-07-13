//go:build integration

package message

// =============================================================================
// Per-user thread visibility follow_version bump — DB-backed tests (plan T5/T6).
//
// gate 2 (partial): reminder_done (T5) 后 (uid, space) follow_version +1；
// reminder 触发侧 (T6) 新 per-uid @ 后 (被@uid, space) follow_version +1，@所有人(uid='')不 bump。
// =============================================================================

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	convext "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReminderDoneBump_T5 verifies reminder_done 侧 bump（T5）：
// done 掉一个子区 per-uid @ 后，(loginUID, space) follow_version +1（空 space 也算一次）。
func TestReminderDoneBump_T5(t *testing.T) {
	setupPerUserEnv(t)
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	m := New(ctx)
	fvDB := convext.NewFollowVersionDB(ctx)

	const g = "gt5"
	uid := "alice"
	// seed 一个子区 per-uid @ reminder（channel_type=5）。
	rid := seedReminder(t, ctx, uid, g, "t1")

	// group 不存在 → space 解析为空串；bump 落在 (uid, "").
	before, err := fvDB.Get(uid, "")
	require.NoError(t, err)

	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	chs, err := m.remindersDB.queryThreadChannelsByIDsTx(tx, []int64{rid}, uid)
	require.NoError(t, err)
	require.Contains(t, chs, g+"____t1")
	require.NoError(t, m.bumpFollowVersionForThreadChannelsTx(tx, uid, chs))
	require.NoError(t, tx.Commit())

	after, err := fvDB.Get(uid, "")
	require.NoError(t, err)
	assert.Equal(t, before+1, after, "reminder_done bumps (uid, space) follow_version by 1")
}

// TestReminderDoneBump_DedupPerSpace verifies 多个同 space 子区 done 只 bump 一次。
func TestReminderDoneBump_DedupPerSpace(t *testing.T) {
	setupPerUserEnv(t)
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	m := New(ctx)
	fvDB := convext.NewFollowVersionDB(ctx)

	const g = "gt5b"
	uid := "bob"
	r1 := seedReminder(t, ctx, uid, g, "t1")
	r2 := seedReminder(t, ctx, uid, g, "t2") // 同群 → 同 space

	before, err := fvDB.Get(uid, "")
	require.NoError(t, err)

	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	chs, err := m.remindersDB.queryThreadChannelsByIDsTx(tx, []int64{r1, r2}, uid)
	require.NoError(t, err)
	require.Len(t, chs, 2)
	require.NoError(t, m.bumpFollowVersionForThreadChannelsTx(tx, uid, chs))
	require.NoError(t, tx.Commit())

	after, err := fvDB.Get(uid, "")
	require.NoError(t, err)
	assert.Equal(t, before+1, after, "two threads in same space bump follow_version only once")
}

// TestReminderDoneBump_BroadcastExcluded verifies @所有人(uid='')的 reminder
// 不会被 queryThreadChannelsByIDsTx 当成本人 per-uid @（不 bump）。
func TestReminderDoneBump_BroadcastExcluded(t *testing.T) {
	setupPerUserEnv(t)
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	m := New(ctx)

	const g = "gt5c"
	// 广播 reminder（uid=''）。
	rid := seedReminder(t, ctx, "", g, "t1")

	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	// 以 alice 身份反查：广播行 uid='' ≠ alice，不返回。
	chs, err := m.remindersDB.queryThreadChannelsByIDsTx(tx, []int64{rid}, "alice")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	assert.Empty(t, chs, "broadcast (uid='') must not be treated as alice's per-uid @")
}

// TestReminderTriggerBump_T6 verifies reminder 触发侧 bump（T6）：
// handleRemindersVisibilityBump 对 per-uid @（uid≠''）落 reminder 后 bump 被@uid follow_version；
// @所有人(uid='')不 bump。用 fire-and-forget 的同步内核 bumpFollowVersionForReminders 验证。
func TestReminderTriggerBump_T6(t *testing.T) {
	setupPerUserEnv(t)
	t.Setenv("DM_THREAD_PERUSER_VISIBILITY", "true") // T6 内核自判 flag，需开启
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	m := New(ctx)
	fvDB := convext.NewFollowVersionDB(ctx)

	const g = "gt6"
	reminders := []*remindersModel{
		{ChannelID: g + "____t1", ChannelType: uint8(common.ChannelTypeCommunityTopic), UID: "carol", ReminderType: 1},
		{ChannelID: g + "____t1", ChannelType: uint8(common.ChannelTypeCommunityTopic), UID: "", ReminderType: 1}, // 广播
	}

	beforeCarol, err := fvDB.Get("carol", "")
	require.NoError(t, err)

	// 直接调同步内核（生产是 fire-and-forget 包一层 goroutine）。
	m.bumpFollowVersionForReminders(reminders)

	afterCarol, err := fvDB.Get("carol", "")
	require.NoError(t, err)
	assert.Equal(t, beforeCarol+1, afterCarol, "per-uid @ bumps the mentioned user's follow_version")

	// 广播 uid='' 不产生任何 (,'') 之外的 bump —— 断言空 uid 不写。
	emptyUIDVer, err := fvDB.Get("", "")
	require.NoError(t, err)
	assert.Equal(t, int64(0), emptyUIDVer, "broadcast (uid='') must not bump")
}
