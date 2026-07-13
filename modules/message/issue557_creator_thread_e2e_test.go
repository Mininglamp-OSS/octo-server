package message

// =============================================================================
// Issue #557 端到端回归 —— 创建者自建子区必须出现在自己的关注 Tab（读侧）。
//
// 背景：PR #565 写侧已修（CreateThread 提交后无条件给创建者补一条 thread ext 行，
// 见 thread/service.go EnsureThreadFollowForCreator）。但读侧（sidebar follow tab）
// 仍会因「父群未关注 / 未分类 / 显式取关」这条前置判断把创建者自己的子区丢掉
// （mergeThreadEntries）。RC-2 指出现有 5 个 #557 测试只断言 user_conversation_ext
// 有行，从未断言 CreateThread → 渲染 端到端，且这些测试挂 //go:build integration，
// CI 未加 -tags integration，从未执行。
//
// 本测试有意 **不挂 integration tag**，落在默认 `go test ./...` lane（该 lane 已
// provision MySQL/Redis/WuKongIM，见 .github/workflows/ci.yml test job），因此
// 真正随 CI 每次跑。用 **真实 thread.Service.CreateThread**（不直接调
// EnsureThreadFollowForCreator），断言创建者随后构建 follow tab 能看到该子区，
// 且非创建者 / 被移出父群的创建者看不到（防误放）。
//
// 测试基建约定（照搬本包 thread_ext_blacklist_filter_test.go 的硬教训）：
//   - 不跑 sql-migrate、不碰共享 test 库：先在 CI 的 MySQL 实例上建一个隔离库，
//     再手建本测试触达的最小表（DDL 与真实迁移列对齐），破坏性 DDL 只落隔离库，
//     跨包并行的 `go test ./...` 下不会外溢到别包查询。
//   - conversation_ext 全局单例（CreateThread 内部 GetGlobalConvExtService 消费）
//     用 sync.Once：整个 message 测试进程只能绑定一个 ctx，故本文件用**单个**测试
//     函数 + 一个 ctx + 子用例，避免第二个 ctx 绑不上单例导致 CreateThread 写错库。
//   - 需要支持 CommunityTopic 频道的 WuKongIM（CI 固定 v2.2.4 支持；本地常无），
//     故先探测 /health，不可达即 skip（与 incomingwebhook requireThreadCapableIM 同口径）。
// =============================================================================

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	convext "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// issue557DBName 是本文件专用隔离库名（与共享 test 库、别包迁移/查询完全隔离）。
const issue557DBName = "octo_msg_issue557_test"

// issue557SpaceID 用空串：本 E2E 覆盖「写侧落行 space == 读侧 ListThreadExts space」
// 的同 space 渲染路径（CreateThread → follow tab 渲染）。
//
// ⚠️ 注意（reviewer yujiawei P1）：**空串让写==读，恰好绕过了 legacy 群的真实 bug**。
// 真实缺陷是：legacy 群（group.space_id=''）里内部成员建子区时，写侧 effective space
// 为空，但现代客户端读 legacy 群时带的是**非空** default space（#484 口径），
// ListThreadExts 的 `WHERE space_id=?` 在 SQL 层丢掉 space_id='' 的补行——即写≠读。
// 本 E2E 不 seed 创建者的 space_member，故写侧归一化（thread.resolveCreatorBackfillSpaceID）
// 因拿不到 default space 而 no-op 保持 ''，写读仍然都为 ''，**不覆盖**该 legacy 非空读场景。
//
// 该 legacy 非空读场景的确定性覆盖放在 thread 包的纯 DB 测试
// Test_Issue557_LegacyGroup_CreatorBackfillSurvivesSpaceScopedRead（只需 MySQL，不依赖
// WuKongIM / conv_ext 单例，`-shuffle=on` 下无条件运行，直接命中 SQL 过滤层）。本 E2E
// 仅补充「CreateThread → 端到端渲染」的同 space 路径断言。
const issue557SpaceID = ""

// issue557RequireIM 探测 WuKongIM：不可达即 skip（CreateThread 会真实建子区频道）。
func issue557RequireIM(t *testing.T) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://127.0.0.1:5001/health")
	if err != nil {
		t.Skip("WuKongIM not reachable at 127.0.0.1:5001; #557 CreateThread e2e runs in CI (v2.2.4)")
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Skipf("WuKongIM at 127.0.0.1:5001 lacks CommunityTopic support (GET /health=%d); #557 e2e runs in CI", resp.StatusCode)
	}
}

// issue557NewCtx 建隔离库并连入（Migration=false，不经 module.Setup）。
func issue557NewCtx(t *testing.T) *config.Context {
	t.Helper()
	bootCfg := config.New()
	bootCfg.Test = true
	bootCfg.DB.Migration = false
	boot := config.NewContext(bootCfg)
	_, err := boot.DB().Exec(
		"CREATE DATABASE IF NOT EXISTS " + issue557DBName +
			" CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	require.NoError(t, err, "create isolated db")

	addr := os.Getenv("MSG_ISSUE557_TEST_MYSQL_ADDR")
	if addr == "" {
		addr = "root:demo@tcp(127.0.0.1:3306)/" + issue557DBName + "?charset=utf8mb4&parseTime=true"
	}
	cfg := config.New()
	cfg.Test = true
	cfg.DB.Migration = false
	cfg.DB.MySQLAddr = addr
	return config.NewContext(cfg)
}

// issue557EnsureTables 手建 CreateThread 全链路（含 GetMembers / GetMemberExternalFields /
// OnThreadCreated / EnsureThreadFollowForCreator）+ 读侧 sidebar 触达的最小表。
// 列集与 modules/{group,thread,conversation_ext} 迁移文件对齐。
func issue557EnsureTables(t *testing.T, ctx *config.Context) {
	t.Helper()
	for _, tbl := range []string{
		"user_conversation_ext", "user_follow_version", "thread_member",
		"thread", "group_member", "`group`", "user", "space", "seq",
	} {
		_, err := ctx.DB().Exec("DROP TABLE IF EXISTS " + tbl)
		require.NoError(t, err, "drop %s", tbl)
	}
	stmts := []string{
		// group：列是 group.Model 字段的子集，保证 QueryWithGroupNo 的 SELECT * 安全。
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
		// group_member：覆盖 queryMembersWithGroupNo / ExistMemberActive(s) /
		// queryMemberExternalMarker 触达的列（remark / forbidden_expir_time / bot_admin
		// 是 GetMembers 详情查询必需列）。
		"CREATE TABLE `group_member` (" +
			"  `id` INT NOT NULL AUTO_INCREMENT PRIMARY KEY," +
			"  `group_no` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `uid` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `remark` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `role` SMALLINT NOT NULL DEFAULT 0," +
			"  `version` BIGINT NOT NULL DEFAULT 0," +
			"  `status` SMALLINT NOT NULL DEFAULT 1," +
			"  `vercode` VARCHAR(100) NOT NULL DEFAULT ''," +
			"  `is_deleted` SMALLINT NOT NULL DEFAULT 0," +
			"  `robot` SMALLINT NOT NULL DEFAULT 0," +
			"  `forbidden_expir_time` INTEGER NOT NULL DEFAULT 0," +
			"  `bot_admin` SMALLINT NOT NULL DEFAULT 0," +
			"  `is_external` SMALLINT NOT NULL DEFAULT 0," +
			"  `source_space_id` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  UNIQUE KEY `group_no_uid` (`group_no`, `uid`)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		// user：queryMembersWithGroupNo LEFT JOIN user 取 name。
		"CREATE TABLE `user` (" +
			"  `id` INT NOT NULL AUTO_INCREMENT PRIMARY KEY," +
			"  `uid` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `name` VARCHAR(100) NOT NULL DEFAULT ''" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		// space：queryMemberExternalMarker LEFT JOIN space 取 source space name。
		"CREATE TABLE `space` (" +
			"  `id` INT NOT NULL AUTO_INCREMENT PRIMARY KEY," +
			"  `space_id` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `name` VARCHAR(100) NOT NULL DEFAULT ''" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		// thread：列集为 modules/thread/sql 的合成结果——CreateThread 的
		// InsertTxReturningID 用 util.AttrToUnderscore(Model) 绑定全部非结构体字段，
		// 缺任一列都会 "Unknown column" 失败，故必须齐全。
		"CREATE TABLE `thread` (" +
			"  `id` BIGINT AUTO_INCREMENT PRIMARY KEY," +
			"  `short_id` VARCHAR(32) NOT NULL," +
			"  `group_no` VARCHAR(40) NOT NULL," +
			"  `name` VARCHAR(100) NOT NULL," +
			"  `creator_uid` VARCHAR(40) NOT NULL," +
			"  `source_message_id` BIGINT DEFAULT NULL," +
			"  `status` TINYINT NOT NULL DEFAULT 1," +
			"  `version` BIGINT NOT NULL DEFAULT 0," +
			"  `message_count` BIGINT NOT NULL DEFAULT 0," +
			"  `last_message_at` TIMESTAMP NULL DEFAULT NULL," +
			"  `last_message_content` VARCHAR(500) NOT NULL DEFAULT ''," +
			"  `last_message_sender_uid` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `thread_md` TEXT DEFAULT NULL," +
			"  `thread_md_version` BIGINT NOT NULL DEFAULT 0," +
			"  `thread_md_updated_at` TIMESTAMP NULL DEFAULT NULL," +
			"  `thread_md_updated_by` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  UNIQUE KEY `uk_short_id` (`short_id`)," +
			"  UNIQUE KEY `uk_group_short` (`group_no`, `short_id`)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		"CREATE TABLE `thread_member` (" +
			"  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY," +
			"  `thread_id` BIGINT UNSIGNED NOT NULL," +
			"  `uid` VARCHAR(40) NOT NULL," +
			"  `role` TINYINT NOT NULL DEFAULT 0," +
			"  `version` BIGINT NOT NULL DEFAULT 0," +
			"  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  UNIQUE KEY `uk_thread_uid` (`thread_id`, `uid`)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		// user_conversation_ext / user_follow_version：与 thread_ext_blacklist_filter_test.go
		// 同源（conversation_ext 迁移合成结果）。
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
			"  UNIQUE KEY `uk` (`uid`, `space_id`, `target_type`, `target_id`)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
		"CREATE TABLE `user_follow_version` (" +
			"  `uid` VARCHAR(40) NOT NULL," +
			"  `space_id` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `version` BIGINT NOT NULL DEFAULT 0," +
			"  `updated_at` DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP," +
			"  PRIMARY KEY (`uid`, `space_id`)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
		// seq：CreateThread 内部 ctx.GenSeq(ThreadSeqKey) 生成 thread 序列号，
		// GenSeq 读写 seq 表（octo-lib config/seq.go）。列集与 octo-lib GenSeq 期望
		// 及 modules/common/sql/20211108000001_common_legacy01.sql 的真实迁移逐列对齐
		// （key 唯一、min_seq/step 数值列），缺此表 GenSeq 报 "Table 'seq' doesn't exist"。
		"CREATE TABLE `seq` (" +
			"  `id` INTEGER NOT NULL PRIMARY KEY AUTO_INCREMENT," +
			"  `key` VARCHAR(100) NOT NULL DEFAULT ''," +
			"  `min_seq` BIGINT NOT NULL DEFAULT 1000000," +
			"  `step` INTEGER NOT NULL DEFAULT 1000," +
			"  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  UNIQUE KEY `seq_uidx` (`key`)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
	}
	for _, s := range stmts {
		_, err := ctx.DB().Exec(s)
		require.NoError(t, err, "issue557EnsureTables: %s", s[:40])
	}
}

// issue557SeedGroupMember 插入一个活跃(status=Normal, is_deleted=0)群成员。
func issue557SeedGroupMember(t *testing.T, ctx *config.Context, groupNo, uid string, role int) {
	t.Helper()
	_, err := ctx.DB().Exec(
		"INSERT INTO group_member (group_no, uid, role, vercode, is_deleted, status, version) VALUES (?, ?, ?, ?, 0, ?, 1)",
		groupNo, uid, role, util.GenerUUID(), int(common.GroupMemberStatusNormal),
	)
	require.NoError(t, err, "seed group_member %s", uid)
}

// issue557FollowTabThreadItems 复刻 sidebar Sync handler 的 DB-only thread 渲染
// 全链路（api_sidebar.go 中 loadThreadLastMsgAt → selfCreatedThreads → mergeThreadEntries
// 的那一段），以 loginUID 视角构建 follow tab 的 thread 条目。
// categorySetting 传空 map（模拟父群未分类）；unfollowedGroups 由入参给出。
func issue557FollowTabThreadItems(t *testing.T, sb *Sidebar, loginUID string, unfollowedGroups map[string]struct{}) []*SidebarItem {
	t.Helper()
	rows, err := sb.convExtDB.ListThreadExts(loginUID, issue557SpaceID)
	require.NoError(t, err, "ListThreadExts")
	// 与 handler 2a.6 一致：先按父群成员身份过滤（被移出父群者的行在此丢弃）。
	rows, err = sb.filterThreadExtsByParentMembership(rows, loginUID)
	require.NoError(t, err, "filterThreadExtsByParentMembership")

	lastMsgAt, _, creatorMap, err := sb.loadThreadLastMsgAt(rows)
	require.NoError(t, err, "loadThreadLastMsgAt")

	selfCreated := make(map[string]struct{})
	for key, creatorUID := range creatorMap {
		if creatorUID == loginUID {
			selfCreated[key] = struct{}{}
		}
	}

	return mergeThreadEntries(
		nil, rows, lastMsgAt,
		map[string]*GroupCategorySetting{}, // 父群未分类
		unfollowedGroups,
		map[string]string{}, map[string]string{}, "",
		selfCreated,
	)
}

// findThreadItem 在 items 中找到指定 channelID 的 thread 条目（找不到返回 nil）。
func findThreadItem(items []*SidebarItem, channelID string) *SidebarItem {
	for _, it := range items {
		if it.TargetType == int(common.ChannelTypeCommunityTopic) && it.TargetID == channelID {
			return it
		}
	}
	return nil
}

// TestE2E_Issue557_CreatorThreadRendersInFollowTab 端到端回归：
// 真实 CreateThread 后，创建者在父群「从未关注」和「显式取关」两种场景下都能在自己
// 的关注 Tab 看到该子区；非创建者（即使残留一条 thread ext 行）与被移出父群的创建者
// 都看不到（防误放，保留 space/成员安全过滤）。
func TestE2E_Issue557_CreatorThreadRendersInFollowTab(t *testing.T) {
	issue557RequireIM(t)
	ctx := issue557NewCtx(t)
	issue557EnsureTables(t, ctx)

	// CreateThread 内部走 conversation_ext 全局单例（GetGlobalConvExtService）。
	// sync.Once：整个进程只绑定一次。若同进程内另一个测试（如 testutil.NewTestServer
	// 的 module.Setup，或 -tags integration 下的 default_followed_group_guard_e2e）已
	// 先把单例绑到别的 ctx，本测试的 CreateThread 会写到那个库、而断言读的是本隔离库
	// → 假失败。故先探测：若单例已被别人绑定，clean skip（本 issue 的读侧修复另有
	// 纯函数测试 TestMergeThreadEntries_SelfCreated_* 无条件守卫，不依赖本 E2E）。
	if convext.GetGlobalConvExtService() != nil {
		t.Skip("conv_ext global singleton already bound by another test in this process; " +
			"#557 read-side fix is guarded by TestMergeThreadEntries_SelfCreated_* (pure, always runs)")
	}
	convext.InitGlobalConvExtService(ctx)
	convext.InitGlobalConvExtDB(ctx)
	require.NotNil(t, convext.GetGlobalConvExtService(),
		"conv_ext 单例必须就绪，否则 CreateThread 不会给创建者补 thread ext 行")

	threadSvc := thread.NewService(ctx)
	sb := NewSidebar(ctx)

	const creator = "u557-creator"
	const other = "u557-other"

	// scenario 建一个父群、创建者+另一成员，按需预置父群 ext 行，然后真实 CreateThread。
	// 返回子区 channelID。
	newScenarioThread := func(t *testing.T, groupNo string, seedUnfollowedRow bool) string {
		t.Helper()
		_, err := ctx.DB().Exec(
			"INSERT INTO `group` (group_no, name, creator, status, version, space_id) VALUES (?, '父群', ?, 1, 1, '')",
			groupNo, creator,
		)
		require.NoError(t, err, "seed group")
		issue557SeedGroupMember(t, ctx, groupNo, creator, 1 /* creator */)
		issue557SeedGroupMember(t, ctx, groupNo, other, 0 /* common */)

		if seedUnfollowedRow {
			// 显式取关父群：target_type=2 群行 group_unfollowed=1，且
			// auto_follow_threads=0（避免 OnThreadCreated fanout 复活行——本测试要
			// 证明创建者行来自 EnsureThreadFollowForCreator，而非 fanout）。
			one := int8(1)
			zero := int8(0)
			require.NoError(t, convext.NewDB(ctx).Upsert(creator, issue557SpaceID, 2 /* group */, groupNo, convext.ConvExtFields{
				GroupUnfollowed:   &one,
				AutoFollowThreads: &zero,
			}), "seed group_unfollowed ext row")
		}

		tr, err := threadSvc.CreateThread(&thread.CreateThreadReq{
			GroupNo:     groupNo,
			Name:        "创建者的子区",
			CreatorUID:  creator,
			CreatorName: "创建者",
		})
		require.NoError(t, err, "real CreateThread")
		return thread.BuildChannelID(groupNo, tr.ShortID)
	}

	t.Run("父群从未关注_创建者可见", func(t *testing.T) {
		groupNo := "g557a" + strings.ReplaceAll(util.GenerUUID(), "-", "")[:8]
		channelID := newScenarioThread(t, groupNo, false)

		// 写侧断言：CreateThread 确实给创建者补了 thread ext 行（不直接调 Ensure*）。
		row, err := convext.NewDB(ctx).Get(creator, issue557SpaceID, 5, channelID)
		require.NoError(t, err)
		require.NotNil(t, row, "CreateThread 应给创建者补一条 thread ext 行")

		// 读侧断言（本 PR 修复点）：父群未分类且不在 unfollowed 集合，创建者仍可见。
		items := issue557FollowTabThreadItems(t, sb, creator, map[string]struct{}{})
		it := findThreadItem(items, channelID)
		require.NotNil(t, it, "创建者应在关注 Tab 看到自建子区（父群从未关注）")
		assert.True(t, it.IsFollowed, "子区条目 IsFollowed 应为 true")
		assert.Equal(t, groupNo, it.ParentChannelID, "ParentChannelID 应为父群 groupNo")
		assert.Nil(t, it.CategoryID, "父群未分类 → 落未分类桶（CategoryID=nil）")
	})

	t.Run("父群显式取关_创建者可见", func(t *testing.T) {
		groupNo := "g557b" + strings.ReplaceAll(util.GenerUUID(), "-", "")[:8]
		channelID := newScenarioThread(t, groupNo, true)

		row, err := convext.NewDB(ctx).Get(creator, issue557SpaceID, 5, channelID)
		require.NoError(t, err)
		require.NotNil(t, row, "CreateThread 应给创建者补一条 thread ext 行")

		// 父群 group_unfollowed=1 未被清零（octo-web #293 边界）。
		groupRow, err := convext.NewDB(ctx).Get(creator, issue557SpaceID, 2, groupNo)
		require.NoError(t, err)
		require.NotNil(t, groupRow)
		assert.Equal(t, int8(1), groupRow.GroupUnfollowed, "补子区行不应清父群 group_unfollowed")

		unfollowed := map[string]struct{}{groupNo: {}}
		items := issue557FollowTabThreadItems(t, sb, creator, unfollowed)
		it := findThreadItem(items, channelID)
		require.NotNil(t, it, "创建者应在关注 Tab 看到自建子区（父群显式取关）")
		assert.True(t, it.IsFollowed)
		assert.Equal(t, groupNo, it.ParentChannelID)
	})

	t.Run("非创建者即使有残留thread行也不可见_防误放", func(t *testing.T) {
		groupNo := "g557c" + strings.ReplaceAll(util.GenerUUID(), "-", "")[:8]
		channelID := newScenarioThread(t, groupNo, false)

		// 人为给 other（非创建者、非关注者）塞一条 thread ext 行 —— 模拟 fanout/残留。
		// 豁免键严格等于 creator_uid==loginUID，other 不匹配 → 父群未分类前置照常丢弃。
		require.NoError(t, convext.NewDB(ctx).Upsert(other, issue557SpaceID, 5, channelID, convext.ConvExtFields{}),
			"seed stale thread ext row for non-creator")

		items := issue557FollowTabThreadItems(t, sb, other, map[string]struct{}{})
		assert.Nil(t, findThreadItem(items, channelID),
			"非创建者不应被豁免：父群未分类 → 该子区必须被丢弃（防误放）")
	})

	t.Run("被移出父群的创建者不可见_安全过滤保留", func(t *testing.T) {
		groupNo := "g557d" + strings.ReplaceAll(util.GenerUUID(), "-", "")[:8]
		channelID := newScenarioThread(t, groupNo, false)

		// 创建者被拉黑（非活跃父群成员）：父群成员过滤在 mergeThreadEntries 之前丢弃其行。
		_, err := ctx.DB().Exec(
			"UPDATE group_member SET status=? WHERE group_no=? AND uid=?",
			int(common.GroupMemberStatusBlacklist), groupNo, creator,
		)
		require.NoError(t, err)

		items := issue557FollowTabThreadItems(t, sb, creator, map[string]struct{}{})
		assert.Nil(t, findThreadItem(items, channelID),
			"被移出/拉黑父群的创建者不应看到子区（父群成员安全过滤保留）")
	})
}
