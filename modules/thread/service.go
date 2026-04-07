package thread

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"go.uber.org/zap"
)

// IService 子区服务接口
type IService interface {
	// CreateThread 创建子区
	CreateThread(req *CreateThreadReq) (*ThreadResp, error)
	// GetThreads 获取群下的所有子区
	GetThreads(groupNo string) ([]*ThreadResp, error)
	// GetThread 获取子区详情
	GetThread(groupNo, shortID string) (*ThreadResp, error)
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
	GroupNo         string
	Name            string
	CreatorUID      string
	CreatorName     string
	SourceMessageID *int64
}

// ThreadResp 子区响应
type ThreadResp struct {
	ShortID         string `json:"short_id"`
	GroupNo         string `json:"group_no"`
	ChannelID       string `json:"channel_id"`
	ChannelType     uint8  `json:"channel_type"`
	Name            string `json:"name"`
	CreatorUID      string `json:"creator_uid"`
	SourceMessageID *int64 `json:"source_message_id,omitempty"`
	Status          int    `json:"status"`
	MemberCount     int    `json:"member_count"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

// MemberResp 子区成员响应
type MemberResp struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Role      int    `json:"role"` // 0=普通成员, 1=创建者
	CreatedAt string `json:"created_at"`
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

	// 创建 IM 频道，只添加创建者为订阅者（只有主动加入的成员才收到消息通知）
	// IMDatasource.Subscribers 返回父群所有成员用于发送权限校验
	channelID := BuildChannelID(req.GroupNo, shortID)
	err = s.ctx.IMCreateOrUpdateChannel(&config.ChannelCreateReq{
		ChannelID:   channelID,
		ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
		Subscribers: []string{req.CreatorUID},
	})
	if err != nil {
		return nil, fmt.Errorf("create IM channel: %w", err)
	}

	// 在父群发送子区创建消息
	s.sendThreadCreatedMessage(req.GroupNo, shortID, req.Name, req.CreatorUID, req.CreatorName)

	resp := s.toThreadRespWithID(thread)
	resp.MemberCount = 1 // 创建者
	return resp, nil
}

// sendThreadCreatedMessage 发送子区创建消息到父群
func (s *Service) sendThreadCreatedMessage(groupNo, shortID, name, creatorUID, creatorName string) {
	channelID := BuildChannelID(groupNo, shortID)

	// 发送可见的通知消息到父群
	err := s.ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			NoPersist: 0,
			RedDot:    1,
			SyncOnce:  0,
		},
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload: []byte(util.ToJson(map[string]interface{}{
			"type":         ContentTypeThreadCreated,
			"content":      fmt.Sprintf("%s 创建了子区「%s」", creatorName, name),
			"from_uid":     creatorUID,
			"from_name":    creatorName,
			"short_id":     shortID,
			"channel_id":   channelID,
			"channel_type": common.ChannelTypeCommunityTopic.Uint8(),
			"thread_name":  name,
		})),
	})
	if err != nil {
		s.Error("发送子区创建消息失败", zap.Error(err), zap.String("groupNo", groupNo))
	}
}

// GetThreads 获取群下的所有子区
func (s *Service) GetThreads(groupNo string) ([]*ThreadResp, error) {
	threads, err := s.db.QueryByGroupNo(groupNo)
	if err != nil {
		return nil, fmt.Errorf("query threads by group: %w", err)
	}

	if len(threads) == 0 {
		return []*ThreadResp{}, nil
	}

	// 批量查询成员数量
	threadIDs := make([]int64, 0, len(threads))
	for _, t := range threads {
		if t.Id > 0 {
			threadIDs = append(threadIDs, t.Id)
		}
	}
	memberCounts, _ := s.db.CountMembersBatch(threadIDs)

	results := make([]*ThreadResp, 0, len(threads))
	for _, t := range threads {
		resp := &ThreadResp{
			ShortID:         t.ShortID,
			GroupNo:         t.GroupNo,
			ChannelID:       BuildChannelID(t.GroupNo, t.ShortID),
			ChannelType:     common.ChannelTypeCommunityTopic.Uint8(),
			Name:            t.Name,
			CreatorUID:      t.CreatorUID,
			SourceMessageID: t.SourceMessageID,
			Status:          t.Status,
			MemberCount:     memberCounts[t.Id],
			CreatedAt:       util.ToyyyyMMddHHmmss(time.Time(t.CreatedAt)),
			UpdatedAt:       util.ToyyyyMMddHHmmss(time.Time(t.UpdatedAt)),
		}
		results = append(results, resp)
	}
	return results, nil
}

// GetThread 获取子区详情
func (s *Service) GetThread(groupNo, shortID string) (*ThreadResp, error) {
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
	return s.toThreadResp(thread), nil
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

	version, err := s.ctx.GenSeq(ThreadSeqKey)
	if err != nil {
		return fmt.Errorf("generate sequence: %w", err)
	}
	if err := s.db.UpdateStatus(shortID, ThreadStatusArchived, version); err != nil {
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

	version, err := s.ctx.GenSeq(ThreadSeqKey)
	if err != nil {
		return fmt.Errorf("generate sequence: %w", err)
	}
	if err := s.db.UpdateStatus(shortID, ThreadStatusActive, version); err != nil {
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

	version, err := s.ctx.GenSeq(ThreadSeqKey)
	if err != nil {
		return fmt.Errorf("generate sequence: %w", err)
	}
	if err := s.db.UpdateStatus(shortID, ThreadStatusDeleted, version); err != nil {
		return fmt.Errorf("update thread status: %w", err)
	}
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

	return &ThreadResp{
		ShortID:         m.ShortID,
		GroupNo:         m.GroupNo,
		ChannelID:       BuildChannelID(m.GroupNo, m.ShortID),
		ChannelType:     common.ChannelTypeCommunityTopic.Uint8(),
		Name:            m.Name,
		CreatorUID:      m.CreatorUID,
		SourceMessageID: m.SourceMessageID,
		Status:          m.Status,
		MemberCount:     memberCount,
		CreatedAt:       util.ToyyyyMMddHHmmss(time.Time(m.CreatedAt)),
		UpdatedAt:       util.ToyyyyMMddHHmmss(time.Time(m.UpdatedAt)),
	}
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

	// 同步订阅者到 IM
	channelID := BuildChannelID(groupNo, shortID)
	err = s.ctx.IMAddSubscriber(&config.SubscriberAddReq{
		ChannelID:   channelID,
		ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
		Subscribers: []string{uid},
	})
	if err != nil {
		s.Error("添加IM订阅者失败", zap.Error(err), zap.String("uid", uid))
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

	// 同步移除 IM 订阅者
	channelID := BuildChannelID(groupNo, shortID)
	err = s.ctx.IMRemoveSubscriber(&config.SubscriberRemoveReq{
		ChannelID:   channelID,
		ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
		Subscribers: []string{uid},
	})
	if err != nil {
		s.Error("移除IM订阅者失败", zap.Error(err), zap.String("uid", uid))
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
