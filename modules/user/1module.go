package user

import (
	"embed"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/model"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

//go:embed sql
var sqlFS embed.FS

//go:embed swagger/api.yaml
var swaggerContent string

//go:embed swagger/friend.yaml
var friendSwaggerContent string

func init() {

	// ====================== 注册用户模块 ======================
	register.AddModule(func(ctx interface{}) register.Module {
		x := ctx.(*config.Context)
		api := New(x)
		return register.Module{
			Name: "user",
			SetupAPI: func() register.APIRouter {
				return api
			},
			Service: api.userService,
			Swagger: swaggerContent,
			SQLDir:  register.NewSQLFS(sqlFS),
			IMDatasource: register.IMDatasource{
				SystemUIDs: func() ([]string, error) {
					users, err := api.userService.GetUsersWithCategories([]string{CategoryCustomerService, CategorySystem})
					if err != nil {
						return nil, err
					}
					uids := make([]string, 0, len(users))
					if len(users) > 0 {
						for _, user := range users {
							uids = append(uids, user.UID)
						}
					}
					return uids, nil
				},
			},
			BussDataSource: register.BussDataSource{
				ChannelGet: func(channelID string, channelType uint8, loginUID string) (*model.ChannelResp, error) {
					if channelType != common.ChannelTypePerson.Uint8() {
						return nil, register.ErrDatasourceNotProcess
					}
					userDetailResp, err := api.userService.GetUserDetail(channelID, loginUID)
					if err != nil {
						return nil, err
					}
					if userDetailResp == nil {
						api.Error("用户不存在！", zap.String("channel_id", channelID))
						return nil, errors.New("用户不存在！")
					}
					return newChannelRespWithUserDetailResp(userDetailResp), nil
				},
				GetDevice: func(ids []int64) ([]*model.DeviceResp, error) {
					list, err := api.deviceDB.queryDevicesWithIds(ids)
					if err != nil {
						return nil, err
					}
					if len(list) == 0 {
						return nil, nil
					}
					result := make([]*model.DeviceResp, 0, len(list))
					for _, device := range list {
						result = append(result, &model.DeviceResp{
							ID:          device.Id,
							UID:         device.UID,
							DeviceID:    device.DeviceID,
							DeviceName:  device.DeviceName,
							DeviceModel: device.DeviceModel,
						})
					}
					return result, nil
				},
			},
		}
	})

	// ====================== 注册好友模块 ======================
	register.AddModule(func(ctx interface{}) register.Module {
		friendCtx := ctx.(*config.Context)
		api := NewFriend(friendCtx)
		return register.Module{
			Name: "friend",
			SetupAPI: func() register.APIRouter {
				return api
			},
			Swagger: friendSwaggerContent,
			IMDatasource: register.IMDatasource{
				HasData: func(channelID string, channelType uint8) register.IMDatasourceType {
					if channelType == common.ChannelTypePerson.Uint8() {
						return register.IMDatasourceTypeWhitelist
					}
					return register.IMDatasourceTypeNone
				},
				Whitelist: func(channelID string, channelType uint8) ([]string, error) {
					// Space channel_id 格式: s{spaceId}_{uid}，提取真实 uid
					// 用 LastIndex("_") 避免 spaceId 含下划线时 ParseChannelID 解析错误
					realUID := channelID
					if strings.HasPrefix(channelID, "s") {
						if idx := strings.LastIndex(channelID, "_"); idx >= 0 {
							realUID = channelID[idx+1:]
						}
					}
					friends, err := api.userService.GetFriends(realUID)
					if err != nil {
						return nil, err
					}
					uidSet := make(map[string]struct{})
					if len(friends) > 0 {
						for _, friend := range friends {
							if friend.IsAlone == 0 {
								uidSet[friend.UID] = struct{}{}
							}
						}
					}
					// 合并空间共同成员到白名单
					coMembers, err := space.GetCoMemberUIDs(friendCtx, realUID)
					if err == nil && len(coMembers) > 0 {
						for _, uid := range coMembers {
							uidSet[uid] = struct{}{}
						}
					}
					result := make([]string, 0, len(uidSet))
					for uid := range uidSet {
						result = append(result, uid)
					}
					return result, nil
				},
			},
			BussDataSource: register.BussDataSource{
				GetFriends: func(uid string) ([]*model.FriendResp, error) {
					friends, err := api.userService.GetFriends(uid)
					if err != nil {
						return nil, err
					}
					list := make([]*model.FriendResp, 0, len(friends))
					for _, friend := range friends {
						list = append(list, &model.FriendResp{
							Remark:  friend.Remark,
							ToUID:   friend.UID,
							IsAlone: friend.IsAlone,
						})
					}
					return list, nil
				},
			},
		}
	})

	// ====================== 注册用户管理模块 ======================
	register.AddModule(func(ctx interface{}) register.Module {

		return register.Module{
			Name: "user_manager",
			SetupAPI: func() register.APIRouter {
				return NewManager(ctx.(*config.Context))
			},
		}
	})

}

func newChannelRespWithUserDetailResp(user *UserDetailResp) *model.ChannelResp {

	resp := &model.ChannelResp{}
	resp.Channel.ChannelID = user.UID
	resp.Channel.ChannelType = uint8(common.ChannelTypePerson)
	resp.Name = user.Name
	resp.Username = user.Username
	resp.Logo = fmt.Sprintf("users/%s/avatar", user.UID)
	resp.Mute = user.Mute
	resp.Stick = user.Top
	resp.Receipt = user.Receipt
	resp.Robot = user.Robot
	resp.Online = user.Online
	resp.LastOffline = int64(user.LastOffline)
	resp.DeviceFlag = user.DeviceFlag
	resp.Category = user.Category
	resp.Follow = user.Follow
	resp.Remark = user.Remark
	resp.Status = user.Status
	resp.BeBlacklist = user.BeBlacklist
	resp.BeDeleted = user.BeDeleted
	resp.Flame = user.Flame
	resp.FlameSecond = user.FlameSecond
	extraMap := make(map[string]interface{})
	extraMap["sex"] = user.Sex
	extraMap["chat_pwd_on"] = user.ChatPwdOn
	extraMap["short_no"] = user.ShortNo
	extraMap["source_desc"] = user.SourceDesc
	extraMap["vercode"] = user.Vercode
	extraMap["screenshot"] = user.Screenshot
	extraMap["revoke_remind"] = user.RevokeRemind
	if user.BotCommands != "" {
		extraMap["bot_commands"] = user.BotCommands
	}
	if user.BotDescription != "" {
		extraMap["bot_description"] = user.BotDescription
	}
	if user.BotCreatorUID != "" {
		extraMap["bot_creator_uid"] = user.BotCreatorUID
		extraMap["bot_creator_name"] = user.BotCreatorName
	}
	if user.Robot == 1 {
		extraMap["bot_auto_approve"] = user.BotAutoApprove
	}
	if user.BotAgentPlatform != "" {
		extraMap["bot_agent_platform"] = user.BotAgentPlatform
	}
	if user.BotAgentVersion != "" {
		extraMap["bot_agent_version"] = user.BotAgentVersion
	}
	if user.BotPluginVersion != "" {
		extraMap["bot_plugin_version"] = user.BotPluginVersion
	}
	resp.Extra = extraMap

	return resp
}
