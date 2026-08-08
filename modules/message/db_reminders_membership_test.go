// =============================================================================
// modules/message/db_reminders_membership_test.go
//
// 频道级（uid=''）提醒的成员可见性回归。对应任务
// .octospec/tasks/reminder-sync-membership-scope/brief.md。
//
// 被修的缺陷：remindersDB.sync 的 uid='' 分支此前没有任何成员校验 —— channelIDs
// 为空时连 channel_id 约束都没有，任意登录用户都能把全表的频道级提醒拉走，拿到
// 自己从未加入频道的 channel_id / publisher / message_id / message_seq。
//
// 本文件的主判据刻意**不依赖删除 X-Space-Id 请求头**：渗透报告是靠删头触发的，
// 但那只是绕过了 SpaceMiddleware 这道门，泄露本身与 Space 无关。只堵中间件的修复
// 会让报告的复现变绿而漏洞仍在（换一个自己确实是成员的 Space ID 即可照旧倒表）。
// 因此这里直接在 DB 层验证：调用方无论从哪条路径进来，都只能看到自己所属频道的
// 频道级提醒。
//
// 用独立库 + 手建最小表，理由同 thread_ext_blacklist_filter_test.go：不碰共享
// test 库，避免跨包并行时破坏性 DDL 外溢，也避开每个模块各自迁移集导致的
// "unknown migration in database"。
// =============================================================================

package message

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const remMemberDBName = "octo_msg_reminders_member_test"

// 固定的测试主体。uidA 是被测调用方。
const (
	remUIDA   = "u_alice"
	remUIDB   = "u_bob"
	remGroupA = "g_alice_in"       // A 是活跃成员
	remGroupB = "g_alice_not_in"   // A 完全不是成员 —— 泄露的主角
	remGroupC = "g_alice_left"     // A 曾是成员，is_deleted=1
	remGroupD = "g_alice_blocked"  // A 是成员但 status!=Normal（被拉黑）
	remTopicA = "g_alice_in____t1" // 子区，父群 = remGroupA
	remTopicB = "g_alice_not_in____t2"
)

// remMemberBaseDSN 是本文件所有连接的服务器地址与凭据来源。
// MSG_REMINDERS_TEST_MYSQL_ADDR 可覆盖（CI 上 MySQL 未必在 127.0.0.1）。
func remMemberBaseDSN() string {
	if raw := os.Getenv("MSG_REMINDERS_TEST_MYSQL_ADDR"); raw != "" {
		return raw
	}
	return "root:demo@tcp(127.0.0.1:3306)/" + remMemberDBName + "?charset=utf8mb4&parseTime=true"
}

// remMemberDSNWithDB 取 base DSN 的服务器与凭据，把库名换成 dbName。
//
// 库名必须由代码强制、不能直接采信环境变量（PR#717 review）：remMemberEnsureTables
// 会 DROP 掉 reminders / reminder_done / group_member 三张表，若环境变量指向的是
// 一个真实库，一次配置失误就能删掉真实数据。同理建库用的连接也必须与测试连接指向
// **同一台**服务器 —— 否则隔离库建在 A、测试连到 B，症状是"表不存在"而不是报错。
//
// 用驱动自带的 ParseDSN 而不是字符串切割：unix socket 形式
// (user@unix(/var/run/mysqld/mysqld.sock)/db) 的 DSN 里含斜杠，手写切割会切错。
func remMemberDSNWithDB(t *testing.T, dbName string) string {
	t.Helper()
	cfg, err := mysql.ParseDSN(remMemberBaseDSN())
	require.NoError(t, err, "MSG_REMINDERS_TEST_MYSQL_ADDR 不是合法的 MySQL DSN")
	cfg.DBName = dbName
	return cfg.FormatDSN()
}

func remMemberNewCtx(t *testing.T, collate string) *config.Context {
	t.Helper()

	// 建库连接：与测试连接同源，只把库名指到 information_schema（必然存在，且这里
	// 只发 server 级的 DROP/CREATE DATABASE，不碰其中任何表）。
	bootCfg := config.New()
	bootCfg.Test = true
	bootCfg.DB.Migration = false
	bootCfg.DB.MySQLAddr = remMemberDSNWithDB(t, "information_schema")
	boot := config.NewContext(bootCfg)
	_, err := boot.DB().Exec("DROP DATABASE IF EXISTS " + remMemberDBName)
	require.NoError(t, err, "drop isolated db")
	_, err = boot.DB().Exec(
		"CREATE DATABASE " + remMemberDBName + " CHARACTER SET utf8mb4 COLLATE " + collate)
	require.NoError(t, err, "create isolated db")

	cfg := config.New()
	cfg.Test = true
	cfg.DB.Migration = false
	cfg.DB.MySQLAddr = remMemberDSNWithDB(t, remMemberDBName)
	return config.NewContext(cfg)
}

// remMemberEnsureTables 手建最小表。
//
// memberCollate 单独可配：reminders 被 20260711 迁移强制转成 utf8mb4_general_ci，
// 而 group_member 建表未声明 CHARSET、继承建库默认，两者在部分部署上会落到不同
// collation。TestRemindersSync_SurvivesGroupMemberCollationMismatch 用它复现那种
// 部署形状。
func remMemberEnsureTables(t *testing.T, ctx *config.Context, memberCollate string) {
	t.Helper()
	for _, tbl := range []string{"reminders", "reminder_done", "group_member"} {
		_, err := ctx.DB().Exec("DROP TABLE IF EXISTS " + tbl)
		require.NoError(t, err, "drop %s", tbl)
	}
	stmts := []string{
		// modules/message/sql/20220418000001 的 reminders（本测试触达的列），
		// collation 与 20260711 迁移归一后的结果一致。
		"CREATE TABLE `reminders` (" +
			"  `id` BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY," +
			"  `channel_id` VARCHAR(100) NOT NULL DEFAULT ''," +
			"  `channel_type` SMALLINT NOT NULL DEFAULT 0," +
			"  `reminder_type` INT NOT NULL DEFAULT 0," +
			"  `uid` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `publisher` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `client_msg_no` VARCHAR(100) NOT NULL DEFAULT ''," +
			"  `text` VARCHAR(255) NOT NULL DEFAULT ''," +
			"  `data` VARCHAR(1000) NOT NULL DEFAULT ''," +
			"  `is_locate` SMALLINT NOT NULL DEFAULT 0," +
			"  `message_seq` BIGINT NOT NULL DEFAULT 0," +
			"  `message_id` VARCHAR(20) NOT NULL DEFAULT ''," +
			"  `version` BIGINT NOT NULL DEFAULT 0," +
			"  `is_deleted` SMALLINT NOT NULL DEFAULT 0," +
			"  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
		"CREATE TABLE `reminder_done` (" +
			"  `id` BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY," +
			"  `reminder_id` BIGINT NOT NULL DEFAULT 0," +
			"  `uid` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
		"CREATE TABLE `group_member` (" +
			"  `id` INT NOT NULL AUTO_INCREMENT PRIMARY KEY," +
			"  `group_no` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `uid` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `is_deleted` SMALLINT NOT NULL DEFAULT 0," +
			"  `status` SMALLINT NOT NULL DEFAULT 1," +
			"  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  UNIQUE KEY `group_no_uid` (`group_no`, `uid`)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=" + memberCollate,
	}
	for _, s := range stmts {
		_, err := ctx.DB().Exec(s)
		require.NoError(t, err, "create table: %s", s)
	}
}

// remMemberSeed 种下成员关系与提醒行，返回 uidA 的活跃群集合（模拟
// group.IService.ActiveMemberGroupNos 的返回）。
func remMemberSeed(t *testing.T, ctx *config.Context) []string {
	t.Helper()
	members := []struct {
		groupNo   string
		uid       string
		isDeleted int
		status    int
	}{
		{remGroupA, remUIDA, 0, int(common.GroupMemberStatusNormal)},
		{remGroupC, remUIDA, 1, int(common.GroupMemberStatusNormal)}, // 已退群
		{remGroupD, remUIDA, 0, 2},                                   // 非 Normal（拉黑）
		{remGroupB, remUIDB, 0, int(common.GroupMemberStatusNormal)}, // B 在，A 不在
	}
	for _, m := range members {
		_, err := ctx.DB().Exec(
			"INSERT INTO group_member (group_no, uid, is_deleted, status) VALUES (?,?,?,?)",
			m.groupNo, m.uid, m.isDeleted, m.status)
		require.NoError(t, err, "seed group_member")
	}

	rows := []struct {
		channelID   string
		channelType uint8
		uid         string
		publisher   string
		version     int64
	}{
		// 频道级（uid=''）——泄露面
		{remGroupA, common.ChannelTypeGroup.Uint8(), "", remUIDB, 10},
		{remGroupB, common.ChannelTypeGroup.Uint8(), "", remUIDB, 11},
		{remGroupC, common.ChannelTypeGroup.Uint8(), "", remUIDB, 12},
		{remGroupD, common.ChannelTypeGroup.Uint8(), "", remUIDB, 13},
		{remTopicA, common.ChannelTypeCommunityTopic.Uint8(), "", remUIDB, 14},
		{remTopicB, common.ChannelTypeCommunityTopic.Uint8(), "", remUIDB, 15},
		// Person：channel_id 是**收件人** uid。发给别人的那条对 A 不可见，
		// 发给 A 的那条可见 —— 当事人判定，不需要任何成员表。
		{"person_ch", common.ChannelTypePerson.Uint8(), "", remUIDB, 16},
		{remUIDA, common.ChannelTypePerson.Uint8(), "", remUIDB, 22},
		// 成员来源不可解析的类型：本次刻意不加谓词，行为必须保持不变
		{"cs_ch", common.ChannelTypeCustomerService.Uint8(), "", remUIDB, 17},
		{"community_ch", common.ChannelTypeCommunity.Uint8(), "", remUIDB, 18},
		{"info_ch", common.ChannelTypeInfo.Uint8(), "", remUIDB, 19},
		// per-uid：明确点名 A，即便 A 不在该群也必须保留（不属于本次收敛范围）
		{remGroupB, common.ChannelTypeGroup.Uint8(), remUIDA, remUIDB, 20},
		// A 自己发的频道级广播：YUJ-1377，不回给自己
		{remGroupA, common.ChannelTypeGroup.Uint8(), "", remUIDA, 21},
	}
	for _, r := range rows {
		_, err := ctx.DB().Exec(
			"INSERT INTO reminders (channel_id, channel_type, uid, publisher, version, reminder_type, text) VALUES (?,?,?,?,?,1,'[有人@我]')",
			r.channelID, r.channelType, r.uid, r.publisher, r.version)
		require.NoError(t, err, "seed reminders")
	}

	return []string{remGroupA} // C 已退群、D 被拉黑，都不算活跃成员
}

// channelIDsOf 把结果集压成 channel_id 列表，便于断言。
func channelIDsOf(rows []*remindersDetailModel) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ChannelID)
	}
	return out
}

// channelLevelIDsOf 只取**频道级**（uid=”）行的 channel_id。
//
// 必须与 channelIDsOf 分开：per-uid 行同样带 channel_id，且刻意不受成员谓词约束
// （点名给你的提醒，离群后仍应收到）。混在一起断言会把"per-uid 行仍在"误读成
// "非成员频道泄露"——本文件第一版就踩了这个坑，红的是断言不是代码。
func channelLevelIDsOf(rows []*remindersDetailModel) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.UID == "" {
			out = append(out, r.ChannelID)
		}
	}
	return out
}

// TestRemindersSync_ChannelLevelRequiresMembership 是本次修复的主判据。
//
// 注意断言的是「不含 remGroupB」而不是「只含 remGroupA」：前者才是安全属性。
func TestRemindersSync_ChannelLevelRequiresMembership(t *testing.T) {
	ctx := remMemberNewCtx(t, "utf8mb4_general_ci")
	remMemberEnsureTables(t, ctx, "utf8mb4_general_ci")
	myGroups := remMemberSeed(t, ctx)
	db := newRemindersDB(ctx)

	// channel_ids 为空 —— 渗透报告用的正是这个形状。
	got, err := db.sync(remUIDA, 0, 100, nil, myGroups)
	require.NoError(t, err)
	ids := channelLevelIDsOf(got)

	assert.NotContains(t, ids, remGroupB,
		"从未加入的群的频道级提醒必须不可见 —— 这正是被报告的泄露")
	assert.NotContains(t, ids, remGroupC, "已退群（is_deleted=1）不算成员")
	assert.NotContains(t, ids, remGroupD, "被拉黑（status!=Normal）不算成员")
	assert.NotContains(t, ids, remTopicB, "非成员父群下的子区同样不可见")

	assert.Contains(t, ids, remGroupA, "自己所属群的频道级提醒必须仍然可见")
	assert.Contains(t, ids, remTopicA, "成员父群下的子区必须仍然可见")
}

// TestRemindersSync_PerUIDRemindersUnaffected —— per-uid 行是点名给你的，
// 服务端亲自写上了你的 uid，不在本次收敛范围内。若把成员谓词误加到这一支，
// 用户离群后就再也收不到离群前被 @ 的提醒。
func TestRemindersSync_PerUIDRemindersUnaffected(t *testing.T) {
	ctx := remMemberNewCtx(t, "utf8mb4_general_ci")
	remMemberEnsureTables(t, ctx, "utf8mb4_general_ci")
	myGroups := remMemberSeed(t, ctx)
	db := newRemindersDB(ctx)

	got, err := db.sync(remUIDA, 0, 100, nil, myGroups)
	require.NoError(t, err)

	var found bool
	for _, r := range got {
		if r.UID == remUIDA && r.ChannelID == remGroupB {
			found = true
		}
	}
	assert.True(t, found, "点名 A 的 per-uid 提醒必须保留，即便 A 不是该群成员")
}

// TestRemindersSync_UnresolvableChannelTypesUnchanged —— 类型 1/3/4/6 没有可用的
// 成员来源，本次刻意不加谓词（知情保留的残留，见 brief 的 Out of scope）。
// 这条测试锁住"不误伤"：一律要求 group_member 会把它们静默丢掉。
func TestRemindersSync_UnresolvableChannelTypesUnchanged(t *testing.T) {
	ctx := remMemberNewCtx(t, "utf8mb4_general_ci")
	remMemberEnsureTables(t, ctx, "utf8mb4_general_ci")
	myGroups := remMemberSeed(t, ctx)
	db := newRemindersDB(ctx)

	got, err := db.sync(remUIDA, 0, 100, nil, myGroups)
	require.NoError(t, err)
	ids := channelIDsOf(got)

	for _, ch := range []string{"cs_ch", "community_ch", "info_ch"} {
		assert.Contains(t, ids, ch,
			"成员来源不可解析的频道类型（3/4/6）行为必须保持不变，否则是功能回归而非安全收益")
	}
	// Person（类型 1）不在此列：它有可达生产者且当事人可判定，已单独加谓词。
	assert.NotContains(t, ids, "person_ch",
		"发给别人的 DM 频道级提醒不得对第三方可见")
	assert.Contains(t, ids, remUIDA,
		"发给调用方本人的 DM 频道级提醒必须仍然可见")
}

// TestRemindersSync_ClientChannelIDsCannotWiden —— channelIDs 由客户端提供，
// 只能收窄。这条是"只修中间件"式假修复的照妖镜：把非成员群塞进 channel_ids，
// 若成员谓词缺失就会照样返回。
func TestRemindersSync_ClientChannelIDsCannotWiden(t *testing.T) {
	ctx := remMemberNewCtx(t, "utf8mb4_general_ci")
	remMemberEnsureTables(t, ctx, "utf8mb4_general_ci")
	myGroups := remMemberSeed(t, ctx)
	db := newRemindersDB(ctx)

	got, err := db.sync(remUIDA, 0, 100, []string{remGroupB, remTopicB, remGroupA}, myGroups)
	require.NoError(t, err)
	ids := channelLevelIDsOf(got)

	assert.NotContains(t, ids, remGroupB, "客户端点名非成员群也不得放行")
	assert.NotContains(t, ids, remTopicB, "子区同理")
	assert.Contains(t, ids, remGroupA, "成员群仍按 channel_ids 收窄后返回")
}

// TestRemindersSync_NoGroupsSeesNoChannelLevel —— 不属于任何群的账号（新注册、
// 或专门用来爬数据的账号）拿不到任何类型 2/5 的频道级提醒。
// 同时验证空集合不会生成非法的 `IN ()`。
func TestRemindersSync_NoGroupsSeesNoChannelLevel(t *testing.T) {
	ctx := remMemberNewCtx(t, "utf8mb4_general_ci")
	remMemberEnsureTables(t, ctx, "utf8mb4_general_ci")
	remMemberSeed(t, ctx)
	db := newRemindersDB(ctx)

	got, err := db.sync(remUIDA, 0, 100, nil, nil)
	require.NoError(t, err, "空成员集合不得生成非法 SQL")
	ids := channelLevelIDsOf(got)

	for _, ch := range []string{remGroupA, remGroupB, remGroupC, remGroupD, remTopicA, remTopicB} {
		assert.NotContains(t, ids, ch, "非任何群成员不得看到类型 2/5 的频道级提醒")
	}
	assert.Contains(t, ids, "cs_ch", "不可解析类型（3/4/6）仍不受影响")
	assert.Contains(t, ids, remUIDA, "发给本人的 DM 提醒不依赖群成员关系，仍可见")
	assert.NotContains(t, ids, "person_ch", "发给别人的 DM 仍不可见")
}

// TestRemindersSync_PublisherExclusionStillHolds —— YUJ-1377 行为不回归：
// 自己发的 @所有人 不回给自己。
func TestRemindersSync_PublisherExclusionStillHolds(t *testing.T) {
	ctx := remMemberNewCtx(t, "utf8mb4_general_ci")
	remMemberEnsureTables(t, ctx, "utf8mb4_general_ci")
	myGroups := remMemberSeed(t, ctx)
	db := newRemindersDB(ctx)

	got, err := db.sync(remUIDA, 0, 100, nil, myGroups)
	require.NoError(t, err)

	for _, r := range got {
		if r.UID == "" {
			assert.NotEqual(t, remUIDA, r.Publisher,
				"自己发布的频道级广播不得回给自己（YUJ-1377）")
		}
	}
}

// TestRemindersSync_DoneAndCursorSemanticsPreserved —— 成员谓词不得改变
// reminder_done 排除（version==0 分支）与增量游标（version>0 分支）的语义。
func TestRemindersSync_DoneAndCursorSemanticsPreserved(t *testing.T) {
	ctx := remMemberNewCtx(t, "utf8mb4_general_ci")
	remMemberEnsureTables(t, ctx, "utf8mb4_general_ci")
	myGroups := remMemberSeed(t, ctx)
	db := newRemindersDB(ctx)

	// version==0：把 remGroupA 那条标记为已读后必须消失。
	var idA int64
	_, err := ctx.DB().SelectBySql(
		"SELECT id FROM reminders WHERE channel_id=? AND uid='' AND publisher=?",
		remGroupA, remUIDB).Load(&idA)
	require.NoError(t, err)
	require.NotZero(t, idA)

	_, err = ctx.DB().Exec("INSERT INTO reminder_done (reminder_id, uid) VALUES (?,?)", idA, remUIDA)
	require.NoError(t, err)

	got, err := db.sync(remUIDA, 0, 100, nil, myGroups)
	require.NoError(t, err)
	assert.NotContains(t, channelLevelIDsOf(got), remGroupA, "已读的提醒在全量分支必须被排除")

	// version>0：游标必须越过被成员谓词隐藏的行。remGroupB(v=11)、remGroupC(v=12)、
	// remGroupD(v=13)、remTopicB(v=15) 都不可见，从 v=10 之后拉仍应拿到 remTopicA(v=14)。
	got, err = db.sync(remUIDA, 10, 100, nil, myGroups)
	require.NoError(t, err)
	assert.Contains(t, channelLevelIDsOf(got), remTopicA,
		"游标必须越过被隐藏的行，否则客户端会永久停在同一个 version")
}

// TestRemindersSync_SurvivesGroupMemberCollationMismatch 固化本次的一个设计决定。
//
// reminders 被 20260711 迁移强制转成 utf8mb4_general_ci，而 group_member 建表未声明
// CHARSET、继承建库默认 —— 于是在按 MySQL 8 默认建库的部署上，两表 collation 不同。
// 若把成员校验写成 JOIN/EXISTS 到 group_member 的列对列比较，那种部署会在提醒同步
// 热路径上抛 Error 1267；而用 COLLATE pin 绕开又会让索引失效退化成全表扫（20260711
// 迁移的注释已记录这个陷阱）。所以实现选择了绑定参数 IN —— 字面量按列的 collation
// 强制转换，跨 collation 无害。
//
// 这条测试把 group_member 建成 utf8mb4_0900_ai_ci 来复现那种部署形状。若将来有人
// 把谓词改回列对列比较，这里会直接红。
func TestRemindersSync_SurvivesGroupMemberCollationMismatch(t *testing.T) {
	ctx := remMemberNewCtx(t, "utf8mb4_general_ci")
	remMemberEnsureTables(t, ctx, "utf8mb4_0900_ai_ci")
	myGroups := remMemberSeed(t, ctx)
	db := newRemindersDB(ctx)

	got, err := db.sync(remUIDA, 0, 100, nil, myGroups)
	require.NoError(t, err,
		"group_member 与 reminders collation 不一致时查询仍须可用（Error 1267 回归）")

	ids := channelLevelIDsOf(got)
	assert.Contains(t, ids, remGroupA)
	assert.NotContains(t, ids, remGroupB)
}

// TestChannelLevelVisibility_EmptyMemberSet 是不依赖 DB 的形状守卫：空集合不得
// 落到 `IN ()`（非法 SQL），且必须是"类型 2/5 全部排除"而不是"全部放行"。
func TestChannelLevelVisibility_EmptyMemberSet(t *testing.T) {
	sql, args := channelLevelVisibility(remUIDA, nil)

	assert.NotContains(t, sql, "not in",
		"谓词必须是白名单：黑名单会放行任何枚举外的新类型（PR#717 review P1）")
	assert.Contains(t, sql, "reminders.channel_type in ?")
	assert.Equal(t,
		[]interface{}{unscopedChannelTypes, common.ChannelTypePerson.Uint8(), remUIDA},
		args,
		"空集合下应绑定：无生产者类型白名单 + Person 当事人判定；"+
			"Group/CommunityTopic 一律不可见，但 DM 当事人关系不依赖群成员关系")
	assert.Contains(t, sql, "reminders.channel_id=?",
		"空成员集合分支同样必须带 Person 当事人谓词，否则 DM 边在这条路径上仍然泄露")
}

// TestChannelLevelReminderChannelTypes 守卫"知情保留的残留"。
//
// channelLevelVisibility 只覆盖 Group 与 CommunityTopic：其余频道类型在
// octo-server 侧没有可用的成员关系表，一律要求 group_member 会把它们的合法提醒
// 静默丢掉（功能回归，非安全收益）。但 hasMention 不判频道类型，所以这些类型
// **结构上**同样能产生频道级提醒 —— 残留是真实存在的，只是没有证据表明线上有
// 这类行。
//
// 这条测试把两件事钉在一起：
//
//  1. getReminders 当前会为哪些频道类型产生频道级（uid=”）提醒；
//  2. channelLevelVisibility 当前覆盖哪些类型。
//
// 二者之差就是残留集合，必须与下面的 wantResidual 逐字相等。于是：
//   - 有人给 getReminders 加了频道类型门禁 → 差集变小 → 红，提示可以收缩残留；
//   - 有人扩展了 channelLevelVisibility → 差集变小 → 红，提示更新本表。
//
// **本测试不覆盖「往 common.ChannelType 追加新枚举值」**，而 PR 描述一度声称它覆盖
// （PR#717 review P1）：allTypes 是手写字面量，看不见新值，于是差集不变、测试照绿。
// 追加类型这一路改由**代码**兜底而不是测试：channelLevelVisibility 已从黑名单
// (`not in (2,5)`) 改成白名单，未知类型默认拒绝，失败模式是「不可见」而非「泄露」。
// 见 TestRemindersSync_UnknownChannelTypeIsDenied。
//
// 这个区分要写明白：一个声称覆盖了实则没覆盖的保证，比一个如实记录的开放缺口更糟 ——
// 它会成为将来某人引用的「我们有测试」。
//
// 任何一种都必须由人来复核，而不是悄悄改变授权边界。
func TestChannelLevelReminderChannelTypes(t *testing.T) {
	allTypes := []uint8{
		common.ChannelTypePerson.Uint8(),
		common.ChannelTypeGroup.Uint8(),
		common.ChannelTypeCustomerService.Uint8(),
		common.ChannelTypeCommunity.Uint8(),
		common.ChannelTypeCommunityTopic.Uint8(),
		common.ChannelTypeInfo.Uint8(),
	}

	m := newReminderTestMessage(t)
	emitsChannelLevel := map[uint8]bool{}
	for i, ct := range allTypes {
		msg := &config.MessageResp{
			ChannelID:   fmt.Sprintf("ch_%d", i),
			ChannelType: ct,
			FromUID:     "u_sender",
			MessageID:   int64(2000 + i),
			MessageSeq:  uint32(20 + i),
			ClientMsgNo: fmt.Sprintf("cmn_guard_%d", i),
			Payload: payloadJSON(t, map[string]interface{}{
				"type":    1,
				"content": "msg",
				"mention": map[string]interface{}{"humans": json.Number("1")},
			}),
		}
		for _, r := range m.getReminders([]*config.MessageResp{msg}) {
			if r.UID == "" {
				emitsChannelLevel[ct] = true
			}
		}
	}

	// 受成员约束的类型 = 不在白名单里的类型。白名单是唯一的放行入口，所以从它反推
	// 比解析 SQL 参数位置稳固：谓词形状变了这里不用跟着改，语义变了才会红。
	unscoped := map[uint8]bool{}
	for _, ct := range unscopedChannelTypes {
		unscoped[uint8(ct)] = true
	}
	covered := map[uint8]bool{}
	for _, ct := range allTypes {
		if !unscoped[ct] {
			covered[ct] = true
		}
	}

	residual := make([]uint8, 0, len(allTypes))
	for _, ct := range allTypes {
		if emitsChannelLevel[ct] && !covered[ct] {
			residual = append(residual, ct)
		}
	}

	wantResidual := []uint8{
		// Person(1) 已移出残留：它有可达生产者且当事人可判定，见 channelLevelVisibility
		// 的 Person 分支（PR#717 review）。
		common.ChannelTypeCustomerService.Uint8(),
		common.ChannelTypeCommunity.Uint8(),
		common.ChannelTypeInfo.Uint8(),
	}
	assert.ElementsMatch(t, wantResidual, residual,
		"能产生频道级提醒但未被成员谓词覆盖的类型集合发生了变化 —— "+
			"这会移动授权边界，必须人工复核后再更新本表（见 brief 的 Out of scope）")

	assert.True(t, covered[common.ChannelTypeGroup.Uint8()], "Group 必须被覆盖")
	assert.True(t, covered[common.ChannelTypeCommunityTopic.Uint8()], "CommunityTopic 必须被覆盖")
}

// TestActiveMemberGroupNos_IsTheAuthorizationSource 覆盖授权数据来源本身。
//
// 上面的用例都把成员集合当参数传进 sync，验证的是"给定正确集合时谓词是否正确"。
// 但集合是 group.IService.ActiveMemberGroupNos 算出来的 —— 它错了，谓词再对也没用。
// 这里用 message 侧的真实调用方式（groupService.ActiveMemberGroupNos）打真库，
// 把 fail-closed 口径钉死：退群、被拉黑都不算成员。
func TestActiveMemberGroupNos_IsTheAuthorizationSource(t *testing.T) {
	ctx := remMemberNewCtx(t, "utf8mb4_general_ci")
	remMemberEnsureTables(t, ctx, "utf8mb4_general_ci")
	remMemberSeed(t, ctx)

	svc := group.NewService(ctx)
	got, err := svc.ActiveMemberGroupNos(remUIDA)
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{remGroupA}, got,
		"只有活跃成员关系算数：remGroupC 已退群(is_deleted=1)、remGroupD 被拉黑(status!=Normal)")

	// B 在 remGroupB 里，A 不在 —— 换个人算出来的集合必须不同，否则说明 uid 没生效。
	gotB, err := svc.ActiveMemberGroupNos(remUIDB)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{remGroupB}, gotB)

	// 不存在的用户拿到空集合而不是全量 —— 空集合下 sync 不得放行任何类型 2/5。
	gotNobody, err := svc.ActiveMemberGroupNos("u_nobody")
	require.NoError(t, err)
	assert.Empty(t, gotNobody)
}

// TestRemindersSync_EndToEndWithRealMembershipSource 把两半接起来：成员集合不再是
// 手写常量，而是与 handler 同一条路径算出来的。任何一侧的口径漂移都会在这里显形。
func TestRemindersSync_EndToEndWithRealMembershipSource(t *testing.T) {
	ctx := remMemberNewCtx(t, "utf8mb4_general_ci")
	remMemberEnsureTables(t, ctx, "utf8mb4_general_ci")
	remMemberSeed(t, ctx)

	// handler 里就是这两行（api_reminders.go）。
	memberGroupNos, err := group.NewService(ctx).ActiveMemberGroupNos(remUIDA)
	require.NoError(t, err)
	got, err := newRemindersDB(ctx).sync(remUIDA, 0, 100, nil, memberGroupNos)
	require.NoError(t, err)

	ids := channelLevelIDsOf(got)
	assert.NotContains(t, ids, remGroupB, "端到端仍不得泄露非成员频道")
	assert.NotContains(t, ids, remGroupC, "退群后不再可见")
	assert.NotContains(t, ids, remGroupD, "被拉黑后不再可见")
	assert.Contains(t, ids, remGroupA)
	assert.Contains(t, ids, remTopicA, "子区经父群成员关系可见")
}

// TestRemMemberDSN_ForcesIsolatedDatabase 钉住测试夹具的一条安全属性
// （PR#717 review 的 🔵 建议）。
//
// remMemberEnsureTables 会 DROP 掉 reminders / reminder_done / group_member。
// 只要库名还能被环境变量左右，把 MSG_REMINDERS_TEST_MYSQL_ADDR 指错一次就足以
// 删掉真实数据。因此库名由代码强制，环境变量只能影响服务器与凭据。
func TestRemMemberDSN_ForcesIsolatedDatabase(t *testing.T) {
	t.Setenv("MSG_REMINDERS_TEST_MYSQL_ADDR",
		"someuser:somepass@tcp(10.0.0.1:3306)/production?charset=utf8mb4&parseTime=true")

	cfg, err := mysql.ParseDSN(remMemberDSNWithDB(t, remMemberDBName))
	require.NoError(t, err)

	assert.Equal(t, remMemberDBName, cfg.DBName,
		"库名必须被强制为隔离库；采信 env 的库名会让 DROP TABLE 打到真实库上")
	// 服务器与凭据仍取自 env —— 覆盖能力本身是要保留的，CI 上 MySQL 未必在本机。
	assert.Equal(t, "10.0.0.1:3306", cfg.Addr)
	assert.Equal(t, "someuser", cfg.User)

	// 建库连接必须落在同一台服务器上，否则隔离库建在 A、测试连到 B。
	bootCfg, err := mysql.ParseDSN(remMemberDSNWithDB(t, "information_schema"))
	require.NoError(t, err)
	assert.Equal(t, cfg.Addr, bootCfg.Addr, "建库连接与测试连接必须同服务器")
	assert.Equal(t, "information_schema", bootCfg.DBName)
}

// TestRemindersSync_EmptyCallerUIDIsRejected —— PR#717 review P2-2。
//
// 空 uid 会让 `reminders.uid=?` 退化成 `reminders.uid=”`，那是频道级行的标记，
// 于是第一个析取项直接命中全部频道级行，channelLevelVisibility 根本不会被求值。
// reviewer 在种子库上实测确认过。本函数已是授权边界，退化身份必须拒绝而不是放行。
func TestRemindersSync_EmptyCallerUIDIsRejected(t *testing.T) {
	ctx := remMemberNewCtx(t, "utf8mb4_general_ci")
	remMemberEnsureTables(t, ctx, "utf8mb4_general_ci")
	remMemberSeed(t, ctx)
	db := newRemindersDB(ctx)

	got, err := db.sync("", 0, 100, nil, nil)

	require.Error(t, err, "空 uid 必须报错，而不是返回数据")
	assert.Empty(t, got, "拒绝路径不得返回任何行")
}

// TestChannelLevelVisibility_TopicRequiresExactlyOneSeparator 钉住与
// thread.ParseChannelID 的口径一致（PR#717 review 🔵）。
//
// ParseChannelID 用 strings.Split 要求恰好两段，"g____a____b" 判非法；单靠
// substring_index 取第一段则会把它当成属于 g。谓词里的分隔符计数条件负责这个收敛。
func TestChannelLevelVisibility_TopicRequiresExactlyOneSeparator(t *testing.T) {
	sql, _ := channelLevelVisibility(remUIDA, []string{"g_any"})

	assert.Contains(t, sql, "length(reminders.channel_id)",
		"子区分支必须带分隔符计数条件，否则畸形 ID 会继承第一段的成员关系")
	assert.NotContains(t, sql, "char_length",
		"除数绑的是 Go 的 len()（字节），两侧必须都用 length() 而非 char_length()")
	assert.Contains(t, sql, "substring_index")
}

// TestRemindersSync_MalformedTopicIDDoesNotInheritParent 是上一条的行为版：
// 造一条分隔符多余的畸形子区行，确认它不会因为第一段是调用方的群而被放行。
func TestRemindersSync_MalformedTopicIDDoesNotInheritParent(t *testing.T) {
	ctx := remMemberNewCtx(t, "utf8mb4_general_ci")
	remMemberEnsureTables(t, ctx, "utf8mb4_general_ci")
	myGroups := remMemberSeed(t, ctx)

	// 第一段是 A 确实所属的群，但整体不是合法子区 ID。
	_, err := ctx.DB().Exec(
		"INSERT INTO reminders (channel_id, channel_type, uid, publisher, version, reminder_type, text) VALUES (?,?,'',?,?,1,'[有人@我]')",
		remGroupA+"____x____y", common.ChannelTypeCommunityTopic.Uint8(), remUIDB, 30)
	require.NoError(t, err)

	got, err := newRemindersDB(ctx).sync(remUIDA, 0, 100, nil, myGroups)
	require.NoError(t, err)

	assert.NotContains(t, channelLevelIDsOf(got), remGroupA+"____x____y",
		"畸形子区 ID 不得凭第一段继承父群成员关系（thread.ParseChannelID 会判其非法）")
	// 合法子区仍然可见，确认收紧没有误伤。
	assert.Contains(t, channelLevelIDsOf(got), remTopicA)
}

// TestRemindersSync_UnknownChannelTypeIsDenied 是 PR#717 review P1 的直接回归。
//
// reviewer 用 channel_type=99 实测：黑名单谓词 `not in (2,5)` 下，任何枚举外的值
// 都放行，而守卫测试因为 allTypes 是手写字面量、看不见新值，照样是绿的。
// 白名单谓词把未知类型的失败模式从「悄悄泄露」翻转成「悄悄不可见」。
//
// 99 在这里代表"任何不在 unscopedChannelTypes 也不是 2/5 的值"，包括将来往
// common.ChannelType 里追加的枚举成员。
func TestRemindersSync_UnknownChannelTypeIsDenied(t *testing.T) {
	ctx := remMemberNewCtx(t, "utf8mb4_general_ci")
	remMemberEnsureTables(t, ctx, "utf8mb4_general_ci")
	myGroups := remMemberSeed(t, ctx)

	const unknownType = 99
	_, err := ctx.DB().Exec(
		"INSERT INTO reminders (channel_id, channel_type, uid, publisher, version, reminder_type, text) VALUES (?,?,'',?,?,1,'[有人@我]')",
		"future_ch", unknownType, remUIDB, 40)
	require.NoError(t, err)

	got, err := newRemindersDB(ctx).sync(remUIDA, 0, 100, nil, myGroups)
	require.NoError(t, err)

	assert.NotContains(t, channelLevelIDsOf(got), "future_ch",
		"枚举外的频道类型必须默认拒绝；黑名单谓词会放行它并且守卫测试察觉不到")

	// 成员集合为空的分支走的是另一段 SQL，同样必须拒绝。
	gotNoGroups, err := newRemindersDB(ctx).sync(remUIDA, 0, 100, nil, nil)
	require.NoError(t, err)
	assert.NotContains(t, channelLevelIDsOf(gotNoGroups), "future_ch",
		"空成员集合分支同样必须拒绝未知类型")
}

// TestRemindersSync_TopicWithoutSeparatorIsDenied —— PR#717 review P2-3。
//
// 分隔符计数条件要求恰好一个，所以 channel_id 里一个分隔符都没有的 type-5 行现在
// 被排除；改动前 substring_index 会返回整串、从而匹配上同名的群。方向是 fail-closed
// 且 thread.BuildChannelID 不可能产生这种 ID，但这是比"收紧子区 ID 解析"字面含义
// 更宽的一个行为变化，值得钉住。
func TestRemindersSync_TopicWithoutSeparatorIsDenied(t *testing.T) {
	ctx := remMemberNewCtx(t, "utf8mb4_general_ci")
	remMemberEnsureTables(t, ctx, "utf8mb4_general_ci")
	myGroups := remMemberSeed(t, ctx)

	// channel_id 恰好等于调用方所属的群，但声明成子区类型且不含分隔符。
	_, err := ctx.DB().Exec(
		"INSERT INTO reminders (channel_id, channel_type, uid, publisher, version, reminder_type, text) VALUES (?,?,'',?,?,1,'[有人@我]')",
		remGroupA, common.ChannelTypeCommunityTopic.Uint8(), remUIDB, 41)
	require.NoError(t, err)

	got, err := newRemindersDB(ctx).sync(remUIDA, 0, 100, nil, myGroups)
	require.NoError(t, err)

	var bare int
	for _, r := range got {
		if r.UID == "" && r.ChannelID == remGroupA &&
			r.ChannelType == common.ChannelTypeCommunityTopic.Uint8() {
			bare++
		}
	}
	assert.Zero(t, bare, "无分隔符的 type-5 行不得靠整串匹配群成员关系")
	// 同名的群类型行仍然正常可见，确认没有误伤。
	assert.Contains(t, channelLevelIDsOf(got), remGroupA)
}

// TestRemindersSync_PersonRequiresPartyhood 是 PR#717 两位 reviewer 独立 block 的
// 那条的回归。
//
// 链路：AddMessagesListener 无频道类型过滤 → getReminders 只看 mention.humans、
// 原样拷贝 ChannelType → DM 发送路径接受 Person 且不清洗 mention.humans。于是
// 客户端可以构造一条 {"mention":{"humans":1}} 的 DM，落出频道级 Person 行
// (channel_id=收件人uid, publisher=发送者uid)。此前它走白名单放行 = 把「谁在跟谁
// 私聊」公开给所有登录用户，正是本 PR 要关的那类社交图谱泄露。
//
// 判定不需要任何成员表：收件人 uid 就是 channel_id。
func TestRemindersSync_PersonRequiresPartyhood(t *testing.T) {
	ctx := remMemberNewCtx(t, "utf8mb4_general_ci")
	remMemberEnsureTables(t, ctx, "utf8mb4_general_ci")
	myGroups := remMemberSeed(t, ctx)

	// C 与 B 之间的 DM，A 完全无关 —— 泄露的正是这条边。
	_, err := ctx.DB().Exec(
		"INSERT INTO reminders (channel_id, channel_type, uid, publisher, version, reminder_type, text) VALUES (?,?,'',?,?,1,'[有人@我]')",
		"u_carol", common.ChannelTypePerson.Uint8(), remUIDB, 50)
	require.NoError(t, err)

	got, err := newRemindersDB(ctx).sync(remUIDA, 0, 100, nil, myGroups)
	require.NoError(t, err)
	ids := channelLevelIDsOf(got)

	assert.NotContains(t, ids, "u_carol",
		"第三方之间的 DM 边不得暴露给无关调用方")
	assert.NotContains(t, ids, "person_ch",
		"发给别人的 DM 频道级提醒不得对第三方可见")
	assert.Contains(t, ids, remUIDA,
		"发给调用方本人的 DM 频道级提醒必须可见 —— 收紧不能误伤当事人")

	// 空成员集合分支走的是另一段 SQL，当事人判定必须同样生效。
	gotNoGroups, err := newRemindersDB(ctx).sync(remUIDA, 0, 100, nil, nil)
	require.NoError(t, err)
	idsNoGroups := channelLevelIDsOf(gotNoGroups)
	assert.NotContains(t, idsNoGroups, "u_carol", "空成员集合分支同样不得泄露 DM 边")
	assert.Contains(t, idsNoGroups, remUIDA, "当事人判定不依赖群成员关系")
}

// TestRemindersSync_PersonSenderStillExcluded —— 发送者不该收到自己发的 DM 广播。
// 现有的 NOT (uid=” AND publisher=?) 负责这个，新加的当事人谓词不得把它推翻。
func TestRemindersSync_PersonSenderStillExcluded(t *testing.T) {
	ctx := remMemberNewCtx(t, "utf8mb4_general_ci")
	remMemberEnsureTables(t, ctx, "utf8mb4_general_ci")
	myGroups := remMemberSeed(t, ctx)

	// A 给自己发（收件人=A 且 publisher=A）——两个条件同时成立的边界情形。
	_, err := ctx.DB().Exec(
		"INSERT INTO reminders (channel_id, channel_type, uid, publisher, version, reminder_type, text) VALUES (?,?,'',?,?,1,'[有人@我]')",
		remUIDA, common.ChannelTypePerson.Uint8(), remUIDA, 51)
	require.NoError(t, err)

	got, err := newRemindersDB(ctx).sync(remUIDA, 0, 100, nil, myGroups)
	require.NoError(t, err)

	var selfPublished int
	for _, r := range got {
		if r.UID == "" && r.ChannelID == remUIDA && r.Publisher == remUIDA {
			selfPublished++
		}
	}
	assert.Zero(t, selfPublished,
		"自己发布的频道级广播仍不得回给自己（YUJ-1377），当事人谓词不覆盖它")
}
