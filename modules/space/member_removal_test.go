package space

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	mysqldrv "github.com/go-sql-driver/mysql"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
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
// 这是整条链路的入口：没有工单，后面的退群清理全都不会发生。
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
// 迟到的重试绝不能把他的群拆掉。
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
	long := "group_cascade: " + strings.Repeat("中", 200)
	got := truncateCleanupError(long)
	assert.True(t, utf8.ValidString(got), "截断结果必须是合法 UTF-8")
	assert.LessOrEqual(t, len(got), 255)
	assert.Greater(t, len(got), 240, "不该为了对齐边界丢掉过多内容")
}

// TestCleanupWorkerContainsStepPanic panic 必须在单条工单这一层兜住并走正常的
// 失败路径，否则 attempts 不增、状态留 pending，工单被反复认领反复 panic，
// 永远到不了 abandoned，还会一轮一轮白占批次名额，把同批的健康工单挤出去。
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
// 空间的所有群里、WuKongIM 群订阅原样保留，而 space.status=0
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
// 这个上限是新加的、且**对线上可见**：一次请求要在一个事务里锁住并翻转最多 N 行、
// 逐个失效 N 份鉴权缓存（Redis DEL）、批量插入 N 条工单，不设限就是一个拒绝服务杠杆。
// 没有回归用例的话，谁把这段挪走都不会有人发现，而超限请求会安静地放行。
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

// ---------- 加入侧：恢复步骤的接线 ----------

// TestCleanupRunsEveryStepDespiteFailure 一个步骤失败不能让同一轮里的其余步骤
// 被跳过。
//
// 之前是 fail-fast，而步骤顺序就是注册顺序（由 import 方向决定，
// 仅仅因为 modules/group import 了 modules/user）。WuKongIM 持续故障时
// 排在前面的步骤每轮都失败时，20 次尝试全烧在它身上，工单走到 abandoned 时
// **群级联一次都没跑过**——被移除的人保留着全部群权限。那正是这条链路要消灭的
// 隔离失败，却发生在最需要它生效的场景里。
func TestCleanupRunsEveryStepDespiteFailure(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	var secondRan atomic.Bool
	RegisterMemberRemovalCleanupStep("test_failing_first", func(*config.Context, MemberRemoval) error {
		return errors.New("IM unavailable")
	})
	RegisterMemberRemovalCleanupStep("test_second", func(*config.Context, MemberRemoval) error {
		secondRan.Store(true)
		return nil
	})
	t.Cleanup(func() {
		RegisterMemberRemovalCleanupStep("test_failing_first", func(*config.Context, MemberRemoval) error { return nil })
		RegisterMemberRemovalCleanupStep("test_second", func(*config.Context, MemberRemoval) error { return nil })
	})

	const spaceID = "step-starvation"
	seedMember(t, f, spaceID, "owner-s", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "victim-s", Role: 0, Status: 1}))
	mustRemoveMember(t, f, spaceID, "victim-s", 1, "owner-s", MemberRemoveReasonKicked)

	f.processMemberRemovalCleanups()

	assert.True(t, secondRan.Load(),
		"第一个步骤失败之后，后面的步骤仍然必须跑——否则它会独占整个重试预算")

	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Equal(t, removalCleanupPending, jobs[0].Status, "有步骤失败，工单要留着重试")
	assert.Contains(t, jobs[0].LastError, "test_failing_first",
		"last_error 要指向真正失败的那个步骤")
}

// TestCleanupRunsRemainingStepsAfterStepPanic 一个步骤 **panic** 也不能让同一轮里
// 其余步骤被跳过。
//
// 这是 TestCleanupRunsEveryStepDespiteFailure 的姊妹用例，走的是另一条路径：
// 之前只有函数级 recover，panic 会直接跳出步骤循环，于是一个步骤 panic 时
// group_cascade 本轮一次都不跑，而 attempts 在认领时已自增，工单照样走到
// abandoned——被移除的人保留着全部群权限。恰恰是 error 那条路径刚修好的那个失败。
func TestCleanupRunsRemainingStepsAfterStepPanic(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	var secondRan atomic.Bool
	RegisterMemberRemovalCleanupStep("test_panicking_first", func(*config.Context, MemberRemoval) error {
		panic("step boom")
	})
	RegisterMemberRemovalCleanupStep("test_second_after_panic", func(*config.Context, MemberRemoval) error {
		secondRan.Store(true)
		return nil
	})
	t.Cleanup(func() {
		RegisterMemberRemovalCleanupStep("test_panicking_first", func(*config.Context, MemberRemoval) error { return nil })
		RegisterMemberRemovalCleanupStep("test_second_after_panic", func(*config.Context, MemberRemoval) error { return nil })
	})

	const spaceID = "step-panic-starve"
	seedMember(t, f, spaceID, "owner-sp", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "victim-sp", Role: 0, Status: 1}))
	mustRemoveMember(t, f, spaceID, "victim-sp", 1, "owner-sp", MemberRemoveReasonKicked)

	require.NotPanics(t, func() { f.processMemberRemovalCleanups() })

	assert.True(t, secondRan.Load(),
		"前一个步骤 panic 之后，后面的步骤仍然必须跑——否则它独占整个重试预算，"+
			"工单到 abandoned 时群级联一次都没跑过")

	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Equal(t, removalCleanupPending, jobs[0].Status, "有步骤失败，工单要留着重试")
	assert.Contains(t, jobs[0].LastError, "test_panicking_first",
		"last_error 要指向 panic 的那个步骤")
}

// TestCleanupErrorNamesAllFailedSteps 多个步骤同时失败时，last_error 要能看出
// 「不止一个」，否则运维只会去查第一个。
func TestCleanupErrorNamesAllFailedSteps(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	RegisterMemberRemovalCleanupStep("test_fail_a", func(*config.Context, MemberRemoval) error {
		return errors.New("a down")
	})
	RegisterMemberRemovalCleanupStep("test_fail_b", func(*config.Context, MemberRemoval) error {
		return errors.New("b down")
	})
	t.Cleanup(func() {
		RegisterMemberRemovalCleanupStep("test_fail_a", func(*config.Context, MemberRemoval) error { return nil })
		RegisterMemberRemovalCleanupStep("test_fail_b", func(*config.Context, MemberRemoval) error { return nil })
	})

	const spaceID = "step-multifail"
	seedMember(t, f, spaceID, "owner-m", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "victim-m", Role: 0, Status: 1}))
	mustRemoveMember(t, f, spaceID, "victim-m", 1, "owner-m", MemberRemoveReasonKicked)

	f.processMemberRemovalCleanups()

	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Contains(t, jobs[0].LastError, "(+1)",
		"多个步骤失败时要在 last_error 里标出还有几个，否则只会去查第一个")
}

// TestCleanupRunsWhenSpaceDisbandedDespiteActiveMemberRow 是本次修复的核心回归。
//
// worker 的外层门以前用 queryMember，只看 space_member.status=1，完全不问 Space
// 本身死没死。join-vs-disband 竞态会造出正是这种状态：Space 已经 status=0，成员行
// 却被一次并发 join 写回 status=1。外层门看见 status=1 就把工单当成「人已重新加入」
// 直接作废（done/skipped_rejoined），于是这个人的 group_member 行和 IM 群订阅
// 永远留在一个已解散的空间里，而且再没有任何工单会回来看一眼。
//
// 内层的级联步骤本来判得对（它用 CheckMembership，要求 space.status=1，解散场景
// 判定为「不是成员」→ 照常清理），但外层门先跑、先短路，内层那个精心挑过的谓词
// 根本没机会执行。
func TestCleanupRunsWhenSpaceDisbandedDespiteActiveMemberRow(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "wk-disband-orphan"
	seedMember(t, f, spaceID, "owner-orphan", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID, UID: "orphan", Role: 0, Status: 1,
	}))
	mustRemoveMember(t, f, spaceID, "orphan", 1, "owner-orphan", MemberRemoveReasonKicked)

	// 造出 join-vs-disband 孤儿：成员行被并发 join 写回 status=1，Space 已解散。
	require.NoError(t, f.db.reactivateMember(spaceID, "orphan", 0))
	_, err = testCtx.DB().Exec(
		"UPDATE space SET status=? WHERE space_id=?", SpaceStatusDisbanded, spaceID)
	require.NoError(t, err)

	called := 0
	restore := swapCleanupStepsForTest([]namedCleanupStep{{
		name: "must-run",
		fn: func(*config.Context, MemberRemoval) error {
			called++
			return nil
		},
	}})
	defer restore()

	f.processMemberRemovalCleanups()

	assert.Equal(t, 1, called, "Space 已解散，成员行只是 join-vs-disband 孤儿，清理必须照常执行")
	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Equal(t, removalCleanupDone, jobs[0].Status)
	assert.NotEqual(t, "skipped_rejoined", jobs[0].LastError,
		"外层门不得把已解散空间里的孤儿成员行当成『已重新加入』")
}

// TestCleanupSkipsMemberInBannedSpace 封禁不是解散：成员资格仍然有效，
// Manager.addMembers 也仍允许往封禁空间加人（只挡 SpaceStatusDisbanded）。
// 所以在职成员绝不能因为空间被封禁就被清理管线拆出所有群。
func TestCleanupSkipsMemberInBannedSpace(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "wk-banned"
	seedMember(t, f, spaceID, "owner-banned", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID, UID: "banned-member", Role: 0, Status: 1,
	}))
	mustRemoveMember(t, f, spaceID, "banned-member", 1, "owner-banned", MemberRemoveReasonKicked)

	// 人被加了回来，随后整个 Space 被封禁。
	require.NoError(t, f.db.reactivateMember(spaceID, "banned-member", 0))
	_, err = testCtx.DB().Exec(
		"UPDATE space SET status=? WHERE space_id=?", SpaceStatusBanned, spaceID)
	require.NoError(t, err)

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

	assert.Zero(t, called, "空间只是被封禁、人还是成员，不得触发任何清理步骤")
	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Equal(t, "skipped_rejoined", jobs[0].LastError)
}

// TestCheckMembershipForCleanupMatrix 直接把谓词的真值表钉死。
//
// 上面那两条走 worker 的用例只覆盖「成员行活跃」这一行；这里补齐另一行，
// 并把三种 Space 状态一次说清楚：判定只由「Space 是否已解散」和「成员行是否
// 活跃」两件事决定，封禁与正常同档。
func TestCheckMembershipForCleanupMatrix(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	cases := []struct {
		name        string
		spaceStatus int
		memberAlive bool
		wantHeld    bool // CheckMembershipForCleanup：true = 席位仍在 → 清理必须跳过
		wantAuth    bool // CheckMembership：true = 允许访问该 Space
	}{
		{"正常空间/活跃成员：人还在，跳过清理", SpaceStatusNormal, true, true, true},
		{"正常空间/已移除：正常的被踢路径，清理执行", SpaceStatusNormal, false, false, false},
		// ↓ 这一格是两个谓词唯一分歧之处，也是整条修复的核心。
		{"封禁空间/活跃成员：清理跳过，但鉴权必须拒绝", SpaceStatusBanned, true, true, false},
		{"封禁空间/已移除：照常清理", SpaceStatusBanned, false, false, false},
		{"解散空间/活跃成员：join-vs-disband 孤儿，必须清理", SpaceStatusDisbanded, true, false, false},
		{"解散空间/已移除：照常清理", SpaceStatusDisbanded, false, false, false},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spaceID := fmt.Sprintf("matrix-%d", i)
			uid := fmt.Sprintf("matrix-u-%d", i)
			memberStatus := 0
			if tc.memberAlive {
				memberStatus = 1
			}
			require.NoError(t, f.db.insertSpaceNoTx(&SpaceModel{
				SpaceId: spaceID, Name: spaceID, Creator: uid,
				Status: tc.spaceStatus, MaxUsers: 100,
			}))
			require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
				SpaceId: spaceID, UID: uid, Role: 0, Status: memberStatus,
			}))

			held, err := spacepkg.CheckMembershipForCleanup(f.ctx.DB(), spaceID, uid)
			require.NoError(t, err)
			assert.Equal(t, tc.wantHeld, held)

			// 同一份数据上再问一次鉴权谓词。两者必须在「封禁 + 活跃成员」
			// 这一格上分道扬镳：清理谓词说「席位还在，别清」，鉴权谓词说
			// 「不许访问」。这就是 #797 原方案要合并、而合并会开安全口子的那一格。
			auth, err := spacepkg.CheckMembership(f.ctx.DB(), spaceID, uid)
			require.NoError(t, err)
			assert.Equal(t, tc.wantAuth, auth,
				"CheckMembership 是 SpaceMiddleware 的鉴权谓词，只有正常空间的活跃成员才能通过")
		})
	}

	// Space 行整个不存在（而非 status=0）也必须判成「席位已失效」：
	// INNER JOIN 不命中，方向是 fail-safe——清理照常执行，而不是静默作废工单。
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: "matrix-no-space", UID: "matrix-orphan", Role: 0, Status: 1,
	}))
	held, err := spacepkg.CheckMembershipForCleanup(f.ctx.DB(), "matrix-no-space", "matrix-orphan")
	require.NoError(t, err)
	assert.False(t, held, "Space 行不存在时必须 fail-safe：判成席位已失效，清理照常跑")
}

// seedExhaustedJob 把某个 Space 的工单直接顶到指定 attempts，并按需清掉租约，
// 模拟「进程在作业跑到一半被 SIGKILL / OOM 干掉」后留下的行：
// attempts 已在认领时自增过，但没有任何人走到 releaseCleanupJob 去写终态。
func seedExhaustedJob(t *testing.T, spaceID string, attempts uint32, leaseUntil interface{}) {
	t.Helper()
	_, err := testCtx.DB().Exec(
		"UPDATE space_member_removal_cleanup SET attempts=?, lease_owner=?, lease_until=? WHERE space_id=?",
		attempts, "dead-worker", leaseUntil, spaceID)
	require.NoError(t, err)
}

// TestClaimSkipsJobAtAttemptsCeiling 认领必须自己卡住重试预算。
//
// abandoned 的转换今天只在 releaseCleanupJob 里发生，而那条路径只覆盖「作业跑完
// 并返回了错误」。进程被 SIGKILL / OOM 打死时谁也没走到那里：租约到期后同一行
// 又被认领，attempts 再 +1，如此无限。认领处不设防，abandoned 就永远到不了。
func TestClaimSkipsJobAtAttemptsCeiling(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "claim-ceiling"
	seedMember(t, f, spaceID, "owner-c1", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "v-c1", Role: 0, Status: 1}))
	mustRemoveMember(t, f, spaceID, "v-c1", 1, "owner-c1", MemberRemoveReasonKicked)

	seedExhaustedJob(t, spaceID, removalCleanupMaxAttempts, nil)

	job, err := f.db.claimMemberRemovalCleanup("worker-x", time.Now().UTC())
	require.NoError(t, err)
	assert.Nil(t, job, "attempts 已达上限的工单不得再被认领")
}

// TestClaimAllowsExactlyMaxAttempts 上限的边界：预算是 removalCleanupMaxAttempts 次
// **真实尝试**，不能少一次也不能多一次。attempts 在认领时自增，所以差一个偏移量
// 就会悄悄少跑一轮或多跑一轮。
func TestClaimAllowsExactlyMaxAttempts(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "claim-boundary"
	seedMember(t, f, spaceID, "owner-c2", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "v-c2", Role: 0, Status: 1}))
	mustRemoveMember(t, f, spaceID, "v-c2", 1, "owner-c2", MemberRemoveReasonKicked)

	// 差一次到顶：这是第 max 次认领，必须放行。
	seedExhaustedJob(t, spaceID, removalCleanupMaxAttempts-1, nil)
	job, err := f.db.claimMemberRemovalCleanup("worker-y", time.Now().UTC())
	require.NoError(t, err)
	require.NotNil(t, job, "还差一次到上限时必须仍可认领")
	assert.Equal(t, removalCleanupMaxAttempts, job.Attempts, "认领后 attempts 应恰好等于上限")

	// 现在到顶了，下一次不得放行。
	_, err = testCtx.DB().Exec(
		"UPDATE space_member_removal_cleanup SET lease_until=NULL WHERE space_id=?", spaceID)
	require.NoError(t, err)
	none, err := f.db.claimMemberRemovalCleanup("worker-z", time.Now().UTC())
	require.NoError(t, err)
	assert.Nil(t, none, "到达上限后不得再认领")
}

// TestExhaustedJobNeitherRunsNorBlocksOthers 一条打死进程的工单若永远可认领，每一轮
// 都会白占一个批次名额，把同批本该被处理的健康工单挤出去。
//
// 这条断言的是**批次层**的性质：同一轮 processMemberRemovalCleanups 里，耗尽预算的
// 工单不执行，且不妨碍健康工单执行。它与 TestClaimSkipsJobAtAttemptsCeiling 不重复——
// 那条只看单次 claim 返回 nil，这条看整轮调度的结果。
//
// 早先这条叫 TestExhaustedJobDoesNotHeadOfLineQueue，前提是认领按 `ORDER BY id` 取
// 队首、毒丸必然排在最前。那个排序已在本次改动中去掉（见 claimMemberRemovalCleanup），
// 于是"队首"不再存在，名字也就名不副实了。性质本身仍然成立，改名以对齐它真正证明的东西。
func TestExhaustedJobNeitherRunsNorBlocksOthers(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const poisonSpace, healthySpace = "hol-poison", "hol-healthy"
	seedMember(t, f, poisonSpace, "owner-h1", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: poisonSpace, UID: "v-h1", Role: 0, Status: 1}))
	mustRemoveMember(t, f, poisonSpace, "v-h1", 1, "owner-h1", MemberRemoveReasonKicked)
	seedExhaustedJob(t, poisonSpace, removalCleanupMaxAttempts, nil)

	// 健康工单晚于毒丸入队（id 更大）。认领已无 ORDER BY，取件顺序不确定，
	// 所以这条断言的是"同一轮里健康工单一定被处理到"，而不是"排在毒丸之后仍被处理到"。
	seedMember(t, f, healthySpace, "owner-h2", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: healthySpace, UID: "v-h2", Role: 0, Status: 1}))
	mustRemoveMember(t, f, healthySpace, "v-h2", 1, "owner-h2", MemberRemoveReasonKicked)

	var seen []string
	restore := swapCleanupStepsForTest([]namedCleanupStep{{
		name: "spy",
		fn: func(_ *config.Context, r MemberRemoval) error {
			seen = append(seen, r.SpaceID)
			return nil
		},
	}})
	defer restore()

	f.processMemberRemovalCleanups()

	assert.Contains(t, seen, healthySpace, "耗尽预算的工单不得挤掉同批的健康工单")
	assert.NotContains(t, seen, poisonSpace, "已耗尽预算的工单不得再执行")
}

// TestSweepAbandonsExhaustedJobLeftByHardKill 进程被硬杀后没人写终态，
// 扫描必须把它推到 abandoned，让终态真的可达、可查、可告警。
func TestSweepAbandonsExhaustedJobLeftByHardKill(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "sweep-dead"
	seedMember(t, f, spaceID, "owner-s1", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "v-s1", Role: 0, Status: 1}))
	mustRemoveMember(t, f, spaceID, "v-s1", 1, "owner-s1", MemberRemoveReasonKicked)

	// 租约早已过期，attempts 到顶，状态还停在 pending：典型的 SIGKILL 残留。
	seedExhaustedJob(t, spaceID, removalCleanupMaxAttempts,
		time.Now().UTC().Add(-time.Hour))

	f.sweepExhaustedMemberRemovalCleanups()

	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Equal(t, removalCleanupAbandoned, jobs[0].Status, "预算耗尽且租约已过期的工单必须被扫成 abandoned")
	assert.Equal(t, removalCleanupMaxAttempts, jobs[0].Attempts)
	assert.Contains(t, jobs[0].LastError, "retries exhausted")
	assert.Empty(t, jobs[0].LeaseOwner, "终态必须释放租约")
}

// TestSweepLeavesRecentlyExpiredLeaseAlone 租约刚过期还不够判死。
//
// 群级联「几十个群就能跑上几分钟」，跑过 10 分钟租约是**预期内**的，不是进程死了。
// 若扫描只要求「租约已过期」，一条正在正常推进、随后会成功的作业就会被判成
// abandoned 并触发「需人工介入」告警；而它自己的 finish 因为 status 已变而落空，
// 只留下一句误导的「租约已易主」——完成的工作被永久记成放弃。
// 所以门槛要留一整个租约周期的宽限。
func TestSweepLeavesRecentlyExpiredLeaseAlone(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "sweep-grace"
	seedMember(t, f, spaceID, "owner-s3", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID, UID: "v-s3", Role: 0, Status: 1,
	}))
	mustRemoveMember(t, f, spaceID, "v-s3", 1, "owner-s3", MemberRemoveReasonKicked)

	// 租约刚过期一分钟：作业很可能还在跑（大空间的级联本来就会超租约）。
	seedExhaustedJob(t, spaceID, removalCleanupMaxAttempts,
		time.Now().UTC().Add(-time.Minute))

	f.sweepExhaustedMemberRemovalCleanups()

	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Equal(t, removalCleanupPending, jobs[0].Status,
		"租约刚过期不等于进程死了，必须留出一整个租约周期的宽限再判死")
}

// TestSweepLeavesLiveLeaseAlone 关键边界：一条正在跑最后一次尝试的工单，
// attempts 已经到顶但租约还在手上。扫描若只看 attempts 就会把它判死，
// 而它可能下一秒就成功了——终态被抢写，执行中的 worker 再写就落空。
func TestSweepLeavesLiveLeaseAlone(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "sweep-live"
	seedMember(t, f, spaceID, "owner-s2", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "v-s2", Role: 0, Status: 1}))
	mustRemoveMember(t, f, spaceID, "v-s2", 1, "owner-s2", MemberRemoveReasonKicked)

	// 租约仍然有效：有人正在跑这一次。
	seedExhaustedJob(t, spaceID, removalCleanupMaxAttempts,
		time.Now().UTC().Add(removalCleanupLease))

	f.sweepExhaustedMemberRemovalCleanups()

	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Equal(t, removalCleanupPending, jobs[0].Status,
		"租约仍在手上说明作业正在执行，扫描不得抢先判死")
}

// TestMemberRemovalCleanupMetricsReflectQueue 队列指标必须真的反映库里的状态。
//
// 这一条和上面两条是一套：认领处卡上限 + 扫描推终态，让失败终于会终结；但终结之后
// 依然没人知道——abandoned 没有任何自动重驱动，被移除的人会一直留在群里。只做前两件
// 等于把「无限重试的无声」换成「终态的无声」。
func TestMemberRemovalCleanupMetricsReflectQueue(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const pendingSpace, deadSpace = "metric-pending", "metric-dead"
	for _, sp := range []string{pendingSpace, deadSpace} {
		seedMember(t, f, sp, "owner-"+sp, 2)
		require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
			SpaceId: sp, UID: "v-" + sp, Role: 0, Status: 1,
		}))
		mustRemoveMember(t, f, sp, "v-"+sp, 1, "owner-"+sp, MemberRemoveReasonKicked)
	}
	// 一条已耗尽、租约过期 → 扫描应把它推成 abandoned；另一条保持 pending。
	seedExhaustedJob(t, deadSpace, removalCleanupMaxAttempts, time.Now().UTC().Add(-time.Hour))
	// 把 pending 那条的 created_at 往前挪，好让「最老待处理年龄」有个可断言的下界。
	_, err = testCtx.DB().Exec(
		"UPDATE space_member_removal_cleanup SET created_at=? WHERE space_id=?",
		time.Now().UTC().Add(-10*time.Minute), pendingSpace)
	require.NoError(t, err)

	f.sweepExhaustedMemberRemovalCleanups()

	stats, err := f.db.queryMemberRemovalCleanupStats()
	require.NoError(t, err)
	assert.Equal(t, int64(1), stats.Pending, "只剩一条待处理")
	assert.Equal(t, int64(1), stats.Abandoned, "耗尽的那条应已被扫成 abandoned")
	assert.GreaterOrEqual(t, stats.OldestPendingAgeSec, int64(600),
		"最老待处理年龄要反映真实积压——它是预算耗尽之前唯一能看见的先行信号")

	// 指标现在是独立的稀疏调度（那条查询是全表聚合），不再挂在扫描那一轮上，
	// 所以这里显式刷一次。
	f.refreshMemberRemovalCleanupMetrics()
	assert.Equal(t, float64(1), promtestutil.ToFloat64(removalCleanupPendingGauge))
	assert.Equal(t, float64(1), promtestutil.ToFloat64(removalCleanupAbandonedGauge))
	assert.GreaterOrEqual(t, promtestutil.ToFloat64(removalCleanupOldestPendingGauge), float64(600))
}

// TestRemoveMembersLockedEnqueuesAtomically 整批移除必须是**一个事务**：在它提交之前，
// 同批里任何一个人的清理工单都不得对外可见。
//
// 这条是 PR #804 round-4/5 的回归守卫。逐个提交时工单会陆续可见，群侧的交接通告抑制
// （HasPendingRemovalCleanup 只看得见已提交的行）就会把尚未入队的兄弟读成「不在队列」，
// 发出写下时就已作废的「已成为新群主」，NoPersist=0 永久留在群历史里。
//
// 用一把竞争行锁把整批**卡在中间**来判定，而不是靠时序猜：另一个连接先锁住 m2 的
// space_member 行，整批的单条 SELECT ... FOR UPDATE 扫到 m2 时必然阻塞。此刻整批还没
// 提交，所以一条清理工单都不该可见。断言它**不可见**即证明整批共用一个事务：若实现
// 退回「一人一事务、逐个提交」，m1 早已提交、它的工单此刻就会露出来。
// （注意：现版本阻塞发生在 SELECT 取锁阶段、UPDATE 循环之前，所以 m1 尚未翻转；
//
//	变异成逐个提交后，m1 会在 m2 之前被翻转+提交，断言随即变红。）
//
// 变异验证：把 removeMembersLocked 改回逐个提交，这条断言立刻变红。
func TestRemoveMembersLockedEnqueuesAtomically(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "rm-batch-atomic"
	seedMember(t, f, spaceID, "owner", 2)
	for _, uid := range []string{"m1", "m2", "m3"} {
		require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
			SpaceId: spaceID, UID: uid, Role: 0, Status: 1,
		}))
	}

	// 另一个连接锁住 m2，让整批停在第二个人身上
	blocker, err := testCtx.DB().Begin()
	require.NoError(t, err)
	var held []int
	_, err = blocker.SelectBySql(
		"SELECT role FROM space_member WHERE space_id=? AND uid=? AND status=1 FOR UPDATE",
		spaceID, "m2").Load(&held)
	require.NoError(t, err)
	require.Len(t, held, 1, "m2 应当存在且活跃，否则这把锁挡不住任何东西")

	var removed []string
	var rmErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		removed, rmErr = f.db.removeMembersLocked(
			spaceID, []string{"m1", "m2", "m3"}, 1, "owner", MemberRemoveReasonKicked)
	}()

	// 等到它真的卡在 space_member 的行锁上，而不是靠 sleep 猜。
	//
	// 必须把等待条件收紧到**本表的 WAITING 锁**：information_schema.innodb_trx 是
	// 服务级、不过滤的，用它只能答「这台 MySQL 上有某个事务在等」，若被别的用例的
	// 争用提前放行，下面的核心断言会**空过**——那时批量可能还没翻转 m1，工单本就为空，
	// 断言 trivially 成立，连逐个提交的变异都拦不住。用 performance_schema.data_locks
	// 精确匹配 space_member 上的 WAITING 记录锁，把守卫的判别力钉在本用例自己制造的
	// 争用上（PR #804 round-5 review P2-1）。
	require.Eventually(t, func() bool {
		var n int
		_, err := testCtx.DB().SelectBySql(
			"SELECT COUNT(*) FROM performance_schema.data_locks " +
				"WHERE object_name='space_member' AND lock_type='RECORD' AND lock_status='WAITING' " +
				"AND lock_data LIKE '%rm-batch-atomic%'").Load(&n)
		return err == nil && n > 0
	}, 15*time.Second, 50*time.Millisecond, "批量移除应当阻塞在 m2 的 space_member 行锁上")

	// 核心断言：m1 已在事务内被翻转，但整批尚未提交，所以一条工单都不该可见
	assert.Empty(t, cleanupJobs(t, spaceID),
		"整批提交之前不得有任何工单可见——逐个提交会让 m1 的工单此刻就露出来")

	require.NoError(t, blocker.Rollback())
	<-done
	require.NoError(t, rmErr)

	assert.Equal(t, []string{"m1", "m2", "m3"}, removed)
	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 3, "提交后三条工单一次性全部可见")
	for _, j := range jobs {
		assert.Equal(t, MemberRemoveReasonKicked, j.Reason)
		assert.Equal(t, removalCleanupPending, j.Status)
	}
}

// TestRemoveMembersLockedSkipsOwnerAndPeers 整批版本必须保留 removeMemberLocked 的
// 角色语义：owner、同级及更高角色、以及根本不在空间里的 uid，都**静默跳过**而不是
// 让整批失败，也不得为他们留下工单。
func TestRemoveMembersLockedSkipsOwnerAndPeers(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "rm-batch-roles"
	seedMember(t, f, spaceID, "owner", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID, UID: "peer-admin", Role: 1, Status: 1,
	}))
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID, UID: "normal", Role: 0, Status: 1,
	}))

	// 操作者是 admin(role=1)：owner 不可动、同级 admin 不可动、ghost 不存在，
	// 只有 normal 该被移除。
	removed, err := f.db.removeMembersLocked(
		spaceID, []string{"owner", "peer-admin", "ghost", "normal"}, 1, "peer-admin", MemberRemoveReasonKicked)
	require.NoError(t, err, "不该动的人只是跳过，不能让整批报错")
	assert.Equal(t, []string{"normal"}, removed)

	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1, "只有真正被移除的人才入队")
	assert.Equal(t, "normal", jobs[0].UID)
}

// TestRemoveMembersLockedRollsBackWholeBatchOnError 中途出错整批回滚：成员行与工单
// 同生共死这一点，从「每人一组」升级成「整批一组」。
func TestRemoveMembersLockedRollsBackWholeBatchOnError(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "rm-batch-rollback"
	seedMember(t, f, spaceID, "owner", 2)
	for _, uid := range []string{"r1", "r2"} {
		require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
			SpaceId: spaceID, UID: uid, Role: 0, Status: 1,
		}))
	}

	// 用一个非法 reason 让入队阶段失败——那一步发生在所有成员行都已翻转之后。
	_, err = f.db.removeMembersLocked(spaceID, []string{"r1", "r2"}, 1, "owner", "not-a-real-reason")
	require.Error(t, err, "非法 reason 应当让整批失败")

	assert.Empty(t, cleanupJobs(t, spaceID), "失败后不得留下任何工单")
	for _, uid := range []string{"r1", "r2"} {
		m, qerr := f.db.queryMember(spaceID, uid)
		require.NoError(t, qerr)
		require.NotNil(t, m, "成员行必须还在")
		assert.EqualValues(t, 1, m.Status, "整批回滚后 %s 必须仍是活跃成员", uid)
	}
}

// TestRemoveMembersLockedNoDeadlockOnReversedOverlap 并发反序重叠批次不产生死锁。
//
// 这是一个**并发冒烟**测试，不是本修复的主守卫。它反复并发地对同一空间跑两批反序
// 重叠的移除，断言 0 死锁、0 意外副作用。全部成员 role=1、reject=1，于是不翻转任何
// 行、可无需重播种反复施压，纯粹压加锁路径。
//
// 真正把「加锁顺序确定」钉死的是 TestRemoveMembersLockedForcesUniqueIndexPlan：
// 它断言语句走 (space_id, uid) 而不是 (space_id, status)。为什么需要那条而不能只靠
// 本测试：批次间即便都走 (space_id, status) 也是同序、不会互相成环，所以本测试对
// 「优化器选错索引」这个真正的隐患**不敏感**——真正的跨路径死锁是批次 vs 群主转让
// （不同索引→不同序），计划断言才守得住（PR #804 round-7 review）。
func TestRemoveMembersLockedNoDeadlockOnReversedOverlap(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "rm-deadlock"
	seedMember(t, f, spaceID, "owner", 2)
	members := []string{"d1", "d2", "d3", "d4", "d5", "d6"}
	for _, uid := range members {
		require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
			SpaceId: spaceID, UID: uid, Role: 1, Status: 1, // admin：reject=1 时全部跳过，不翻转
		}))
	}

	forward := append([]string(nil), members...)
	reversed := make([]string, len(members))
	for i, uid := range members {
		reversed[len(members)-1-i] = uid
	}

	const rounds = 40
	var deadlocks, otherErr, unexpectedRemoved atomic.Int64
	isDeadlock := func(e error) bool {
		return e != nil && (strings.Contains(e.Error(), "1213") ||
			strings.Contains(strings.ToLower(e.Error()), "deadlock"))
	}

	var wg sync.WaitGroup
	for w, order := range [][]string{forward, reversed} {
		wg.Add(1)
		go func(order []string) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				// ⚠️ 必须调 removeMembersLockedOnce（单次尝试），不能调外层
				// removeMembersLocked——后者裹了 retryOnDeadlock，会把 1213 吞掉重试，
				// 两个 goroutine × 40 轮的规模下几乎必然重试成功，下面那条
				// `deadlocks == 0` 断言就再也红不了了（PR #804 round-9 review
				// yujiawei P2-2；正是本 PR 自己加的 learnings/pending/
				// mutation-check-the-assertion-not-the-guard.md 说的那种失效方式：
				// 别处的改动悄悄让既有守卫变得无法变红）。
				removed, e := removeMembersLockedOnce(f.db.session, spaceID, order, 1, "owner", MemberRemoveReasonKicked)
				if isDeadlock(e) {
					deadlocks.Add(1)
				} else if e != nil {
					otherErr.Add(1)
				}
				// 不能在测试 goroutine 之外调 require（t.FailNow→runtime.Goexit 只能在
				// 测试自身 goroutine 上跑，否则静默跳过剩余轮次、失败从错误的地方冒出来）。
				// 用原子标志记下，回到主 goroutine 再断言。
				if len(removed) != 0 {
					unexpectedRemoved.Add(1)
				}
			}
		}(order)
		_ = w
	}
	wg.Wait()

	assert.EqualValues(t, 0, deadlocks.Load(),
		"反序重叠批次不得死锁——单语句取锁按索引序，两个批次顺序一致只会互相阻塞")
	assert.EqualValues(t, 0, otherErr.Load(), "除死锁外不应有其它错误")
	assert.EqualValues(t, 0, unexpectedRemoved.Load(), "全是 admin、reject=1，不该移除任何人")

	// 收尾核实：全程无人被移除，成员行原样都在
	for _, uid := range members {
		m, qerr := f.db.queryMember(spaceID, uid)
		require.NoError(t, qerr)
		require.NotNil(t, m)
		assert.EqualValues(t, 1, m.Status, "%s 应当仍是活跃成员", uid)
	}
}

// TestUpsertMembersLocksInSameOrderAsBatchRemoval 守住批量加人 vs 批量移除这一对。
//
// 背景（PR #804 round-9 review，Jerry-Xin 实测定位）：upsertMembers 逐条 upsert，此前
// 按**调用方给的 uid 顺序**取锁（normalizeUIDs 不排序），而当时的批量移除走 FORCE INDEX 按
// **索引升序**取锁——两边顺序相反即构成 AB-BA。这一对在 main 上不可能死锁：那时用户端
// 移除是一人一事务、只握一把锁，做不了 hold-and-wait 的那一侧；是本 PR 的整批单事务
// （最多 200 行）把它变成了可能。
//
// ⚠️ 为什么光包 retryOnDeadlock 不够——这是本轮最值钱的一条：
// 有界重试只能救**短命**的对手（群主转让、强制移除，实测已归零），碰上**长命**的对手会被
// **饿死**：批量移除每次尝试都是全新事务、回滚代价为零，InnoDB 反复选它当牺牲者，而还在
// 跑的 upsert 一直在推进；重试立刻又撞上同一个活事务，几次退避跑完仍在人家生命周期内。
// 实测（200 uid 逆序重叠、60 轮）：只包重试时移除侧 **60/60** 把 1213 抛给了调用方。
// 排序之后两边同序、结构上不成环，实测 0/60（含**两侧都不包重试**的对照，证明是真的没
// 死锁，而不是被重试吸收掉了）。
//
// 本测试**刻意调 …Once**（两侧都不包重试）：包一层重试就会把要数的 1213 吞掉，守卫就再
// 也红不了了——正是本 PR 自己 round-8 犯过的错，见 learnings/pending/
// mutation-check-the-assertion-not-the-guard.md。
// 变异验证：删掉 upsertMembersOnce 里的 sort.Strings，本测试立刻变红。
func TestUpsertMembersLocksInSameOrderAsBatchRemoval(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)
	mgr := newManagerDB(testCtx.DB())

	const spaceID = "upsert-order"
	seedMember(t, f, spaceID, "owner", 2)

	// 字母表刻意选成「字节序与 utf8mb4_general_ci 序在相邻元素上翻转」的一组。
	//
	// 之前这里是 `m%04d` —— 数字加一个小写字母，正好是两种顺序**重合**的那一个
	// 字母表，于是这条 guard 对「Go 排序 ≠ 索引顺序」这个失败模式完全不敏感
	// （PR #804 round-10 review P2-1）。
	//
	// 现在每一对都翻转，靠的是 '_'(0x5F) 落在大写区(0x41-0x5A)之后、小写区
	// (0x61-0x7A)之前，而 general_ci 把小写折叠进大写：
	//   - `u_%03d` vs `ua%03d` —— 字节序 '_' < 'a'；general_ci 下 'a'→'A'(0x41) < '_'。
	//   - `A_%03d` vs `Ab%03d` —— 同样的翻转，且把大写字母带进字母表。
	//
	// 注意不能用「同名只差大小写」的一对（`M001`/`m001`）来引入大写：唯一索引
	// (space_id, uid) 在 general_ci 下视两者为同一个键，插第二行会直接 1062。
	//
	// 带下划线的标识符在本系统里是现成的（`app_…_bot`、`iwh_…` 都是 Space 成员），
	// 而批量加人对调用方给的 uid 不做字符集校验。
	const n = 200
	asc := make([]string, 0, n)
	for i := 0; i < n/4; i++ {
		asc = append(asc,
			fmt.Sprintf("u_%03d", i),
			fmt.Sprintf("ua%03d", i),
			fmt.Sprintf("A_%03d", i),
			fmt.Sprintf("Ab%03d", i),
		)
	}
	require.Len(t, asc, n)
	for _, uid := range asc {
		require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
			SpaceId: spaceID, UID: uid, Role: 0, Status: 1,
		}))
	}
	// 逆序：加人侧按调用方顺序取锁，与移除侧的索引升序完全相反——最坏的一对。
	desc := make([]string, n)
	for i, uid := range asc {
		desc[n-1-i] = uid
	}
	if _, e := testCtx.DB().Exec("ANALYZE TABLE space_member"); e != nil {
		require.NoError(t, e)
	}

	isDeadlock := func(e error) bool {
		return e != nil && (strings.Contains(e.Error(), "1213") ||
			strings.Contains(strings.ToLower(e.Error()), "deadlock"))
	}

	const rounds = 30
	var removeDL, upsertDL, otherErr atomic.Int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			// 移除侧：reject=1，role=0 的成员会真的被翻成 status=0 并持锁到提交。
			_, e := removeMembersLockedOnce(f.db.session, spaceID, asc, 1, "owner", MemberRemoveReasonKicked)
			if isDeadlock(e) {
				removeDL.Add(1)
			} else if e != nil {
				otherErr.Add(1)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			// 加人侧把他们重新激活，于是两个 goroutine 能一直互相制造重叠。
			e := mgr.upsertMembersOnce(spaceID, desc)
			if isDeadlock(e) {
				upsertDL.Add(1)
			} else if e != nil {
				otherErr.Add(1)
			}
		}
	}()
	wg.Wait()

	assert.EqualValues(t, 0, removeDL.Load(),
		"批量移除不得与批量加人死锁——两侧必须过同一个 sortForLockOrder")
	assert.EqualValues(t, 0, upsertDL.Load(), "加人侧同样不得死锁")
	assert.EqualValues(t, 0, otherErr.Load(), "除死锁外不应有其它错误")
}

// TestBatchRemovalUsesUniqueIndexWithoutForcing 断言整批锁定查询在**不加 FORCE INDEX**
// 的情况下也必然走唯一索引 (space_id, uid)。
//
// 这条取代了此前的 TestRemoveMembersLockedForcesUniqueIndexPlan。背景：round 7 那条
// 语句是 `uid IN (...)`，是范围查询，优化器按代价选索引，在真实规模（250 人空间、
// 200 uid 批）上会选 (space_id, status) 去锁全空间的活跃行——所以当时必须 FORCE。
//
// 现在每个 uid 是**独立分支的单行等值**查询（见
// buildSelectMembersForRemovalForUpdateSQL），(space_id, uid) 是唯一键上的完整等值
// 匹配，优化器不可能选一个更差的计划。于是：
//   - FORCE INDEX 不再需要，对索引名的运行时硬依赖（缺失即 1176、且不可重试）随之消失；
//   - 锁面天然就是目标行本身，不依赖代价估算。
//
// 这条测试就是把「不 FORCE 也走唯一索引」这个前提钉住：哪天有人改回范围查询、或者
// 索引形状变了，它会红。
func TestBatchRemovalUsesUniqueIndexWithoutForcing(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "rm-plan"
	seedMember(t, f, spaceID, "owner", 2)
	// 250 名成员：正是 round 7 实测中优化器会倾向 (space_id, status) 的规模。
	for i := 0; i < 250; i++ {
		require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
			SpaceId: spaceID, UID: fmt.Sprintf("m%04d", i), Role: 0, Status: 1,
		}))
	}
	if _, err := testCtx.DB().Exec("ANALYZE TABLE space_member"); err != nil {
		require.NoError(t, err)
	}

	// 生产语句里绝不能再出现 FORCE INDEX —— 它是这次要移除的那个硬依赖。
	require.NotContains(t, buildSelectMembersForRemovalForUpdateSQL(3), "FORCE INDEX",
		"单行等值查询不需要 FORCE，留着它等于把索引名变成运行时硬依赖")

	// 用生产语句本身 EXPLAIN，只把占位符换成字面量。取一个分支即可：每个分支形状相同。
	oneBranch := buildSelectMembersForRemovalForUpdateSQL(1)
	concrete := strings.Replace(oneBranch, "space_id=?", "space_id='"+spaceID+"'", 1)
	concrete = strings.Replace(concrete, "uid=?", "uid='m0100'", 1)

	var js string
	require.NoError(t, testCtx.DB().SelectBySql("EXPLAIN FORMAT=JSON "+concrete).LoadOne(&js))
	assert.Contains(t, js, `"key": "spacemember_spaceid_uid"`,
		"单行等值必须走唯一索引 (space_id, uid)；走别的就说明索引形状变了")
}

// TestRetryOnDeadlockRecoversFrom1213 是跨路径死锁修复的**主守卫**（PR #804 round-8）。
//
// space_member 上批量移除 vs 群主转让/强制移除的加锁顺序互不相同、且无法从单个调用点
// 统一（转让是两段式非单调），所以 FORCE INDEX 收窄锁面后仍会跨路径 AB-BA 死锁。三条
// 写路径都套了 retryOnDeadlock 兜底。真实死锁竞态在本机难以稳定复现，所以这里**注入**
// 一个 InnoDB 死锁(1213)直接钉住重试逻辑本身：
//   - 前 N 次返回 1213、之后成功 → 整体成功，且确实重跑了 N+1 次；
//   - 一直 1213 → 有界次数后放弃并包装报错，不会无限重试；
//   - 1205 锁等待超时 → **不重试**、原样透传（理由见 isDeadlockErr）；
//   - 领域错误（非 1213）→ 第一次就原样透传，errors.Is 仍成立。
func TestRetryOnDeadlockRecoversFrom1213(t *testing.T) {
	deadlock := &mysqldrv.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock; try restarting transaction"}
	lockWait := &mysqldrv.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}

	t.Run("前几次死锁后成功", func(t *testing.T) {
		calls := 0
		err := retryOnDeadlock(func() error {
			calls++
			if calls < 3 {
				return deadlock
			}
			return nil
		})
		require.NoError(t, err, "死锁回滚后重跑应当最终成功")
		assert.Equal(t, 3, calls, "应当正好重试到成功那一次")
	})

	// 1205（锁等待超时）**不重试**，与 1213 相反。等满 innodb_lock_wait_timeout
	// （默认 50s）才返回的错误，重试既贵又最不可能有用；这几条写路径挂在用户 HTTP
	// 请求上，重试 3 次最坏要占着 handler ~150s。见 isDeadlockErr 的注释
	// （PR #804 round-9 review：yujiawei P2-1 / mochashanyao P2-3）。
	t.Run("锁等待超时不重试而是原样透传", func(t *testing.T) {
		calls := 0
		err := retryOnDeadlock(func() error { calls++; return lockWait })
		assert.Equal(t, 1, calls, "1205 不得重试——重试满 3 次会把请求拖到 ~150s")
		assert.ErrorIs(t, err, lockWait, "1205 必须原样透传，与合入前行为一致")
	})

	t.Run("持续死锁则有界放弃", func(t *testing.T) {
		calls := 0
		err := retryOnDeadlock(func() error { calls++; return deadlock })
		require.Error(t, err, "重试用尽必须报错，不能无限重试")
		assert.Equal(t, spaceMutatorMaxAttempts, calls, "严格重试 spaceMutatorMaxAttempts 次")
		assert.ErrorIs(t, err, deadlock, "包装后原始死锁错误仍可被 errors.Is 取到")
	})

	t.Run("领域错误第一次就原样透传", func(t *testing.T) {
		calls := 0
		err := retryOnDeadlock(func() error { calls++; return ErrCannotRemoveOwner })
		assert.Equal(t, 1, calls, "非死锁错误不得重试")
		assert.ErrorIs(t, err, ErrCannotRemoveOwner, "领域错误必须原样透传，否则调用方的 errors.Is 失效")
	})

	t.Run("一次成功不重试", func(t *testing.T) {
		calls := 0
		err := retryOnDeadlock(func() error { calls++; return nil })
		require.NoError(t, err)
		assert.Equal(t, 1, calls)
	})
}

// TestRemoveMembersLockedHonoursStoredUIDSpelling 大小写变体的 uid 必须照常被移除。
//
// space_member.uid 没有显式 COLLATE，继承库默认 utf8mb4_general_ci —— 大小写不敏感。
// 批量锁定语句的 `uid IN ?` 因此会匹中变体，并把行按**库里存的拼写**返回。若把角色
// 判定的查表放到 Go 里、又用请求里的拼写当 key，两者不一致时就查不中：成员被当成
// 「不在空间」静默跳过，不翻 status、不入清理工单、不失效鉴权缓存，而 removeMembers
// 照样 c.ResponseOK()。在一个专门用来收回访问权的端点上，「报成功、什么也没做」是
// 最坏的失败形状。
//
// 老的逐个路径两条语句都在 SQL 里比（SELECT ... WHERE uid=? / UPDATE ... WHERE uid=?），
// 没有这个落差，所以那会是**回归**而不是既有行为（PR #804 round-10 review P0-1）。
func TestRemoveMembersLockedHonoursStoredUIDSpelling(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "rm-batch-collation"
	// 只差大小写的两种拼写：stored 是库里的，requested 是调用方给的。
	const stored, requested = "AbCdEf01", "abcdef01"

	seedMember(t, f, spaceID, "owner", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID, UID: stored, Role: 0, Status: 1,
	}))

	// 前置条件：确认这个库的 uid 列真的是大小写不敏感的。否则本测试会因为
	// 「SQL 也没匹中」而假绿，测的就不是它自称在测的东西了。
	precheck, err := f.db.queryMember(spaceID, requested)
	require.NoError(t, err)
	require.NotNil(t, precheck,
		"本测试要求 uid 列是大小写不敏感的 collation（CI 用 utf8mb4_general_ci）；"+
			"查不到说明测试库建错了，不是被测代码的问题")

	removed, err := f.db.removeMembersLocked(
		spaceID, []string{requested}, 1, "owner", MemberRemoveReasonKicked)
	require.NoError(t, err)
	require.Equal(t, []string{stored}, removed,
		"必须真的移除，且返回库里的拼写——清理工单和鉴权缓存 key 都要用它")

	// 直查而不走 queryMember：后者带 status=1 过滤，移除成功时本来就查不到。
	assert.EqualValues(t, 0, memberStatus(t, spaceID, stored), "成员行必须真的翻成 status=0")

	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1, "必须入队清理工单，否则人还留在这个 Space 的每个群里")
	assert.Equal(t, stored, jobs[0].UID, "工单里的 uid 必须是库里的拼写")
}

// TestRemoveMembersLockedDedupesCaseVariants 同一批里塞两个互为大小写变体的 uid，
// 只该产生一次移除、一条工单。
//
// 去重来源是唯一索引 (space_id, uid) —— 锁定查询对同一个成员至多返回一行。此前那个
// 基于字节相等的 seen 集合做不到这一点：它把两种拼写看成两个人，于是写出两条指向
// 同一个人的清理工单，worker 会把整条级联跑两遍（重复的退群 Tip、重复的 IM 退订）。
func TestRemoveMembersLockedDedupesCaseVariants(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "rm-batch-collation-dup"
	const stored = "AbCdEf02"

	seedMember(t, f, spaceID, "owner", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID, UID: stored, Role: 0, Status: 1,
	}))

	// 刻意**不**传库里那个拼写：两个都是变体，去重与匹中必须同时成立才会绿。
	// 若把 stored 也塞进来，即便变体一个都没匹中，本用例也会假绿。
	removed, err := f.db.removeMembersLocked(
		spaceID, []string{"abcdef02", "ABCDEF02"}, 1, "owner", MemberRemoveReasonKicked)
	require.NoError(t, err)
	assert.Equal(t, []string{stored}, removed, "同一个人只该被移除一次，且返回库里的拼写")
	require.Len(t, cleanupJobs(t, spaceID), 1, "同一个人只该有一条清理工单")
}

// memberStatus 直查成员行的 status。db.queryMember 带 `status=1` 过滤，验证
// 「有没有被移除」时用不了它 —— 移除成功和行不存在都返回 nil，两者分不开。
func memberStatus(t *testing.T, spaceID, uid string) int {
	t.Helper()
	var rows []struct {
		Status int `db:"status"`
	}
	_, err := testCtx.DB().SelectBySql(
		"SELECT status FROM space_member WHERE space_id=? AND uid=?", spaceID, uid).Load(&rows)
	require.NoError(t, err)
	require.Len(t, rows, 1, "space_member 里应当恰好有一行 %s/%s", spaceID, uid)
	return rows[0].Status
}

// TestBatchRemovalLocksInBranchOrder 是本轮的**主守卫**：证明整批锁定查询的加锁
// 顺序由 UNION ALL 的分支顺序决定，**与列的 collation 无关**。
//
// 为什么这条必须存在，而且必须跑两种 collation：
//
// space_member 建表时没写 COLLATE，继承库默认。CI 显式建库为 utf8mb4_general_ci
// （ci.yml），而 MySQL 8.0 的默认是 utf8mb4_0900_ai_ci，生产库即为后者。两者对 '_'
// 的排序**相反**。此前的实现用一条 `uid IN (...) FOR UPDATE`，加锁顺序 = 索引物理
// 顺序 = 列的 collation 序，于是 upsert 侧必须在 Go 里复现 collation —— 而任何这种
// 复现都只能对一种环境成立。round 10 复刻了 general_ci（CI 那种），在生产上反而把
// 顺序排反了，且因为 CI 只跑 general_ci，测试全绿。
//
// 所以这条测试自己建表、自己指定 collation，不依赖 CI 建库时用的那一种。只跑一种
// 就等于重犯上一轮的错。
//
// 判定方法：让一个会话先锁住分支 2 的目标行，再跑本语句。若按分支顺序取锁，它应当
// **恰好持有分支 1 的行锁、且未触及分支 3**；若按索引序取锁，持有的会是另一组。
// 每种 collation 的分支顺序都刻意与该 collation 的索引序**相反** —— 否则两种假设
// 给出同样结果，测不出任何东西（这正是 round 10 那个 m%04d 字母表的毛病）。
func TestBatchRemovalLocksInBranchOrder(t *testing.T) {
	_, _, err := setup(t)
	require.NoError(t, err)

	cases := []struct {
		collation string
		// order 的顺序刻意与该 collation 的索引序相反
		order []string
	}{
		// 0900_ai_ci 索引序: A_000 a_b aab Ab000 u_000 ua000
		{"utf8mb4_0900_ai_ci", []string{"u_000", "aab", "A_000"}},
		// general_ci 索引序: aab Ab000 A_000 a_b ua000 u_000
		{"utf8mb4_general_ci", []string{"a_b", "aab", "u_000"}},
	}
	all := []string{"a_b", "aab", "u_000", "ua000", "A_000", "Ab000"}

	for _, tc := range cases {
		t.Run(tc.collation, func(t *testing.T) {
			table := "sm_lockorder"
			db := testCtx.DB()
			_, _ = db.Exec("DROP TABLE IF EXISTS " + table)
			t.Cleanup(func() { _, _ = db.Exec("DROP TABLE IF EXISTS " + table) })
			_, err := db.Exec(fmt.Sprintf(`CREATE TABLE %s (
				id int NOT NULL AUTO_INCREMENT,
				space_id varchar(40) COLLATE %s NOT NULL DEFAULT '',
				uid varchar(40) COLLATE %s NOT NULL DEFAULT '',
				role smallint NOT NULL DEFAULT 0,
				status smallint NOT NULL DEFAULT 1,
				PRIMARY KEY (id),
				UNIQUE KEY spacemember_spaceid_uid (space_id, uid)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=%s`, table, tc.collation, tc.collation, tc.collation))
			require.NoError(t, err)
			for _, u := range all {
				_, err := db.Exec("INSERT INTO "+table+" (space_id, uid) VALUES ('s', ?)", u)
				require.NoError(t, err)
			}

			// 前置：确认这组数据在本 collation 下，分支顺序确实与索引序不同。
			// 否则本用例无判别力，会假绿。
			var idxOrder []string
			_, err = db.SelectBySql("SELECT uid FROM " + table + " WHERE space_id='s'").Load(&idxOrder)
			require.NoError(t, err)
			pos := func(u string) int {
				for i, v := range idxOrder {
					if v == u {
						return i
					}
				}
				return -1
			}
			require.Greater(t, pos(tc.order[0]), pos(tc.order[1]),
				"分支顺序必须与索引序相反，否则两种假设给出同样结果（索引序=%v）", idxOrder)

			// blocker：锁住分支 2 的目标行，事务不提交
			blockerTx, err := db.Begin()
			require.NoError(t, err)
			defer blockerTx.RollbackUnlessCommitted()
			var got string
			_, err = blockerTx.SelectBySql(
				"SELECT uid FROM "+table+" WHERE space_id='s' AND uid=? FOR UPDATE", tc.order[1]).Load(&got)
			require.NoError(t, err)

			// probe：按 tc.order 分支顺序取锁，预期卡在分支 2
			probeTx, err := db.Begin()
			require.NoError(t, err)
			defer probeTx.RollbackUnlessCommitted()
			var probeConnID uint64
			require.NoError(t, probeTx.SelectBySql("SELECT CONNECTION_ID()").LoadOne(&probeConnID))

			stmt := strings.ReplaceAll(buildSelectMembersForRemovalForUpdateSQL(len(tc.order)), "space_member", table)
			args := make([]interface{}, 0, len(tc.order)*2)
			for _, u := range tc.order {
				args = append(args, "s", u)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				var rows []spaceMemberRoleRow
				_, _ = probeTx.SelectBySql(stmt, args...).Load(&rows)
			}()

			// 等 probe 跑到阻塞点
			time.Sleep(1500 * time.Millisecond)

			var held []string
			_, err = db.SelectBySql(`
				SELECT dl.LOCK_DATA FROM performance_schema.data_locks dl
				JOIN performance_schema.threads th ON th.THREAD_ID = dl.THREAD_ID
				WHERE th.PROCESSLIST_ID = ? AND dl.LOCK_STATUS='GRANTED'
				  AND dl.LOCK_TYPE='RECORD' AND dl.INDEX_NAME='spacemember_spaceid_uid'`,
				probeConnID).Load(&held)
			require.NoError(t, err)
			heldStr := strings.Join(held, " ")

			assert.Contains(t, heldStr, tc.order[0],
				"应当已持有分支 1 (%s) 的行锁——说明按分支顺序取锁；索引序=%v 持有=%v",
				tc.order[0], idxOrder, held)
			assert.NotContains(t, heldStr, tc.order[2],
				"不应触及分支 3 (%s)——它排在阻塞点之后", tc.order[2])

			require.NoError(t, blockerTx.Rollback())
			<-done
		})
	}
}

// TestSortForLockOrderIsSpellingInvariant 钉住取锁排序的**真正**不变量：两个调用方
// 即便用不同的拼写指代同一批行，也必须排出同一个取锁顺序。
//
// 上一版这里用的是 sort.Strings，注释写着「序本身是什么不重要，用最朴素的字节序
// 正是因为它不需要任何前提」。那句话是假的，而且是这个 PR 反复犯的同一类错误
// （PR #804 round-11 review P1-1）。
//
// 前提在这里：两边必须排**同一组字符串**。它们收到的却不一定是——`space_member.uid`
// 没有显式 COLLATE，继承的库默认（CI general_ci / 生产 0900_ai_ci）**都是大小写
// 不敏感**的，所以 `ABC` 与 `abc` 是同一行；而两个入口都不折叠大小写
// （normalizeUIDs 按字节去重，用户端 removeMembers 连它都不调）。于是：
//
//	batch-add    收到 ["ABC", "abd"] → 字节序 → 锁 abc、再锁 abd
//	batch-remove 收到 ["abc", "ABD"] → 字节序 → 锁 abd、再锁 abc
//
// 同两行、相反顺序 —— 正是那句注释宣称不可能的 AB-BA。而这一对恰恰是 round 9b 实测
// 中 retryOnDeadlock **饿死**而非吸收的组合（60/60 未恢复），所以退化形态不是「重试
// 一次就好」，而是一个访问撤销端点上持续的 500。
//
// 唯一索引把大小写变体折叠成同一个键，所以并发用例
// TestUpsertMembersLocksInSameOrderAsBatchRemoval 结构上覆盖不到这个场景（插第二行
// 直接 1062）。这条只测排序函数本身，正因为那里测不了。
func TestSortForLockOrderIsSpellingInvariant(t *testing.T) {
	// 同一批行（abc / abd），两个调用方各用一种拼写
	addSide := sortForLockOrder([]string{"ABC", "abd"})
	removeSide := sortForLockOrder([]string{"abc", "ABD"})

	fold := func(uids []string) []string {
		out := make([]string, 0, len(uids))
		for _, u := range uids {
			out = append(out, strings.ToLower(u))
		}
		return out
	}
	assert.Equal(t, fold(addSide), fold(removeSide),
		"两个调用方指代同一批行时必须排出同一个取锁顺序；不同拼写排出不同顺序即 AB-BA")

	// 不改调用方切片
	orig := []string{"b", "a"}
	_ = sortForLockOrder(orig)
	assert.Equal(t, []string{"b", "a"}, orig, "必须排副本，不得改动调用方切片")

	// 折叠后相等时用原始字节序兜底，保证是**全序**：sort.Slice 不稳定，
	// 同一组输入必须排出同一个结果。
	got := sortForLockOrder([]string{"AbC", "abc", "ABC"})
	assert.Equal(t, []string{"ABC", "AbC", "abc"}, got,
		"折叠后同键时按原始字节序，结果必须确定")
}
