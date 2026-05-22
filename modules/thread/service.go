package thread

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"go.uber.org/zap"
)

// parsePayloadContent 从消息 payload 中提取 content 字段
func parsePayloadContent(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var m struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(payload, &m); err != nil {
		return ""
	}
	return m.Content
}

// IService 子区服务接口
type IService interface {
	// CreateThread 创建子区
	CreateThread(req *CreateThreadReq) (*ThreadResp, error)
	// UpdateName 修改子区名称
	UpdateName(groupNo, shortID, operatorUID, name string) error
	// GetThreads 分页获取群下的子区，同时返回总数。
	// statuses 决定包含的 status 集合（active / archived / 二者）；nil 或空一律按 active。
	GetThreads(groupNo string, statuses []int, pageIndex, pageSize int64) ([]*ThreadResp, int64, error)
	// GetThread 获取子区详情，loginUID 非空时填充当前用户的 mute 状态
	GetThread(groupNo, shortID, loginUID string) (*ThreadResp, error)
	// ArchiveThread 归档子区
	ArchiveThread(groupNo, shortID, operatorUID string) error
	// UnarchiveThread 取消归档
	UnarchiveThread(groupNo, shortID, operatorUID string) error
	// DeleteThread 删除子区
	DeleteThread(groupNo, shortID, operatorUID string) error
	// CanDelete 检查是否可以删除
	CanDelete(groupNo, shortID, uid string) (bool, error)
	// ExistThread 检查子区是否存在
	ExistThread(groupNo, shortID string) (bool, error)
	// JoinThread 加入子区
	JoinThread(groupNo, shortID, uid string) error
	// LeaveThread 离开子区
	LeaveThread(groupNo, shortID, uid string) error
	// GetMembers 获取子区成员
	GetMembers(groupNo, shortID string) ([]*MemberResp, error)
	// GetMemberUIDs 获取子区成员 UID 列表
	GetMemberUIDs(groupNo, shortID string) ([]string, error)
	// IsMember 检查是否是子区成员
	IsMember(groupNo, shortID, uid string) (bool, error)
	// RemoveUserFromGroupThreads 退群时移除用户在该群所有子区的成员身份和 IM 订阅
	RemoveUserFromGroupThreads(groupNo, uid string) error
	// GetThreadMd 获取子区 GROUP.md
	GetThreadMd(groupNo, shortID string) (*ThreadMdResult, error)
	// UpdateThreadMd 更新子区 GROUP.md（纯透传，不含权限检查）
	UpdateThreadMd(groupNo, shortID, content, updatedBy string) (int64, error)
	// DeleteThreadMd 删除子区 GROUP.md（纯透传，不含权限检查）
	DeleteThreadMd(groupNo, shortID, deletedBy string) (int64, error)
	// CanEditThreadMd 检查是否有编辑子区 GROUP.md 的权限（供 API Handler 层调用）
	CanEditThreadMd(groupNo, shortID, uid string) (bool, error)
	// UpdateSetting 更新当前用户对子区的个人设置(目前支持 mute)
	UpdateSetting(groupNo, shortID, uid string, settings map[string]interface{}) error
	// GetSettingsWithUIDs 批量查询一批用户对某子区的设置,无记录则不返回
	GetSettingsWithUIDs(groupNo, shortID string, uids []string) ([]*SettingResp, error)
}

// SettingResp 子区用户设置响应
type SettingResp struct {
	UID  string `json:"uid"`
	Mute int    `json:"mute"`
}

// Service 子区服务实现
type Service struct {
	ctx          *config.Context
	db           *DB
	groupService group.IService
	userService  user.IService
	log.Log
}

// NewService 创建子区服务
func NewService(ctx *config.Context) IService {
	return &Service{
		ctx:          ctx,
		db:           NewDB(ctx),
		groupService: group.NewService(ctx),
		userService:  user.NewService(ctx),
		Log:          log.NewTLog("threadService"),
	}
}

// CreateThreadReq 创建子区请求
type CreateThreadReq struct {
	GroupNo              string
	Name                 string
	CreatorUID           string
	CreatorName          string
	SourceMessageID      *int64
	SourceMessagePayload json.RawMessage // 源消息原始 payload，用于拷贝到子区
}

// ThreadResp 子区响应
type ThreadResp struct {
	ShortID               string `json:"short_id"`
	GroupNo               string `json:"group_no"`
	GroupName             string `json:"group_name"`
	ChannelID             string `json:"channel_id"`
	ChannelType           uint8  `json:"channel_type"`
	Name                  string `json:"name"`
	CreatorUID            string `json:"creator_uid"`
	SourceMessageID       *int64 `json:"source_message_id,omitempty"`
	Status                int    `json:"status"`
	MemberCount           int    `json:"member_count"`
	MessageCount          int64  `json:"message_count"`
	LastMessageContent    string `json:"last_message_content,omitempty"`
	LastMessageSenderName string `json:"last_message_sender_name,omitempty"`
	LastMessageAt         string `json:"last_message_at"`
	// GROUP.md 摘要信息
	HasThreadMd       bool   `json:"has_thread_md"`
	ThreadMdVersion   int64  `json:"thread_md_version"`
	ThreadMdUpdatedAt string `json:"thread_md_updated_at"`
	// Mute 当前用户的子区免打扰状态，仅 GetThread 填充。
	// nil = 用户未设置（前端应继承父群组 mute）；0 = 显式未静音；1 = 显式静音。
	Mute      *int   `json:"mute"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// MemberResp 子区成员响应
type MemberResp struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Role      int    `json:"role"` // 0=普通成员, 1=创建者
	CreatedAt string `json:"created_at"`
}

// threadVersionGen 返回一个绑定 s.ctx 的 GenSeq 回调，供 DB 层 CAS 重试时复用。
// 抽出来避免在每个调用点重复 closure 字面量。
func (s *Service) threadVersionGen() func() (int64, error) {
	return func() (int64, error) { return s.ctx.GenSeq(ThreadSeqKey) }
}

// CreateThread 创建子区
func (s *Service) CreateThread(req *CreateThreadReq) (*ThreadResp, error) {
	// 验证是群成员
	isMember, err := s.groupService.ExistMember(req.GroupNo, req.CreatorUID)
	if err != nil {
		return nil, fmt.Errorf("check group membership: %w", err)
	}
	if !isMember {
		return nil, errors.New("not a group member")
	}

	// 生成 shortID（snowflake ID）
	shortID := fmt.Sprintf("%d", s.ctx.UserIDGen.Generate().Int64())

	// 生成版本号
	version, err := s.ctx.GenSeq(ThreadSeqKey)
	if err != nil {
		return nil, fmt.Errorf("generate sequence: %w", err)
	}

	thread := &Model{
		ShortID:         shortID,
		GroupNo:         req.GroupNo,
		Name:            req.Name,
		CreatorUID:      req.CreatorUID,
		SourceMessageID: req.SourceMessageID,
		Status:          ThreadStatusActive,
		Version:         version,
	}

	// 使用事务插入 thread 和 member
	tx, err := s.db.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	threadID, err := s.db.InsertTxReturningID(thread, tx)
	if err != nil {
		return nil, fmt.Errorf("insert thread: %w", err)
	}
	thread.Id = threadID

	// 添加创建者为子区成员
	memberVersion, err := s.ctx.GenSeq(ThreadSeqKey)
	if err != nil {
		return nil, fmt.Errorf("generate member sequence: %w", err)
	}
	err = s.db.InsertMemberTx(&MemberModel{
		ThreadID: threadID,
		UID:      req.CreatorUID,
		Role:     MemberRoleCreator,
		Version:  memberVersion,
	}, tx)
	if err != nil {
		return nil, fmt.Errorf("insert creator as member: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}

	// 获取父群所有成员作为订阅者（所有群成员都有发消息权限）
	// 注意：thread_member 表记录主动加入的成员（决定通知推送），这里是 IM 发送权限
	members, err := s.groupService.GetMembers(req.GroupNo)
	if err != nil {
		return nil, fmt.Errorf("get group members: %w", err)
	}
	subscribers := make([]string, 0, len(members))
	for _, m := range members {
		subscribers = append(subscribers, m.UID)
	}

	// 创建 IM 频道
	channelID := BuildChannelID(req.GroupNo, shortID)
	err = s.ctx.IMCreateOrUpdateChannel(&config.ChannelCreateReq{
		ChannelID:   channelID,
		ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
		Subscribers: subscribers,
	})
	if err != nil {
		return nil, fmt.Errorf("create IM channel: %w", err)
	}

	// 给所有"已对父 channel 开启 auto_follow_threads=1"的用户 fanout 一行 thread ext。
	//
	// 顺序约束（round-2 lml2468 blocker → round-4 Jerry-Xin/lml2468 重申）：
	// fanout 必须发生在**任何客户端可观察的子区频道消息之前**。当前 thread 创建路径
	// 里两个客户端可观察事件是：
	//   1. sendSourceMessage —— 把源消息拷贝到新子区频道（可推送），
	//   2. sendThreadCreatedMessage —— 在父群发送"X 创建了子区 Y"通知。
	// 任意一个先于 fanout commit 都让客户端可能在 thread ext 行尚未存在时拉 sidebar，
	// 漏看新子区。所以 OnThreadCreated 放在 IM 频道建好之后 + 两条消息之前。
	//
	// 与 IM 频道 / 源消息一样在 commit 之后做 best-effort：失败只警告，不让 thread
	// 创建本身回滚（用户已经看到子区，下次 FollowChannel / refollow 会把缺失的
	// fanout 补齐）。GetGlobalConvExtService 在单测环境（未 init 单例）返回 nil 时跳过。
	if convSvc := conversation_ext.GetGlobalConvExtService(); convSvc != nil {
		if err := convSvc.OnThreadCreated(req.GroupNo, shortID); err != nil {
			s.Warn("OnThreadCreated fanout 失败（thread 已创建，sidebar 会延迟到下次 FollowChannel/refollow 补齐）",
				zap.String("groupNo", req.GroupNo),
				zap.String("shortID", shortID),
				zap.Error(err))
		}
	}

	// 拷贝源消息到子区作为首条消息（顺序：在 fanout 之后，客户端收到这条消息时
	// thread ext 行已 commit）。
	if req.SourceMessageID != nil && len(req.SourceMessagePayload) > 0 {
		// 从消息表查询原始发送者，防止客户端伪造
		sourceFromUID, err := s.db.QueryMessageFromUID(req.GroupNo, *req.SourceMessageID)
		if err != nil {
			s.Warn("查询源消息发送者失败，使用创建者作为发送者",
				zap.Error(err),
				zap.Int64("messageID", *req.SourceMessageID))
			sourceFromUID = req.CreatorUID
		} else if sourceFromUID == "" {
			sourceFromUID = req.CreatorUID
		}
		s.sendSourceMessage(channelID, sourceFromUID, req.SourceMessagePayload)
	}

	// 在父群发送子区创建消息（同样在 fanout 之后；与 sendSourceMessage 同序约束）。
	s.sendThreadCreatedMessage(req.GroupNo, shortID, req.Name, req.CreatorUID, req.CreatorName, req.SourceMessageID, req.SourceMessagePayload)

	resp := s.toThreadRespWithID(thread)
	resp.MemberCount = 1 // 创建者
	return resp, nil
}

// buildThreadCreatedPayload 构建子区创建通知消息的 payload
func buildThreadCreatedPayload(shortID, name, channelID, creatorUID, creatorName string, sourceMessageID *int64, sourcePayload json.RawMessage) map[string]interface{} {
	participants := []map[string]string{
		{"uid": creatorUID, "name": creatorName},
	}

	payload := map[string]interface{}{
		"type":         ContentTypeThreadCreated,
		"content":      fmt.Sprintf("%s 创建了子区「%s」", creatorName, name),
		"from_uid":     creatorUID,
		"from_name":    creatorName,
		"short_id":     shortID,
		"channel_id":   channelID,
		"channel_type": common.ChannelTypeCommunityTopic.Uint8(),
		"thread_name":  name,
		"participants": participants,
	}

	if sourceMessageID != nil {
		payload["source_message_id"] = *sourceMessageID
	}

	var messageCount int64
	if len(sourcePayload) > 0 {
		messageCount = 1
		payload["last_message"] = map[string]interface{}{
			"from_uid":  creatorUID,
			"from_name": creatorName,
			"content":   parsePayloadContent(sourcePayload),
			"timestamp": time.Now().Unix(),
		}
	}
	payload["message_count"] = messageCount

	return payload
}

// sendThreadCreatedMessage 发送子区创建消息到父群
func (s *Service) sendThreadCreatedMessage(groupNo, shortID, name, creatorUID, creatorName string, sourceMessageID *int64, sourcePayload json.RawMessage) {
	channelID := BuildChannelID(groupNo, shortID)
	payload := buildThreadCreatedPayload(shortID, name, channelID, creatorUID, creatorName, sourceMessageID, sourcePayload)

	err := s.ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			NoPersist: 0,
			RedDot:    1,
			SyncOnce:  0,
		},
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload:     []byte(util.ToJson(payload)),
	})
	if err != nil {
		s.Error("发送子区创建消息失败", zap.Error(err), zap.String("groupNo", groupNo))
	}
}

// sendSourceMessage 将源消息拷贝到子区频道作为首条消息
// fromUID 应该是经过服务端验证的原始消息发送者
func (s *Service) sendSourceMessage(channelID, fromUID string, payload json.RawMessage) {
	err := s.ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			NoPersist: 0,
			RedDot:    1,
			SyncOnce:  0,
		},
		FromUID:     fromUID,
		ChannelID:   channelID,
		ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
		Payload:     payload,
	})
	if err != nil {
		s.Error("拷贝源消息到子区失败", zap.Error(err), zap.String("channelID", channelID))
	}
}

// UpdateName 修改子区名称
func (s *Service) UpdateName(groupNo, shortID, operatorUID, name string) error {
	if name == "" || len([]rune(name)) > 100 {
		return errors.New("name is required and must not exceed 100 characters")
	}

	thread, err := s.db.QueryByGroupNoAndShortID(groupNo, shortID)
	if err != nil {
		return fmt.Errorf("query thread: %w", err)
	}
	if thread == nil {
		return errors.New("thread not found")
	}
	if thread.Status == ThreadStatusDeleted {
		return errors.New("thread has been deleted")
	}

	if thread.CreatorUID != operatorUID {
		isManager, err := s.groupService.IsCreatorOrManager(groupNo, operatorUID)
		if err != nil {
			return fmt.Errorf("check permission: %w", err)
		}
		if !isManager {
			return errors.New("no permission to update")
		}
	}

	if err := s.db.UpdateName(shortID, name, s.threadVersionGen()); err != nil {
		switch {
		case errors.Is(err, ErrThreadNotFound):
			return errors.New("thread not found")
		case errors.Is(err, ErrThreadDeleted):
			return errors.New("thread has been deleted")
		}
		return fmt.Errorf("update thread name: %w", err)
	}
	return nil
}

// GetThreads 分页获取群下的子区，同时返回总数。
func (s *Service) GetThreads(groupNo string, statuses []int, pageIndex, pageSize int64) ([]*ThreadResp, int64, error) {
	if pageIndex < 1 {
		pageIndex = 1
	}
	if pageSize <= 0 {
		pageSize = DefaultThreadPageSize
	}
	if pageSize > MaxThreadPageSize {
		pageSize = MaxThreadPageSize
	}
	// Defense-in-depth：API 层（parseListThreadStatuses）已经做了 allowlist，但 service
	// 也可能被其它模块（bot_api / botfather / 未来的内部调用）直接调用。这里再做一次
	// allowlist 归一，避免内部误传 ThreadStatusDeleted 把已删除子区透出。
	statuses = sanitizeListStatuses(statuses)

	total, err := s.db.CountByGroupNoWithStatus(groupNo, statuses)
	if err != nil {
		return nil, 0, fmt.Errorf("count threads by group: %w", err)
	}
	if total == 0 {
		return []*ThreadResp{}, 0, nil
	}

	offset := (pageIndex - 1) * pageSize
	threads, err := s.db.QueryByGroupNoWithStatus(groupNo, statuses, offset, pageSize)
	if err != nil {
		return nil, 0, fmt.Errorf("query threads by group: %w", err)
	}

	if len(threads) == 0 {
		return []*ThreadResp{}, total, nil
	}

	// 批量查询成员数量
	threadIDs := make([]int64, 0, len(threads))
	for _, t := range threads {
		if t.Id > 0 {
			threadIDs = append(threadIDs, t.Id)
		}
	}
	memberCounts, err := s.db.CountMembersBatch(threadIDs)
	if err != nil {
		s.Warn("批量查询成员数量失败", zap.Error(err))
		memberCounts = make(map[int64]int)
	}

	// 查询群名称
	var groupName string
	if groupInfo, err := s.groupService.GetGroupWithGroupNo(groupNo); err == nil && groupInfo != nil {
		groupName = groupInfo.Name
	}

	// 批量查询最新消息发送者名称
	senderUIDs := make([]string, 0)
	for _, t := range threads {
		if t.LastMessageSenderUID != "" {
			senderUIDs = append(senderUIDs, t.LastMessageSenderUID)
		}
	}
	senderNames := s.batchGetUserNames(senderUIDs)

	results := make([]*ThreadResp, 0, len(threads))
	for _, t := range threads {
		resp := &ThreadResp{
			ShortID:               t.ShortID,
			GroupNo:               t.GroupNo,
			GroupName:             groupName,
			ChannelID:             BuildChannelID(t.GroupNo, t.ShortID),
			ChannelType:           common.ChannelTypeCommunityTopic.Uint8(),
			Name:                  t.Name,
			CreatorUID:            t.CreatorUID,
			SourceMessageID:       t.SourceMessageID,
			Status:                t.Status,
			MemberCount:           memberCounts[t.Id],
			MessageCount:          t.MessageCount,
			LastMessageContent:    t.LastMessageContent,
			LastMessageSenderName: senderNames[t.LastMessageSenderUID],
			LastMessageAt:         util.ToyyyyMMddHHmmss(time.Time(t.CreatedAt)), // 默认 created_at
			HasThreadMd:           t.ThreadMd != nil,
			ThreadMdVersion:       t.ThreadMdVersion,
			CreatedAt:             util.ToyyyyMMddHHmmss(time.Time(t.CreatedAt)),
			UpdatedAt:             util.ToyyyyMMddHHmmss(time.Time(t.UpdatedAt)),
		}
		if t.ThreadMdUpdatedAt != nil {
			resp.ThreadMdUpdatedAt = util.ToyyyyMMddHHmmss(*t.ThreadMdUpdatedAt)
		}
		if t.LastMessageAt != nil {
			resp.LastMessageAt = util.ToyyyyMMddHHmmss(*t.LastMessageAt)
		}
		results = append(results, resp)
	}
	return results, total, nil
}

// GetThread 获取子区详情
func (s *Service) GetThread(groupNo, shortID, loginUID string) (*ThreadResp, error) {
	thread, err := s.db.QueryByGroupNoAndShortID(groupNo, shortID)
	if err != nil {
		return nil, fmt.Errorf("query thread: %w", err)
	}
	if thread == nil {
		return nil, errors.New("thread not found")
	}
	if thread.Status == ThreadStatusDeleted {
		return nil, errors.New("thread has been deleted")
	}
	resp := s.toThreadResp(thread)
	if groupInfo, err := s.groupService.GetGroupWithGroupNo(groupNo); err == nil && groupInfo != nil {
		resp.GroupName = groupInfo.Name
	}
	if loginUID != "" {
		setting, err := s.db.QuerySetting(groupNo, shortID, loginUID)
		if err != nil {
			s.Warn("查询子区免打扰设置失败，mute 字段返回 nil（前端按未设置处理）",
				zap.Error(err),
				zap.String("groupNo", groupNo),
				zap.String("shortID", shortID),
				zap.String("loginUID", loginUID))
		} else if setting != nil {
			mute := setting.Mute
			resp.Mute = &mute
		}
	}
	return resp, nil
}

// ArchiveThread 归档子区
func (s *Service) ArchiveThread(groupNo, shortID, operatorUID string) error {
	thread, err := s.db.QueryByGroupNoAndShortID(groupNo, shortID)
	if err != nil {
		return fmt.Errorf("query thread: %w", err)
	}
	if thread == nil {
		return errors.New("thread not found")
	}
	if thread.Status == ThreadStatusDeleted {
		return errors.New("thread has been deleted")
	}
	if thread.Status == ThreadStatusArchived {
		return nil // 已归档，无需操作
	}

	// 检查权限：创建者或管理员可以归档
	canOperate, err := s.canOperate(groupNo, shortID, operatorUID)
	if err != nil {
		return fmt.Errorf("check permission: %w", err)
	}
	if !canOperate {
		return errors.New("no permission to archive")
	}

	if err := s.db.UpdateStatusFrom(shortID, ThreadStatusActive, ThreadStatusArchived, s.threadVersionGen()); err != nil {
		switch {
		case errors.Is(err, ErrThreadNotFound):
			return errors.New("thread not found")
		case errors.Is(err, ErrThreadDeleted):
			return errors.New("thread has been deleted")
		case errors.Is(err, ErrThreadStatusMismatch):
			return errors.New("thread status changed concurrently")
		}
		return fmt.Errorf("update thread status: %w", err)
	}
	return nil
}

// UnarchiveThread 取消归档
func (s *Service) UnarchiveThread(groupNo, shortID, operatorUID string) error {
	thread, err := s.db.QueryByGroupNoAndShortID(groupNo, shortID)
	if err != nil {
		return fmt.Errorf("query thread: %w", err)
	}
	if thread == nil {
		return errors.New("thread not found")
	}
	if thread.Status == ThreadStatusDeleted {
		return errors.New("thread has been deleted")
	}
	if thread.Status == ThreadStatusActive {
		return nil // 已激活，无需操作
	}

	// 检查权限
	canOperate, err := s.canOperate(groupNo, shortID, operatorUID)
	if err != nil {
		return fmt.Errorf("check permission: %w", err)
	}
	if !canOperate {
		return errors.New("no permission to unarchive")
	}

	if err := s.db.UpdateStatusFrom(shortID, ThreadStatusArchived, ThreadStatusActive, s.threadVersionGen()); err != nil {
		switch {
		case errors.Is(err, ErrThreadNotFound):
			return errors.New("thread not found")
		case errors.Is(err, ErrThreadDeleted):
			return errors.New("thread has been deleted")
		case errors.Is(err, ErrThreadStatusMismatch):
			return errors.New("thread status changed concurrently")
		}
		return fmt.Errorf("update thread status: %w", err)
	}
	return nil
}

// DeleteThread 删除子区
func (s *Service) DeleteThread(groupNo, shortID, operatorUID string) error {
	thread, err := s.db.QueryByGroupNoAndShortID(groupNo, shortID)
	if err != nil {
		return fmt.Errorf("query thread: %w", err)
	}
	if thread == nil {
		return errors.New("thread not found")
	}
	if thread.Status == ThreadStatusDeleted {
		return nil // 已删除，无需操作
	}

	canDelete, err := s.CanDelete(groupNo, shortID, operatorUID)
	if err != nil {
		return fmt.Errorf("check delete permission: %w", err)
	}
	if !canDelete {
		return errors.New("no permission to delete")
	}

	if err := s.db.MarkDeleted(shortID, s.threadVersionGen()); err != nil {
		if errors.Is(err, ErrThreadNotFound) {
			return errors.New("thread not found")
		}
		return fmt.Errorf("update thread status: %w", err)
	}

	channelID := BuildChannelID(groupNo, shortID)
	err = s.ctx.IMCreateOrUpdateChannelInfo(&config.ChannelInfoCreateReq{
		ChannelID:   channelID,
		ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
		Ban:         1,
	})
	if err != nil {
		s.Warn("通知 WuKongIM 禁用已删除子区频道失败", zap.String("channelID", channelID), zap.Error(err))
	}

	// 清理所有用户对该子区的置顶
	user.RemovePinnedForChannel(channelID, common.ChannelTypeCommunityTopic.Uint8())
	conversation_ext.RemoveConvExtForChannel(channelID, common.ChannelTypeCommunityTopic.Uint8())

	return nil
}

// CanDelete 检查是否可以删除
func (s *Service) CanDelete(groupNo, shortID, uid string) (bool, error) {
	return s.canOperate(groupNo, shortID, uid)
}

// ExistThread 检查子区是否存在
func (s *Service) ExistThread(groupNo, shortID string) (bool, error) {
	exist, err := s.db.ExistByGroupNoAndShortID(groupNo, shortID)
	if err != nil {
		return false, fmt.Errorf("check thread existence: %w", err)
	}
	return exist, nil
}

// canOperate 检查是否有操作权限（创建者或群管理员）
// 注：此方法存在 TOCTOU 竞态条件，但实际删除/归档操作会再次检查状态，
// 最坏情况仅是在极短时间窗口内返回已过期的权限判断，风险可接受。
func (s *Service) canOperate(groupNo, shortID, uid string) (bool, error) {
	thread, err := s.db.QueryByGroupNoAndShortID(groupNo, shortID)
	if err != nil {
		return false, fmt.Errorf("query thread: %w", err)
	}
	if thread == nil {
		return false, nil
	}

	// 创建者可以操作
	if thread.CreatorUID == uid {
		return true, nil
	}

	// 群管理员可以操作
	isManager, err := s.groupService.IsCreatorOrManager(groupNo, uid)
	if err != nil {
		return false, fmt.Errorf("check manager permission: %w", err)
	}
	return isManager, nil
}

// GetThreadMd 获取子区 GROUP.md
func (s *Service) GetThreadMd(groupNo, shortID string) (*ThreadMdResult, error) {
	return s.db.QueryThreadMd(groupNo, shortID)
}

// UpdateThreadMd 更新子区 GROUP.md
// 纯数据操作透传，权限检查由 API Handler 层完成
func (s *Service) UpdateThreadMd(groupNo, shortID, content, updatedBy string) (int64, error) {
	return s.db.UpdateThreadMd(groupNo, shortID, content, updatedBy)
}

// DeleteThreadMd 删除子区 GROUP.md
// 纯数据操作透传，权限检查由 API Handler 层完成
func (s *Service) DeleteThreadMd(groupNo, shortID, deletedBy string) (int64, error) {
	return s.db.DeleteThreadMd(groupNo, shortID, deletedBy)
}

// CanEditThreadMd 检查是否有编辑子区 GROUP.md 的权限
// 权限规则：子区创建者 或 群创建者/管理员
// 供 API Handler 层在调用 UpdateThreadMd/DeleteThreadMd 前使用
func (s *Service) CanEditThreadMd(groupNo, shortID, uid string) (bool, error) {
	return s.canOperate(groupNo, shortID, uid)
}

// toThreadResp 转换为响应（需要额外查询 ID）
func (s *Service) toThreadResp(m *Model) *ThreadResp {
	// 如果 Model 没有 ID，需要查询
	if m.Id == 0 {
		m.Id, _ = s.db.QueryThreadIDByShortID(m.ShortID)
	}
	return s.toThreadRespWithID(m)
}

// toThreadRespWithID 转换为响应（Model 已有 ID）
func (s *Service) toThreadRespWithID(m *Model) *ThreadResp {
	memberCount := 0
	if m.Id > 0 {
		memberCount, _ = s.db.CountMembers(m.Id)
	}

	resp := &ThreadResp{
		ShortID:            m.ShortID,
		GroupNo:            m.GroupNo,
		ChannelID:          BuildChannelID(m.GroupNo, m.ShortID),
		ChannelType:        common.ChannelTypeCommunityTopic.Uint8(),
		Name:               m.Name,
		CreatorUID:         m.CreatorUID,
		SourceMessageID:    m.SourceMessageID,
		Status:             m.Status,
		MemberCount:        memberCount,
		MessageCount:       m.MessageCount,
		LastMessageContent: m.LastMessageContent,
		LastMessageAt:      util.ToyyyyMMddHHmmss(time.Time(m.CreatedAt)), // 默认 created_at
		HasThreadMd:        m.ThreadMd != nil,
		ThreadMdVersion:    m.ThreadMdVersion,
		CreatedAt:          util.ToyyyyMMddHHmmss(time.Time(m.CreatedAt)),
		UpdatedAt:          util.ToyyyyMMddHHmmss(time.Time(m.UpdatedAt)),
	}
	if m.ThreadMdUpdatedAt != nil {
		resp.ThreadMdUpdatedAt = util.ToyyyyMMddHHmmss(*m.ThreadMdUpdatedAt)
	}
	if m.LastMessageSenderUID != "" {
		resp.LastMessageSenderName = s.getUserName(m.LastMessageSenderUID)
	}
	if m.LastMessageAt != nil {
		resp.LastMessageAt = util.ToyyyyMMddHHmmss(*m.LastMessageAt)
	}
	return resp
}

// getUserName 根据 UID 获取用户名
func (s *Service) getUserName(uid string) string {
	users, err := s.userService.GetUsers([]string{uid})
	if err != nil || len(users) == 0 {
		return ""
	}
	return users[0].Name
}

// batchGetUserNames 批量获取用户名
func (s *Service) batchGetUserNames(uids []string) map[string]string {
	result := make(map[string]string, len(uids))
	if len(uids) == 0 {
		return result
	}
	// 去重
	unique := make(map[string]struct{}, len(uids))
	deduped := make([]string, 0, len(uids))
	for _, uid := range uids {
		if _, ok := unique[uid]; !ok {
			unique[uid] = struct{}{}
			deduped = append(deduped, uid)
		}
	}
	users, err := s.userService.GetUsers(deduped)
	if err != nil {
		s.Warn("批量查询用户名失败", zap.Error(err))
		return result
	}
	for _, u := range users {
		result[u.UID] = u.Name
	}
	return result
}

// BuildChannelID 构建 channelID
func BuildChannelID(groupNo, shortID string) string {
	return fmt.Sprintf("%s%s%s", groupNo, ChannelIDSeparator, shortID)
}

// ParseChannelID 解析 channelID
func ParseChannelID(channelID string) (groupNo, shortID string, err error) {
	parts := strings.Split(channelID, ChannelIDSeparator)
	if len(parts) != 2 {
		return "", "", errors.New("invalid thread channel ID format")
	}
	if parts[0] == "" || parts[1] == "" {
		return "", "", errors.New("invalid thread channel ID format")
	}
	return parts[0], parts[1], nil
}

// IsValidShortID 验证 shortID 格式（snowflake ID: 纯数字，15-20位）
func IsValidShortID(shortID string) bool {
	if len(shortID) < 15 || len(shortID) > 20 {
		return false
	}
	for _, c := range shortID {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// IsValidGroupNo 验证 groupNo 格式（32位十六进制）
func IsValidGroupNo(groupNo string) bool {
	if len(groupNo) != 32 {
		return false
	}
	for _, c := range groupNo {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// JoinThread 加入子区
func (s *Service) JoinThread(groupNo, shortID, uid string) error {
	// 验证是父群成员
	isMember, err := s.groupService.ExistMember(groupNo, uid)
	if err != nil {
		return fmt.Errorf("check group membership: %w", err)
	}
	if !isMember {
		return errors.New("not a group member")
	}

	// 获取子区
	thread, err := s.db.QueryByGroupNoAndShortID(groupNo, shortID)
	if err != nil {
		return fmt.Errorf("query thread: %w", err)
	}
	if thread == nil {
		return errors.New("thread not found")
	}
	if thread.Status != ThreadStatusActive {
		return errors.New("thread is not active")
	}

	threadID, err := s.db.QueryThreadIDByShortID(shortID)
	if err != nil {
		return fmt.Errorf("query thread id: %w", err)
	}

	// 检查是否已经是成员
	exist, err := s.db.ExistMember(threadID, uid)
	if err != nil {
		return fmt.Errorf("check member: %w", err)
	}
	if exist {
		return nil // 已经是成员
	}

	// 添加成员
	version, err := s.ctx.GenSeq(ThreadSeqKey)
	if err != nil {
		return fmt.Errorf("generate sequence: %w", err)
	}
	err = s.db.InsertMember(&MemberModel{
		ThreadID: threadID,
		UID:      uid,
		Role:     MemberRoleNormal,
		Version:  version,
	})
	if err != nil {
		return fmt.Errorf("insert member: %w", err)
	}

	return nil
}

// LeaveThread 离开子区
func (s *Service) LeaveThread(groupNo, shortID, uid string) error {
	thread, err := s.db.QueryByGroupNoAndShortID(groupNo, shortID)
	if err != nil {
		return fmt.Errorf("query thread: %w", err)
	}
	if thread == nil {
		return errors.New("thread not found")
	}

	// 创建者不能离开
	if thread.CreatorUID == uid {
		return errors.New("creator cannot leave thread")
	}

	threadID, err := s.db.QueryThreadIDByShortID(shortID)
	if err != nil {
		return fmt.Errorf("query thread id: %w", err)
	}

	// 删除成员
	err = s.db.DeleteMember(threadID, uid)
	if err != nil {
		return fmt.Errorf("delete member: %w", err)
	}

	return nil
}

// GetMembers 获取子区成员
func (s *Service) GetMembers(groupNo, shortID string) ([]*MemberResp, error) {
	threadID, err := s.db.QueryThreadIDByShortID(shortID)
	if err != nil {
		return nil, fmt.Errorf("query thread id: %w", err)
	}

	members, err := s.db.QueryMembers(threadID)
	if err != nil {
		return nil, fmt.Errorf("query members: %w", err)
	}

	if len(members) == 0 {
		return []*MemberResp{}, nil
	}

	// 批量查询用户名
	uids := make([]string, 0, len(members))
	for _, m := range members {
		uids = append(uids, m.UID)
	}
	users, _ := s.userService.GetUsers(uids)
	userNameMap := make(map[string]string, len(users))
	for _, u := range users {
		userNameMap[u.UID] = u.Name
	}

	results := make([]*MemberResp, 0, len(members))
	for _, m := range members {
		results = append(results, &MemberResp{
			UID:       m.UID,
			Name:      userNameMap[m.UID],
			Role:      m.Role,
			CreatedAt: util.ToyyyyMMddHHmmss(time.Time(m.CreatedAt)),
		})
	}
	return results, nil
}

// GetMemberUIDs 获取子区成员 UID 列表
func (s *Service) GetMemberUIDs(groupNo, shortID string) ([]string, error) {
	threadID, err := s.db.QueryThreadIDByShortID(shortID)
	if err != nil {
		return nil, fmt.Errorf("query thread id: %w", err)
	}
	return s.db.QueryMemberUIDs(threadID)
}

// IsMember 检查是否是子区成员
func (s *Service) IsMember(groupNo, shortID, uid string) (bool, error) {
	threadID, err := s.db.QueryThreadIDByShortID(shortID)
	if err != nil {
		return false, fmt.Errorf("query thread id: %w", err)
	}
	return s.db.ExistMember(threadID, uid)
}

// UpdateSetting 更新用户对某子区的个人设置(目前支持 mute)
// 权限: 必须是父群成员; 无需是子区成员(与群聊 setting 行为保持一致)
func (s *Service) UpdateSetting(groupNo, shortID, uid string, settings map[string]interface{}) error {
	isGroupMember, err := s.groupService.ExistMember(groupNo, uid)
	if err != nil {
		return fmt.Errorf("check group membership: %w", err)
	}
	if !isGroupMember {
		return errors.New("not a group member")
	}

	thread, err := s.db.QueryByGroupNoAndShortID(groupNo, shortID)
	if err != nil {
		return fmt.Errorf("query thread: %w", err)
	}
	if thread == nil {
		return errors.New("thread not found")
	}
	if thread.Status == ThreadStatusDeleted {
		return errors.New("thread has been deleted")
	}

	existing, err := s.db.QuerySetting(groupNo, shortID, uid)
	if err != nil {
		return fmt.Errorf("query thread setting: %w", err)
	}

	target := &SettingModel{
		GroupNo: groupNo,
		ShortID: shortID,
		UID:     uid,
	}
	if existing != nil {
		target.Mute = existing.Mute
	}

	for key, raw := range settings {
		switch key {
		case "mute":
			val, ok := raw.(float64)
			if !ok {
				return fmt.Errorf("invalid mute value type")
			}
			intVal := int(val)
			if intVal != 0 && intVal != 1 {
				return fmt.Errorf("mute must be 0 or 1")
			}
			target.Mute = intVal
		default:
			// 未知字段忽略,保持向前兼容
		}
	}

	// 校验通过后再申请版本号,避免校验失败时浪费全局序列
	version, err := s.ctx.GenSeq(ThreadSeqKey)
	if err != nil {
		return fmt.Errorf("generate sequence: %w", err)
	}
	target.Version = version

	if err := s.db.UpsertSetting(target); err != nil {
		return fmt.Errorf("upsert thread setting: %w", err)
	}

	channelID := BuildChannelID(groupNo, shortID)
	if err := s.ctx.SendChannelUpdate(
		config.ChannelReq{ChannelID: uid, ChannelType: common.ChannelTypePerson.Uint8()},
		config.ChannelReq{ChannelID: channelID, ChannelType: common.ChannelTypeCommunityTopic.Uint8()},
	); err != nil {
		s.Warn("下发子区频道更新失败", zap.Error(err), zap.String("channelID", channelID), zap.String("uid", uid))
	}

	return nil
}

// GetSettingsWithUIDs 批量查询一批用户对某子区的设置,无记录不返回
func (s *Service) GetSettingsWithUIDs(groupNo, shortID string, uids []string) ([]*SettingResp, error) {
	if len(uids) == 0 {
		return []*SettingResp{}, nil
	}
	models, err := s.db.QuerySettingsWithUIDs(groupNo, shortID, uids)
	if err != nil {
		return nil, fmt.Errorf("query thread settings: %w", err)
	}
	resp := make([]*SettingResp, 0, len(models))
	for _, m := range models {
		resp = append(resp, &SettingResp{UID: m.UID, Mute: m.Mute})
	}
	return resp, nil
}

// RemoveUserFromGroupThreads 退群时清理用户在该群所有子区的成员身份、个人设置、IM 订阅
// 注:即使用户未加入任何子区,也要清理 thread_setting(用户可能只设置了 mute 但未 join)
func (s *Service) RemoveUserFromGroupThreads(groupNo, uid string) error {
	// 查询用户在该群加入的所有子区(用于 IM 退订)
	threads, err := s.db.QueryThreadsByGroupNoAndUID(groupNo, uid)
	if err != nil {
		return fmt.Errorf("query user threads in group: %w", err)
	}

	// 批量清理 thread_member + thread_setting(同一事务)
	tx, err := s.db.session.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	err = s.db.DeleteMembersByGroupNoAndUIDTx(groupNo, uid, tx)
	if err != nil {
		return fmt.Errorf("delete thread members: %w", err)
	}

	err = s.db.DeleteSettingsByGroupNoAndUIDTx(groupNo, uid, tx)
	if err != nil {
		return fmt.Errorf("delete thread settings: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	// 移除 IM 订阅（事务外，失败仅记日志）
	for _, t := range threads {
		channelID := BuildChannelID(groupNo, t.ShortID)
		rmErr := s.ctx.IMRemoveSubscriber(&config.SubscriberRemoveReq{
			ChannelID:   channelID,
			ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
			Subscribers: []string{uid},
		})
		if rmErr != nil {
			s.Error("移除子区IM订阅者失败", zap.Error(rmErr), zap.String("groupNo", groupNo), zap.String("shortID", t.ShortID), zap.String("uid", uid))
		}
	}

	return nil
}

// sanitizeListStatuses 把入参里只保留 listThreads 允许列出的 status（active / archived），
// 去重；若过滤后为空（nil、空切片、或全是非法值如 deleted/未知码），fallback 到
// [active]。即便 service 被旁路调用，也不会把 deleted 子区透出。
func sanitizeListStatuses(statuses []int) []int {
	seen := make(map[int]struct{}, 2)
	out := make([]int, 0, 2)
	for _, s := range statuses {
		if s != ThreadStatusActive && s != ThreadStatusArchived {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return []int{ThreadStatusActive}
	}
	return out
}
