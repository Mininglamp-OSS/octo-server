package group

import (
	"embed"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/model"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/util"
)

//go:embed sql
var sqlFS embed.FS

//go:embed swagger/api.yaml
var swaggerContent string

func init() {
	register.AddModule(func(ctx interface{}) register.Module {

		api := New(ctx.(*config.Context))
		// 注册群成员检查函数，供 user 模块置顶校验使用
		user.RegisterGroupMemberChecker(api.groupService.ExistMember)
		// YUJ-206：注册群成员外部来源 / 归属 Space 提供者，
		// 供 /users/{uid}?group_no= 路径补齐 GroupMemberResp 的 is_external /
		// source_space_* / home_space_* 字段，让 Web/Android/iOS UserInfo 能区分
		// "同 Space 非好友 → 直接发消息" vs "跨 Space 外部成员 → 仅可在群内交流"。
		user.RegisterGroupMemberExternalProvider(api.groupService.GetMemberExternalFields)
		// 注册共同群判定，供 user 模块的 /v1/users/:uid 做对象级资料可见性判定
		// （与 /v1/channels/:id/:type 共用 channel/service 的判定函数）。
		user.RegisterCommonGroupChecker(api.groupService.ExistCommonGroup)
		// 活跃成员口径（排该群黑名单），供 /v1/users/:uid 的 ?group_no= 富化门禁使用；
		// 与 RegisterGroupMemberChecker（ExistMember，服务置顶等既有路径）分开，避免
		// 收紧一处波及另一处。
		user.RegisterActiveGroupMemberChecker(api.groupService.ExistMemberActive)
		return register.Module{
			Name: "group",
			SetupAPI: func() register.APIRouter {
				return api
			},
			SQLDir:  register.NewSQLFS(sqlFS),
			Swagger: swaggerContent,
			IMDatasource: register.IMDatasource{
				HasData: func(channelID string, channelType uint8) register.IMDatasourceType {
					if channelType == common.ChannelTypeGroup.Uint8() {
						return register.IMDatasourceTypeChannelInfo | register.IMDatasourceTypeSubscribers | register.IMDatasourceTypeBlacklist | register.IMDatasourceTypeWhitelist
					}
					return register.IMDatasourceTypeNone
				},
				ChannelInfo: func(channelID string, channelType uint8) (map[string]interface{}, error) {
					groupInfo, err := api.groupService.GetGroupWithGroupNo(channelID)
					if err != nil {
						return nil, err
					}
					channelInfoMap := map[string]interface{}{}
					if groupInfo != nil {
						if groupInfo.Status == GroupStatusDisabled {
							channelInfoMap["ban"] = 1
						}
						if groupInfo.GroupType == GroupTypeSuper {
							channelInfoMap["large"] = 1
						}
						if groupInfo.Status == GroupStatusDisband {
							channelInfoMap["disband"] = 1
						}
					}
					return channelInfoMap, nil
				},
				Subscribers: func(channelID string, channelType uint8) ([]string, error) {

					// 父群权威订阅源排除被拉黑成员（status=blacklist）：WuKongIM 缓存
					// 这份列表做 WS push，若含黑名单用户，重载订阅会把他加回 → 拉黑后
					// 仍能收父群实时消息（YUJ-4185 P0-2 根因收口，使 blacklist handler 的
					// 父群 IMRemoveSubscriber 持久生效）。Blacklist 回调仍单独返回黑名单
					// 挡发送，两者互补。
					subscribers, err := api.groupService.GetSubscribableMemberUIDs(channelID)
					if err != nil {
						return nil, err
					}
					return subscribers, nil
				},
				Blacklist: func(channelID string, channelType uint8) ([]string, error) {
					return api.groupService.GetBlacklistMemberUIDs(channelID)
				},
				Whitelist: func(channelID string, channelType uint8) ([]string, error) {
					groupInfo, err := api.groupService.GetGroupWithGroupNo(channelID)
					if err != nil {
						return nil, err
					}
					if groupInfo == nil {
						return nil, nil
					}
					if groupInfo.Forbidden == 1 {
						return api.groupService.GetMemberUIDsOfManager(channelID)
					}
					return make([]string, 0), nil
				},
			},
			BussDataSource: register.BussDataSource{
				ChannelGet: func(channelID string, channelType uint8, loginUID string) (*model.ChannelResp, error) {
					if channelType != common.ChannelTypeGroup.Uint8() {
						return nil, register.ErrDatasourceNotProcess
					}
					groupResp, err := api.groupService.GetGroupDetail(channelID, loginUID)
					if err != nil {
						return nil, err
					}
					if groupResp == nil {
						// 群不存在：交还链路，由上层优雅回落（频道不存在 / forbidden），
						// 不把 nil 传给 newChannelRespWithGroupResp 解引用 panic（历史上
						// 群不存在返回 500 空 body，构成存在性枚举 oracle）。
						return nil, register.ErrDatasourceNotProcess
					}
					return newChannelRespWithGroupResp(groupResp), nil
				},
				IsShowShortNo: func(groupNO, uid, loginUID string) (bool, string, error) {
					if groupNO == "" || uid == "" || loginUID == "" {
						return false, "", nil
					}
					groupInfo, err := api.groupService.GetGroupWithGroupNo(groupNO)
					if err != nil {
						return false, "", err
					}
					if groupInfo == nil {
						return false, "", nil
					}
					member, err := api.groupService.GetMember(groupNO, uid)
					if err != nil {
						return false, "", err
					}
					if member == nil {
						return false, "", nil
					}
					if groupInfo.ForbiddenAddFriend == 0 {
						return true, member.Vercode, nil
					}
					isCreatorOrManager, err := api.groupService.IsCreatorOrManager(groupNO, loginUID)
					return isCreatorOrManager, member.Vercode, err
				},
				GetGroupMember: func(groupNO, uid string) (*model.GroupMemberResp, error) {
					if groupNO == "" || uid == "" {
						return nil, nil
					}

					member, err := api.groupService.GetMember(groupNO, uid)
					if err != nil {
						return nil, err
					}
					if member == nil {
						return nil, nil
					}
					return &model.GroupMemberResp{
						UID:                member.UID,
						GroupNo:            member.GroupNo,
						Name:               member.Name,
						Remark:             member.Remark,
						InviteUID:          member.InviteUID,
						IsDeleted:          member.IsDeleted,
						Role:               member.Role,
						Status:             member.Status,
						ForbiddenExpirTime: member.ForbiddenExpirTime,
						CreatedAt:          util.ToyyyyMMddHHmm(time.Unix(member.CreatedAt, 0)),
					}, nil
				},
			},
		}
	})

	register.AddModule(func(ctx interface{}) register.Module {
		return register.Module{
			SetupAPI: func() register.APIRouter {
				return NewManager(ctx.(*config.Context))
			},
		}
	})
}

func newChannelRespWithGroupResp(groupResp *GroupResp) *model.ChannelResp {
	resp := &model.ChannelResp{}
	resp.Channel.ChannelID = groupResp.GroupNo
	resp.Channel.ChannelType = uint8(common.ChannelTypeGroup)
	resp.Name = groupResp.Name
	resp.Remark = groupResp.Remark
	resp.Logo = fmt.Sprintf("groups/%s/avatar", groupResp.GroupNo)
	resp.Notice = groupResp.Notice
	resp.Mute = groupResp.Mute
	resp.Stick = groupResp.Top
	resp.Receipt = groupResp.Receipt
	resp.ShowNick = groupResp.ShowNick
	resp.Forbidden = groupResp.Forbidden
	resp.Invite = groupResp.Invite
	resp.Status = groupResp.Status
	resp.Save = groupResp.Save
	resp.Remark = groupResp.Remark
	resp.Flame = groupResp.Flame
	resp.FlameSecond = groupResp.FlameSecond
	resp.Category = groupResp.Category
	extraMap := make(map[string]interface{})
	extraMap["forbidden_add_friend"] = groupResp.ForbiddenAddFriend
	extraMap["screenshot"] = groupResp.Screenshot
	extraMap["revoke_remind"] = groupResp.RevokeRemind
	extraMap["join_group_remind"] = groupResp.JoinGroupRemind
	extraMap["chat_pwd_on"] = groupResp.ChatPwdOn
	extraMap["allow_view_history_msg"] = groupResp.AllowViewHistoryMsg
	extraMap["group_type"] = groupResp.GroupType
	extraMap["allow_member_pinned_message"] = groupResp.AllowMemberPinnedMessage
	// 群级「允许免@回答」总开关：前端 channelInfo.orgData.allow_no_mention 据此回读
	// 真实 0/1 值，否则开关永远显示「开」且关不掉（refresh 弹回）。默认 1=允许，零回归。
	extraMap["allow_no_mention"] = groupResp.AllowNoMention
	if groupResp.MemberCount != 0 {
		extraMap["member_count"] = groupResp.MemberCount
	}
	if groupResp.OnlineCount != 0 {
		extraMap["online_count"] = groupResp.OnlineCount
	}
	if groupResp.Quit != 0 {
		extraMap["quit"] = groupResp.Quit
	}
	if groupResp.Role != 0 {
		extraMap["role"] = groupResp.Role
	}
	if groupResp.ForbiddenExpirTime != 0 {
		extraMap["forbidden_expir_time"] = groupResp.ForbiddenExpirTime
	}

	// Space 隔离：前端 channelInfo 需要 space_id 用于实时会话过滤
	if groupResp.SpaceID != "" {
		extraMap["space_id"] = groupResp.SpaceID
	}

	// 外部群标记：前端 UI 需要根据此字段渲染「外部群」标签
	extraMap["is_external_group"] = groupResp.IsExternalGroup

	// GROUP.md fields
	extraMap["has_group_md"] = groupResp.HasGroupMd
	extraMap["group_md_version"] = groupResp.GroupMdVersion
	if groupResp.GroupMdUpdatedAt != nil {
		extraMap["group_md_updated_at"] = *groupResp.GroupMdUpdatedAt
	}
	extraMap["can_edit_group_md"] = groupResp.CanEditGroupMd
	extraMap["can_manage_bot_admin"] = groupResp.CanManageBotAdmin

	resp.Extra = extraMap

	return resp
}
