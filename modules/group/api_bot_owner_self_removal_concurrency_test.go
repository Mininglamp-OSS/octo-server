package group

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本文件钉住自助移除在**并发 / 多副本**下的两条性质。合并前的确认发现：
// 这两条此前都没有覆盖，而且其中一条在测试库里长期是坏的。
//
// 背景：级联移除走的是
//
//	SELECT gm.uid FROM group_member gm
//	  INNER JOIN robot r ON r.robot_id = gm.uid AND r.status = 1
//	 WHERE gm.group_no = ? AND gm.robot = 1 AND gm.is_deleted = 0
//	   AND r.creator_uid = ? FOR UPDATE
//
// 有 idx_robot_creator_uid 时，MySQL 从 robot 侧 ref 进入（只扫该主人名下的 bot），
// 再按 group_no_uid 唯一索引 eq_ref 回查，锁面只覆盖「真正要改的那几行」。
// 没有该索引时退化成 robot 全表扫描，而 FOR UPDATE 会把**整张 robot 表**加 X 锁——
// 跨群、跨副本地串行化所有 bot 操作，并稳定产生死锁。
//
// 实测对比（40 轮 × 3 并发）：无索引 120 次操作里 8 次撞 MySQL 死锁 1213；
// 补齐索引后 360 次操作 0 死锁。

// TestBotOwnerSelfRemoval_RobotIndexesMatchProduction 直接钉住上面那个前提。
//
// 为什么值得单独一条：api_test.go 里建索引的语句原本写成
// `CREATE UNIQUE INDEX IF NOT EXISTS ...`，而 MySQL 8.0 不支持 CREATE INDEX 的
// IF NOT EXISTS（那是 MariaDB/Postgres 的写法），于是它每次都以 1064 语法错误失败，
// 且 db.Exec 的 error 没人检查——索引就这样静默地从来没建成过。
// 这条断言让同样的静默失败下次直接红，而不是变成一个「偶尔死锁」的线上谜题。
func TestBotOwnerSelfRemoval_RobotIndexesMatchProduction(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	for _, idx := range []string{"robot_id_robot_index", "idx_robot_creator_uid"} {
		var n int64
		_, err := f.ctx.DB().SelectBySql(
			"SELECT COUNT(*) FROM information_schema.STATISTICS "+
				"WHERE table_schema = DATABASE() AND table_name = 'robot' AND index_name = ?",
			idx,
		).Load(&n)
		require.NoError(t, err)
		assert.NotZero(t, n,
			"robot.%s 缺失：级联的 FOR UPDATE 查询会退化成 robot 全表加锁（线上由 "+
				"modules/robot 迁移建立，测试库由 api_test.go 的 TestMain 补齐）", idx)
	}
}

// TestBotOwnerSelfRemoval_ConcurrentWithOwnerKick 覆盖最容易出问题的交错：
// 所有者正在自助移除自己的 bot，同一时刻群主把这位所有者踢了 —— 踢人的级联
// 指向同一批行。再叠一个重复的自助移除，制造「同一目标被并发处理两次」。
//
// 断言两件事：
//  1. 不产生死锁 / 锁等待超时（锁面足够窄、加锁顺序一致）；
//  2. 无论哪条路径先赢，最终状态一致 —— 主人和 bot 都不在群里，且没有重复成员行。
//
// 注意：级联失败时 service 返回的是 errors.New("failed to cascade-remove invited
// bots")，**丢掉了底层原因**（死锁 1213 也会被吞成这句）。所以这里不能只看
// error 字符串，必须把它单独归一类并让测试红——否则死锁会伪装成普通失败。
func TestBotOwnerSelfRemoval_ConcurrentWithOwnerKick(t *testing.T) {
	svc, userDB := setupServiceTest(t)
	s := svc.(*Service)
	insertTestUsers(t, userDB, testutil.UID)

	const rounds = 20
	var deadlocks, lockwaits, cascadeFailures, okCnt int64

	for r := 0; r < rounds; r++ {
		owner := fmt.Sprintf("cc_own_%03d", r)
		bot := fmt.Sprintf("cc_bot_%03d", r)

		insertTestUsers(t, userDB, owner)
		resp, err := svc.CreateGroup(&CreateGroupServiceReq{
			Creator: testutil.UID, Members: []string{owner},
			Name: fmt.Sprintf("cc_%03d", r),
		})
		require.NoError(t, err)
		seedBotMember(t, s, resp.GroupNo, bot, bot, owner)

		classify := func(rerr error) {
			if rerr == nil {
				atomic.AddInt64(&okCnt, 1)
				return
			}
			msg := rerr.Error()
			switch {
			case strings.Contains(msg, "Deadlock") || strings.Contains(msg, "1213"):
				atomic.AddInt64(&deadlocks, 1)
			case strings.Contains(msg, "Lock wait timeout") || strings.Contains(msg, "1205"):
				atomic.AddInt64(&lockwaits, 1)
			case strings.Contains(msg, "cascade-remove"):
				// service 把底层错误吞成了这句：死锁最可能藏在这里。
				atomic.AddInt64(&cascadeFailures, 1)
			}
		}

		var wg sync.WaitGroup
		run := func(req *RemoveGroupMembersServiceReq) {
			defer wg.Done()
			_, rerr := svc.RemoveGroupMembers(req)
			classify(rerr)
		}
		wg.Add(3)
		go run(&RemoveGroupMembersServiceReq{
			GroupNo: resp.GroupNo, Members: []string{bot},
			OperatorUID: owner, OperatorName: owner, BotOwnerSelfRemoval: true,
		})
		go run(&RemoveGroupMembersServiceReq{
			GroupNo: resp.GroupNo, Members: []string{owner},
			OperatorUID: testutil.UID, OperatorName: "creator",
		})
		go run(&RemoveGroupMembersServiceReq{
			GroupNo: resp.GroupNo, Members: []string{bot},
			OperatorUID: owner, OperatorName: owner, BotOwnerSelfRemoval: true,
		})
		wg.Wait()

		for _, uid := range []string{owner, bot} {
			exist, eerr := s.db.ExistMember(uid, resp.GroupNo)
			require.NoError(t, eerr)
			assert.False(t, exist, "round %d: %s 无论哪条路径先赢都应已离群", r, uid)
		}
		var dup int64
		_, err = s.ctx.DB().SelectBySql(
			"SELECT COUNT(*) FROM (SELECT uid FROM group_member "+
				"WHERE group_no=? GROUP BY uid HAVING COUNT(*)>1) x",
			resp.GroupNo).Load(&dup)
		require.NoError(t, err)
		assert.Zero(t, dup, "round %d: 并发不应产生重复成员行", r)
	}

	t.Logf("ops=%d ok=%d deadlock=%d lockwait=%d cascadeFail=%d",
		rounds*3, okCnt, deadlocks, lockwaits, cascadeFailures)
	assert.Zero(t, deadlocks, "并发自助移除 / 踢人不应产生死锁")
	assert.Zero(t, lockwaits, "并发自助移除 / 踢人不应产生锁等待超时")
	assert.Zero(t, cascadeFailures,
		"级联失败会吞掉底层错误（含死锁 1213）；出现即说明锁面或加锁顺序退化了")
}
