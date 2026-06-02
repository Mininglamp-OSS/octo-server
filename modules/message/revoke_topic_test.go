package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/stretchr/testify/assert"
)

// issue #222: 子区（CommunityTopic）撤回权限单元测试。
//
// 验证 hasRevokePermission 对子区的处理：解析出父群后，复用群聊撤回权限矩阵
// （基于父群角色判断），忽略子区创建人概念。撤回自己消息 / bot-owner 等短路
// 分支在群聊用例（revoke_bot_test.go）已覆盖，这里聚焦父群角色矩阵与子区
// 频道 ID 解析。
//
// fakeGroupService.GetMember 忽略 groupNo、按 uid 返回，因此父群 GroupNo 用
// 解析自 topicChannelID 的 "PG1" 即可，无需在 members 里区分群号。

const (
	// topicChannelID 形如 "父群No____shortID"，thread.ParseChannelID 解析得父群 "PG1"。
	topicChannelID  = "PG1____123456789012345"
	topicCreaterUID = "topic_creater"
	topicManagerUID = "topic_manager"
	topicManager2ID = "topic_manager2"
	topicNormalUID  = "topic_normal"
	topicNormal2ID  = "topic_normal2"
	topicOutsiderID = "topic_outsider"
)

func member(uid string, role common.GroupMemberRole) *group.MemberResp {
	return &group.MemberResp{GroupNo: "PG1", UID: uid, Role: int(role)}
}

func TestHasRevokePermission_CommunityTopic_ParentRoleMatrix(t *testing.T) {
	tests := []struct {
		name     string
		members  map[string]*group.MemberResp // key: uid（父群成员关系，nil 表示不在群）
		fromUID  string
		loginUID string
		want     bool
	}{
		{
			name: "群主可撤回普通成员消息",
			members: map[string]*group.MemberResp{
				topicCreaterUID: member(topicCreaterUID, common.GroupMemberRoleCreater),
				topicNormalUID:  member(topicNormalUID, common.GroupMemberRoleNormal),
			},
			fromUID:  topicNormalUID,
			loginUID: topicCreaterUID,
			want:     true,
		},
		{
			name: "群主可撤回管理员消息",
			members: map[string]*group.MemberResp{
				topicCreaterUID: member(topicCreaterUID, common.GroupMemberRoleCreater),
				topicManagerUID: member(topicManagerUID, common.GroupMemberRoleManager),
			},
			fromUID:  topicManagerUID,
			loginUID: topicCreaterUID,
			want:     true,
		},
		{
			name: "管理员可撤回普通成员消息",
			members: map[string]*group.MemberResp{
				topicManagerUID: member(topicManagerUID, common.GroupMemberRoleManager),
				topicNormalUID:  member(topicNormalUID, common.GroupMemberRoleNormal),
			},
			fromUID:  topicNormalUID,
			loginUID: topicManagerUID,
			want:     true,
		},
		{
			name: "管理员不能撤回其他管理员消息",
			members: map[string]*group.MemberResp{
				topicManagerUID: member(topicManagerUID, common.GroupMemberRoleManager),
				topicManager2ID: member(topicManager2ID, common.GroupMemberRoleManager),
			},
			fromUID:  topicManager2ID,
			loginUID: topicManagerUID,
			want:     false,
		},
		{
			name: "管理员不能撤回群主消息",
			members: map[string]*group.MemberResp{
				topicManagerUID: member(topicManagerUID, common.GroupMemberRoleManager),
				topicCreaterUID: member(topicCreaterUID, common.GroupMemberRoleCreater),
			},
			fromUID:  topicCreaterUID,
			loginUID: topicManagerUID,
			want:     false,
		},
		{
			name: "普通成员不能撤回他人消息",
			members: map[string]*group.MemberResp{
				topicNormalUID: member(topicNormalUID, common.GroupMemberRoleNormal),
				topicNormal2ID: member(topicNormal2ID, common.GroupMemberRoleNormal),
			},
			fromUID:  topicNormal2ID,
			loginUID: topicNormalUID,
			want:     false,
		},
		{
			name: "非父群成员不能撤回消息",
			members: map[string]*group.MemberResp{
				topicNormalUID: member(topicNormalUID, common.GroupMemberRoleNormal),
				// topicOutsiderID 不在父群 → loginMember 为 nil
			},
			fromUID:  topicNormalUID,
			loginUID: topicOutsiderID,
			want:     false,
		},
		{
			name: "发送者已退群-管理员可撤回",
			members: map[string]*group.MemberResp{
				topicManagerUID: member(topicManagerUID, common.GroupMemberRoleManager),
				// fromUID 已退群 → fromMember 为 nil
			},
			fromUID:  topicNormalUID,
			loginUID: topicManagerUID,
			want:     true,
		},
		{
			name: "发送者已退群-普通成员不可撤回",
			members: map[string]*group.MemberResp{
				topicNormalUID: member(topicNormalUID, common.GroupMemberRoleNormal),
				// fromUID 已退群 → fromMember 为 nil
			},
			fromUID:  topicNormal2ID,
			loginUID: topicNormalUID,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// robotService 注入空实现：消息发送者非 bot，GetCreatorUID 返回空，
			// 不会命中 bot-owner 短路，确保走父群角色矩阵。
			rb := &fakeRobotService{byRobotID: map[string]string{}}
			m := newRevokeTestMessage(rb, &fakeGroupService{members: tt.members})

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

// 子区频道 ID 无法解析为父群时，fail-closed 返回 false 且不报错。
func TestHasRevokePermission_CommunityTopic_InvalidChannelID(t *testing.T) {
	rb := &fakeRobotService{byRobotID: map[string]string{}}
	// 即便操作者在某群是群主，channelID 解析失败也必须拒绝。
	gs := &fakeGroupService{members: map[string]*group.MemberResp{
		topicCreaterUID: member(topicCreaterUID, common.GroupMemberRoleCreater),
		topicNormalUID:  member(topicNormalUID, common.GroupMemberRoleNormal),
	}}
	m := newRevokeTestMessage(rb, gs)

	msg := &messageModel{
		FromUID:     topicNormalUID,
		ChannelID:   "no-separator-here", // 缺少 "____"，ParseChannelID 报错
		ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
	}

	allow, err := m.hasRevokePermission(msg, topicCreaterUID)
	assert.NoError(t, err, "解析失败应 fail-closed，不向上传播 error")
	assert.False(t, allow, "子区频道ID解析失败必须拒绝撤回")
}

// 子区内 bot 创建者可撤回自己 bot 发的消息（验证 bot-owner 短路对子区同样生效）。
func TestHasRevokePermission_CommunityTopic_BotOwner(t *testing.T) {
	rb := &fakeRobotService{byRobotID: map[string]string{botUID: ownerUID}}
	// 故意让操作者在父群只是普通成员，确保若 bot-owner 分支未命中必然落 false。
	gs := &fakeGroupService{members: map[string]*group.MemberResp{
		ownerUID: member(ownerUID, common.GroupMemberRoleNormal),
		botUID:   member(botUID, common.GroupMemberRoleNormal),
	}}
	m := newRevokeTestMessage(rb, gs)

	msg := &messageModel{
		FromUID:     botUID,
		ChannelID:   topicChannelID,
		ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
	}

	allow, err := m.hasRevokePermission(msg, ownerUID)
	assert.NoError(t, err)
	assert.True(t, allow, "bot 创建者应能在子区撤回自己 bot 发的消息")
}
