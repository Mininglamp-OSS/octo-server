package message

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/model"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	"github.com/Mininglamp-OSS/octo-server/modules/channel"
	chservice "github.com/Mininglamp-OSS/octo-server/modules/channel/service"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Conversation 最近会话
type Conversation struct {
	ctx *config.Context
	log.Log
	userDB              *user.DB
	groupDB             *group.DB
	messageExtraDB      *messageExtraDB
	messageReactionDB   *messageReactionDB
	messageUserExtraDB  *messageUserExtraDB
	channelOffsetDB     *channelOffsetDB
	deviceOffsetDB      *deviceOffsetDB
	userLastOffsetDB    *userLastOffsetDB
	groupCategoryDB     *groupCategoryDB
	userService         user.IService
	groupService        group.IService
	service             IService
	channelService      chservice.IService
	conversationExtraDB *conversationExtraDB
	threadDB            *thread.DB

	syncConversationResultCacheMap  map[string][]string
	syncConversationVersionMap      map[string]int64
	syncConversationResultCacheLock sync.RWMutex
}

// New New
func NewConversation(ctx *config.Context) *Conversation {
	return &Conversation{
		ctx:                            ctx,
		Log:                            log.NewTLog("Coversation"),
		userDB:                         user.NewDB(ctx),
		groupDB:                        group.NewDB(ctx),
		messageExtraDB:                 newMessageExtraDB(ctx),
		messageUserExtraDB:             newMessageUserExtraDB(ctx),
		messageReactionDB:              newMessageReactionDB(ctx),
		channelOffsetDB:                newChannelOffsetDB(ctx),
		deviceOffsetDB:                 newDeviceOffsetDB(ctx.DB()),
		userLastOffsetDB:               newUserLastOffsetDB(ctx),
		groupCategoryDB:                newGroupCategoryDB(ctx),
		conversationExtraDB:            newConversationExtraDB(ctx),
		threadDB:                       thread.NewDB(ctx),
		userService:                    user.NewService(ctx),
		groupService:                   group.NewService(ctx),
		channelService:                 channel.NewService(ctx),
		service:                        NewService(ctx),
		syncConversationResultCacheMap: map[string][]string{},
		syncConversationVersionMap:     map[string]int64{},
	}
}

// Route 路由配置
func (co *Conversation) Route(r *wkhttp.WKHttp) {

	// 拼写错误的旧路由（deprecated）
	deprecatedLog := func(c *wkhttp.Context) {
		co.Warn("deprecated route called, use /v1/conversation(s) instead", zap.String("path", c.Request.URL.Path))
		// 废弃路由不处理 space_id，删除以免影响后续逻辑
		q := c.Request.URL.Query()
		q.Del("space_id")
		c.Request.URL.RawQuery = q.Encode()
		c.Next()
	}
	// UID 限流：Web 端轮询叠加易触发全局 per-IP 桶（见 wukongim#92 / octo-server#1086 P2），
	// 共享 keyspace "ratelimit:uid:{uid}"，配额跨所有挂载端点统一
	uidLimit := appwkhttp.SharedUIDRateLimiter(co.ctx)

	coversations := r.Group("/v1/coversations", co.ctx.AuthMiddleware(r), uidLimit, deprecatedLog)
	{
		coversations.GET("", co.getConversations)
	}
	cnversation := r.Group("/v1/coversation", co.ctx.AuthMiddleware(r), uidLimit, deprecatedLog)
	{
		cnversation.PUT("/clearUnread", co.clearConversationUnread)
	}

	conversation := r.Group("/v1/conversation", co.ctx.AuthMiddleware(r), uidLimit, spacepkg.SpaceMiddleware(co.ctx))
	{
		// 离线的最近会话
		conversation.POST("/sync", co.syncUserConversation)
		conversation.POST("/syncack", co.syncUserConversationAck)
		conversation.POST("/extra/sync", co.conversationExtraSync)   // 同步最近会话扩展
		conversation.PUT("/clearUnread", co.clearConversationUnread) // 清除未读（正确拼写路径）
	}
	conversations := r.Group("/v1/conversations", co.ctx.AuthMiddleware(r), uidLimit)
	{
		conversations.DELETE("/:channel_id/:channel_type", co.deleteConversation)          // 删除最近会话
		conversations.POST("/:channel_id/:channel_type/extra", co.conversationExtraUpdate) // 添加或更新最近会话扩展
	}

	co.ctx.AddEventListener(event.ConversationDelete, func(data []byte, commit config.EventCommit) {
		co.handleConversationDeleteEvent(data, commit)
	})

	// sidebar 聚合接口（/v1/sidebar/sync）
	RegisterSidebarRoutes(r, co.ctx)
}

func (co *Conversation) handleConversationDeleteEvent(data []byte, commit config.EventCommit) {
	var req config.DeleteConversationReq
	err := util.ReadJsonByByte([]byte(data), &req)
	if err != nil {
		co.Error("解析最近会话删除JSON失败！", zap.Error(err), zap.String("data", string(data)))
		commit(err)
		return
	}

	err = co.service.DeleteConversation(req.UID, req.ChannelID, req.ChannelType)
	if err != nil {
		co.Error("删除最近会话失败！", zap.Error(err))
		commit(err)
		return
	}
	commit(nil)
}

// 最近会话扩展同步
func (co *Conversation) conversationExtraSync(c *wkhttp.Context) {
	var req struct {
		Version int64 `json:"version"`
	}
	if err := c.BindJSON(&req); err != nil {
		co.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	loginUID := c.GetLoginUID()

	conversationExtraModels, err := co.conversationExtraDB.sync(loginUID, req.Version)
	if err != nil {
		co.Error("同步消息扩展失败！", zap.Error(err))
		c.ResponseError(errors.New("同步消息扩展失败！"))
		return
	}
	resps := make([]*conversationExtraResp, 0, len(conversationExtraModels))
	for _, conversationExtraM := range conversationExtraModels {
		resps = append(resps, newConversationExtraResp(conversationExtraM))
	}
	c.JSON(http.StatusOK, resps)
}

// 更新最近会话扩展
func (co *Conversation) conversationExtraUpdate(c *wkhttp.Context) {
	var req struct {
		BrowseTo       uint32 `json:"browse_to"`        // 预览位置 预览到的位置，与会话保持位置不同的是 预览到的位置是用户读到的最大的messageSeq。跟未读消息数量有关系
		KeepMessageSeq uint32 `json:"keep_message_seq"` // 保存位置的messageSeq
		KeepOffsetY    int    `json:"keep_offset_y"`    //  Y的偏移量
		Draft          string `json:"draft"`            // 草稿
	}
	if err := c.BindJSON(&req); err != nil {
		co.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	channelID := c.Param("channel_id")
	channelTypeStr := c.Param("channel_type")
	loginUID := c.GetLoginUID()

	channelTypeI64, _ := strconv.ParseInt(channelTypeStr, 10, 64)

	version, err := co.ctx.GenSeq(common.SyncConversationExtraKey)
	if err != nil {
		c.ResponseError(err)
		return
	}

	err = co.conversationExtraDB.insertOrUpdate(&conversationExtraModel{
		UID:            loginUID,
		ChannelID:      channelID,
		ChannelType:    uint8(channelTypeI64),
		BrowseTo:       req.BrowseTo,
		KeepMessageSeq: req.KeepMessageSeq,
		KeepOffsetY:    req.KeepOffsetY,
		Draft:          req.Draft,
		Version:        version,
	})
	if err != nil {
		co.Error("添加或更新最近会话扩展失败！", zap.Error(err))
		c.ResponseError(errors.New("添加或更新最近会话扩展失败！"))
		return
	}
	err = co.ctx.SendCMD(config.MsgCMDReq{
		NoPersist:   true,
		ChannelID:   loginUID,
		ChannelType: uint8(common.ChannelTypePerson),
		CMD:         common.CMDSyncConversationExtra,
	})
	if err != nil {
		co.Error("发送同步扩展会话cmd失败！", zap.Error(err))
		c.ResponseError(errors.New("发送同步扩展会话cmd失败！"))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"version": version,
	})
}

// 删除最近会话
func (co *Conversation) deleteConversation(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	channelID := c.Param("channel_id")
	channelType, err := strconv.ParseInt(c.Param("channel_type"), 10, 64)
	if err != nil {
		c.ResponseError(errors.New("频道类型格式错误"))
		return
	}
	if strings.TrimSpace(channelID) == "" {
		c.ResponseError(errors.New("频道ID不能为空"))
		return
	}

	// Verify the conversation belongs to the current user before deleting
	if uint8(channelType) == common.ChannelTypeGroup.Uint8() {
		// For group channels, verify the user is (or was) a member
		isMember, err := co.groupService.ExistMember(channelID, loginUID)
		if err != nil {
			co.Error("查询群成员失败", zap.Error(err))
			c.ResponseError(errors.New("操作失败"))
			return
		}
		if !isMember {
			c.ResponseError(errors.New("无权删除此会话"))
			return
		}
	} else if uint8(channelType) == common.ChannelTypePerson.Uint8() {
		// For person channels, verify channelID is a valid user
		if channelID == loginUID {
			c.ResponseError(errors.New("无法删除与自己的会话"))
			return
		}
		userInfo, err := co.userService.GetUser(channelID)
		if err != nil {
			co.Error("查询用户信息失败", zap.Error(err))
			c.ResponseError(errors.New("操作失败"))
			return
		}
		if userInfo == nil {
			c.ResponseError(errors.New("会话不存在或无权删除"))
			return
		}
	}

	err = co.service.DeleteConversation(loginUID, channelID, uint8(channelType))
	if err != nil {
		co.Error("删除最近会话失败！", zap.Error(err))
		c.ResponseError(errors.New("删除最近会话失败！"))
		return
	}
	c.ResponseOK()
}

// 获取离线的最近会话
func (co *Conversation) syncUserConversation(c *wkhttp.Context) {
	var req struct {
		Version     int64  `json:"version"`       // 当前客户端的会话最大版本号(客户端最新会话的时间戳)
		LastMsgSeqs string `json:"last_msg_seqs"` // 客户端所有会话的最后一条消息序列号 格式： channelID:channelType:last_msg_seq|channelID:channelType:last_msg_seq
		MsgCount    int64  `json:"msg_count"`     // 每个会话消息数量
		DeviceUUID  string `json:"device_uuid"`   // 设备uuid
	}
	if err := c.BindJSON(&req); err != nil {
		co.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}

	// Space 过滤（从 middleware 获取，已校验 membership）
	filterSpaceID := spacepkg.GetSpaceID(c)
	hasSpaceFilter := filterSpaceID != ""

	version := req.Version
	loginUID := c.GetLoginUID()

	deviceOffsetModelMap := map[string]*deviceOffsetModel{}
	lastMsgSeqs := req.LastMsgSeqs
	if !co.ctx.GetConfig().MessageSaveAcrossDevice {
		/**
		1.获取设备的最大version 作为同步version
		2. 如果设备最大version不存在 则把用户最大的version 作为设备version
		**/
		cacheVersion, err := co.getDeviceConversationMaxVersion(loginUID, req.DeviceUUID)
		if err != nil {
			co.Error("获取缓存的最近会话版本号失败！", zap.Error(err), zap.String("loginUID", loginUID), zap.String("deviceUUID", req.DeviceUUID))
			c.ResponseError(errors.New("获取缓存的最近会话版本号失败！"))
			return
		}
		if cacheVersion == 0 {
			userMaxVersion, err := co.getUserConversationMaxVersion(loginUID)
			if err != nil {
				co.Error("获取用户最近会很最大版本失败！", zap.Error(err))
				c.ResponseError(errors.New("获取用户最近会很最大版本失败！"))
				return
			}
			if userMaxVersion > 0 {
				err = co.setDeviceConversationMaxVersion(loginUID, req.DeviceUUID, userMaxVersion)
				if err != nil {
					co.Error("设置设备最近会话最大版本号失败！", zap.Error(err))
					c.ResponseError(errors.New("设置设备最近会话最大版本号失败！"))
					return
				}
			}
			cacheVersion = userMaxVersion
		}
		if cacheVersion > version {
			version = cacheVersion
		}

		// ---------- 设备消息偏移  ----------

		if !co.ctx.GetConfig().MessageSaveAcrossDevice { // 以下为不开启夸设备存储的逻辑

			lastMsgSeqList := make([]string, 0)

			deviceOffsetModels, err := co.deviceOffsetDB.queryWithUIDAndDeviceUUID(loginUID, req.DeviceUUID)
			if err != nil {
				co.Error("查询用户设备偏移量失败！", zap.Error(err))
				c.ResponseError(errors.New("查询用户设备偏移量失败！"))
				return
			}
			if len(deviceOffsetModels) > 0 {
				for _, deviceOffsetM := range deviceOffsetModels {
					deviceOffsetModelMap[fmt.Sprintf("%s-%d", deviceOffsetM.ChannelID, deviceOffsetM.ChannelType)] = deviceOffsetM
					lastMsgSeqList = append(lastMsgSeqList, fmt.Sprintf("%s:%d:%d", deviceOffsetM.ChannelID, deviceOffsetM.ChannelType, deviceOffsetM.MessageSeq))
				}
			} else { // 如果没有设备的偏移量 则取用户最新的偏移量作为设备偏移量
				userLastOffsetModels, err := co.userLastOffsetDB.queryWithUID(loginUID)
				if err != nil {
					co.Error("查询用户偏移量失败！", zap.Error(err))
					c.ResponseError(errors.New("查询用户偏移量失败！"))
					return
				}
				if len(userLastOffsetModels) > 0 {
					deviceOffsetList := make([]*deviceOffsetModel, 0, len(userLastOffsetModels))
					for _, userLastOffsetM := range userLastOffsetModels {
						deviceOffsetList = append(deviceOffsetList, &deviceOffsetModel{
							UID:         userLastOffsetM.UID,
							DeviceUUID:  req.DeviceUUID,
							ChannelID:   userLastOffsetM.ChannelID,
							ChannelType: userLastOffsetM.ChannelType,
							MessageSeq:  userLastOffsetM.MessageSeq,
						})
						lastMsgSeqList = append(lastMsgSeqList, fmt.Sprintf("%s:%d:%d", userLastOffsetM.ChannelID, userLastOffsetM.ChannelType, userLastOffsetM.MessageSeq))
					}
					err := co.insertDeviceOffsets(deviceOffsetList)
					if err != nil {
						c.ResponseError(errors.New("插入设备偏移数据失败！"))
						return
					}
				}
			}
			if len(lastMsgSeqList) > 0 {
				lastMsgSeqs = strings.Join(lastMsgSeqList, "|")
			}
		}
	}

	// 获取用户的超大群
	largeGroupInfos, err := co.groupService.GetUserSupers(loginUID)
	if err != nil {
		co.Error("获取用户超大群失败！", zap.Error(err))
		c.ResponseError(errors.New("获取用户超大群失败！"))
		return
	}
	largeChannels := make([]*config.Channel, 0)
	if len(largeGroupInfos) > 0 {
		for _, largeGroupInfo := range largeGroupInfos {
			largeChannels = append(largeChannels, &config.Channel{
				ChannelID:   largeGroupInfo.GroupNo,
				ChannelType: common.ChannelTypeGroup.Uint8(),
			})
		}
	}
	conversations, err := co.ctx.IMSyncUserConversation(loginUID, version, req.MsgCount, lastMsgSeqs, largeChannels)
	if err != nil {
		co.Error("同步离线后的最近会话失败！", zap.Error(err), zap.String("loginUID", loginUID))
		c.ResponseError(errors.New("同步离线后的最近会话失败！"))
		return
	}
	groupNos := make([]string, 0, len(conversations))
	uids := make([]string, 0, len(conversations))
	channelIDs := make([]string, 0, len(conversations))
	threadChannelShortIDMap := make(map[string]string)
	// groupNoSeen 用于 groupNos 的去重：COMMUNITY_TOPIC 频道除了把自身
	// channel_id（"{groupNo}____{shortID}"）加入 groupNos 外，还要把解析出
	// 的 parent groupNo 也加进去，否则当父群本批不在 IM 返回里时，
	// fillConversationSpaceIDs 拿不到父群的 SpaceID，导致 thread 频道的
	// SpaceID 被回填为空（GH octo-server#153 Round-2 Critical 1）。
	groupNoSeen := make(map[string]struct{}, len(conversations))
	addGroupNo := func(no string) {
		if no == "" {
			return
		}
		if _, ok := groupNoSeen[no]; ok {
			return
		}
		groupNoSeen[no] = struct{}{}
		groupNos = append(groupNos, no)
	}
	if len(conversations) > 0 {
		for _, conversation := range conversations {
			if len(conversation.Recents) == 0 {
				continue
			}
			if conversation.ChannelType == common.ChannelTypePerson.Uint8() {
				uids = append(uids, conversation.ChannelID)
			} else {
				addGroupNo(conversation.ChannelID)
			}
			channelIDs = append(channelIDs, conversation.ChannelID)
			if conversation.ChannelType == common.ChannelTypeCommunityTopic.Uint8() {
				if parentNo, shortID, err := thread.ParseChannelID(conversation.ChannelID); err == nil {
					threadChannelShortIDMap[conversation.ChannelID] = shortID
					// 父群可能未出现在 IM 批次里（最近无消息），但 fillConversationSpaceIDs
					// 需要从 groupMap[parentNo] 取 SpaceID。这里显式合入预取集合，
					// GetGroupDetails 才会覆盖父群。
					addGroupNo(parentNo)
				}
			}
		}
	}

	userMap := map[string]*user.UserDetailResp{}                // 用户详情
	groupMap := map[string]*group.GroupResp{}                   // 群详情
	conversationExtraMap := map[string]*conversationExtraResp{} // 最近会话扩展
	groupVailds := make([]string, 0, len(conversations))        // 有效群
	activeThreadShortIDs := make(map[string]struct{})           // 有效子区

	// ---------- 是否在群内 ----------
	if len(groupNos) > 0 {
		groupVailds, err = co.groupService.ExistMembers(groupNos, loginUID)
		if err != nil {
			co.Error("查询有效群失败！", zap.Error(err))
			c.ResponseError(errors.New("查询有效群失败！"))
			return
		}

	}

	// ---------- 过滤已删除子区 ----------
	threadFilterEnabled := false
	if len(threadChannelShortIDMap) > 0 {
		shortIDs := make([]string, 0, len(threadChannelShortIDMap))
		for _, shortID := range threadChannelShortIDMap {
			shortIDs = append(shortIDs, shortID)
		}
		// PR-B (#1377): 只把 status=active 的子区放进白名单。archived 子区由 cron (#1376)
		// 维护，被收消息时通过 RecordMessageAndReactivate 自动复活为 active，重新出现在
		// 下一次 sync 中；deleted 子区永久剔除。
		// fail-open：DB 查询失败时跳过子区过滤（threadFilterEnabled 保持 false），
		// 宁可短暂把 archived/deleted 子区透出给客户端，也不阻塞用户的整批 sync。
		// 与 PR-A 之前 QueryNonDeletedShortIDs 的策略一致。
		activeIDs, err := co.threadDB.QueryActiveShortIDs(shortIDs)
		if err != nil {
			co.Error("查询有效子区失败！", zap.Error(err))
		} else {
			threadFilterEnabled = true
			for _, id := range activeIDs {
				activeThreadShortIDs[id] = struct{}{}
			}
		}
	}

	// ---------- 扩展 ----------
	conversationExtras, err := co.conversationExtraDB.queryWithChannelIDs(loginUID, channelIDs)
	if err != nil {
		co.Error("查询最近会话扩展失败！", zap.Error(err))
		c.ResponseError(errors.New("查询最近会话扩展失败！"))
		return
	}
	if len(conversationExtras) > 0 {
		for _, conversationExtra := range conversationExtras {
			conversationExtraMap[fmt.Sprintf("%s-%d", conversationExtra.ChannelID, conversationExtra.ChannelType)] = newConversationExtraResp(conversationExtra)
		}
	}

	// ---------- 用户设置 ----------
	users := make([]*user.UserDetailResp, 0)
	if len(uids) > 0 {
		users, err = co.userService.GetUserDetails(uids, c.GetLoginUID())
		if err != nil {
			co.Error("查询用户信息失败！", zap.Error(err))
			c.ResponseError(errors.New("查询用户信息失败！"))
			return
		}
		if len(users) > 0 {
			for _, user := range users {
				userMap[user.UID] = user
			}
		}
	}

	// ---------- App Bot 标记 ----------
	appBotUIDs := make(map[string]bool)
	if len(uids) > 0 {
		var abUIDs []string
		_, abErr := co.ctx.DB().SelectBySql(
			"SELECT uid FROM app_bot WHERE uid IN ? AND status=1", uids,
		).Load(&abUIDs)
		if abErr != nil {
			co.Warn("batch query app_bot failed, skip bot_type tagging", zap.Error(abErr))
		} else {
			for _, uid := range abUIDs {
				appBotUIDs[uid] = true
			}
		}
	}

	// ---------- 群设置  ----------
	groups := make([]*group.GroupResp, 0)
	if len(groupNos) > 0 {
		groups, err = co.groupService.GetGroupDetails(groupNos, c.GetLoginUID())
		if err != nil {
			co.Error("查询群设置信息失败！", zap.Error(err))
			c.ResponseError(errors.New("查询群设置信息失败！"))
			return
		}
		if groups == nil {
			groups = make([]*group.GroupResp, 0)
		}
		if len(groups) > 0 {
			for _, group := range groups {
				groupMap[group.GroupNo] = group
			}
		}
	}

	// ---------- 群原始 space_id（不经 SetEffectiveSpaceID 改写） ----------
	// Round-3 修复 (GH octo-server#154 Round-2 Finding 1)：
	// GetGroupDetails 内部走 SetEffectiveSpaceIDFromMap，会把外部成员视角下的
	// GroupResp.SpaceID 从群表权威值改写成成员的 source Space。
	// fillConversationSpaceIDs 直接用 groupMap[groupNo].SpaceID 时拿到的就是被
	// 改写后的 effective 值 → SyncUserConversationResp.SpaceID 与
	// MySourceSpaceID 同值。响应契约要求 SpaceID 是群表的权威归属 Space，
	// 必须另起一次 GetGroups(groupNos) 取原始 SpaceID 构建 rawGroupSpaceMap。
	// GetGroups 返回的 InfoResp.SpaceID 直接来自群表行，不做 effective rewrite。
	rawGroupSpaceMap := make(map[string]string, len(groupNos))
	if len(groupNos) > 0 {
		rawGroups, rawErr := co.groupService.GetGroups(groupNos)
		if rawErr != nil {
			// 非致命：缺失 SpaceID 回填会让客户端走"未知 Space"分支，
			// 与历史 v1 fail-open 行为一致。FilterConversationsBySpace 走它自己
			// 的 GetGroupSpaceMap 路径，互不影响。
			co.Warn("查询群原始 SpaceID 失败，跳过 conversation-level SpaceID 回填",
				zap.Error(rawErr))
		} else {
			for _, g := range rawGroups {
				if g == nil {
					continue
				}
				rawGroupSpaceMap[g.GroupNo] = g.SpaceID
			}
		}
	}

	// ---------- 群组分类  ----------
	groupCategoryMap := map[string]*GroupCategorySetting{}
	if len(groupNos) > 0 {
		categorySettings, err := co.groupCategoryDB.QueryCategorySettingsByGroupNos(groupNos, loginUID)
		if err != nil {
			co.Error("查询群组分类失败！", zap.Error(err))
			// 不阻塞流程，category 查询失败时继续返回会话列表
		} else if len(categorySettings) > 0 {
			for _, setting := range categorySettings {
				groupCategoryMap[setting.GroupNo] = setting
			}
		}
	}

	// ---------- 用户频道消息偏移  ----------
	channelOffsetModelMap := map[string]*channelOffsetModel{}
	if len(channelIDs) > 0 {
		channelOffsetModels, err := co.channelOffsetDB.queryWithUIDAndChannelIDs(loginUID, channelIDs)
		if err != nil {
			co.Error("查询用户频道偏移量失败！", zap.Error(err))
			c.ResponseError(errors.New("查询用户频道偏移量失败！"))
			return
		}
		if len(channelOffsetModels) > 0 {
			for _, channelOffsetM := range channelOffsetModels {
				channelOffsetModelMap[fmt.Sprintf("%s-%d", channelOffsetM.ChannelID, channelOffsetM.ChannelType)] = channelOffsetM
			}
		}
	}

	// ---------- 频道设置  ----------
	channelSettings, err := co.channelService.GetChannelSettings(channelIDs)
	if err != nil {
		co.Error("查询频道设置失败！", zap.Error(err))
		c.ResponseError(errors.New("查询频道设置失败！"))
		return
	}
	channelSettingMessageOffsetMap := make(map[string]uint32)
	if len(channelSettings) > 0 {
		for _, channelSetting := range channelSettings {
			channelSettingMessageOffsetMap[fmt.Sprintf("%s-%d", channelSetting.ChannelID, channelSetting.ChannelType)] = channelSetting.OffsetMessageSeq
		}
	}

	syncUserConversationResps := make([]*SyncUserConversationResp, 0, len(conversations))
	userKey := loginUID
	if len(conversations) > 0 {
		for _, conversation := range conversations {

			if conversation.ChannelType == common.ChannelTypeGroup.Uint8() {
				vaild := false
				for _, groupVaild := range groupVailds {
					if groupVaild == conversation.ChannelID {
						vaild = true
						break
					}
				}
				if !vaild { // 无效群则跳过
					continue
				}
			}

			if conversation.ChannelType == common.ChannelTypeCommunityTopic.Uint8() && threadFilterEnabled {
				if shortID, ok := threadChannelShortIDMap[conversation.ChannelID]; ok {
					if _, active := activeThreadShortIDs[shortID]; !active {
						continue
					}
				}
			}

			var mute = 0
			var stick = 0
			if conversation.ChannelType == common.ChannelTypePerson.Uint8() {
				userDetail := userMap[conversation.ChannelID]
				if userDetail != nil {
					mute = userDetail.Mute
					stick = userDetail.Top
				}
			} else {
				group := groupMap[conversation.ChannelID]
				if group != nil {
					mute = group.Mute
					stick = group.Top
				}

			}
			channelKey := fmt.Sprintf("%s-%d", conversation.ChannelID, conversation.ChannelType)
			var channelOffsetMessageSeq = channelSettingMessageOffsetMap[channelKey]
			// channelSetting := channelSettingMap[channelKey]
			channelOffsetM := channelOffsetModelMap[channelKey]
			deviceOffsetM := deviceOffsetModelMap[channelKey]
			extra := conversationExtraMap[channelKey]
			syncUserConversationResp := newSyncUserConversationResp(conversation, extra, loginUID, co.messageExtraDB, co.messageReactionDB, co.messageUserExtraDB, mute, stick, channelOffsetM, deviceOffsetM, channelOffsetMessageSeq)
			// 填充群组分类信息
			if conversation.ChannelType == common.ChannelTypeGroup.Uint8() {
				if categorySetting := groupCategoryMap[conversation.ChannelID]; categorySetting != nil {
					syncUserConversationResp.CategoryID = categorySetting.CategoryID
					syncUserConversationResp.CategorySort = categorySetting.CategorySort
				}
			}
			// 填充 App Bot 标记
			if conversation.ChannelType == common.ChannelTypePerson.Uint8() && appBotUIDs[conversation.ChannelID] {
				syncUserConversationResp.BotType = "app_bot"
			}
			if len(syncUserConversationResp.Recents) > 0 {
				syncUserConversationResps = append(syncUserConversationResps, syncUserConversationResp)
			}
			// if channelSetting != nil {
			// 	syncUserConversationResp.ParentChannelID = channelSetting.ParentChannelID
			// 	syncUserConversationResp.ParentChannelType = channelSetting.ParentChannelType
			// }

			// 缓存频道对应的最新的消息messageSeq
			if !co.ctx.GetConfig().MessageSaveAcrossDevice {

				if len(syncUserConversationResp.Recents) > 0 {
					co.syncConversationResultCacheLock.Lock()
					channelMessageSeqs := co.syncConversationResultCacheMap[userKey]
					if channelMessageSeqs == nil {
						channelMessageSeqs = make([]string, 0)
					}
					channelMessageSeqs = append(channelMessageSeqs, co.channelMessageSeqJoin(conversation.ChannelID, conversation.ChannelType, syncUserConversationResp.Recents[0].MessageSeq))
					co.syncConversationResultCacheMap[userKey] = channelMessageSeqs
					co.syncConversationResultCacheLock.Unlock()
				}
			}
		}
	}
	// PR-B (#1377): cursor 必须基于 raw conversations 推进。服务端过滤掉的会话
	// （archived 子区 / 已删除子区 / 当前用户已退群）可能正好是本批最高 version 的那条；
	// 用过滤后列表的尾部 version 会让 cursor 卡在它前面，下次 sync 重复拉同一批。
	lastVersion := maxConversationVersion(conversations, req.Version)
	co.syncConversationResultCacheLock.Lock()
	cacheVersion := co.syncConversationVersionMap[userKey]
	if cacheVersion < lastVersion {
		co.syncConversationVersionMap[userKey] = lastVersion
	}
	co.syncConversationResultCacheLock.Unlock()

	// ---------- 子区 source_message_id ----------
	co.fillThreadMeta(syncUserConversationResps)

	// YUJ-98 / YUJ-101: 会话同步 Recents 里的群消息同样需要回填
	// msg-level 外部来源字段（from_is_external / from_source_space_name /
	// from_home_space_id / from_home_space_name），
	// 保持与 /message/channel/sync 的字段口径一致，
	// 避免前端 fromHomeSpaceId / fromIsExternal getter 在增量同步路径读到空值。
	co.enrichConversationExternalMarkers(syncUserConversationResps)

	// GH#153: 把 resolved space_id 回填到 SyncUserConversationResp，
	// 同时为外部群成员填充 my_source_space_id。
	// 群聊的 channel_id 是裸 group_no，newSyncUserConversationResp 走
	// ParseChannelID 拿不到 SpaceID；客户端 WebSocket 收到群消息时若
	// 没有 conversation-level 的 SpaceID 兜底，就会 fail-open 把消息
	// 渲染到错误 Space tab。这里用 handler 早已批量查好的 rawGroupSpaceMap +
	// externalGroupMap 一次性补齐，避免客户端再发请求。
	//
	// Round-3 修复 (GH octo-server#154 Round-2)：
	//   - SpaceID 走 rawGroupSpaceMap（GetGroups 原始值），不用 groupMap
	//     （GetGroupDetails 已被 SetEffectiveSpaceID 改写）→ Finding 1。
	//   - 把 defaultSpaceID 传入用于 MySourceSpaceID 空值兜底（旧外部成员行
	//     source_space_id=""），与 decideConvKeepInSpace 同口径 → Finding 2。
	externalGroupMap, externalErr := co.groupDB.QueryExternalGroupNosForUser(loginUID)
	if externalErr != nil {
		// 非致命：缺失 my_source_space_id 不影响 conversation-level
		// SpaceID 回填；保持空 map 让 fillConversationSpaceIDs 退化为
		// 仅填 SpaceID。FilterConversationsBySpace 走它自己的失败兜底。
		co.Warn("查询外部群失败，跳过 my_source_space_id 回填", zap.Error(externalErr))
		externalGroupMap = make(map[string]string)
	}
	// defaultSpaceID 用于外部群 source_space_id="" 的空值兜底。
	// 查询失败时返回空串，fillConversationSpaceIDs 自然退化为不写
	// MySourceSpaceID —— omitempty 保持向后兼容。
	defaultSpaceID := space.GetUserDefaultSpaceID(co.ctx, loginUID)
	fillConversationSpaceIDs(syncUserConversationResps, rawGroupSpaceMap, externalGroupMap, defaultSpaceID)

	// 查询通话中的频道
	// 加入的群聊
	joinedGroups, err := co.groupService.GetGroupsWithMemberUID(loginUID)
	if err != nil {
		co.Error("查询加入的群聊错误", zap.Error(err))
		c.ResponseError(errors.New("查询加入的群聊错误"))
		return
	}
	callChannelIDs := make([]string, 0)
	if len(joinedGroups) > 0 {
		for _, g := range joinedGroups {
			callChannelIDs = append(callChannelIDs, g.GroupNo)
		}
	}
	// 好友
	friends, err := co.userService.GetFriends(loginUID)
	if err != nil {
		co.Error("查询好友错误", zap.Error(err))
		c.ResponseError(errors.New("查询好友错误"))
		return
	}
	if len(friends) > 0 {
		for _, f := range friends {
			fakeChannelID := common.GetFakeChannelIDWith(f.UID, loginUID)
			callChannelIDs = append(callChannelIDs, fakeChannelID)
		}
	}
	var callingChannels []*model.CallingChannelResp
	modules := register.GetModules(co.ctx)
	for _, m := range modules {
		if m.BussDataSource.GetCallingChannel != nil {
			callingChannels, _ = m.BussDataSource.GetCallingChannel(loginUID, callChannelIDs)
			break
		}
	}
	channelStates := make([]*ChannelState, 0)
	if len(callingChannels) > 0 {
		for _, channel := range callingChannels {
			channelStates = append(channelStates, &ChannelState{
				ChannelID:   channel.ChannelID,
				ChannelType: channel.ChannelType,
				Calling:     1,
			})
		}
	}
	// Space 过滤
	if hasSpaceFilter {
		// Person 频道：计算 per-Space 未读计数（在过滤之前，需要原始会话数据）
		fillPersonSpaceUnread(syncUserConversationResps, conversations, filterSpaceID, loginUID, co.ctx)

		syncUserConversationResps = FilterConversationsBySpace(
			syncUserConversationResps, filterSpaceID, loginUID, co.ctx, co.groupService,
		)

		// YUJ-216 / GH#1280: 系统 Bot（botfather 等）在所有 Space 都应可见。
		// IM 核心按 version 增量返回 conversation，若系统 Bot 在此次 window 内
		// 没有新消息，Space 过滤后就会缺席；移动端没有前端兜底，会直接丢失 entry。
		// 在过滤结果之后显式补齐占位 entry，保证 SystemBot channel 在每一个
		// X-Space-ID 维度下都可见。
		syncUserConversationResps = EnsureSystemBotsPresent(syncUserConversationResps)
	}

	c.Response(SyncUserConversationRespWrap{
		Conversations: syncUserConversationResps,
		UID:           loginUID,
		Users:         users,
		Groups:        groups,
		ChannelStates: channelStates,
	})
}

func (co *Conversation) channelMessageSeqJoin(channelID string, channelType uint8, lastMessageSeq uint32) string {
	return fmt.Sprintf("%s:%d:%d", channelID, channelType, lastMessageSeq)
}

func (co *Conversation) channelMessageSeqSplit(channelMessageSeqStr string) (channelID string, channelType uint8, lastMessageSeq uint32) {
	channelMessageSeqList := strings.Split(channelMessageSeqStr, ":")
	if len(channelMessageSeqList) == 3 {
		channelID = channelMessageSeqList[0]
		channelTypeI64, _ := strconv.ParseInt(channelMessageSeqList[1], 10, 64)
		channelType = uint8(channelTypeI64)
		lastMessageSeqI64, _ := strconv.ParseInt(channelMessageSeqList[2], 10, 64)
		lastMessageSeq = uint32(lastMessageSeqI64)
	}
	return
}

func (co *Conversation) syncUserConversationAck(c *wkhttp.Context) {
	var req struct {
		CMDVersion int64  `json:"cmd_version"` // cmd版本
		DeviceUUID string `json:"device_uuid"` // 设备uuid
	}
	if err := c.BindJSON(&req); err != nil {
		co.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	if co.ctx.GetConfig().MessageSaveAcrossDevice {
		c.ResponseOK()
		return
	}

	loginUID := c.GetLoginUID()
	userKey := loginUID

	co.syncConversationResultCacheLock.Lock()
	channelMessageSeqStrs := co.syncConversationResultCacheMap[userKey]
	delete(co.syncConversationResultCacheMap, userKey)
	co.syncConversationResultCacheLock.Unlock()

	userLastOffsetModels := make([]*userLastOffsetModel, 0, len(channelMessageSeqStrs))
	if len(channelMessageSeqStrs) > 0 {
		for _, channelMessageSeqStr := range channelMessageSeqStrs {
			channelID, channelType, messageSeq := co.channelMessageSeqSplit(channelMessageSeqStr)

			var has bool
			for _, userLastOffsetM := range userLastOffsetModels {
				if channelID == userLastOffsetM.ChannelID && channelType == userLastOffsetM.ChannelType && messageSeq > uint32(userLastOffsetM.MessageSeq) {
					userLastOffsetM.MessageSeq = int64(messageSeq)
					has = true
					break
				}
			}
			if !has {
				userLastOffsetModels = append(userLastOffsetModels, &userLastOffsetModel{
					UID:         loginUID,
					ChannelID:   channelID,
					ChannelType: channelType,
					MessageSeq:  int64(messageSeq),
				})
			}
		}
	}

	if len(userLastOffsetModels) > 0 {
		err := co.insertUserLastOffsets(userLastOffsetModels)
		if err != nil {
			c.ResponseError(errors.New("插入设备偏移数据失败！"))
			return
		}
	}
	co.syncConversationResultCacheLock.Lock()
	version := co.syncConversationVersionMap[userKey]
	delete(co.syncConversationVersionMap, userKey)
	co.syncConversationResultCacheLock.Unlock()
	if version > 0 {
		err := co.setUserConversationMaxVersion(loginUID, version)
		if err != nil {
			co.Error("设置设备最近会话最大版本号失败！", zap.Error(err))
			c.ResponseError(errors.New("设置设备最近会话最大版本号失败！"))
			return
		}
	}

	c.ResponseOK()
}

func (co *Conversation) insertDeviceOffsets(deviceOffsetModels []*deviceOffsetModel) error {
	tx, err := co.ctx.DB().Begin()
	if err != nil {
		co.Error("开启事务失败！", zap.Error(err))
		return errors.New("开启事务失败！")
	}
	defer func() {
		if err := recover(); err != nil {
			tx.RollbackUnlessCommitted()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	for _, deviceOffsetM := range deviceOffsetModels {
		err := co.deviceOffsetDB.insertOrUpdateTx(tx, deviceOffsetM)
		if err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		co.Error("提交事务失败！", zap.Error(err))
		return err
	}
	return nil
}
func (co *Conversation) insertUserLastOffsets(userLastOffsetModels []*userLastOffsetModel) error {
	tx, err := co.ctx.DB().Begin()
	if err != nil {
		co.Error("开启事务失败！", zap.Error(err))
		return errors.New("开启事务失败！")
	}
	defer func() {
		if err := recover(); err != nil {
			tx.RollbackUnlessCommitted()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	for _, userLastOffsetM := range userLastOffsetModels {
		err := co.userLastOffsetDB.insertOrUpdateTx(tx, userLastOffsetM)
		if err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		co.Error("提交事务失败！", zap.Error(err))
		return err
	}
	return nil
}

func (co *Conversation) getDeviceConversationMaxVersion(uid string, deviceUUID string) (int64, error) {
	versionStr, err := co.ctx.GetRedisConn().GetString(fmt.Sprintf("deviceMaxVersion:%s-%s", uid, deviceUUID))
	if err != nil {
		return 0, err
	}
	if versionStr == "" {
		return 0, nil
	}
	return strconv.ParseInt(versionStr, 10, 64)
}
func (co *Conversation) setDeviceConversationMaxVersion(uid string, deviceUUID string, version int64) error {
	err := co.ctx.GetRedisConn().Set(fmt.Sprintf("deviceMaxVersion:%s-%s", uid, deviceUUID), fmt.Sprintf("%d", version))
	return err
}

func (co *Conversation) getUserConversationMaxVersion(uid string) (int64, error) {
	versionStr, err := co.ctx.GetRedisConn().GetString(fmt.Sprintf("userMaxVersion:%s", uid))
	if err != nil {
		return 0, err
	}
	if versionStr == "" {
		return 0, nil
	}
	return strconv.ParseInt(versionStr, 10, 64)
}
func (co *Conversation) setUserConversationMaxVersion(uid string, version int64) error {
	err := co.ctx.GetRedisConn().Set(fmt.Sprintf("userMaxVersion:%s", uid), fmt.Sprintf("%d", version))
	return err
}

// 获取最近会话列表
func (co *Conversation) getConversations(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	resps, err := co.ctx.IMGetConversations(loginUID)
	if err != nil {
		co.Error("获取最近会话失败！", zap.Error(err))
		c.ResponseError(errors.New("获取最近会话失败！"))
		return
	}
	conversationResps := make([]conversationResp, 0, len(resps))
	userResps := make([]userResp, 0)
	groupResps := make([]groupResp, 0)

	if resps != nil {
		userUIDs := make([]string, 0)
		groupNos := make([]string, 0)
		visitorNos := make([]string, 0)
		channelIds := make([]string, 0)
		for _, resp := range resps {
			fakeChannelID := resp.ChannelID
			if resp.ChannelType == common.ChannelTypePerson.Uint8() {
				fakeChannelID = common.GetFakeChannelIDWith(resp.ChannelID, loginUID)
			}
			channelIds = append(channelIds, fakeChannelID)
		}
		channelSettings, err := co.channelService.GetChannelSettings(channelIds)
		if err != nil {
			co.Error("查询频道设置错误！", zap.Error(err))
			c.ResponseError(errors.New("查询频道设置错误！"))
			return
		}
		channelSettingMessageOffsetMap := make(map[string]uint32)
		if len(channelSettings) > 0 {
			for _, channelSetting := range channelSettings {
				channelSettingMessageOffsetMap[fmt.Sprintf("%s-%d", channelSetting.ChannelID, channelSetting.ChannelType)] = channelSetting.OffsetMessageSeq
			}
		}
		for _, resp := range resps {
			conversationResp := &conversationResp{}
			channelKey := fmt.Sprintf("%s-%d", resp.ChannelID, resp.ChannelType)
			conversationResp.from(resp, loginUID, nil, nil, channelSettingMessageOffsetMap[channelKey])
			conversationResps = append(conversationResps, *conversationResp)
			if resp.ChannelType == common.ChannelTypePerson.Uint8() {
				userUIDs = append(userUIDs, resp.ChannelID)
			} else {
				if co.ctx.GetConfig().IsVisitorChannel(resp.ChannelID) {
					visitorNo, _ := co.ctx.GetConfig().GetCustomerServiceVisitorUID(resp.ChannelID)
					visitorNos = append(visitorNos, visitorNo)
				} else {
					groupNos = append(groupNos, resp.ChannelID)
				}

			}
		}
		userDetails, err := co.userDB.QueryDetailByUIDs(userUIDs, loginUID)
		if err != nil {
			co.Error("查询用户详情失败！")
			c.ResponseError(errors.New("查询用户详情失败！"))
			return
		}
		groupDetails, err := co.groupDB.QueryDetailWithGroupNos(groupNos, loginUID)
		if err != nil {
			co.Error("查询用户详情失败！")
			c.ResponseError(errors.New("查询用户详情失败！"))
			return
		}

		if len(userDetails) > 0 {
			for _, userDetail := range userDetails {
				userResp := userResp{}.from(userDetail, co.ctx.GetConfig().GetAvatarPath(userDetail.UID))
				// if userDetail.UID == loginUID {
				// 	userResp.Name = s.ctx.GetConfig().FileHelperName
				// 	userResp.Avatar = s.ctx.GetConfig().FileHelperAvatar
				// }
				userResps = append(userResps, userResp)

			}
		}
		if len(groupDetails) > 0 {
			for _, group := range groupDetails {
				groupResps = append(groupResps, groupResp{}.from(group))
			}
		}
	}
	c.JSON(http.StatusOK, conversationWrapResp{
		Conversations: conversationResps,
		Groups:        groupResps,
		Users:         userResps,
	})
}

// 清除最近会话未读数
func (co *Conversation) clearConversationUnread(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	var req clearConversationUnreadReq
	if err := c.BindJSON(&req); err != nil {
		co.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(common.ErrData)
		return
	}
	// if co.ctx.GetConfig().IsVisitorChannel(req.ChannelID) {
	// 	c.Request.URL.Path = "/v1/hotline/coversation/clearUnread"
	// 	co.ctx.Server.GetRoute().HandleContext(c)
	// 	return
	// }
	var messageSeq uint32 = 0
	if req.ChannelType == common.ChannelTypeGroup.Uint8() {
		groupInfo, err := co.groupService.GetGroupWithGroupNo(req.ChannelID)
		if err != nil {
			co.Error("查询群聊信息失败！", zap.Error(err))
			c.ResponseError(errors.New("查询群聊信息失败！"))
			return
		}
		if groupInfo != nil && groupInfo.GroupType == group.GroupTypeSuper {
			messageSeq = req.MessageSeq // 只有超级群才传messageSeq
		}
	}

	err := co.ctx.IMClearConversationUnread(config.ClearConversationUnreadReq{
		UID:         loginUID,
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		Unread:      req.Unread,
		MessageSeq:  messageSeq,
	})
	if err != nil {
		c.ResponseError(err)
		return
	}
	// 发送清空红点的命令
	err = co.ctx.SendCMD(config.MsgCMDReq{
		NoPersist:   true,
		ChannelID:   loginUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		CMD:         common.CMDConversationUnreadClear,
		Param: map[string]interface{}{
			"channel_id":   req.ChannelID,
			"channel_type": req.ChannelType,
			"unread":       req.Unread,
		},
	})
	if err != nil {
		co.Error("命令发送失败！", zap.String("cmd", common.CMDConversationUnreadClear))
		c.ResponseError(errors.New("命令发送失败！"))
		return
	}
	c.ResponseOK()
}

// ---------- vo ----------

// SyncUserConversationRespWrap SyncUserConversationRespWrap
type SyncUserConversationRespWrap struct {
	UID           string                      `json:"uid"` // 请求者uid
	Conversations []*SyncUserConversationResp `json:"conversations"`
	Users         []*user.UserDetailResp      `json:"users"`          // 用户详情
	Groups        []*group.GroupResp          `json:"groups"`         // 群
	ChannelStates []*ChannelState             `json:"channel_status"` // 频道状态
}

type clearConversationUnreadReq struct {
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
	Unread      int    `json:"unread"` // 未读数量 0表示清空所有未读数量
	MessageSeq  uint32 `json:"message_seq"`
}

type ChannelState struct {
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
	Calling     int    `json:"calling"` // 是否正在通话
}

type conversationResp struct {
	ChannelID   string       `json:"channel_id"`   // 频道ID
	ChannelType uint8        `json:"channel_type"` // 频道类型
	Unread      int64        `json:"unread"`       // 未读数
	Timestamp   int64        `json:"timestamp"`    // 最后一次会话时间戳
	LastMessage *MsgSyncResp `json:"last_message"` // 最后一条消息
}

type conversationWrapResp struct {
	Conversations []conversationResp `json:"conversations"` // 最近会话
	Groups        []groupResp        `json:"groups"`        // 群组集合
	Users         []userResp         `json:"users"`         // 好友集合
}

func (m *conversationResp) from(resp *config.ConversationResp, loginUID string, messageExtra *messageExtraDetailModel, messageUserExtraM *messageUserExtraModel, channelOffsetMessageSeq uint32) {
	m.ChannelID = resp.ChannelID
	m.ChannelType = resp.ChannelType
	m.Unread = resp.Unread
	m.Timestamp = resp.Timestamp
	msgSyncResp := &MsgSyncResp{}
	msgSyncResp.from(resp.LastMessage, loginUID, messageExtra, messageUserExtraM, nil, channelOffsetMessageSeq)
	m.LastMessage = msgSyncResp
}

type conversationExtraResp struct {
	ChannelID      string `json:"channel_id"`
	ChannelType    uint8  `json:"channel_type"`
	BrowseTo       uint32 `json:"browse_to"`
	KeepMessageSeq uint32 `json:"keep_message_seq"`
	KeepOffsetY    int    `json:"keep_offset_y"`
	Draft          string `json:"draft"` // 草稿
	Version        int64  `json:"version"`
}

func newConversationExtraResp(m *conversationExtraModel) *conversationExtraResp {

	return &conversationExtraResp{
		ChannelID:      m.ChannelID,
		ChannelType:    m.ChannelType,
		BrowseTo:       m.BrowseTo,
		KeepMessageSeq: m.KeepMessageSeq,
		KeepOffsetY:    m.KeepOffsetY,
		Draft:          m.Draft,
		Version:        m.Version,
	}
}

type groupResp struct {
	GroupNo   string `json:"group_no"`  // 群编号
	Name      string `json:"name"`      // 群名称
	Notice    string `json:"notice"`    // 群公告
	Mute      int    `json:"mute"`      // 免打扰
	Top       int    `json:"top"`       // 置顶
	ShowNick  int    `json:"show_nick"` // 显示昵称
	Save      int    `json:"save"`      // 是否保存
	Forbidden int    `json:"forbidden"` // 是否全员禁言
	Invite    int    `json:"invite"`    // 群聊邀请确认
}

func (g groupResp) from(group *group.DetailModel) groupResp {
	return groupResp{
		GroupNo:   group.GroupNo,
		Name:      group.Name,
		Notice:    group.Notice,
		Mute:      group.Mute,
		Top:       group.Top,
		ShowNick:  group.ShowNick,
		Save:      group.Save,
		Forbidden: group.Forbidden,
		Invite:    group.Invite,
	}
}

type userResp struct {
	ID     int64  `json:"id"`
	UID    string `json:"uid"`    // 好友uid
	Name   string `json:"name"`   // 好友名称
	Avatar string `json:"avatar"` // 头像
	Mute   int    `json:"mute"`
	Top    int    `json:"top"`
	Online int    `json:"online"` // 是否在线
}

func (u userResp) from(user *user.Detail, avatarPath string) userResp {
	return userResp{
		ID:     user.Id,
		UID:    user.UID,
		Name:   user.Name,
		Mute:   user.Mute,
		Top:    user.Top,
		Avatar: avatarPath,
	}
}

// type messageHeader struct {
// 	NoPersist int `json:"no_persist"` // 是否不持久化
// 	RedDot    int `json:"red_dot"`    // 是否显示红点
// 	SyncOnce  int `json:"sync_once"`  // 此消息只被同步或被消费一次
// }

// type msgSyncResp struct {
// 	Header       messageHeader          `json:"header"`        // 消息头部
// 	MessageID    int64                  `json:"message_id"`    // 服务端的消息ID(全局唯一)
// 	MessageIDStr string                 `json:"message_idstr"` // 服务端的消息ID(全局唯一)
// 	MessageSeq   uint32                 `json:"message_seq"`   // 消息序列号 （用户唯一，有序递增）
// 	ClientMsgNo  string                 `json:"client_msg_no"` // 客户端消息唯一编号
// 	FromUID      string                 `json:"from_uid"`      // 发送者UID
// 	ToUID        string                 `json:"to_uid"`        // 接受者uid
// 	ChannelID    string                 `json:"channel_id"`    // 频道ID
// 	ChannelType  uint8                  `json:"channel_type"`  // 频道类型
// 	Timestamp    int32                  `json:"timestamp"`     // 服务器消息时间戳(10位，到秒)
// 	Payload      map[string]interface{} `json:"payload"`       // 消息内容
// 	IsDeleted    uint8                  `json:"is_deleted"`    // 是否已删除
// }

// func (m *msgSyncResp) from(msgResp *config.MessageResp, loginUID string) {
// 	m.Header.NoPersist = msgResp.Header.NoPersist
// 	m.Header.RedDot = msgResp.Header.RedDot
// 	m.Header.SyncOnce = msgResp.Header.SyncOnce
// 	m.MessageID = msgResp.MessageID
// 	m.MessageIDStr = strconv.FormatInt(msgResp.MessageID, 10)
// 	m.MessageSeq = msgResp.MessageSeq
// 	m.ClientMsgNo = msgResp.ClientMsgNo
// 	m.FromUID = msgResp.FromUID
// 	m.ToUID = msgResp.ToUID
// 	m.ChannelID = msgResp.ChannelID
// 	m.ChannelType = msgResp.ChannelType
// 	m.Timestamp = msgResp.Timestamp
// 	var payloadMap map[string]interface{}
// 	err := util.ReadJsonByByte(msgResp.Payload, &payloadMap)
// 	if err != nil {
// 		log.Warn("负荷数据不是json格式！", zap.Error(err), zap.String("payload", string(msgResp.Payload)))
// 	}
// 	if len(payloadMap) > 0 {
// 		visibles := payloadMap["visibles"]
// 		if visibles != nil {
// 			visiblesArray := visibles.([]interface{})
// 			if len(visiblesArray) > 0 {
// 				m.IsDeleted = 1
// 				for _, limitUID := range visiblesArray {
// 					if limitUID == loginUID {
// 						m.IsDeleted = 0
// 					}
// 				}
// 			}
// 		}
// 	}
// 	m.Payload = payloadMap
// }

// SyncUserConversationResp 最近会话离线返回
type SyncUserConversationResp struct {
	ChannelID   string `json:"channel_id"`             // 频道ID
	ChannelType uint8  `json:"channel_type"`           // 频道类型
	SpaceID     string `json:"space_id,omitempty"`     // Space ID
	// MySourceSpaceID 仅在 GROUP / COMMUNITY_TOPIC 频道且当前用户以外部成员
	// 身份加入时非空。值取自 group_member.source_space_id，对应"我从哪个
	// Space 加入了这个外部群"。客户端 WebSocket 收到该群实时消息时，可据此
	// 把消息归属到当前 user 的 source Space —— 与服务端
	// FilterConversationsBySpace 对外部群的可见性判定保持同口径，避免
	// 三端 fail-open 把跨 Space 消息渲染到错误的 Space tab (GH#153)。
	MySourceSpaceID  string                 `json:"my_source_space_id,omitempty"` // 外部群成员的 source Space ID
	Thread           *threadMetaResp        `json:"thread,omitempty"`             // 子区元数据（仅 thread 频道）
	CategoryID       *string                `json:"category_id,omitempty"`        // 用户自定义分类ID（仅群组）
	CategorySort     int                    `json:"category_sort,omitempty"`      // 分类内排序（仅群组）
	Unread           int                    `json:"unread,omitempty"`             // 未读消息
	SpaceUnread      *int                   `json:"space_unread,omitempty"`       // Space 维度未读（仅 Person 频道）
	SpaceLastMessage *MsgSyncResp           `json:"space_last_message,omitempty"` // Space 维度最后一条消息（仅 Person 频道）
	Mute             int                    `json:"mute,omitempty"`               // 免打扰
	Stick            int                    `json:"stick,omitempty"`              //  置顶
	Timestamp        int64                  `json:"timestamp"`                    // 最后一次会话时间
	LastMsgSeq       int64                  `json:"last_msg_seq"`                 // 最后一条消息seq
	LastClientMsgNo  string                 `json:"last_client_msg_no"`           // 最后一条客户端消息编号
	OffsetMsgSeq     int64                  `json:"offset_msg_seq"`               // 偏移位的消息seq
	Version          int64                  `json:"version,omitempty"`            // 数据版本
	Recents          []*MsgSyncResp         `json:"recents,omitempty"`            // 最近N条消息
	Extra            *conversationExtraResp `json:"extra,omitempty"`              // 扩展
	BotType          string                 `json:"bot_type,omitempty"`           // Bot 类型（"app_bot" 表示应用 Bot）
}

func newSyncUserConversationResp(resp *config.SyncUserConversationResp, extra *conversationExtraResp, loginUID string, messageExtraDB *messageExtraDB, messageReactionDB *messageReactionDB, messageUserExtraDB *messageUserExtraDB, mute int, stick int, channelOffsetM *channelOffsetModel, deviceOffsetM *deviceOffsetModel, channelOffsetMessageSeq uint32) *SyncUserConversationResp {
	recents := make([]*MsgSyncResp, 0, len(resp.Recents))
	lastClientMsgNo := "" // 最新未被删除的消息的clientMsgNo
	if len(resp.Recents) > 0 {
		messageIDs := make([]string, 0, len(resp.Recents))
		for _, message := range resp.Recents {
			messageIDs = append(messageIDs, fmt.Sprintf("%d", message.MessageID))
		}

		// 查询用户个人修改的消息数据
		messageUserExtraModels, err := messageUserExtraDB.queryWithMessageIDsAndUID(messageIDs, loginUID)
		if err != nil {
			log.Error("查询消息编辑字段失败！", zap.Error(err))
		}
		messageUserExtraMap := map[string]*messageUserExtraModel{}
		if len(messageUserExtraModels) > 0 {
			for _, messageUserEditM := range messageUserExtraModels {
				messageUserExtraMap[messageUserEditM.MessageID] = messageUserEditM
			}
		}

		// 消息扩充数据
		messageExtras, err := messageExtraDB.queryWithMessageIDsAndUID(messageIDs, loginUID)
		if err != nil {
			log.Error("查询消息扩展字段失败！", zap.Error(err))
		}
		messageExtraMap := map[string]*messageExtraDetailModel{}
		if len(messageExtras) > 0 {
			for _, messageExtra := range messageExtras {
				messageExtraMap[messageExtra.MessageID] = messageExtra
			}
		}
		// 消息回应
		messageReaction, err := messageReactionDB.queryWithMessageIDs(messageIDs)
		if err != nil {
			log.Error("查询消息回应错误", zap.Error(err))
		}
		messageReactionMap := map[string][]*reactionModel{}
		if len(messageReaction) > 0 {
			for _, reaction := range messageReaction {
				msgReactionList := messageReactionMap[reaction.MessageID]
				if msgReactionList == nil {
					msgReactionList = make([]*reactionModel, 0)
				}
				msgReactionList = append(msgReactionList, reaction)
				messageReactionMap[reaction.MessageID] = msgReactionList
			}
		}
		for _, message := range resp.Recents {
			if channelOffsetM != nil && message.MessageSeq <= channelOffsetM.MessageSeq {
				continue
			}
			if deviceOffsetM != nil && message.MessageSeq <= uint32(deviceOffsetM.MessageSeq) {
				continue
			}
			messageIDStr := strconv.FormatInt(message.MessageID, 10)
			messageExtra := messageExtraMap[messageIDStr]
			messageUserExtra := messageUserExtraMap[messageIDStr]
			msgResp := &MsgSyncResp{}
			msgResp.from(message, loginUID, messageExtra, messageUserExtra, messageReactionMap[messageIDStr], channelOffsetMessageSeq)
			msgResp.ExtraVersion = 0
			if msgResp.MessageExtra != nil {
				msgResp.MessageExtra.ExtraVersion = 0
			}
			recents = append(recents, msgResp)
			if lastClientMsgNo == "" && msgResp.IsDeleted == 0 {
				lastClientMsgNo = msgResp.ClientMsgNo
			}
		}
	}
	if lastClientMsgNo == "" {
		lastClientMsgNo = resp.LastClientMsgNo
	}

	spaceID, _ := spacepkg.ParseChannelID(resp.ChannelID)
	return &SyncUserConversationResp{
		ChannelID:       resp.ChannelID,
		ChannelType:     resp.ChannelType,
		SpaceID:         spaceID,
		Unread:          resp.Unread,
		Timestamp:       resp.Timestamp,
		LastMsgSeq:      resp.LastMsgSeq,
		LastClientMsgNo: lastClientMsgNo,
		OffsetMsgSeq:    resp.OffsetMsgSeq,
		Version:         resp.Version,
		Mute:            mute,
		Stick:           stick,
		Recents:         recents,
		Extra:           extra,
	}
}

// threadMetaResp 子区元数据（仅 thread 频道返回）
type threadMetaResp struct {
	SourceMessageID *int64 `json:"source_message_id,omitempty"` // 源消息ID
	MessageCount    int64  `json:"message_count"`               // 消息数
}

// fillConversationSpaceIDs 把 resolved SpaceID + MySourceSpaceID 回填到 group /
// thread 频道的 SyncUserConversationResp。
//
// 背景 (GH octo-server#153)：
//   - newSyncUserConversationResp 通过 spacepkg.ParseChannelID(channelID) 推导
//     SpaceID。但群聊和子区的 channel_id 是裸 group_no（或 "{groupNo}____{shortID}"），
//     ParseChannelID 返回空串，导致客户端在 conversation/sync 响应里拿不到
//     conversation-level 的 Space 归属。
//   - 三端客户端收到 WebSocket 实时消息时，会 fallback 到 conversation-level
//     SpaceID 决定渲染到哪个 Space tab。空字符串触发 fail-open，跨 Space 消息
//     被错误渲染到当前 tab，构成 P1 信息泄漏（issue #153）。
//
// 回填规则：
//   - GROUP: SpaceID = rawGroupSpaceMap[channelID]（group 表权威值，未经
//     SetEffectiveSpaceID 改写）。用户作为外部成员加入时，再读 externalGroupMap
//     给 MySourceSpaceID 赋值。
//   - COMMUNITY_TOPIC: SpaceID = parent group 的 SpaceID（与 FilterRawConversationsBySpace
//     thread 分支的 fail-closed 同口径）。MySourceSpaceID 同样从 parent groupNo 取。
//   - PERSON: 不动 —— 私聊的 Space 归属在消息级 payload.space_id 上，
//     conversation 级别保持空，避免误把 DM 锁定到某个 Space。
//
// Round-3 修复 (GH octo-server#154 Round-2 Finding 1)：
//   - 之前传 groupMap (来自 GetGroupDetails) 的版本会被 SetEffectiveSpaceIDFromMap
//     污染：外部成员视角下 group.SpaceID 已被改写成 source Space，导致
//     SyncUserConversationResp.SpaceID 与 MySourceSpaceID 同值。响应契约要求
//     SpaceID 是群表权威值，handler 必须额外用 GetGroups 拿原始 space_id 构建
//     rawGroupSpaceMap 传入。
//
// Round-3 修复 (GH octo-server#154 Round-2 Finding 2)：
//   - externalGroupMap[groupNo] 可能存在但值为空串（旧外部成员行
//     source_space_id=""）。空串 + omitempty 会让客户端拿不到 my_source_space_id，
//     无法判断外部群在哪个 Space 下可见。空值兜底到 defaultSpaceID，与
//     decideConvKeepInSpace 同口径。
//
// rawGroupSpaceMap / externalGroupMap 都是 handler 已经查过的现成数据，本函数
// 纯内存操作，不发任何 DB 请求。map 缺失（如 thread 父群本批未活跃）时跳过该条
// —— 客户端拿到空 SpaceID 会自己降级，比写错的值更安全。
func fillConversationSpaceIDs(
	resps []*SyncUserConversationResp,
	rawGroupSpaceMap map[string]string,
	externalGroupMap map[string]string,
	defaultSpaceID string,
) {
	for _, r := range resps {
		if r == nil {
			continue
		}
		switch r.ChannelType {
		case common.ChannelTypeGroup.Uint8():
			if sid, ok := rawGroupSpaceMap[r.ChannelID]; ok {
				if r.SpaceID == "" {
					r.SpaceID = sid
				}
			}
			if src, ok := externalGroupMap[r.ChannelID]; ok {
				r.MySourceSpaceID = resolveMySourceSpaceID(src, defaultSpaceID)
			}
		case common.ChannelTypeCommunityTopic.Uint8():
			parentNo, _, perr := thread.ParseChannelID(r.ChannelID)
			if perr != nil {
				continue
			}
			if sid, ok := rawGroupSpaceMap[parentNo]; ok {
				if r.SpaceID == "" {
					r.SpaceID = sid
				}
			}
			if src, ok := externalGroupMap[parentNo]; ok {
				r.MySourceSpaceID = resolveMySourceSpaceID(src, defaultSpaceID)
			}
		}
	}
}

// resolveMySourceSpaceID 把 externalGroupMap 的 source_space_id 解析为客户端实际
// 可见的 Space：
//   - 非空：直接返回。
//   - 空串（旧外部成员行 source_space_id=""）：兜底到 defaultSpaceID
//     （decideConvKeepInSpace 同口径，space_filter.go:171/234）。defaultSpaceID
//     也是空时回退到空串——保持 omitempty 行为，与历史一致。
func resolveMySourceSpaceID(sourceSpaceID, defaultSpaceID string) string {
	if sourceSpaceID != "" {
		return sourceSpaceID
	}
	return defaultSpaceID
}

// fillThreadMeta 批量填充子区会话的元数据
func (co *Conversation) fillThreadMeta(resps []*SyncUserConversationResp) {
	// 收集所有 thread 频道的 shortID
	threadShortIDs := make([]string, 0)
	for _, resp := range resps {
		if resp.ChannelType != common.ChannelTypeCommunityTopic.Uint8() {
			continue
		}
		_, shortID, err := thread.ParseChannelID(resp.ChannelID)
		if err != nil {
			continue
		}
		threadShortIDs = append(threadShortIDs, shortID)
	}
	if len(threadShortIDs) == 0 {
		return
	}

	// 批量查询
	threadMetaMap, err := co.threadDB.QueryThreadMetaByShortIDs(threadShortIDs)
	if err != nil {
		co.Error("查询子区元数据失败", zap.Error(err))
		return
	}

	// 填充
	for _, resp := range resps {
		if resp.ChannelType != common.ChannelTypeCommunityTopic.Uint8() {
			continue
		}
		_, shortID, err := thread.ParseChannelID(resp.ChannelID)
		if err != nil {
			continue
		}
		if meta, ok := threadMetaMap[shortID]; ok {
			resp.Thread = &threadMetaResp{
				SourceMessageID: meta.SourceMessageID,
				MessageCount:    meta.MessageCount,
			}
		}
	}
}

// enrichConversationExternalMarkers 为会话同步 Recents 中的群消息回填
// msg-level 外部来源字段（YUJ-98 / YUJ-101）。
//
// 口径与 /message/channel/sync 保持一致：
//   - from_is_external / from_source_space_name（发送者视角）
//   - from_home_space_id / from_home_space_name（视角相对渲染 / YUJ-63）
//   - mergeforward content.users 元素的 is_external / source_space_name / home_space_*
//
// 每个群最多一条 DB 查询，遇到错误降级跳过（不让前端主流程崩掉）。
// 非群会话（ChannelTypePerson / thread / visitor）直接跳过——这些路径当前没有
// 多 Space 外部成员语义。
func (co *Conversation) enrichConversationExternalMarkers(resps []*SyncUserConversationResp) {
	if len(resps) == 0 {
		return
	}
	for _, resp := range resps {
		if resp == nil || len(resp.Recents) == 0 {
			continue
		}
		if resp.ChannelType != common.ChannelTypeGroup.Uint8() {
			continue
		}
		markers, err := co.groupService.GetMemberExternalMarkers(resp.ChannelID)
		if err != nil {
			co.Error("查询群成员外部来源标识失败",
				zap.Error(err),
				zap.String("group_no", resp.ChannelID))
			continue
		}
		applyExternalMarkers(resp.Recents, markers)
	}
}
