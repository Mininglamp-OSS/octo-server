package group

import (
	"fmt"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ensureThreadTables 为 group 模块的测试脚手架补上 thread / thread_member 表。
// group 模块的测试不会自动运行 thread 模块的迁移，沿用 TestMain 里手工建表的同款做法。
func ensureThreadTables(t *testing.T, f *Group) {
	t.Helper()
	ensureThreadTablesByCtx(t, f.ctx)
}

// ensureThreadTablesByCtx 同 ensureThreadTables 但直接接受 ctx，方便 service 层 / 纯 ctx 测试复用。
func ensureThreadTablesByCtx(t *testing.T, ctx *config.Context) {
	t.Helper()
	_, err := ctx.DB().UpdateBySql(`CREATE TABLE IF NOT EXISTS thread (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		short_id VARCHAR(32) NOT NULL,
		group_no VARCHAR(40) NOT NULL,
		name VARCHAR(100) NOT NULL DEFAULT '',
		creator_uid VARCHAR(40) NOT NULL DEFAULT '',
		source_message_id BIGINT DEFAULT NULL,
		status TINYINT NOT NULL DEFAULT 1,
		version BIGINT NOT NULL DEFAULT 0,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		UNIQUE KEY uk_short_id (short_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().UpdateBySql(`CREATE TABLE IF NOT EXISTS thread_member (
		id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		thread_id BIGINT UNSIGNED NOT NULL,
		uid VARCHAR(40) NOT NULL,
		role TINYINT NOT NULL DEFAULT 0,
		version BIGINT NOT NULL DEFAULT 0,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		UNIQUE KEY uk_thread_uid (thread_id, uid)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`).Exec()
	require.NoError(t, err)
	// CleanAllTables 之后再 delete 一遍，保证干净
	_, _ = ctx.DB().DeleteFrom("thread_member").Exec()
	_, _ = ctx.DB().DeleteFrom("thread").Exec()
}

// TestGroupExit_CascadeBot_AlsoRemovesFromThread 回归 YUJ-52 / Mininglamp-OSS/octo-server#1189：
//
// PR #1187 在 groupExit 的 bot 级联分支里只做了 IM 订阅摘除 + 发系统 Tip，
// 漏清 thread_member（子区成员）和 Space 内的 pinned，导致被级联带走的 bot
// 还能在子区里收到消息。修复方式和 service.go kick 路径对齐：在 cascade 块里
// 对每个 bot 都调用 removeUserFromGroupThreads + RemovePinnedForUserInSpace。
//
// 本用例直接 seed 子区 + thread_member，然后跑一遍 groupExit 的 cascade 序列
// （cascadeRemoveBotsInvitedByUIDTx → 对每个 bot 调 removeUserFromGroupThreads），
// 在 DB 层直查 thread_member 断言 bot 的子区成员记录已被清理。
//
// HTTP handler 在本测试环境里会被 IMRemoveSubscriber 的 200 前置检查挡住（无 IM 服务），
// 这里选择 DB-level 组装，和 bot_cascade_test.go 里其他 QuitGroup_* 用例的做法一致。
func TestGroupExit_CascadeBot_AlsoRemovesFromThread(t *testing.T) {
	svc, userDB := setupServiceTest(t)
	s := svc.(*Service)
	f := New(s.ctx)
	ensureThreadTables(t, f)

	// --- 1. 用户：群主 owner、退群者 leaver（非群主，cascade 前置条件）、bot 本体 ---
	insertTestUsers(t, userDB, "owner", "leaver")
	botUID := "yuj52_bot_by_leaver"
	require.NoError(t, userDB.Insert(&user.Model{UID: botUID, Name: "bot-by-leaver", ShortNo: "sn_" + botUID, Robot: 1}))
	// robot 行：creator_uid = leaver 才会被级联 SQL 命中
	_, err := f.ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid) VALUES (?, 1, ?)",
		botUID, "leaver",
	).Exec()
	require.NoError(t, err)

	// --- 2. 群：owner 是创建者，leaver 是普通成员（非 creator，满足 cascade 守卫） ---
	spaceID := "space_yuj52"
	groupNo := "g_yuj52_cascade"
	require.NoError(t, f.db.Insert(&Model{
		GroupNo: groupNo,
		Name:    "yuj52-cascade-thread",
		Creator: "owner",
		SpaceID: spaceID,
		Status:  1,
	}))
	require.NoError(t, f.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "owner", Role: MemberRoleCreator,
		Status: 1, Version: 1, Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	}))
	require.NoError(t, f.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "leaver", Role: MemberRoleCommon,
		Status: 1, Version: 1, Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	}))
	require.NoError(t, f.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: botUID, Role: MemberRoleCommon, Robot: 1,
		Status: 1, Version: 1, Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	}))

	// --- 3. 子区：bot 是子区成员（这正是 #1187 漏清的场景） ---
	res, err := f.ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("yuj52_thread", groupNo, "yuj52-sub", "owner", 1, 1).Exec()
	require.NoError(t, err)
	threadID, err := res.LastInsertId()
	require.NoError(t, err)
	_, err = f.ctx.DB().InsertInto("thread_member").
		Columns("thread_id", "uid", "role", "version").
		Values(threadID, botUID, 0, 1).Exec()
	require.NoError(t, err)

	// 预置断言：bot 此刻确实是子区成员
	var preCount int
	_, err = f.ctx.DB().Select("count(*)").From("thread_member").
		Where("thread_id=? AND uid=?", threadID, botUID).Load(&preCount)
	require.NoError(t, err)
	require.Equal(t, 1, preCount, "前置：bot 应已登记进 thread_member")

	// --- 4. 模拟 groupExit 的 cascade 序列 ---
	// 4a. 删除 leaver 的 group_member（api.go groupExit 先做 DeleteMemberTx）
	tx, err := f.ctx.DB().Begin()
	require.NoError(t, err)
	leaverVersion, _ := f.ctx.GenSeq(common.GroupMemberSeqKey)
	require.NoError(t, f.db.DeleteMemberTx(groupNo, "leaver", leaverVersion, tx))

	// 4b. 级联带走 leaver 拉入的 bot（api.go cascade 分支入口）
	cascadedUIDs, err := cascadeRemoveBotsInvitedByUIDTx(f.db, f.ctx, groupNo, "leaver", tx)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.ElementsMatch(t, []string{botUID}, cascadedUIDs, "cascade SQL 必须挑中 leaver 自己的 bot")

	// 4c. YUJ-52 修复的核心：对每个级联 bot 清理 thread_member + pinned
	//    这段和 api.go cascade 块里 "for _, bu := range cascadedBotUsers" 的循环完全一致
	for _, botU := range cascadedUIDs {
		f.removeUserFromGroupThreads(groupNo, botU, spaceID)
		user.RemovePinnedForUserInSpace(botU, spaceID, groupNo, common.ChannelTypeGroup.Uint8())
	}

	// --- 5. 核心断言：DB 层直接查 thread_member ---
	var postCount int
	_, err = f.ctx.DB().Select("count(*)").From("thread_member").
		Where("thread_id=? AND uid=?", threadID, botUID).Load(&postCount)
	require.NoError(t, err)
	assert.Equal(t, 0, postCount, "级联移除 bot 时必须同步清理其 thread_member 行（YUJ-52 / #1189）")

	// 旁证：bot 已退出群，语义未回归
	existInGroup, err := f.db.ExistMember(botUID, groupNo)
	require.NoError(t, err)
	assert.False(t, existInGroup, "cascade 必须带走 bot 的 group_member")
}
