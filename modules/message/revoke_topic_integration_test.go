//go:build integration

package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// issue #222 集成测试：用真实 groupService（真实 MySQL）验证子区
// （CommunityTopic）撤回权限端到端链路。
//
// 与 revoke_topic_test.go 的单元测试互补：单元测试用的 fakeGroupService 忽略
// groupNo（按 uid 返回），无法验证「子区用解析出的父群号查成员角色」是否正确。
// 这里建真实的 group_member 行、调真实 group.Service.GetMember——若
// hasRevokePermission 误用子区 channelID（含 "____shortID"）而非父群号去查，
// GetMember 将查不到任何成员、所有用例都会拒绝，从而暴露回归。
//
// 与 default_followed_group_guard_e2e_test.go / api_sidebar_integration_test.go
// 一致，本测试挂在 `integration` build tag 下、跑在独立的 conv_ext_test 库上：
//   - CI 常规 `go test ./...`（不带 tag）不编译本文件，因此不会与 message 包内
//     裸建表的单元测试（api_channel_files_test.go 等）在共享 test 库的 sql-migrate
//     上冲突（参见 issue #17）。
//   - 手动 `go test -tags=integration -run TestHasRevokePermission_CommunityTopic_Integration ./modules/message/`
//     时才运行，针对 conv_ext_test 库，与 test 库隔离。
//
// robotService 注入空实现（消息发送者非 bot），聚焦父群角色矩阵这条真实 DB 链路；
// bot-owner / channelID 解析失败等分支由 revoke_topic_test.go 单元测试覆盖。
func TestHasRevokePermission_CommunityTopic_Integration(t *testing.T) {
	ctx := newSidebarIntegCtx(t)
	ensureGuardE2ETables(t, ctx) // DROP+CREATE group/group_member/group_setting，天然清空

	// group.Service.GetMember 会 LEFT JOIN user 取 name；该库不一定有 user 表，
	// 建一个最小表满足 JOIN（成员 name 由 IFNULL 兜底，无需插行）。
	_, err := ctx.DB().Exec("CREATE TABLE IF NOT EXISTS `user` (" +
		"`id` INT NOT NULL AUTO_INCREMENT PRIMARY KEY," +
		"`uid` VARCHAR(40) NOT NULL DEFAULT ''," +
		"`name` VARCHAR(100) NOT NULL DEFAULT ''" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4")
	require.NoError(t, err, "create minimal user table")

	const (
		groupNo    = "e2eint-grp"
		creatorUID = "e2eint-creator"
		managerA   = "e2eint-mgr-a"
		managerB   = "e2eint-mgr-b"
		normalA    = "e2eint-normal-a"
		normalB    = "e2eint-normal-b"
		outsider   = "e2eint-outsider"
	)
	// 子区频道 ID 形如 "父群No____shortID"；ParseChannelID 解析得父群 groupNo。
	topicChannelID := groupNo + "____123456789012345"

	seedMember := func(uid string, role int) {
		_, err := ctx.DB().Exec(
			"INSERT INTO group_member (group_no, uid, role, is_deleted, status, version, vercode) "+
				"VALUES (?, ?, ?, 0, 1, 1, ?)",
			groupNo, uid, role, uid+"-vc",
		)
		require.NoError(t, err, "seed group_member %s", uid)
	}
	seedMember(creatorUID, group.MemberRoleCreator)
	seedMember(managerA, group.MemberRoleManager)
	seedMember(managerB, group.MemberRoleManager)
	seedMember(normalA, group.MemberRoleCommon)
	seedMember(normalB, group.MemberRoleCommon)
	// outsider 故意不入群

	// 真实 group.Service（查真实 group_member 表）+ 空 robot 服务（发送者非 bot）。
	m := newRevokeTestMessage(&fakeRobotService{byRobotID: map[string]string{}}, group.NewService(ctx))

	tests := []struct {
		name     string
		fromUID  string
		loginUID string
		want     bool
	}{
		{"群主可撤回普通成员消息", normalA, creatorUID, true},
		{"群主可撤回管理员消息", managerA, creatorUID, true},
		{"管理员可撤回普通成员消息", normalA, managerA, true},
		{"管理员不能撤回其他管理员消息", managerB, managerA, false},
		{"管理员不能撤回群主消息", creatorUID, managerA, false},
		{"普通成员不能撤回他人消息", normalB, normalA, false},
		{"非父群成员不能撤回消息", normalA, outsider, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &messageModel{
				FromUID:     tt.fromUID,
				ChannelID:   topicChannelID,
				ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
			}
			allow, err := m.hasRevokePermission(msg, tt.loginUID)
			assert.NoError(t, err)
			assert.Equal(t, tt.want, allow)
		})
	}
}
