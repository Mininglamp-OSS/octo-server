//go:build integration

package message

// =============================================================================
// E2E coverage for YUJ-4229 必修 2 — checkChannelAccess channel_type 区分。
//
// AuthorizeThreadFollow (FollowThread, 子区/CommunityTopic 分支) require 父群
// 「活跃成员」(ExistMemberActive)：被拉黑(status=Blacklist、is_deleted=0)的父群
// 成员不能 follow 子区、不被 OnThreadCreated fanout 物化子区 ext、不收新子区
// 创建通知。
//
// AuthorizeChannelFollow (FollowChannel, GROUP 分支) 保留 permissive
// ExistMember 语义：被拉黑成员对 GROUP 本身的 follow 不被 over-block（与 server
// 各处 GROUP 分支一致）。
//
// 复用 default_followed_group_guard_e2e_test.go 的 ensureGuardE2ETables /
// seedGroupRow / seedGroupMember helper（同包），额外建一张最小 thread 表。
// =============================================================================

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	convext "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ensureThreadE2ETable 建一张最小 thread 表，仅含 AuthorizeThreadFollow 走的
// QueryActiveByGroupShortIDs 读到的列。
func ensureThreadE2ETable(t *testing.T, ctx *config.Context) {
	t.Helper()
	_, err := ctx.DB().Exec("DROP TABLE IF EXISTS thread")
	require.NoError(t, err, "drop thread")
	_, err = ctx.DB().Exec(
		"CREATE TABLE `thread` (" +
			"  `id` INT NOT NULL AUTO_INCREMENT PRIMARY KEY," +
			"  `group_no` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `short_id` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `name` VARCHAR(100) NOT NULL DEFAULT ''," +
			"  `creator_uid` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `status` INT NOT NULL DEFAULT 1," +
			"  `last_message_at` TIMESTAMP NULL DEFAULT NULL," +
			"  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  UNIQUE KEY `uk_short` (`short_id`)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4")
	require.NoError(t, err, "create thread")
}

func seedThreadE2E(t *testing.T, ctx *config.Context, groupNo, shortID string) {
	t.Helper()
	_, err := ctx.DB().Exec(
		"INSERT INTO thread (group_no, short_id, name, creator_uid, status) VALUES (?, ?, 'topic', '', 1)",
		groupNo, shortID,
	)
	require.NoError(t, err, "seedThreadE2E %s/%s", groupNo, shortID)
}

func setMemberStatusE2E(t *testing.T, ctx *config.Context, groupNo, uid string, status common.GroupMemberStatus) {
	t.Helper()
	_, err := ctx.DB().Exec(
		"UPDATE group_member SET status=? WHERE group_no=? AND uid=?",
		int(status), groupNo, uid,
	)
	require.NoError(t, err, "setMemberStatusE2E %s/%s", groupNo, uid)
}

// TestE2E_ThreadFollow_BlacklistedParentMember verifies the channel_type split:
// FollowThread (CommunityTopic) requires active membership; FollowChannel (GROUP)
// stays permissive.
func TestE2E_ThreadFollow_BlacklistedParentMember(t *testing.T) {
	ctx := newSidebarIntegCtx(t)
	ensureGuardE2ETables(t, ctx)
	ensureThreadE2ETable(t, ctx)
	t.Cleanup(func() { cleanGuardE2ERows(t, ctx) })

	const (
		uid     = "e2e-fl-user"
		spaceA  = "e2e-space-a"
		groupNo = "e2e-fl-group"
		shortID = "e2e-fl-topic"
	)

	seedGroupRow(t, ctx, groupNo, spaceA, 1 /* GroupStatusNormal */)
	seedGroupMember(t, ctx, groupNo, uid, 0, "")
	seedThreadE2E(t, ctx, groupNo, shortID)

	checker := newThreadAuthChecker(ctx)

	// --- 正常成员：两条 follow 都放行 ---
	t.Run("normal member can follow both thread and channel", func(t *testing.T) {
		require.NoError(t, checker.AuthorizeThreadFollow(uid, spaceA, groupNo, shortID),
			"正常成员 follow 子区不应被拦")
		require.NoError(t, checker.AuthorizeChannelFollow(uid, spaceA, groupNo),
			"正常成员 follow GROUP 不应被拦")
	})

	// --- 被拉黑：子区分支拒，GROUP 分支保留 permissive 放行 ---
	setMemberStatusE2E(t, ctx, groupNo, uid, common.GroupMemberStatusBlacklist)

	t.Run("blacklisted member denied FollowThread (CommunityTopic branch)", func(t *testing.T) {
		err := checker.AuthorizeThreadFollow(uid, spaceA, groupNo, shortID)
		require.Error(t, err, "被拉黑父群成员 follow 子区必须被拒")
		assert.True(t, errors.Is(err, convext.ErrThreadForbidden),
			"应返回 ErrThreadForbidden，got %v", err)
	})

	t.Run("blacklisted member still allowed FollowChannel (GROUP branch permissive)", func(t *testing.T) {
		err := checker.AuthorizeChannelFollow(uid, spaceA, groupNo)
		assert.NoError(t, err,
			"GROUP 分支保留 permissive ExistMember，不应 over-block 被拉黑成员的 channel follow")
	})

	// --- 被移出（is_deleted=1）：两条都拒（既有语义，纵深防御） ---
	t.Run("removed member denied both", func(t *testing.T) {
		_, err := ctx.DB().Exec(
			"UPDATE group_member SET is_deleted=1 WHERE group_no=? AND uid=?", groupNo, uid)
		require.NoError(t, err)
		assert.Error(t, checker.AuthorizeThreadFollow(uid, spaceA, groupNo, shortID),
			"被移出成员 follow 子区必须被拒")
		assert.Error(t, checker.AuthorizeChannelFollow(uid, spaceA, groupNo),
			"被移出成员 follow GROUP 也被拒")
	})
}
