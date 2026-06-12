package message

// =============================================================================
// Issue #351（PR #345 mandatory follow-up）— 子区 ext 物化按活跃成员过滤。
//
// AuthorizeChannelFollow 对 GROUP follow 保持 permissive ExistMember，但
// FollowChannel 会物化既有子区 ext 行、OnThreadCreated fanout 会给所有
// auto_follow_threads=1 的群行物化新子区 ext 行。被拉黑（status=Blacklist、
// is_deleted=0）的父群成员两条路径都不应再收到子区 ext 行（元数据/通知层泄漏；
// 内容读已被 ExistMemberActive 门禁兜住）。
//
// 测试基建约定（PR #356 round-1 CI 红 + round-2 review 的教训）：
//  1. 不跑 sql-migrate——本包非 integration-tag 测试一律不经 module.Setup（包内
//     既有 testutil.NewTestServer 用例全部 t.Skip，channel_files_blacklist_test.go
//     等用例手建最小表；混跑迁移会在 -shuffle 下撞 Error 1050 Table already exists）。
//  2. 不碰共享 test 库——本文件要 DROP+CREATE 核心表（user_conversation_ext /
//     group_member / …），而 go test ./... 默认跨包并行、modules/group、modules/
//     thread 等包用 testutil.NewTestServer 连同一个 test 库，跨包并行时破坏性 DDL
//     会撞别包的查询（round-2 review blocking）。因此先在 MySQL 实例上建独立库
//     再连入，所有 DDL/数据只落在隔离库里。
// 写法照搬 integration e2e helper：手建最小表 + 裸 INSERT 种子 + 显式装配
// service（wiring 与 1module.go 注入逻辑逐行对齐）。
// =============================================================================

import (
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	convext "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const extBlSpaceID = "s_ext_bl"

// extBlDBName 是本文件专用的隔离库名：与共享 test 库（testutil.NewTestServer /
// 其它包的迁移与查询）完全隔离，跨包并行的 go test ./... 下破坏性 DDL 不会外溢。
const extBlDBName = "octo_msg_blacklist_test"

// extBlNewCtx 构造指向隔离库的 *config.Context：先用 config.New 默认 DSN（与 CI
// MySQL service 同源的 root:demo@…/test）建隔离库，再把 DSN 的库名换成隔离库连入。
// 显式 Migration=false——不经 module.Setup。可用 MSG_BLACKLIST_TEST_MYSQL_ADDR
// 覆盖隔离库 DSN（与 newSidebarIntegCtx 的 env 覆盖风格一致）。
func extBlNewCtx(t *testing.T) *config.Context {
	t.Helper()
	bootCfg := config.New()
	bootCfg.Test = true
	bootCfg.DB.Migration = false
	boot := config.NewContext(bootCfg)
	_, err := boot.DB().Exec(
		"CREATE DATABASE IF NOT EXISTS " + extBlDBName +
			" CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	require.NoError(t, err, "create isolated db")

	addr := os.Getenv("MSG_BLACKLIST_TEST_MYSQL_ADDR")
	if addr == "" {
		addr = "root:demo@tcp(127.0.0.1:3306)/" + extBlDBName + "?charset=utf8mb4&parseTime=true"
	}
	cfg := config.New()
	cfg.Test = true
	cfg.DB.Migration = false
	cfg.DB.MySQLAddr = addr
	return config.NewContext(cfg)
}

// extBlEnsureTables 在隔离库里手建本测试用到的最小表（DDL 与 modules/group、
// modules/conversation_ext、modules/thread 的迁移文件中本测试触达的列对齐；
// 写法照搬 default_followed_group_guard_e2e_test.go / thread_follow_blacklist_
// e2e_test.go 的同名 helper）。DROP + CREATE 保证每个用例拿到本文件期望的
// schema；隔离库只被本包顺序执行的测试使用，破坏性 DDL 安全。
func extBlEnsureTables(t *testing.T, ctx *config.Context) {
	t.Helper()
	for _, tbl := range []string{"user_conversation_ext", "user_follow_version", "thread", "group_member", "`group`"} {
		_, err := ctx.DB().Exec("DROP TABLE IF EXISTS " + tbl)
		require.NoError(t, err, "drop %s", tbl)
	}
	stmts := []string{
		// modules/group/sql/20191106000002 起的 group 表（仅本测试触达的列，
		// 与 default_followed_group_guard_e2e_test.go ensureGuardE2ETables 一致）。
		"CREATE TABLE `group` (" +
			"  `id` INT NOT NULL AUTO_INCREMENT PRIMARY KEY," +
			"  `group_no` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `name` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `creator` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `status` SMALLINT NOT NULL DEFAULT 0," +
			"  `version` BIGINT NOT NULL DEFAULT 0," +
			"  `group_type` SMALLINT NOT NULL DEFAULT 0," +
			"  `space_id` VARCHAR(40) DEFAULT ''," +
			"  `is_external_group` SMALLINT NOT NULL DEFAULT 0," +
			"  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  UNIQUE KEY `group_groupNo` (`group_no`)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		"CREATE TABLE `group_member` (" +
			"  `id` INT NOT NULL AUTO_INCREMENT PRIMARY KEY," +
			"  `group_no` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `uid` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `role` SMALLINT NOT NULL DEFAULT 0," +
			"  `version` BIGINT NOT NULL DEFAULT 0," +
			"  `is_deleted` SMALLINT NOT NULL DEFAULT 0," +
			"  `status` SMALLINT NOT NULL DEFAULT 1," +
			"  `vercode` VARCHAR(100) NOT NULL DEFAULT ''," +
			"  `robot` SMALLINT NOT NULL DEFAULT 0," +
			"  `is_external` SMALLINT NOT NULL DEFAULT 0," +
			"  `source_space_id` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  UNIQUE KEY `group_no_uid` (`group_no`, `uid`)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		// modules/thread 迁移中的 thread 表（最小列集，与 thread_follow_blacklist_
		// e2e_test.go ensureThreadE2ETable 一致）。
		"CREATE TABLE `thread` (" +
			"  `id` INT NOT NULL AUTO_INCREMENT PRIMARY KEY," +
			"  `group_no` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `short_id` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `name` VARCHAR(100) NOT NULL DEFAULT ''," +
			"  `creator_uid` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `status` INT NOT NULL DEFAULT 1," +
			"  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  UNIQUE KEY `uk_short` (`short_id`)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		// modules/conversation_ext/sql/20260513000001 + 20260514000001(dm_category_id
		// VARCHAR) + 20260514000002(去 version 列) + 20260522000001(auto_follow_threads
		// + idx_channel_auto_follow) 的合成结果。
		"CREATE TABLE `user_conversation_ext` (" +
			"  `id` BIGINT AUTO_INCREMENT PRIMARY KEY," +
			"  `uid` VARCHAR(40) NOT NULL," +
			"  `space_id` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `target_type` TINYINT NOT NULL," +
			"  `target_id` VARCHAR(100) NOT NULL," +
			"  `followed_dm` TINYINT NOT NULL DEFAULT 0," +
			"  `dm_category_id` VARCHAR(32) NULL," +
			"  `group_unfollowed` TINYINT NOT NULL DEFAULT 0," +
			"  `follow_sort` INT NOT NULL DEFAULT 0," +
			"  `auto_follow_threads` TINYINT(1) NOT NULL DEFAULT 0," +
			"  `created_at` DATETIME DEFAULT CURRENT_TIMESTAMP," +
			"  `updated_at` DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP," +
			"  UNIQUE KEY `uk` (`uid`, `space_id`, `target_type`, `target_id`)," +
			"  KEY `idx_channel_auto_follow` (`target_type`, `target_id`, `auto_follow_threads`)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
		// modules/conversation_ext/sql/20260513000002。
		"CREATE TABLE `user_follow_version` (" +
			"  `uid` VARCHAR(40) NOT NULL," +
			"  `space_id` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `version` BIGINT NOT NULL DEFAULT 0," +
			"  `updated_at` DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP," +
			"  PRIMARY KEY (`uid`, `space_id`)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
	}
	for _, s := range stmts {
		_, err := ctx.DB().Exec(s)
		require.NoError(t, err, "extBlEnsureTables: %s", s[:40])
	}
}

// setupThreadExtBlacklistData 建一个父群（space_id 为空 → legacy wildcard 可见）+
// 两个正常成员 normalUID / victimUID，并按 1module.go 的注入逻辑显式装配
// conversation_ext.Service（ThreadAuthChecker / ChannelAuthChecker /
// ThreadEnumerator / ActiveMemberFilter 同源 wiring）。
func setupThreadExtBlacklistData(t *testing.T) (*config.Context, *convext.Service, string, string, string) {
	t.Helper()
	ctx := extBlNewCtx(t)
	extBlEnsureTables(t, ctx)

	svc := convext.NewService(ctx)
	checker := newThreadAuthChecker(ctx)
	svc.SetThreadAuthChecker(checker)
	svc.SetChannelAuthChecker(checker)
	svc.SetThreadEnumerator(newThreadEnumerator(ctx))
	svc.SetActiveMemberFilter(checker)

	groupNo := strings.ReplaceAll(util.GenerUUID(), "-", "")
	normalUID := "u_ext_normal_" + util.GenerUUID()[:8]
	victimUID := "u_ext_victim_" + util.GenerUUID()[:8]

	_, err := ctx.DB().Exec(
		"INSERT INTO `group` (group_no, name, creator, status, version, space_id) VALUES (?, '父群', ?, 1, 1, '')",
		groupNo, normalUID,
	)
	require.NoError(t, err, "seed group")
	for _, u := range []string{normalUID, victimUID} {
		_, err = ctx.DB().Exec(
			"INSERT INTO group_member (group_no, uid, vercode, is_deleted, status, version) VALUES (?, ?, ?, 0, ?, 1)",
			groupNo, u, util.GenerUUID(), int(common.GroupMemberStatusNormal),
		)
		require.NoError(t, err, "seed member %s", u)
	}
	return ctx, svc, groupNo, normalUID, victimUID
}

func extBlSetMemberStatus(t *testing.T, ctx *config.Context, groupNo, uid string, status common.GroupMemberStatus) {
	t.Helper()
	_, err := ctx.DB().Exec(
		"UPDATE group_member SET status=? WHERE group_no=? AND uid=?",
		int(status), groupNo, uid,
	)
	require.NoError(t, err)
}

// hasThreadExtRow 查 user_conversation_ext 是否存在 (uid, space, target_type=5, channelID) 行。
func hasThreadExtRow(t *testing.T, ctx *config.Context, uid, channelID string) bool {
	t.Helper()
	var count int64
	_, err := ctx.DB().Select("count(*)").From("user_conversation_ext").
		Where("uid=? AND space_id=? AND target_type=5 AND target_id=?", uid, extBlSpaceID, channelID).
		Load(&count)
	require.NoError(t, err)
	return count > 0
}

// seedActiveThread 在 thread 表插入一个 active(status=1) 子区，返回 channelID。
func seedActiveThread(t *testing.T, ctx *config.Context, groupNo, shortID string) string {
	t.Helper()
	_, err := ctx.DB().Exec(
		"INSERT INTO thread (group_no, short_id, name, creator_uid, status) VALUES (?, ?, 'topic', 'creator', 1)",
		groupNo, shortID,
	)
	require.NoError(t, err, "seed thread %s/%s", groupNo, shortID)
	return groupNo + "____" + shortID
}

// TestOnThreadCreated_BlacklistedMemberExcludedFromFanout 验证：两个成员都开启了
// auto_follow_threads（正常时 FollowChannel），其中一个随后被拉黑——新建子区的
// fanout 只给活跃成员物化 ext 行；解除拉黑后 fanout 自动恢复。
func TestOnThreadCreated_BlacklistedMemberExcludedFromFanout(t *testing.T) {
	ctx, svc, groupNo, normalUID, victimUID := setupThreadExtBlacklistData(t)

	// 两人在正常状态下关注 channel（写 auto_follow_threads=1 群行）。
	require.NoError(t, svc.FollowChannel(normalUID, extBlSpaceID, groupNo))
	require.NoError(t, svc.FollowChannel(victimUID, extBlSpaceID, groupNo))

	// victim 被拉黑后新建子区。
	extBlSetMemberStatus(t, ctx, groupNo, victimUID, common.GroupMemberStatusBlacklist)
	sid1 := "1489104291682713601"
	require.NoError(t, svc.OnThreadCreated(groupNo, sid1))

	assert.True(t, hasThreadExtRow(t, ctx, normalUID, groupNo+"____"+sid1),
		"正常成员应收到新子区 ext 行")
	assert.False(t, hasThreadExtRow(t, ctx, victimUID, groupNo+"____"+sid1),
		"被拉黑成员不应收到新子区 ext 行（issue #351 元数据泄漏）")

	// 解除拉黑 → 下一条新子区恢复 fanout（auto_follow_threads=1 保留的语义）。
	extBlSetMemberStatus(t, ctx, groupNo, victimUID, common.GroupMemberStatusNormal)
	sid2 := "1489104291682713602"
	require.NoError(t, svc.OnThreadCreated(groupNo, sid2))
	assert.True(t, hasThreadExtRow(t, ctx, victimUID, groupNo+"____"+sid2),
		"解除拉黑后 fanout 应自动恢复")
}

// TestFollowChannel_BlacklistedMemberSkipsExistingThreadMaterialization 验证：
// 被拉黑成员调用 FollowChannel（GROUP 门禁 permissive，调用本身放行）时，
// 既有子区的 ext 物化被跳过；正常成员则正常物化。
func TestFollowChannel_BlacklistedMemberSkipsExistingThreadMaterialization(t *testing.T) {
	ctx, svc, groupNo, normalUID, victimUID := setupThreadExtBlacklistData(t)

	existingChannelID := seedActiveThread(t, ctx, groupNo, "1489104291682713603")

	extBlSetMemberStatus(t, ctx, groupNo, victimUID, common.GroupMemberStatusBlacklist)

	// 被拉黑成员 FollowChannel：GROUP 行写入放行（permissive 语义不变），但
	// 不得物化既有子区 ext 行。
	require.NoError(t, svc.FollowChannel(victimUID, extBlSpaceID, groupNo),
		"GROUP follow 对被拉黑成员保持 permissive，调用不应报错")
	assert.False(t, hasThreadExtRow(t, ctx, victimUID, existingChannelID),
		"被拉黑成员不应通过 FollowChannel 重新物化既有子区 ext 行（issue #351）")

	var groupRowCount int64
	_, err := ctx.DB().Select("count(*)").From("user_conversation_ext").
		Where("uid=? AND space_id=? AND target_type=2 AND target_id=?",
			victimUID, extBlSpaceID, groupNo).
		Load(&groupRowCount)
	require.NoError(t, err)
	assert.Equal(t, int64(1), groupRowCount, "GROUP 级 ext 行本身应照常写入（permissive 语义不变）")

	// 对照组：正常成员 FollowChannel 正常物化既有子区。
	require.NoError(t, svc.FollowChannel(normalUID, extBlSpaceID, groupNo))
	assert.True(t, hasThreadExtRow(t, ctx, normalUID, existingChannelID),
		"正常成员 FollowChannel 应物化既有子区 ext 行")
}
