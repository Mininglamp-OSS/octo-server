package space

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 会话面清理工单的端到端行为。需要 MySQL（TestMain 已保证）。
// 清理步骤一律用测试替身：这里验证的是「工单被正确入队、认领、重试、跳过」，
// 真正的退群 / 摘白名单归 group / user 侧的用例。

type cleanupJobRow struct {
	ID            uint64    `db:"id"`
	SpaceID       string    `db:"space_id"`
	UID           string    `db:"uid"`
	OperatorUID   string    `db:"operator_uid"`
	Reason        string    `db:"reason"`
	Status        uint8     `db:"status"`
	Attempts      uint32    `db:"attempts"`
	NextAttemptAt time.Time `db:"next_attempt_at"`
	LeaseOwner    string    `db:"lease_owner"`
	LastError     string    `db:"last_error"`
}

func cleanupJobs(t *testing.T, spaceID string) []*cleanupJobRow {
	t.Helper()
	var rows []*cleanupJobRow
	_, err := testCtx.DB().SelectBySql(
		"SELECT id, space_id, uid, operator_uid, reason, status, attempts, next_attempt_at, lease_owner, last_error "+
			"FROM space_member_removal_cleanup WHERE space_id=? ORDER BY id", spaceID,
	).Load(&rows)
	require.NoError(t, err)
	return rows
}

// seedMember 建一个 Space 和一名活跃成员。
func seedMember(t *testing.T, f *Space, spaceID, uid string, role int) {
	t.Helper()
	require.NoError(t, f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceID, Name: "cleanup-" + spaceID, Creator: uid, Status: 1, MaxUsers: 100,
	}))
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID, UID: uid, Role: role, Status: 1,
	}))
}

// TestRemoveMemberLockedEnqueuesCleanup 移除成员必须在同一事务内写出清理工单。
// 这是整条链路的入口：没有工单，后面的退群 / 断私聊全都不会发生。
func TestRemoveMemberLockedEnqueuesCleanup(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "rm-enqueue"
	seedMember(t, f, spaceID, "owner-1", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID, UID: "victim-1", Role: 0, Status: 1,
	}))

	mustRemoveMember(t, f, spaceID, "victim-1", 1, "owner-1", MemberRemoveReasonKicked)

	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Equal(t, "victim-1", jobs[0].UID)
	assert.Equal(t, "owner-1", jobs[0].OperatorUID)
	assert.Equal(t, MemberRemoveReasonKicked, jobs[0].Reason)
	assert.Equal(t, removalCleanupPending, jobs[0].Status)
	assert.EqualValues(t, 0, jobs[0].Attempts)
}

// TestRemoveMemberLockedSkipsEnqueueWhenNothingRemoved 三条提前返回分支都没改动成员行，
// 因此都不能入队——否则会产出永远无事可做的工单，甚至对着不该动的人跑一遍清理。
func TestRemoveMemberLockedSkipsEnqueueWhenNothingRemoved(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "rm-noop"
	seedMember(t, f, spaceID, "owner-2", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID, UID: "admin-2", Role: 1, Status: 1,
	}))

	// 成员行不存在 → 幂等 nil，且必须报告「没有移除」
	ghostRemoved, err := f.db.removeMemberLocked(spaceID, "ghost", 2, "owner-2", MemberRemoveReasonKicked)
	require.NoError(t, err)
	assert.False(t, ghostRemoved, "成员行不存在时不能报告成移除，否则调用方会对着非成员空跑一整套收尾")
	// owner 不可移除
	ownerRemoved, err := f.db.removeMemberLocked(spaceID, "owner-2", 2, "admin-2", MemberRemoveReasonKicked)
	assert.ErrorIs(t, err, ErrCannotRemoveOwner)
	assert.False(t, ownerRemoved)
	// 同级不可移除
	peerRemoved, err := f.db.removeMemberLocked(spaceID, "admin-2", 1, "admin-2", MemberRemoveReasonKicked)
	assert.ErrorIs(t, err, ErrRemoveHierarchy)
	assert.False(t, peerRemoved)

	assert.Empty(t, cleanupJobs(t, spaceID), "没有真正移除任何人时不得留下工单")
}

// TestRemoveMembersForceEnqueuesOnlyActuallyRemoved 管理端强制移除：
// 只给真的被改动的成员行入队。传进来的 uid 可能已经退过或压根不在这个 Space。
func TestRemoveMembersForceEnqueuesOnlyActuallyRemoved(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)
	mgr := newManagerDB(testCtx.DB())

	const spaceID = "rm-force"
	seedMember(t, f, spaceID, "owner-3", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "active-3", Role: 0, Status: 1}))
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "gone-3", Role: 0, Status: 0}))

	forced, err := mgr.removeMembersForce(spaceID, []string{"active-3", "gone-3", "stranger-3"}, "su-3")
	require.NoError(t, err)
	assert.Equal(t, []string{"active-3"}, forced, "只返回真正被改动的成员行")

	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1, "已移除的成员和非成员都不该入队")
	assert.Equal(t, "active-3", jobs[0].UID)
	assert.Equal(t, MemberRemoveReasonForceRemoved, jobs[0].Reason)
	assert.Equal(t, "su-3", jobs[0].OperatorUID)
}

// TestForceDisbandSpaceEnqueuesEveryActiveMember 解散空间要给每个活跃成员各留一条工单，
// 并把名单返回给调用方做缓存失效。
func TestForceDisbandSpaceEnqueuesEveryActiveMember(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)
	mgr := newManagerDB(testCtx.DB())

	const spaceID = "rm-disband"
	seedMember(t, f, spaceID, "owner-4", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "m-4a", Role: 0, Status: 1}))
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "m-4b", Role: 0, Status: 0}))

	removed, err := mgr.forceDisbandSpace(spaceID, "su-4")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"owner-4", "m-4a"}, removed, "只返回本次真正移除的活跃成员")

	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 2)
	for _, job := range jobs {
		assert.Equal(t, MemberRemoveReasonSpaceDisbanded, job.Reason)
	}
	// 解散会带走 owner —— 这是唯一一条 owner 也会进清理队列的路径。
	uids := []string{jobs[0].UID, jobs[1].UID}
	assert.ElementsMatch(t, []string{"owner-4", "m-4a"}, uids)
}

// TestEnqueueRejectsUnknownReason 原因是低基数枚举，拼错必须当场失败，
// 而不是写进库里变成一条谁也认不出的工单。
func TestEnqueueRejectsUnknownReason(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	tx, err := f.db.session.Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()
	assert.Error(t, enqueueMemberRemovalCleanupTx(tx, "s", "u", "op", "typo_reason"))
	assert.Error(t, enqueueMemberRemovalCleanupTx(tx, "", "u", "op", MemberRemoveReasonKicked))
	assert.Error(t, enqueueMemberRemovalCleanupTx(tx, "s", "", "op", MemberRemoveReasonKicked))
}

// TestCleanupWorkerRunsStepsAndCompletes 正常路径：认领 → 跑完所有步骤 → 置 done。
func TestCleanupWorkerRunsStepsAndCompletes(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "wk-ok"
	seedMember(t, f, spaceID, "owner-5", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "victim-5", Role: 0, Status: 1}))
	mustRemoveMember(t, f, spaceID, "victim-5", 1, "owner-5", MemberRemoveReasonKicked)

	var seen []MemberRemoval
	restore := swapCleanupStepsForTest([]namedCleanupStep{{
		name: "spy",
		fn: func(_ *config.Context, removal MemberRemoval) error {
			seen = append(seen, removal)
			return nil
		},
	}})
	defer restore()

	f.processMemberRemovalCleanups()

	require.Len(t, seen, 1)
	assert.Equal(t, spaceID, seen[0].SpaceID)
	assert.Equal(t, "victim-5", seen[0].UID)
	assert.Equal(t, "owner-5", seen[0].OperatorUID)
	assert.Equal(t, MemberRemoveReasonKicked, seen[0].Reason)

	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Equal(t, removalCleanupDone, jobs[0].Status)
	assert.Empty(t, jobs[0].LeaseOwner, "终态必须释放租约")
}

// TestCleanupWorkerSkipsRejoinedMember 关键回归：成员被移除后又重新加入，
// 迟到的重试绝不能把他的群和私聊拆掉。
func TestCleanupWorkerSkipsRejoinedMember(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "wk-rejoin"
	seedMember(t, f, spaceID, "owner-6", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "boomerang", Role: 0, Status: 1}))
	mustRemoveMember(t, f, spaceID, "boomerang", 1, "owner-6", MemberRemoveReasonKicked)

	// 工单还没执行，人已经回来了
	require.NoError(t, f.db.reactivateMember(spaceID, "boomerang", 0))

	called := 0
	restore := swapCleanupStepsForTest([]namedCleanupStep{{
		name: "must-not-run",
		fn: func(*config.Context, MemberRemoval) error {
			called++
			return nil
		},
	}})
	defer restore()

	f.processMemberRemovalCleanups()

	assert.Zero(t, called, "已重新加入的成员不得触发任何清理步骤")
	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Equal(t, removalCleanupDone, jobs[0].Status)
	assert.Equal(t, "skipped_rejoined", jobs[0].LastError)
}

// TestCleanupWorkerRetriesFailedStep 步骤失败 → 计数 +1、推迟下次尝试、保持 pending。
func TestCleanupWorkerRetriesFailedStep(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "wk-retry"
	seedMember(t, f, spaceID, "owner-7", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "victim-7", Role: 0, Status: 1}))
	mustRemoveMember(t, f, spaceID, "victim-7", 1, "owner-7", MemberRemoveReasonKicked)

	restore := swapCleanupStepsForTest([]namedCleanupStep{{
		name: "always-fails",
		fn: func(*config.Context, MemberRemoval) error {
			return errors.New("downstream unavailable")
		},
	}})
	defer restore()

	f.processMemberRemovalCleanups()

	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Equal(t, removalCleanupPending, jobs[0].Status, "失败的工单必须留在 pending 等待重试")
	assert.EqualValues(t, 1, jobs[0].Attempts)
	assert.Empty(t, jobs[0].LeaseOwner, "失败后必须释放租约，否则别的副本接不了手")
	assert.Contains(t, jobs[0].LastError, "always-fails")
	assert.True(t, jobs[0].NextAttemptAt.After(time.Now()), "下次尝试时间必须被推到未来")
}

// TestCleanupWorkerAbandonsAfterMaxAttempts 重试耗尽后置为 abandoned，
// 不再无限重试拖垮下游，同时把状态留在库里可查。
func TestCleanupWorkerAbandonsAfterMaxAttempts(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "wk-abandon"
	seedMember(t, f, spaceID, "owner-8", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "victim-8", Role: 0, Status: 1}))
	mustRemoveMember(t, f, spaceID, "victim-8", 1, "owner-8", MemberRemoveReasonKicked)

	// 直接把 attempts 顶到上限前一次，避免真的跑十轮退避
	_, err = testCtx.DB().Exec(
		"UPDATE space_member_removal_cleanup SET attempts=? WHERE space_id=?",
		removalCleanupMaxAttempts-1, spaceID)
	require.NoError(t, err)

	restore := swapCleanupStepsForTest([]namedCleanupStep{{
		name: "always-fails",
		fn: func(*config.Context, MemberRemoval) error {
			return errors.New("still broken")
		},
	}})
	defer restore()

	f.processMemberRemovalCleanups()

	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Equal(t, removalCleanupAbandoned, jobs[0].Status)
	assert.Contains(t, jobs[0].LastError, "retries exhausted")
}

// TestClaimCleanupRespectsLeaseAndSchedule 租约与 next_attempt_at 都要拦住重复认领：
// 前者防两个副本同时跑同一条工单，后者防退避形同虚设。
func TestClaimCleanupRespectsLeaseAndSchedule(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "wk-lease"
	seedMember(t, f, spaceID, "owner-9", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "victim-9", Role: 0, Status: 1}))
	mustRemoveMember(t, f, spaceID, "victim-9", 1, "owner-9", MemberRemoveReasonKicked)

	now := time.Now()
	first, err := f.db.claimMemberRemovalCleanup("worker-a", now)
	require.NoError(t, err)
	require.NotNil(t, first)

	// 租约未到期，另一个 worker 认领不到
	second, err := f.db.claimMemberRemovalCleanup("worker-b", now)
	require.NoError(t, err)
	assert.Nil(t, second, "租约内不得被第二个 worker 认领")

	// 租约到期后可以接手
	takeover, err := f.db.claimMemberRemovalCleanup("worker-b", now.Add(removalCleanupLease+time.Second))
	require.NoError(t, err)
	require.NotNil(t, takeover, "租约到期后必须允许接管")
	assert.Equal(t, first.ID, takeover.ID)

	// 非租约持有者写终态必须落空，避免覆盖接手方的结果
	ok, err := f.db.finishMemberRemovalCleanup(first.ID, "worker-a", removalCleanupDone, "")
	require.NoError(t, err)
	assert.False(t, ok, "租约已易主，旧 owner 不得写终态")
}

// TestCleanupJobSurvivesRemoveRejoinRemove 移除 → 重新加入 → 再移除要产生两条工单，
// 所以表上刻意不做 (space_id, uid) 唯一约束。
func TestCleanupJobSurvivesRemoveRejoinRemove(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "wk-twice"
	seedMember(t, f, spaceID, "owner-10", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "victim-10", Role: 0, Status: 1}))

	mustRemoveMember(t, f, spaceID, "victim-10", 1, "owner-10", MemberRemoveReasonKicked)
	require.NoError(t, f.db.reactivateMember(spaceID, "victim-10", 0))
	mustRemoveMember(t, f, spaceID, "victim-10", 1, "owner-10", MemberRemoveReasonKicked)

	assert.Len(t, cleanupJobs(t, spaceID), 2, "第二次移除必须能再入队")
}

var _ = testutil.CleanAllTables

// TestRemoveMembersInvalidatesMembershipCache 隔离边界回归：
// SpaceMiddleware 的 Redis 正向缓存 TTL 60s，移除时不清它，被移除的人还能带着
// 这个 space_id 正常访问接口最长一分钟。走真实 HTTP handler，断言缓存键真的没了。
func TestRemoveMembersInvalidatesMembershipCache(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "cache-invalidate"
	// testutil.UID 作为 admin 操作者
	seedMember(t, f, spaceID, testutil.UID, 1)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID, UID: "m-cached", Role: 0, Status: 1,
	}))

	// 预热成 SpaceMiddleware 命中过的状态
	conn := testCtx.GetRedisConn()
	cacheKey := "space:member:" + spaceID + ":m-cached"
	require.NoError(t, conn.SetAndExpire(cacheKey, "1", time.Minute))
	cached, err := conn.GetString(cacheKey)
	require.NoError(t, err)
	require.Equal(t, "1", cached, "前提：缓存已预热")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/space/"+spaceID+"/members/remove",
		bytes.NewReader([]byte(util.ToJson(map[string]interface{}{"uids": []string{"m-cached"}}))))
	req.Header.Set("token", testutil.Token)
	testSrv.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	after, err := conn.GetString(cacheKey)
	require.NoError(t, err)
	assert.Empty(t, after, "移除后成员缓存必须立即失效，不能留 60s 窗口")
}

// TestFireSpaceMemberRemoveEventWritesEvent 观察者事件的名字与载荷是下游模块的契约，
// 写错了没人会报错，只会静默没人监听。
func TestFireSpaceMemberRemoveEventWritesEvent(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	// 本包不 import modules/base，拿不到它的迁移，event 表要手工建
	// （与 TestMain 为 group / robot / user 建夹具表同一套路）。列跟随
	// modules/base/sql/20191106000001_event_legacy01.sql + 20250423000001。
	_, err = testCtx.DB().Exec("CREATE TABLE IF NOT EXISTS `event` (" +
		"id INTEGER NOT NULL PRIMARY KEY AUTO_INCREMENT, " +
		"event VARCHAR(40) NOT NULL DEFAULT '', `type` SMALLINT NOT NULL DEFAULT 0, " +
		"data VARCHAR(10000) NOT NULL DEFAULT '', status SMALLINT NOT NULL DEFAULT 0, " +
		"reason VARCHAR(1000) NOT NULL DEFAULT '', version_lock INTEGER NOT NULL DEFAULT 0, " +
		"created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, " +
		"updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)")
	require.NoError(t, err)
	_, err = testCtx.DB().Exec("DELETE FROM `event`")
	require.NoError(t, err)

	// testutil 默认不装 ctx.Event（fire 会直接 return），这里显式装上。
	previous := testCtx.Event
	testCtx.Event = event.New(testCtx)
	defer func() { testCtx.Event = previous }()

	f.fireSpaceMemberRemoveEvent("evt-space", "evt-uid", "evt-op", MemberRemoveReasonForceRemoved)

	var rows []struct {
		Event string `db:"event"`
		Data  string `db:"data"`
	}
	_, err = testCtx.DB().SelectBySql("SELECT event, data FROM event ORDER BY id DESC LIMIT 1").Load(&rows)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, event.SpaceMemberRemove, rows[0].Event)
	assert.Contains(t, rows[0].Data, "evt-space")
	assert.Contains(t, rows[0].Data, "evt-uid")
	assert.Contains(t, rows[0].Data, "evt-op")
	assert.Contains(t, rows[0].Data, MemberRemoveReasonForceRemoved)
}

// mustRemoveMember 移除成员并断言这次调用确实改动了成员行。
// removeMemberLocked 对「行本来就不存在」也返回 nil error，只查 error 会让
// 「什么都没做」的用例悄悄变绿。
func mustRemoveMember(t *testing.T, f *Space, spaceID, uid string, reject int, operator, reason string) {
	t.Helper()
	removed, err := f.db.removeMemberLocked(spaceID, uid, reject, operator, reason)
	require.NoError(t, err)
	require.True(t, removed, "这一步必须真的移除了成员行")
}

// TestRemoveMembersOnlyWrapsUpActuallyRemoved 批量移除里被静默跳过的成员
// （owner / 同级管理员）不能被当成"已移除"收尾：给他们清鉴权缓存会把在职成员
// 的缓存打掉（下次访问要多查一次库），发事件则会让下游以为人已经走了。
func TestRemoveMembersOnlyWrapsUpActuallyRemoved(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "batch-precision"
	seedMember(t, f, spaceID, testutil.UID, 1) // 操作者：admin
	for uid, role := range map[string]int{"b-owner": 2, "b-peer": 1, "b-low": 0} {
		require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
			SpaceId: spaceID, UID: uid, Role: role, Status: 1,
		}))
	}

	conn := testCtx.GetRedisConn()
	keyOf := func(uid string) string { return "space:member:" + spaceID + ":" + uid }
	for _, uid := range []string{"b-owner", "b-peer", "b-low"} {
		require.NoError(t, conn.SetAndExpire(keyOf(uid), "1", time.Minute))
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/space/"+spaceID+"/members/remove",
		bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
			"uids": []string{"b-owner", "b-peer", "b-low"},
		}))))
	req.Header.Set("token", testutil.Token)
	testSrv.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	low, err := conn.GetString(keyOf("b-low"))
	require.NoError(t, err)
	assert.Empty(t, low, "真正被移除的成员，缓存必须失效")

	for _, uid := range []string{"b-owner", "b-peer"} {
		v, err := conn.GetString(keyOf(uid))
		require.NoError(t, err)
		assert.Equal(t, "1", v, "%s 被静默跳过、仍是成员，缓存不该被动", uid)
	}

	// 也不该给他们留下清理工单
	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Equal(t, "b-low", jobs[0].UID)
}
