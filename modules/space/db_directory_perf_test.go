package space

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// 目录接口的规模化性能验证。
//
// 默认跳过：要灌 10 个 Space × 6000 成员（6 万 space_member + 6 万 user +
// 4.3 万 robot），耗时远超常规单测，不该进 CI 的默认路径。
// 显式开启：OCTO_PERF=1 go test ./modules/space/ -run TestDirectoryPerf -v
//
// 规模取自线上最大 Space 的真实构成：约 1700 真人 + 4300 bot ≈ 6000 成员，
// 不含系统账号。
//
// ---- 实测结论（MySQL 8.0.46，10×6000 数据集，2026-08）------------------------
//
//	count(all/human/bot)      29 / 20 / 35 ms
//	queryDirectory limit=50   42 ms      ← 与全量几乎同价，原因见下
//	queryDirectory limit=6000 55 ms
//	relation 4300 个 bot      23 ms
//	HTTP 端到端 limit=50      64 ms   / 6 KB
//	HTTP 端到端 limit=6000    115 ms  / 707 KB
//
// 两个值得记住的结论：
//
//  1. **LIMIT 无法下推，分页与全量同价。** EXPLAIN ANALYZE 显示 Sort 之前有
//     `Filter: ((ifnull(u.robot,0)=0) or (r.robot_id is not null))`——停用 bot 的
//     排除条件引用了 JOIN 出来的 u 和 r，优化器必须把 6001 行全部 join 完、过滤完
//     才能排序取前 50。这是「一行的身份要靠三张表才能判定」的固有代价，不是索引
//     能解决的问题。
//
//  2. **补排序索引无效，已验证后撤回。** 曾加过
//     `(space_id, status, role DESC, created_at, uid)`：索引确实被选中、
//     using_filesort 变 false，但四项耗时全部落在噪声内（limit=50 从 31ms 到 34ms）。
//     原因是排序只占总耗时约 2ms（41.3ms 的 Sort 减去 39.6ms 的 Stream results），
//     真正的开销是 6001 次 u 与 r 的 nested-loop 单行查找（各约 12ms）。
//     为 2ms 给一张写频表加 5 列索引不划算，故不引入。
//
//     注意当时差点被自己的 EXPLAIN 助手骗过去：它写的是 `SELECT sm.uid`，
//     窄投影能被索引覆盖，于是报告「命中新索引、无 filesort」，而线上查询取
//     `sm.*` 覆盖不了、优化器根本不选那个索引。explainAnalyze 现在强制复用
//     directorySelectList，见其注释。
//
// 707 KB 是精简行的成果——若按 space_bots 那样带上 bot_commands 等字段，同规模
// 会到 MB 级。端到端 115ms 里查询占 55ms，另一半是序列化与传输。
const (
	perfSpaceCount   = 10
	perfHumansPerSp  = 1700
	perfBotsPerSp    = 4300
	perfMembersPerSp = perfHumansPerSp + perfBotsPerSp
	perfBatchSize    = 1000
)

func perfEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("OCTO_PERF") != "1" {
		t.Skip("性能用例默认跳过；用 OCTO_PERF=1 开启")
	}
}

func perfSpaceID(i int) string { return fmt.Sprintf("perf_space_%02d", i) }

// batchInsert 按 perfBatchSize 分批执行，避免单条语句过长。
func batchInsert(t *testing.T, prefix string, rows []string) {
	t.Helper()
	for start := 0; start < len(rows); start += perfBatchSize {
		end := start + perfBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		_, err := testCtx.DB().InsertBySql(prefix + strings.Join(rows[start:end], ",")).Exec()
		assert.NoError(t, err)
	}
}

// seedPerfDataset 灌入 10 个 Space，每个 1700 真人 + 4300 bot。
// 调用者本人（testutil.UID）是第 0 个 Space 的成员，便于走 HTTP 端到端。
func seedPerfDataset(t *testing.T) time.Duration {
	t.Helper()
	started := time.Now()

	for s := 0; s < perfSpaceCount; s++ {
		spaceID := perfSpaceID(s)
		assert.NoError(t, testSpaceDB.insertSpaceNoTx(&SpaceModel{
			SpaceId: spaceID, Name: spaceID, Creator: testutil.UID, Status: 1,
		}))

		users := make([]string, 0, perfMembersPerSp)
		members := make([]string, 0, perfMembersPerSp)
		robots := make([]string, 0, perfBotsPerSp)

		for i := 0; i < perfHumansPerSp; i++ {
			uid := fmt.Sprintf("pf_h_%02d_%04d", s, i)
			users = append(users, fmt.Sprintf("('%s','Human %d',0)", uid, i))
			members = append(members, fmt.Sprintf("('%s','%s',0,1)", spaceID, uid))
		}
		for i := 0; i < perfBotsPerSp; i++ {
			uid := fmt.Sprintf("pf_b_%02d_%04d", s, i)
			users = append(users, fmt.Sprintf("('%s','Bot %d',1)", uid, i))
			members = append(members, fmt.Sprintf("('%s','%s',0,1)", spaceID, uid))
			// creator 轮换，确保数据集里绝大多数 bot 都"不是调用者创建的"——
			// 这正是 listMembers 会滤掉、而目录必须返回的那部分。
			robots = append(robots, fmt.Sprintf("('%s',1,'pf_h_%02d_%04d')", uid, s, i%perfHumansPerSp))
		}

		batchInsert(t, "INSERT INTO `user` (uid, name, robot) VALUES ", users)
		batchInsert(t, "INSERT INTO space_member (space_id, uid, role, status) VALUES ", members)
		batchInsert(t, "INSERT INTO robot (robot_id, status, creator_uid) VALUES ", robots)
	}

	// 调用者加入第 0 个 Space，作为 owner。
	assert.NoError(t, testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: perfSpaceID(0), UID: testutil.UID, Role: 2, Status: 1,
	}))
	// 给调用者造一批好友关系，让 relation 查询命中真实数据而非空集。
	friends := make([]string, 0, 500)
	for i := 0; i < 500; i++ {
		friends = append(friends, fmt.Sprintf("('%s','pf_b_00_%04d',0)", testutil.UID, i))
	}
	batchInsert(t, "INSERT INTO friend (uid, to_uid, is_deleted) VALUES ", friends)

	return time.Since(started)
}

// timeIt 跑 n 轮，返回平均耗时与最慢一轮。
func timeIt(n int, fn func()) (avg, worst time.Duration) {
	for i := 0; i < n; i++ {
		start := time.Now()
		fn()
		elapsed := time.Since(start)
		if elapsed > worst {
			worst = elapsed
		}
		avg += elapsed
	}
	return avg / time.Duration(n), worst
}

// directorySelectList 必须与 queryDirectory 的投影列表逐字一致。
//
// 这一点是本文件最容易踩的坑：早先这里图省事写成 `SELECT sm.uid`，
// EXPLAIN 于是报告「命中覆盖索引、using_filesort=false」——因为只取 uid 时
// space_member 上的索引确实能覆盖。而真实查询取的是 sm.*，索引覆盖不了，
// 优化器转而选窄索引 + Sort。用错的 select list 去 EXPLAIN，得到的是一份
// 与线上无关的查询计划。
const directorySelectList = `SELECT sm.*,
	IFNULL(u.name, '') as name,
	IFNULL(uv.real_name, '') as real_name,
	IFNULL(u.robot, 0) as robot,
	CASE WHEN r.robot_id IS NOT NULL THEN 1 ELSE 0 END as active_bot`

// explainAnalyze 打印真实执行计划与逐算子实测耗时。
// 用 EXPLAIN ANALYZE 而不是 FORMAT=JSON：后者只给估算，前者给 actual time，
// 才能判断时间到底花在扫描、join 还是排序上。
func explainAnalyze(t *testing.T, label, sql string, args ...interface{}) {
	t.Helper()
	var plan string
	_, err := testCtx.DB().SelectBySql("EXPLAIN ANALYZE "+directorySelectList+sql, args...).Load(&plan)
	if err != nil {
		t.Logf("  EXPLAIN ANALYZE %s: %v", label, err)
		return
	}
	t.Logf("  ---- EXPLAIN ANALYZE %s ----", label)
	for _, line := range strings.Split(strings.TrimRight(plan, "\n"), "\n") {
		t.Logf("    %s", line)
	}
}

// TestDirectoryPerf 在 10×6000 的数据集上测量目录接口的各条查询路径。
func TestDirectoryPerf(t *testing.T) {
	perfEnabled(t)

	assert.NoError(t, testutil.CleanAllTables(testCtx))
	seedElapsed := seedPerfDataset(t)

	var total int64
	_, err := testCtx.DB().SelectBySql("SELECT COUNT(*) FROM space_member").Load(&total)
	assert.NoError(t, err)
	t.Logf("数据集：%d 个 Space × %d 成员（%d 真人 + %d bot），共 %d 条 space_member，灌入耗时 %v",
		perfSpaceCount, perfMembersPerSp, perfHumansPerSp, perfBotsPerSp, total, seedElapsed.Round(time.Millisecond))

	spaceID := perfSpaceID(0)

	t.Run("countDirectory", func(t *testing.T) {
		for _, listType := range []string{directoryTypeAll, directoryTypeHuman, directoryTypeBot} {
			var got int64
			avg, worst := timeIt(10, func() {
				got, _ = testSpaceDB.countDirectory(spaceID, listType)
			})
			t.Logf("count(type=%-5s) = %-5d  avg=%-10v worst=%v", listType, got, avg.Round(time.Microsecond), worst.Round(time.Microsecond))
		}
	})

	t.Run("queryDirectory_分页", func(t *testing.T) {
		cases := []struct {
			label       string
			page, limit uint64
		}{
			{"首页 limit=50", 1, 50},
			{"深翻 page=100 limit=50", 100, 50},
			{"通讯录一把梭 limit=6000", 1, 6000},
			{"上限 limit=10000", 1, 10000},
		}
		for _, c := range cases {
			var n int
			avg, worst := timeIt(5, func() {
				rows, err := testSpaceDB.queryDirectory(spaceID, directoryTypeAll, c.page, c.limit)
				assert.NoError(t, err)
				n = len(rows)
			})
			t.Logf("%-26s 返回 %-5d 行  avg=%-10v worst=%v", c.label, n, avg.Round(time.Microsecond), worst.Round(time.Microsecond))
		}
	})

	t.Run("queryBotRelations", func(t *testing.T) {
		rows, err := testSpaceDB.queryDirectory(spaceID, directoryTypeBot, 1, 10000)
		assert.NoError(t, err)
		botUIDs := make([]string, 0, len(rows))
		for _, r := range rows {
			botUIDs = append(botUIDs, r.UID)
		}
		for _, size := range []int{50, 1000, len(botUIDs)} {
			if size > len(botUIDs) {
				continue
			}
			subset := botUIDs[:size]
			var hits int
			avg, worst := timeIt(5, func() {
				rel, err := testSpaceDB.queryBotRelations(testutil.UID, subset)
				assert.NoError(t, err)
				hits = len(rel)
			})
			t.Logf("relation(%-5d 个 bot) 命中 %-4d  avg=%-10v worst=%v", size, hits, avg.Round(time.Microsecond), worst.Round(time.Microsecond))
		}
	})

	t.Run("EXPLAIN", func(t *testing.T) {
		where, args := directoryWhereClause(spaceID, directoryTypeAll)
		explainAnalyze(t, "limit=50", directoryFromJoin+" WHERE "+where+directoryOrderBy+" LIMIT 50", args...)
	})

	t.Run("HTTP端到端", func(t *testing.T) {
		for _, limit := range []int{50, 6000} {
			var status, bytes int
			avg, worst := timeIt(3, func() {
				w := getDirectory(t, testSrv, testCtx, spaceID, directoryQuery(directoryTypeAll, 1, limit))
				status, bytes = w.Code, w.Body.Len()
			})
			t.Logf("GET /directory?limit=%-5d status=%d body=%.1f KB  avg=%-10v worst=%v",
				limit, status, float64(bytes)/1024, avg.Round(time.Microsecond), worst.Round(time.Microsecond))
		}
	})

	// 与旧接口的同规模对照：listMembers 的 creator 过滤让它只返回极少数 bot，
	// 这正是客户端必须补一次 space_bots 的原因。
	t.Run("对照_listMembers", func(t *testing.T) {
		var n int
		avg, worst := timeIt(5, func() {
			rows, err := testSpaceDB.queryMembers(spaceID, testutil.UID, 1, 10000)
			assert.NoError(t, err)
			n = len(rows)
		})
		t.Logf("queryMembers(limit=10000) 返回 %-5d 行  avg=%-10v worst=%v （目录同参数返回 %d 行）",
			n, avg.Round(time.Microsecond), worst.Round(time.Microsecond), perfMembersPerSp+1)
	})
}
