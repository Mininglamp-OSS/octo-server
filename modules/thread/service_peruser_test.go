package thread

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	convext "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedThreadReminder inserts an unhandled per-uid @ (P1) reminder for a thread channel.
func seedThreadReminder(t *testing.T, ctx *config.Context, uid, groupNo, shortID string) {
	t.Helper()
	channelID := groupNo + "____" + shortID
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO reminders (channel_id, channel_type, reminder_type, uid, is_deleted, version) VALUES (?,?,?,?,0,1)",
		channelID, uint8(common.ChannelTypeCommunityTopic), 1, uid,
	).Exec()
	require.NoError(t, err)
}

// seedActiveThread inserts an active thread row directly (avoids CreateThread's live-IM dep,
// which is why the DB-heavy service tests are otherwise skipped). Uses a snowflake-shaped shortID.
func seedActiveThread(t *testing.T, svc *Service, groupNo, shortID string) {
	t.Helper()
	require.NoError(t, svc.db.Insert(&Model{
		ShortID:    shortID,
		GroupNo:    groupNo,
		Name:       "thr-" + shortID,
		CreatorUID: testutil.UID,
		Status:     ThreadStatusActive,
		Version:    1,
	}))
}

const testShortID = "170000000000001" // 15-digit snowflake-shaped shortID (creator can operate)

// TestArchiveThread_PerUser_FlagOn 覆盖 plan T7 + T8 + gate 1/5：
// flag=on 时 ArchiveThread 写 per-uid intent（不改全局 status），A 视角归档、他人不变，且 bump follow_version；
// detail 同源。
func TestArchiveThread_PerUser_FlagOn(t *testing.T) {
	t.Setenv("DM_THREAD_PERUSER_VISIBILITY", "true")
	svc, groupNo := setupServiceTestData(t)
	shortID := testShortID
	seedActiveThread(t, svc, groupNo, shortID)

	fvDB := convext.NewFollowVersionDB(svc.ctx)
	before, err := fvDB.Get(testutil.UID, "")
	require.NoError(t, err)

	// A（creator）归档：写 per-uid intent，全局 status 不变。
	require.NoError(t, svc.ArchiveThread(groupNo, shortID, testutil.UID))

	globalRow, err := svc.db.QueryByGroupNoAndShortID(groupNo, shortID)
	require.NoError(t, err)
	assert.Equal(t, ThreadStatusActive, globalRow.Status, "flag=on 不改全局 status")

	states, err := svc.db.QueryUserStates(testutil.UID, []ShortRef{{GroupNo: groupNo, ShortID: shortID}})
	require.NoError(t, err)
	require.Contains(t, states, groupNo+"____"+shortID)
	assert.Equal(t, 1, states[groupNo+"____"+shortID].ArchiveIntent)

	otherStates, err := svc.db.QueryUserStates("user2", []ShortRef{{GroupNo: groupNo, ShortID: shortID}})
	require.NoError(t, err)
	assert.Empty(t, otherStates, "他人不受影响")

	after, err := fvDB.Get(testutil.UID, "")
	require.NoError(t, err)
	assert.Equal(t, before+1, after, "archive per-uid bumps follow_version")

	// detail 同源（T8）：A 视角 detail.status == 2；user2 仍 active。
	resp, err := svc.GetThread(groupNo, shortID, testutil.UID)
	require.NoError(t, err)
	assert.Equal(t, ThreadStatusArchived, resp.Status, "detail per-uid archived for A")

	respOther, err := svc.GetThread(groupNo, shortID, "user2")
	require.NoError(t, err)
	assert.Equal(t, ThreadStatusActive, respOther.Status, "detail active for others")

	// Unarchive 恢复：intent=0。
	require.NoError(t, svc.UnarchiveThread(groupNo, shortID, testutil.UID))
	states, err = svc.db.QueryUserStates(testutil.UID, []ShortRef{{GroupNo: groupNo, ShortID: shortID}})
	require.NoError(t, err)
	assert.Equal(t, 0, states[groupNo+"____"+shortID].ArchiveIntent, "unarchive sets intent 0")
	resp, err = svc.GetThread(groupNo, shortID, testutil.UID)
	require.NoError(t, err)
	assert.Equal(t, ThreadStatusActive, resp.Status)
}

// TestArchiveThread_Global_FlagOff 覆盖 gate 5：flag=off 时走原全局 CAS（现状），不写 per-uid 表。
func TestArchiveThread_Global_FlagOff(t *testing.T) {
	t.Setenv("DM_THREAD_PERUSER_VISIBILITY", "false")
	svc, groupNo := setupServiceTestData(t)
	shortID := testShortID
	seedActiveThread(t, svc, groupNo, shortID)

	require.NoError(t, svc.ArchiveThread(groupNo, shortID, testutil.UID))

	globalRow, err := svc.db.QueryByGroupNoAndShortID(groupNo, shortID)
	require.NoError(t, err)
	assert.Equal(t, ThreadStatusArchived, globalRow.Status, "flag=off 改全局 status（现状）")

	states, err := svc.db.QueryUserStates(testutil.UID, []ShortRef{{GroupNo: groupNo, ShortID: shortID}})
	require.NoError(t, err)
	assert.Empty(t, states, "flag=off 不写 thread_user_state")

	resp, err := svc.GetThread(groupNo, shortID, testutil.UID)
	require.NoError(t, err)
	assert.Equal(t, ThreadStatusArchived, resp.Status)
}

// TestGetThread_P1OverIntent 覆盖 T8 + gate 1：detail 端 P1 压过 intent 重新可见。
func TestGetThread_P1OverIntent(t *testing.T) {
	t.Setenv("DM_THREAD_PERUSER_VISIBILITY", "true")
	svc, groupNo := setupServiceTestData(t)
	shortID := testShortID
	seedActiveThread(t, svc, groupNo, shortID)

	require.NoError(t, svc.ArchiveThread(groupNo, shortID, testutil.UID))
	resp, err := svc.GetThread(groupNo, shortID, testutil.UID)
	require.NoError(t, err)
	require.Equal(t, ThreadStatusArchived, resp.Status)

	// 给 A 一个未处理 per-uid @（P1）→ detail 拉回 active。
	seedThreadReminder(t, svc.ctx, testutil.UID, groupNo, shortID)
	resp, err = svc.GetThread(groupNo, shortID, testutil.UID)
	require.NoError(t, err)
	assert.Equal(t, ThreadStatusActive, resp.Status, "P1 pulls archived thread back to active in detail")
}

// TestDeleteThread_GC 覆盖 T-GC：DeleteThread 后 user_state 行被清（无孤儿）。
func TestDeleteThread_GC(t *testing.T) {
	t.Setenv("DM_THREAD_PERUSER_VISIBILITY", "true")
	svc, groupNo := setupServiceTestData(t)
	shortID := testShortID
	seedActiveThread(t, svc, groupNo, shortID)

	require.NoError(t, svc.ArchiveThread(groupNo, shortID, testutil.UID))
	states, err := svc.db.QueryUserStates(testutil.UID, []ShortRef{{GroupNo: groupNo, ShortID: shortID}})
	require.NoError(t, err)
	require.NotEmpty(t, states)

	require.NoError(t, svc.DeleteThread(groupNo, shortID, testutil.UID))
	states, err = svc.db.QueryUserStates(testutil.UID, []ShortRef{{GroupNo: groupNo, ShortID: shortID}})
	require.NoError(t, err)
	assert.Empty(t, states, "DeleteThread cleans thread_user_state (no orphan)")
}
