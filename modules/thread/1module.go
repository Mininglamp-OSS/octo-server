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

					// 父群解散后，子区频道与历史保留（成员仍可查看），但任何人（含群主）
					// 都不能再发消息。与父群 datasource（modules/group/1module.go 的
					// ChannelInfo：Status==Disband 时吐 disband=1）对齐，让 WuKongIM 在
					// 发送权限检查最前面就拒绝所有发送者。子区的 Whitelist 只继承父群
					// Forbidden（禁言），不感知解散，所以解散拦截必须由这里的 disband
					// flag 承担。
					// fail-closed：查询父群状态出错时直接返回错误，不能吐一个不带
					// disband 的 channelInfoMap——否则 WuKongIM 会缓存「未解散」态，
					// 解散群的子区在缓存有效期内仍可发消息（CR 阻塞项）。
					groupInfo, gErr := groupService.GetGroupWithGroupNo(groupNo)
					if gErr != nil {
						return nil, gErr
					}
					if groupInfo != nil && groupInfo.Status == group.GroupStatusDisband {
						channelInfoMap["disband"] = 1
					}

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

					// 返回父群可订阅成员（允许查看和发送消息）。必须排除被拉黑
					// （status=blacklist）成员：WuKongIM 缓存这份列表做 WS push，
					// 若包含黑名单用户，重载订阅时会把他加回，导致拉黑不自愈
					// （YUJ-4185 P0-2 根因）。Blacklist 回调仍单独继承父群黑名单
					// 做纵深防御（挡发送），两者互补。
					uids, err := groupService.GetSubscribableMemberUIDs(groupNo)
					if err != nil {
						return nil, err
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
