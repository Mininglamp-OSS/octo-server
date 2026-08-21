package space

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	assert.EqualValues(t, 1, jobs[0].Attempts, "认领时已自增，失败释放不再重复计数")
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
	_, _, err := setup(t)
	require.NoError(t, err)

	// 事件装在**专属 ctx** 上，绝不写 testCtx.Event。
	//
	// config.Context.Event 是个没有任何同步的普通字段，而这个包里随时可能有上一条
	// 用例留下的后台 goroutine 正在读它：afterMembersRemoved 是 `go` 出去的，
	// 里面第一件事就是 s.ctx.Event == nil 判空。测试之间顺序执行，goroutine 不会，
	// 于是「本用例写 / 上条用例的 goroutine 读」构成 -race 能抓到的数据竞争
	// （-shuffle=on 下相邻用例会变，所以时有时无）。
	// 换成专属 ctx 后这个字段只有本 goroutine 碰得到，竞争从根上消失。
	//
	// 监听方注册表（config 包的 eventListeners）本来就是进程级全局且带锁，
	// 不随 ctx 走，所以拆 ctx 不影响下面的注册/投递断言。
	evtCtx := newEventTestContext(t)
	f := New(evtCtx)

	// 本包不 import modules/base，拿不到它的迁移，event 表要手工建
	// （与 TestMain 为 group / robot / user 建夹具表同一套路）。列跟随
	// modules/base/sql/20191106000001_event_legacy01.sql + 20250423000001。
	_, err = evtCtx.DB().Exec("CREATE TABLE IF NOT EXISTS `event` (" +
		"id INTEGER NOT NULL PRIMARY KEY AUTO_INCREMENT, " +
		"event VARCHAR(40) NOT NULL DEFAULT '', `type` SMALLINT NOT NULL DEFAULT 0, " +
		"data VARCHAR(10000) NOT NULL DEFAULT '', status SMALLINT NOT NULL DEFAULT 0, " +
		"reason VARCHAR(1000) NOT NULL DEFAULT '', version_lock INTEGER NOT NULL DEFAULT 0, " +
		"created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, " +
		"updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)")
	require.NoError(t, err)
	_, err = evtCtx.DB().Exec("DELETE FROM `event`")
	require.NoError(t, err)

	// 先断「无监听方不落库」，再注册监听方断投递 —— 两件事必须在同一个用例里做：
	// AddEventListener 是全局注册且没有注销 API，拆成两个用例的话，先跑的那个会把
	// 监听方漏给后跑的，后者永远测不到"无监听"这一支。
	f.fireSpaceMemberRemoveEvent("nolistener-space", "u", "op", MemberRemoveReasonKicked)
	var pre int
	_, err = evtCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `event` WHERE event=?", event.SpaceMemberRemove).Load(&pre)
	require.NoError(t, err)
	require.Zero(t, pre, "无监听方时不该写事件行（解散大空间会逐成员触发，写了就是纯浪费）")

	evtCtx.AddEventListener(event.SpaceMemberRemove, func(data []byte, commit config.EventCommit) {
		commit(nil)
	})

	f.fireSpaceMemberRemoveEvent("evt-space", "evt-uid", "evt-op", MemberRemoveReasonForceRemoved)

	var rows []struct {
		Event string `db:"event"`
		Data  string `db:"data"`
	}
	_, err = evtCtx.DB().SelectBySql("SELECT event, data FROM event ORDER BY id DESC LIMIT 1").Load(&rows)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, event.SpaceMemberRemove, rows[0].Event)
	assert.Contains(t, rows[0].Data, "evt-space")
	assert.Contains(t, rows[0].Data, "evt-uid")
	assert.Contains(t, rows[0].Data, "evt-op")
	assert.Contains(t, rows[0].Data, MemberRemoveReasonForceRemoved)
}

// newEventTestContext 造一个只服务于事件用例的 config.Context。
//
// 指向同一个 test 库（断言仍然读得到 event 行），但 Event 字段独立，
// 从而不用去写共享的 testCtx.Event。config.NewContext 不跑迁移、不注册路由，
// 只起 pool 和 timingwheel，代价是一份连接池；整个包只建这一个。
func newEventTestContext(t *testing.T) *config.Context {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	cfg.DB.MySQLAddr = testCtx.GetConfig().DB.MySQLAddr
	cfg.DB.Migration = false
	ctx := config.NewContext(cfg)
	ctx.Event = event.New(ctx)
	return ctx
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

// TestMemberRemovalRetryDelayReachesCap 退避必须真的能取到 5 分钟上限。
// 先把 attempt 夹到 8 再算 1<<attempt 会让上限永远取不到（2^8=256s < 300s），
// 整个重试预算跟着缩水到约 12 分钟。
func TestMemberRemovalRetryDelayReachesCap(t *testing.T) {
	assert.Equal(t, 256*time.Second, memberRemovalRetryDelay(8))
	assert.Equal(t, 5*time.Minute, memberRemovalRetryDelay(9), "2^9=512s 必须被夹到 5 分钟")
	assert.Equal(t, 5*time.Minute, memberRemovalRetryDelay(64), "超大 attempt 不得移位溢出")

	total := time.Duration(0)
	for i := uint32(1); i <= removalCleanupMaxAttempts; i++ {
		total += memberRemovalRetryDelay(i)
	}
	assert.Greater(t, total, time.Hour,
		"隔离性清理的重试窗口要能扛过一次像样的下游故障，否则工单会被打成 abandoned 且无人重驱")
}

// TestTruncateCleanupErrorKeepsValidUTF8 按字节切会切出半个字符，
// utf8mb4 列在 strict 模式下拒写，那条 UPDATE 一失败 attempts 就不增、
// next_attempt_at 也不推进——保护重试的函数反而把退避弄断。
func TestTruncateCleanupErrorKeepsValidUTF8(t *testing.T) {
	long := "dm_cutoff: " + strings.Repeat("中", 200)
	got := truncateCleanupError(long)
	assert.True(t, utf8.ValidString(got), "截断结果必须是合法 UTF-8")
	assert.LessOrEqual(t, len(got), 255)
	assert.Greater(t, len(got), 240, "不该为了对齐边界丢掉过多内容")
}

// TestCleanupWorkerContainsStepPanic panic 必须在单条工单这一层兜住并走正常的
// 失败路径，否则 attempts 不增、状态留 pending，工单被反复认领反复 panic，
// 永远到不了 abandoned，还会按 ORDER BY id 卡在队首把后面的工单饿死。
func TestCleanupWorkerContainsStepPanic(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "wk-panic"
	seedMember(t, f, spaceID, "owner-p", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "victim-p", Role: 0, Status: 1}))
	mustRemoveMember(t, f, spaceID, "victim-p", 1, "owner-p", MemberRemoveReasonKicked)

	restore := swapCleanupStepsForTest([]namedCleanupStep{{
		name: "panics",
		fn: func(*config.Context, MemberRemoval) error {
			panic("boom")
		},
	}})
	defer restore()

	assert.NotPanics(t, func() { f.processMemberRemovalCleanups() })

	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Equal(t, removalCleanupPending, jobs[0].Status)
	assert.EqualValues(t, 1, jobs[0].Attempts, "panic 也必须计入 attempts，否则永远到不了 abandoned")
	assert.Empty(t, jobs[0].LeaseOwner, "panic 后必须释放租约")
	assert.Contains(t, jobs[0].LastError, "panic")
}

// TestClaimCleanupOwnerIsPerClaim 租约持有者必须每次认领都不同。
// 若是进程级常量，同进程内两个 goroutine 的 `AND lease_owner=?` 守卫会同时成立，
// 先跑完的把工单标终态，另一个还在半路——群里出现重复的「被移出」系统消息。
func TestClaimCleanupOwnerIsPerClaim(t *testing.T) {
	a, b := newRemovalClaimOwner(), newRemovalClaimOwner()
	assert.NotEqual(t, a, b)
	// 必须放得进 lease_owner VARCHAR(64)：超长会被 MySQL 以 "Data too long"
	// 拒掉整条认领，worker 于是一条工单都处理不了。
	assert.LessOrEqual(t, len(a), 64, "租约标识不得超过 lease_owner 列宽")
}

// TestOwnerDisbandSpaceEnqueuesCleanup 群主解散自己的空间（用户侧 DELETE /v1/space/:id）
// 必须和管理端强制解散走同一条级联。
//
// 此前这个 handler 只翻 space.status，成员行原样留着 status=1：解散后成员仍在该
// 空间的所有群里、私聊白名单原封不动，而 SharesActiveSpace 因为 space.status=0
// 已判定为"无共同空间"——服务端说不该能发，WuKongIM 的缓存却还允许发。
// 用户侧解散比管理端强制解散常见得多，不能只接后者。
func TestOwnerDisbandSpaceEnqueuesCleanup(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "owner-disband"
	seedMember(t, f, spaceID, testutil.UID, 2) // testutil.UID 是 owner
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID, UID: "m-other", Role: 0, Status: 1,
	}))

	conn := testCtx.GetRedisConn()
	cacheKey := "space:member:" + spaceID + ":m-other"
	require.NoError(t, conn.SetAndExpire(cacheKey, "1", time.Minute))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/v1/space/"+spaceID, nil)
	req.Header.Set("token", testutil.Token)
	testSrv.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	// 空间与成员行都要置 0
	var spaceStatus, activeMembers int
	_, err = testCtx.DB().SelectBySql("SELECT status FROM space WHERE space_id=?", spaceID).Load(&spaceStatus)
	require.NoError(t, err)
	assert.Equal(t, 0, spaceStatus)
	_, err = testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND status=1", spaceID).Load(&activeMembers)
	require.NoError(t, err)
	assert.Zero(t, activeMembers, "解散必须把成员行一并置 0")

	// 每个成员一条清理工单
	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 2)
	for _, job := range jobs {
		assert.Equal(t, MemberRemoveReasonSpaceDisbanded, job.Reason)
	}

	// 鉴权缓存同步失效
	after, err := conn.GetString(cacheKey)
	require.NoError(t, err)
	assert.Empty(t, after, "解散后成员的 Space 鉴权缓存必须立即失效")
}

// TestCleanupWorkerAbandonRecordsAttempts 被放弃的工单必须把最后一次尝试计入 attempts，
// 否则运维按 `attempts >= removalCleanupMaxAttempts` 查不出任何被放弃的工单，
// 也无法与「改了常量之后提前放弃」的行区分开。
func TestCleanupWorkerAbandonRecordsAttempts(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "wk-abandon-attempts"
	seedMember(t, f, spaceID, "owner-a", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "victim-a", Role: 0, Status: 1}))
	mustRemoveMember(t, f, spaceID, "victim-a", 1, "owner-a", MemberRemoveReasonKicked)
	_, err = testCtx.DB().Exec(
		"UPDATE space_member_removal_cleanup SET attempts=? WHERE space_id=?",
		removalCleanupMaxAttempts-1, spaceID)
	require.NoError(t, err)

	restore := swapCleanupStepsForTest([]namedCleanupStep{{
		name: "always-fails",
		fn:   func(*config.Context, MemberRemoval) error { return errors.New("down") },
	}})
	defer restore()

	f.processMemberRemovalCleanups()

	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Equal(t, removalCleanupAbandoned, jobs[0].Status)
	assert.EqualValues(t, removalCleanupMaxAttempts, jobs[0].Attempts,
		"放弃时必须把最后一次尝试计进去")
}

// TestTruncateCleanupErrorSanitizesInteriorInvalidByte 原串中间带非法字节时，
// 必须把它清掉而不是原样放行。
//
// 只修「末尾被截断的那个 rune」是不够的：中间的裸字节照样写不进 utf8mb4 列，
// strict 模式会拒掉整条 UPDATE，attempts 与 next_attempt_at 都不会推进，
// 工单在租约到期后被反复认领、永远到不了 abandoned —— 用来保护重试的函数
// 反倒把退避弄断了。同时也不能因为一个中间非法字节就把整条摘要裁没。
func TestTruncateCleanupErrorSanitizesInteriorInvalidByte(t *testing.T) {
	// 第 4 个字节是裸的 0xff（上游代理错误页里很常见），后面接足够长的内容
	input := "dm_" + string([]byte{0xff}) + strings.Repeat("a", 400)
	got := truncateCleanupError(input)
	assert.True(t, utf8.ValidString(got), "中间的非法字节必须被清掉")
	assert.LessOrEqual(t, len(got), 255)
	assert.Greater(t, len(got), 200, "不该因为一个非法字节把整条摘要裁没")
	assert.True(t, strings.HasPrefix(got, "dm_"), "有效前缀必须保留")
}

// TestRemoveMembersRejectsOversizedBatch 用户侧 members/remove 的批量上限。
//
// 这个上限是新加的、且**对线上可见**：每个 uid 要跑一个独立事务 + 一次 Redis DEL
// + 一条工单插入，不设限就是一个拒绝服务杠杆。没有回归用例的话，谁把这段挪走
// 都不会有人发现，而超限请求会安静地退化回逐个处理。
// 同时钉住「刚好等于上限」仍然放行，避免把边界改成 >= 而悄悄收紧一个。
func TestRemoveMembersRejectsOversizedBatch(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "batch-cap"
	seedMember(t, f, spaceID, testutil.UID, 2) // 操作者：owner

	post := func(n int) *httptest.ResponseRecorder {
		uids := make([]string, 0, n)
		for i := 0; i < n; i++ {
			uids = append(uids, "cap-"+strconv.Itoa(i))
		}
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/space/"+spaceID+"/members/remove",
			bytes.NewReader([]byte(util.ToJson(map[string]interface{}{"uids": uids}))))
		req.Header.Set("token", testutil.Token)
		testSrv.GetRoute().ServeHTTP(w, req)
		return w
	}

	over := post(managerMaxBatchUIDs + 1)
	assert.NotEqual(t, http.StatusOK, over.Code, "超过上限必须被拒，不能静默逐个处理")
	assert.Contains(t, over.Body.String(), "batch",
		"错误体要能看出是批量上限，否则客户端不知道该分片")

	assert.Equal(t, http.StatusOK, post(managerMaxBatchUIDs).Code,
		"刚好等于上限必须放行——边界写成 >= 会悄悄少收一个")
}

// TestTruncateCleanupErrorCutsOnRuneBoundary 截断点落在**任意宽度**的 rune 中间
// 时都必须退回完整边界。
//
// 之前只削一个字节，对 3 字节的「中」够用，对 4 字节的 emoji 不够：
// DecodeLastRuneInString 面对悬空序列每次只报 (RuneError, 1)，砍掉 1 字节后
// 仍留着 f0 9f 这样的半个 rune。写进 utf8mb4 的 last_error 会被 strict 模式
// 以 Incorrect string value 拒掉整条 release，工单卡在 running 上白等一轮租约。
// 逐个宽度铺开，是因为这个 bug 只在特定宽度上现形。
func TestTruncateCleanupErrorCutsOnRuneBoundary(t *testing.T) {
	for _, r := range []string{"a", "é", "中", "😀"} {
		width := len(r)
		// 让这个 rune 恰好跨过第 255 字节：前缀填到 256-width，
		// 于是它的第一个字节落在界内、剩余字节落在界外。
		for pad := 256 - width; pad < 256; pad++ {
			input := strings.Repeat("x", pad) + r + strings.Repeat("y", 400)
			got := truncateCleanupError(input)
			require.Truef(t, utf8.ValidString(got),
				"width=%d pad=%d 截断结果必须是合法 UTF-8，实际尾部 %x", width, pad, got[max(0, len(got)-4):])
			require.LessOrEqualf(t, len(got), 255, "width=%d pad=%d 不能超列宽", width, pad)
			// 最多只该为对齐边界退掉 width-1 个字节
			require.Greaterf(t, len(got), 255-width, "width=%d pad=%d 退得太多", width, pad)
		}
	}
}

// TestProcessMemberRemovalCleanupsIsNotReentrant 同一进程内只允许一轮在跑。
// 定时器是「先安排下一次、再执行本次」，慢批次会把并发轮数越堆越多。
func TestProcessMemberRemovalCleanupsIsNotReentrant(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "wk-reentrant"
	seedMember(t, f, spaceID, "owner-r", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "victim-r", Role: 0, Status: 1}))
	mustRemoveMember(t, f, spaceID, "victim-r", 1, "owner-r", MemberRemoveReasonKicked)

	var inner int
	restore := swapCleanupStepsForTest([]namedCleanupStep{{
		name: "reenter",
		fn: func(*config.Context, MemberRemoval) error {
			// 步骤内部再调一次：必须直接让位，不能又跑一轮
			f.processMemberRemovalCleanups()
			inner++
			return nil
		},
	}})
	defer restore()

	f.processMemberRemovalCleanups()
	assert.Equal(t, 1, inner, "重入的那次必须直接返回，不得再执行一轮")
}

// TestPurgeKeepsAbandonedJobs 保留期清理只删 done。abandoned 是「隔离性清理最终
// 放弃了」的唯一持久记录——除了一条 error 日志之外没别的东西记得它，删掉就查不出来了。
func TestPurgeKeepsAbandonedJobs(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "purge-keep"
	seedMember(t, f, spaceID, "owner-pk", 2)
	for _, uid := range []string{"done-1", "abandoned-1"} {
		require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
			SpaceId: spaceID, UID: uid, Role: 0, Status: 1,
		}))
		mustRemoveMember(t, f, spaceID, uid, 1, "owner-pk", MemberRemoveReasonKicked)
	}
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	_, err = testCtx.DB().Exec(
		"UPDATE space_member_removal_cleanup SET status=?, finished_at=? WHERE space_id=? AND uid=?",
		removalCleanupDone, old, spaceID, "done-1")
	require.NoError(t, err)
	_, err = testCtx.DB().Exec(
		"UPDATE space_member_removal_cleanup SET status=?, finished_at=? WHERE space_id=? AND uid=?",
		removalCleanupAbandoned, old, spaceID, "abandoned-1")
	require.NoError(t, err)

	deleted, err := f.db.purgeFinishedMemberRemovalCleanups(time.Now().UTC(), 100)
	require.NoError(t, err)
	assert.EqualValues(t, 1, deleted)

	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Equal(t, "abandoned-1", jobs[0].UID, "abandoned 必须保留")
}

// TestClaimIncrementsAttempts 认领即计数。只在失败释放时计数的话，进程在作业中途
// 被杀（OOM / 驱逐）就没人写 attempts，租约到期后重认领计数原地不动，
// 一条必然打死进程的作业能无限循环、永远到不了 abandoned。
func TestClaimIncrementsAttempts(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "claim-attempts"
	seedMember(t, f, spaceID, "owner-ca", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "victim-ca", Role: 0, Status: 1}))
	mustRemoveMember(t, f, spaceID, "victim-ca", 1, "owner-ca", MemberRemoveReasonKicked)

	now := time.Now().UTC()
	_, err = f.db.claimMemberRemovalCleanup("worker-x", now)
	require.NoError(t, err)
	assert.EqualValues(t, 1, cleanupJobs(t, spaceID)[0].Attempts, "认领即计数")

	// 模拟进程被杀：既不 finish 也不 release，等租约过期后被接管
	_, err = f.db.claimMemberRemovalCleanup("worker-y", now.Add(removalCleanupLease+time.Second))
	require.NoError(t, err)
	assert.EqualValues(t, 2, cleanupJobs(t, spaceID)[0].Attempts,
		"崩溃后被接管也必须计数，否则永远到不了 abandoned")
}
