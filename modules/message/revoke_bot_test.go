package message

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/stretchr/testify/assert"
)

// YUJ-60: hasRevokePermission bot-owner 分支单元测试。
//
// 目标是在不起 HTTP server / DB 的前提下，直接校验 hasRevokePermission
// 对 bot-owner 场景的分支逻辑：
//   1. bot 创建者 == loginUID → true
//   2. bot 创建者 != loginUID → 继续走既有分支（DM 场景返回 false，
//      群管理员场景返回 true）
//   3. creator_uid 为空（历史数据）→ 继续走既有分支
//   4. robotService 返回 error → 降级继续走既有分支，不向上传播

// --- 最小 fake 实现，只满足本组用例 ---

type fakeRobotService struct {
	robot.IService

	byRobotID map[string]string
	err       error
}

func (f *fakeRobotService) GetCreatorUID(robotID string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if f.byRobotID == nil {
		return "", nil
	}
	return f.byRobotID[robotID], nil
}

// fakeGroupService 只需要实现 hasRevokePermission 里实际调用到的 GetMember，
// 其余方法由嵌入的 group.IService 占位；若被误调用会 nil-ptr panic，
// 测试里只覆盖已写到的代码路径即可。
type fakeGroupService struct {
	group.IService
	members map[string]*group.MemberResp // key: uid
}

func (f *fakeGroupService) GetMember(groupNo, uid string) (*group.MemberResp, error) {
	return f.members[uid], nil
}

// newRevokeTestMessage 组装一个最小可用的 *Message，仅注入
// hasRevokePermission 需要的两个字段 + 一个非 nil logger。
func newRevokeTestMessage(robotSvc robot.IService, groupSvc group.IService) *Message {
	return &Message{
		Log:          log.NewTLog("revoke-test"),
		robotService: robotSvc,
		groupService: groupSvc,
	}
}

const (
	botUID     = "bot_abc"
	ownerUID   = "owner_u1"
	strangerID = "user_u2"
	adminUID   = "admin_u3"
	groupNo    = "G1"
)

// 场景 1: creator_uid == loginUID, DM 场景
func TestHasRevokePermission_BotOwner_DM(t *testing.T) {
	rb := &fakeRobotService{byRobotID: map[string]string{botUID: ownerUID}}
	m := newRevokeTestMessage(rb, &fakeGroupService{})

	msg := &messageModel{
		FromUID:     botUID,
		ChannelID:   ownerUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
	}

	allow, err := m.hasRevokePermission(msg, ownerUID)
	assert.NoError(t, err)
	assert.True(t, allow, "bot 创建者应能在 DM 中撤回自己 bot 发的消息")
}

// 场景 2: creator_uid == loginUID, 群聊场景, loginUID 非群管理员
// 期望：bot-owner 分支在群管理员分支之前命中 → true
func TestHasRevokePermission_BotOwner_Group(t *testing.T) {
	rb := &fakeRobotService{byRobotID: map[string]string{botUID: ownerUID}}
	// 特意让 groupService 返回普通成员，保证若 bot-owner 分支未命中必然落 false。
	gs := &fakeGroupService{members: map[string]*group.MemberResp{
		ownerUID: {GroupNo: groupNo, UID: ownerUID, Role: int(common.GroupMemberRoleNormal)},
		botUID:   {GroupNo: groupNo, UID: botUID, Role: int(common.GroupMemberRoleNormal)},
	}}
	m := newRevokeTestMessage(rb, gs)

	msg := &messageModel{
		FromUID:     botUID,
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
	}

	allow, err := m.hasRevokePermission(msg, ownerUID)
	assert.NoError(t, err)
	assert.True(t, allow, "bot-owner 分支应优先于群管理员分支命中")
}

// 场景 3: creator_uid != loginUID, DM 场景 → false
func TestHasRevokePermission_NotBotOwner_DM(t *testing.T) {
	rb := &fakeRobotService{byRobotID: map[string]string{botUID: ownerUID}}
	m := newRevokeTestMessage(rb, &fakeGroupService{})

	msg := &messageModel{
		FromUID:     botUID,
		ChannelID:   strangerID,
		ChannelType: common.ChannelTypePerson.Uint8(),
	}

	allow, err := m.hasRevokePermission(msg, strangerID)
	assert.NoError(t, err)
	assert.False(t, allow, "非 bot 创建者在 DM 中不应能撤回 bot 消息")
}

// 场景 4: creator_uid != loginUID, 群聊, loginUID 普通成员 → false
func TestHasRevokePermission_NotBotOwner_Group_NotAdmin(t *testing.T) {
	rb := &fakeRobotService{byRobotID: map[string]string{botUID: ownerUID}}
	gs := &fakeGroupService{members: map[string]*group.MemberResp{
		strangerID: {GroupNo: groupNo, UID: strangerID, Role: int(common.GroupMemberRoleNormal)},
		botUID:     {GroupNo: groupNo, UID: botUID, Role: int(common.GroupMemberRoleNormal)},
	}}
	m := newRevokeTestMessage(rb, gs)

	msg := &messageModel{
		FromUID:     botUID,
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
	}

	allow, err := m.hasRevokePermission(msg, strangerID)
	assert.NoError(t, err)
	assert.False(t, allow, "非 bot-owner 普通成员不应能撤回 bot 消息")
}

// 场景 5: creator_uid != loginUID, 群聊, loginUID 群管理员 → true (走管理员分支)
func TestHasRevokePermission_NotBotOwner_Group_Admin(t *testing.T) {
	rb := &fakeRobotService{byRobotID: map[string]string{botUID: ownerUID}}
	gs := &fakeGroupService{members: map[string]*group.MemberResp{
		adminUID: {GroupNo: groupNo, UID: adminUID, Role: int(common.GroupMemberRoleManager), Status: int(common.GroupMemberStatusNormal)},
		botUID:   {GroupNo: groupNo, UID: botUID, Role: int(common.GroupMemberRoleNormal), Status: int(common.GroupMemberStatusNormal)},
	}}
	m := newRevokeTestMessage(rb, gs)

	msg := &messageModel{
		FromUID:     botUID,
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
	}

	allow, err := m.hasRevokePermission(msg, adminUID)
	assert.NoError(t, err)
	assert.True(t, allow, "群管理员仍应能撤回非自己拥有的 bot 消息")
}

// 场景 6: robot 存在但 creator_uid 为空（历史数据） → 落到既有分支
// DM 场景下最终返回 false
func TestHasRevokePermission_EmptyCreatorUID(t *testing.T) {
	rb := &fakeRobotService{byRobotID: map[string]string{botUID: ""}}
	m := newRevokeTestMessage(rb, &fakeGroupService{})

	msg := &messageModel{
		FromUID:     botUID,
		ChannelID:   ownerUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
	}

	allow, err := m.hasRevokePermission(msg, ownerUID)
	assert.NoError(t, err)
	assert.False(t, allow, "creator_uid 为空时不应意外授予 bot-owner 权限")
}

// 场景 7-blacklisted: 黑名单 manager 不能撤回 webhook 消息（fromMember==nil 分支）
//
// PR Mininglamp-OSS/octo-server#31 round-4 (Jerry-Xin)。webhook 消息的 FromUID
// 形如 "iwh_..."，不是真实群成员 → 走 fromMember==nil 分支。该分支历史上
// 「只要 loginMember.Role != normal 就允许撤回」，忽略 loginMember.Status，
// 等于让被踢出（status=Blacklist 但 is_deleted=0）的 manager 仍能任意撤回。
// 修复后必须 status=Normal 才视为有效管理者。
func TestHasRevokePermission_BlacklistedManager_CannotRevokeWebhookMsg(t *testing.T) {
	const webhookUID = "iwh_abcdef" // 模拟 webhook 发送方（非群成员）
	rb := &fakeRobotService{}       // bot-owner 分支不命中
	gs := &fakeGroupService{members: map[string]*group.MemberResp{
		// 黑名单 manager：is_deleted=0 但 status=Blacklist —— 脏数据 / 被踢出但未硬删
		adminUID: {GroupNo: groupNo, UID: adminUID, Role: int(common.GroupMemberRoleManager), Status: int(common.GroupMemberStatusBlacklist)},
		// webhook 不写 member 表，故 webhookUID 不存在 → GetMember 返回 nil
	}}
	m := newRevokeTestMessage(rb, gs)

	msg := &messageModel{
		FromUID:     webhookUID,
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
	}

	allow, err := m.hasRevokePermission(msg, adminUID)
	assert.NoError(t, err)
	assert.False(t, allow, "黑名单 manager 不应能撤回 webhook 消息")
}

// 场景 7-blacklisted-2: 黑名单 manager 也不能撤回普通成员消息
// （证明修复不只针对 fromMember==nil 分支，所有 group-revoke 路径都受 status 校验拦截）
func TestHasRevokePermission_BlacklistedManager_CannotRevokeNormalMemberMsg(t *testing.T) {
	rb := &fakeRobotService{} // bot-owner 分支不命中
	gs := &fakeGroupService{members: map[string]*group.MemberResp{
		adminUID:   {GroupNo: groupNo, UID: adminUID, Role: int(common.GroupMemberRoleManager), Status: int(common.GroupMemberStatusBlacklist)},
		strangerID: {GroupNo: groupNo, UID: strangerID, Role: int(common.GroupMemberRoleNormal), Status: int(common.GroupMemberStatusNormal)},
	}}
	m := newRevokeTestMessage(rb, gs)

	msg := &messageModel{
		FromUID:     strangerID,
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
	}

	allow, err := m.hasRevokePermission(msg, adminUID)
	assert.NoError(t, err)
	assert.False(t, allow, "黑名单 manager 不应能撤回普通成员消息")
}

// 场景 8: robotService.GetCreatorUID 返回 error → 降级不阻断后续分支
// 并且不应向上返回 error。通过设置群管理员分支可命中来验证降级正确。
func TestHasRevokePermission_RobotServiceError(t *testing.T) {
	rb := &fakeRobotService{err: errors.New("mock db down")}
	gs := &fakeGroupService{members: map[string]*group.MemberResp{
		adminUID: {GroupNo: groupNo, UID: adminUID, Role: int(common.GroupMemberRoleCreater), Status: int(common.GroupMemberStatusNormal)},
		botUID:   {GroupNo: groupNo, UID: botUID, Role: int(common.GroupMemberRoleNormal), Status: int(common.GroupMemberStatusNormal)},
	}}
	m := newRevokeTestMessage(rb, gs)

	msg := &messageModel{
		FromUID:     botUID,
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
	}

	allow, err := m.hasRevokePermission(msg, adminUID)
	assert.NoError(t, err, "robotService 错误不应向上传播")
	assert.True(t, allow, "robotService 失败时群主仍应可撤回 bot 消息")
}
