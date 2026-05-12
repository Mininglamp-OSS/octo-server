package thread

import (
	"context"
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

//go:embed swagger/api.yaml
var swaggerContent string

func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		// thread schema is always registered so the DB layout is identical
		// across DM_THREAD_ON=true/false deployments. Without this, an
		// operator who flips DM_THREAD_ON=true on a previously-disabled
		// install would hit either "1050 table exists" (snapshot already
		// built the tables) or missing thread tables when sql-migrate
		// discovers thread-* migrations for the first time. Decoupling
		// schema registration from runtime feature-gating eliminates that
		// trap — DM_THREAD_ON now only controls whether the API surface
		// and archive worker come up, not whether the tables exist.
		threadOn := strings.ToLower(os.Getenv("DM_THREAD_ON"))
		if threadOn != "true" && threadOn != "1" {
			lg := log.NewTLog("Thread")
			lg.Info("thread module runtime disabled: API + archive worker stay down; schema migrations still apply. Set DM_THREAD_ON=true to enable.")
			return register.Module{
				Name:   "thread",
				SQLDir: register.NewSQLFS(sqlFS),
			}
		}

		api := New(ctx.(*config.Context))
		groupService := group.NewService(ctx.(*config.Context))

		archiveWorker := NewArchiveWorker(ctx.(*config.Context), LoadArchiveConfig())
		// workerCancel 在 Start 里每次重建，让 Stop→Start 重启路径拿到新鲜 ctx；
		// 框架当前只 Start 一次，但避免给未来的 graceful-reload 留陷阱。
		var workerCancel context.CancelFunc

		return register.Module{
			Name: "thread",
			Start: func() error {
				workerCtx, cancel := context.WithCancel(context.Background())
				workerCancel = cancel
				archiveWorker.Start(workerCtx)
				return nil
			},
			Stop: func() error {
				if workerCancel != nil {
					workerCancel()
				}
				archiveWorker.Stop()
				return nil
			},
			SetupAPI: func() register.APIRouter {
				return api
			},
			Swagger: swaggerContent,
			SQLDir:  register.NewSQLFS(sqlFS),
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
					// 已删除的子区标记为禁用（归档子区允许发消息，发消息后自动解档）
					if thread.Status == ThreadStatusDeleted {
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
	extraMap["message_count"] = thread.MessageCount
	if thread.SourceMessageID != nil {
		extraMap["source_message_id"] = *thread.SourceMessageID
	}
	resp.Extra = extraMap

	return resp
}
