package thread

import (
	"embed"
	"fmt"
	"os"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/model"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
)

//go:embed sql
var sqlFS embed.FS

func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		// Beta 功能开关：DM_THREAD_ON=true 启用
		threadOn := strings.ToLower(os.Getenv("DM_THREAD_ON"))
		if threadOn != "true" && threadOn != "1" {
			lg := log.NewTLog("Thread")
			lg.Info("thread module disabled: set DM_THREAD_ON=true to enable")
			return register.Module{Name: "thread"}
		}

		api := New(ctx.(*config.Context))
		groupService := group.NewService(ctx.(*config.Context))

		return register.Module{
			Name: "thread",
			SetupAPI: func() register.APIRouter {
				return api
			},
			SQLDir: register.NewSQLFS(sqlFS),
			IMDatasource: register.IMDatasource{
				HasData: func(channelID string, channelType uint8) register.IMDatasourceType {
					// 只处理 ChannelTypeCommunityTopic (=5)
					if channelType != common.ChannelTypeCommunityTopic.Uint8() {
						return register.IMDatasourceTypeNone
					}

					// 解析 channelID
					groupNo, shortID, err := ParseChannelID(channelID)
					if err != nil {
						return register.IMDatasourceTypeNone
					}

					// 检查子区是否存在
					exist, err := api.db.ExistByGroupNoAndShortID(groupNo, shortID)
					if err != nil || !exist {
						return register.IMDatasourceTypeNone
					}

					return register.IMDatasourceTypeSubscribers |
						register.IMDatasourceTypeBlacklist |
						register.IMDatasourceTypeWhitelist |
						register.IMDatasourceTypeChannelInfo
				},
				ChannelInfo: func(channelID string, channelType uint8) (map[string]interface{}, error) {
					if channelType != common.ChannelTypeCommunityTopic.Uint8() {
						return nil, register.ErrDatasourceNotProcess
					}

					groupNo, shortID, err := ParseChannelID(channelID)
					if err != nil {
						return nil, err
					}

					thread, err := api.db.QueryByGroupNoAndShortID(groupNo, shortID)
					if err != nil {
						return nil, err
					}
					if thread == nil {
						return nil, register.ErrDatasourceNotProcess
					}

					channelInfoMap := map[string]interface{}{}
					// 已删除或归档的子区标记为禁用
					if thread.Status == ThreadStatusDeleted || thread.Status == ThreadStatusArchived {
						channelInfoMap["ban"] = 1
					}
					channelInfoMap["status"] = thread.Status

					return channelInfoMap, nil
				},
				Subscribers: func(channelID string, channelType uint8) ([]string, error) {
					if channelType != common.ChannelTypeCommunityTopic.Uint8() {
						return nil, register.ErrDatasourceNotProcess
					}

					groupNo, _, err := ParseChannelID(channelID)
					if err != nil {
						return nil, err
					}

					// 返回父群成员（允许所有父群成员查看和发送消息）
					members, err := groupService.GetMembers(groupNo)
					if err != nil {
						return nil, err
					}

					uids := make([]string, 0, len(members))
					for _, m := range members {
						uids = append(uids, m.UID)
					}
					return uids, nil
				},
				Blacklist: func(channelID string, channelType uint8) ([]string, error) {
					if channelType != common.ChannelTypeCommunityTopic.Uint8() {
						return nil, register.ErrDatasourceNotProcess
					}

					groupNo, _, err := ParseChannelID(channelID)
					if err != nil {
						return nil, err
					}

					// 继承父群黑名单
					return groupService.GetBlacklistMemberUIDs(groupNo)
				},
				Whitelist: func(channelID string, channelType uint8) ([]string, error) {
					if channelType != common.ChannelTypeCommunityTopic.Uint8() {
						return nil, register.ErrDatasourceNotProcess
					}

					groupNo, _, err := ParseChannelID(channelID)
					if err != nil {
						return nil, err
					}

					// 检查父群是否禁言
					groupInfo, err := groupService.GetGroupWithGroupNo(groupNo)
					if err != nil || groupInfo == nil {
						return make([]string, 0), err
					}

					// 禁言时返回管理员（可以发言的人）
					if groupInfo.Forbidden == 1 {
						return groupService.GetMemberUIDsOfManager(groupNo)
					}

					return make([]string, 0), nil
				},
			},
			BussDataSource: register.BussDataSource{
				ChannelGet: func(channelID string, channelType uint8, loginUID string) (*model.ChannelResp, error) {
					if channelType != common.ChannelTypeCommunityTopic.Uint8() {
						return nil, register.ErrDatasourceNotProcess
					}

					groupNo, shortID, err := ParseChannelID(channelID)
					if err != nil {
						return nil, err
					}

					thread, err := api.db.QueryByGroupNoAndShortID(groupNo, shortID)
					if err != nil {
						return nil, err
					}
					if thread == nil {
						return nil, register.ErrDatasourceNotProcess
					}

					return newChannelRespWithThread(thread), nil
				},
			},
		}
	})
}

// newChannelRespWithThread 将 thread Model 转换为 ChannelResp
func newChannelRespWithThread(thread *Model) *model.ChannelResp {
	resp := &model.ChannelResp{}
	resp.Channel.ChannelID = BuildChannelID(thread.GroupNo, thread.ShortID)
	resp.Channel.ChannelType = common.ChannelTypeCommunityTopic.Uint8()
	resp.Name = thread.Name
	resp.Logo = fmt.Sprintf("groups/%s/avatar", thread.GroupNo) // 使用父群头像

	extraMap := make(map[string]interface{})
	extraMap["short_id"] = thread.ShortID
	extraMap["group_no"] = thread.GroupNo
	extraMap["creator_uid"] = thread.CreatorUID
	extraMap["status"] = thread.Status
	if thread.SourceMessageID != nil {
		extraMap["source_message_id"] = *thread.SourceMessageID
	}
	resp.Extra = extraMap

	return resp
}
