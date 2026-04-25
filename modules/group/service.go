package group

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkevent"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// IService 群相关
type IService interface {
	// 获取群总数
	GetAllGroupCount() (int64, error)
	// 查询某天的新建群数量
	GetCreatedCountWithDate(date string) (int64, error)
	// 添加一个群
	AddGroup(model *AddGroupReq) error
	// 某个时间段的建群数据
	GetGroupWithDateSpace(startDate, endDate string) (map[string]int64, error)
	// 查询某个群信息
	GetGroupWithGroupNo(groupNo string) (*InfoResp, error)
	// GetGroups 获取群集合
	GetGroups(groupNos []string) ([]*InfoResp, error)
	// 获取某一批群与指定用户的详情（包括用户对群的设置等等）
	GetGroupDetails(groupNos []string, uid string) ([]*GroupResp, error)
	// 获取群详情
	GetGroupDetail(groupNo string, uid string) (*GroupResp, error)

	// -------------------- 群设置 --------------------
	// GetSettings 获取群的设置
	GetSettings(groupNos []string, uid string) ([]*SettingResp, error)
	// GetSettingsWithUids 获取一批用户对某个群的设置
	GetSettingsWithUIDs(groupNo string, uids []string) ([]*SettingResp, error)

	// -------------------- 群成员 --------------------
	// 获取指定群的群成员列表
	GetMembers(groupNo string) ([]*MemberResp, error)
	// 获取指定群的指定成员信息
	GetMember(groupNo, uid string) (*MemberResp, error)
	// 获取黑名单成员uid集合
	GetBlacklistMemberUIDs(groupNo string) ([]string, error)
	// 查询管理员成员uid列表（包括创建者）
	GetMemberUIDsOfManager(groupNo string) ([]string, error)
	// 是否是创建者或管理者
	IsCreatorOrManager(groupNo string, uid string) (bool, error)
	// 获取成员总数量和在线数量
	// 第一个返回参数为成员总数量
	// 第二个返回参数为在线数量
	GetMemberTotalAndOnlineCount(groupNo string) (int, int, error)
	// 是否存在群成员
	ExistMember(groupNo string, uid string) (bool, error)
	// 成员是否在某群里存在 返回对应在群里的群编号
	ExistMembers(groupNos []string, uid string) ([]string, error)
	// GetGroupsWithMemberUID 获取某个用户的所有群
	GetGroupsWithMemberUID(uid string) ([]*InfoResp, error)
	// 获取指定群的群成员的最大数据版本
	GetGroupMemberMaxVersion(groupNo string) (int64, error)
	// 获取用户所有超级群信息
	GetUserSupers(uid string) ([]*InfoResp, error)
	// 新增群成员
	AddMember(model *AddMemberReq) error
	// 获取指定一批群的指定成员信息
	GetMembersWithUIDAndGroupIds(uid string, groupNos []string) ([]*MemberResp, error)
	// 查询一批群的管理员及群主
	GetManagersWithGroupNos(groupNos []string) ([]*MemberResp, error)
	// GetGroupMd returns GROUP.md content for a group
	GetGroupMd(groupNo string) (*GroupMdResult, error)
	// UpdateGroupMd updates GROUP.md content
	UpdateGroupMd(groupNo string, content string, updatedBy string) (int64, error)
	// DeleteGroupMd deletes GROUP.md content
	DeleteGroupMd(groupNo string) (int64, error)
	// IsBotAdmin checks if a member is a bot admin
	IsBotAdmin(groupNo string, uid string) (bool, error)
	// GetBotMemberUIDs returns UIDs of robot members in the group
	GetBotMemberUIDs(groupNo string) ([]string, error)

	// CreateGroup 创建群（统一入口，Web 和 Bot 共用）
	CreateGroup(req *CreateGroupServiceReq) (*CreateGroupServiceResp, error)
	// AddGroupMembers 添加群成员
	AddGroupMembers(req *AddGroupMembersServiceReq) (*AddGroupMembersServiceResp, error)
	// RemoveGroupMembers 移除群成员
	RemoveGroupMembers(req *RemoveGroupMembersServiceReq) (*RemoveGroupMembersServiceResp, error)
	// UpdateGroupInfo 更新群信息
	UpdateGroupInfo(req *UpdateGroupInfoServiceReq) error
}

// Service Service
type Service struct {
	ctx       *config.Context
	db        *DB
	managerDB *managerDB
	log.Log
	settingDB *settingDB
	userDB    *user.DB
}

// NewService NewService
func NewService(ctx *config.Context) IService {
	return &Service{
		ctx:       ctx,
		db:        NewDB(ctx),
		managerDB: newManagerDB(ctx.DB()),
		Log:       log.NewTLog("groupService"),
		settingDB: newSettingDB(ctx),
		userDB:    user.NewDB(ctx),
	}
}

// GetManagersWithGroupNos 查询一批群的管理员及群主
func (s *Service) GetManagersWithGroupNos(groupNos []string) ([]*MemberResp, error) {
	models, err := s.db.queryManagersWithGroupNos(groupNos)
	if err != nil {
		return nil, err
	}
	list := make([]*MemberResp, 0, len(models))
	if len(models) > 0 {
		for _, model := range models {
			list = append(list, &MemberResp{
				UID:     model.UID,
				Name:    model.Name,
				Role:    model.Role,
				GroupNo: model.GroupNo,
				Remark:  model.Remark,
			})
		}
	}
	return list, nil
}

// GetAllGroupCount 获取群总数
func (s *Service) GetAllGroupCount() (int64, error) {
	return s.db.queryGroupCount()
}

// GetCreatedCountWithDate 获取某天的新建群数量
func (s *Service) GetCreatedCountWithDate(date string) (int64, error) {
	if date == "" {
		return 0, errors.New("时间不能为空")
	}
	return s.db.queryCreatedCountWithDate(date)
}

// AddGroup 添加一个群
func (s *Service) AddGroup(model *AddGroupReq) error {
	err := s.db.Insert(&Model{
		GroupNo: model.GroupNo,
		Name:    model.Name,
	})
	return err
}

func (s *Service) GetGroupsWithMemberUID(uid string) ([]*InfoResp, error) {
	groups, err := s.db.queryGroupsWithMemberUID(uid)
	if err != nil {
		return nil, err
	}
	infos := make([]*InfoResp, 0, len(groups))
	if len(groups) > 0 {
		for _, gp := range groups {
			infos = append(infos, toInfoResp(gp))
		}
	}
	return infos, nil
}

// GetGroupWithDateSpace 某个时间段的建群数据
func (s *Service) GetGroupWithDateSpace(startDate, endDate string) (map[string]int64, error) {
	if startDate == "" || endDate == "" {
		return nil, errors.New("时间不能为空")
	}
	list, err := s.managerDB.queryRegisterCountWithDateSpace(startDate, endDate)
	if err != nil {
		s.Error("查询群列表错误", zap.Error(err))
		return nil, err
	}
	result := make(map[string]int64)
	if len(list) > 0 {
		for _, model := range list {
			key := util.Toyyyy_MM_dd(time.Time(model.CreatedAt))
			if _, ok := result[key]; ok {
				//存在某个
				result[key]++
			} else {
				result[key] = 1
			}
		}
	}
	return result, nil
}

// GetGroupWithGroupNo 查询一个群信息
func (s *Service) GetGroupWithGroupNo(groupNo string) (*InfoResp, error) {
	if groupNo == "" {
		return nil, errors.New("群编号不能为空")
	}
	group, err := s.db.QueryWithGroupNo(groupNo)
	if err != nil {
		return nil, err
	}
	if group == nil {
		return nil, errors.New("不存在此群")
	}
	return toInfoResp(group), nil
}

func (s *Service) GetGroupDetails(groupNos []string, uid string) ([]*GroupResp, error) {
	groupDetails, err := s.db.QueryDetailWithGroupNos(groupNos, uid)
	if err != nil {
		return nil, err
	}
	groupResps := make([]*GroupResp, 0)
	if len(groupDetails) == 0 {
		return groupResps, nil
	}
	externalMap, err := s.db.QueryExternalGroupNosForUser(uid)
	if err != nil {
		s.Error("query external group nos failed", zap.Error(err), zap.String("uid", uid))
		externalMap = nil
	}
	for _, groupDetail := range groupDetails {
		groupResp := &GroupResp{}
		groupResp = groupResp.from(groupDetail)
		groupResp.SetEffectiveSpaceIDFromMap(externalMap)
		groupResps = append(groupResps, groupResp)
	}
	return groupResps, nil
}

func (s *Service) GetGroupDetail(groupNo string, uid string) (*GroupResp, error) {
	groupDetailModel, err := s.db.QueryDetailWithGroupNo(groupNo, uid)
	if err != nil {
		s.Error("查询群信息失败！", zap.Error(err))
		return nil, errors.New("查询群信息失败！")
	}
	if groupDetailModel == nil {
		return nil, nil
	}
	memberCount, onlineCount, err := s.GetMemberTotalAndOnlineCount(groupNo)
	if err != nil {
		s.Error("查询成员数量和在线数量失败！")
		return nil, err
	}
	memberOfMe, err := s.db.QueryMemberWithUID(uid, groupNo)
	if err != nil {
		s.Error("查询成员失败！", zap.Error(err))
		return nil, err
	}
	quit := 0
	if memberOfMe == nil {
		quit = 1
	}
	groupResp := &GroupResp{}
	groupResp = groupResp.from(groupDetailModel)
	groupResp.MemberCount = memberCount
	groupResp.OnlineCount = onlineCount
	groupResp.Quit = quit
	if memberOfMe != nil {
		groupResp.Role = memberOfMe.Role
		groupResp.ForbiddenExpirTime = memberOfMe.ForbiddenExpirTime
		isManagerOrCreator := memberOfMe.Role == MemberRoleCreator || memberOfMe.Role == MemberRoleManager
		groupResp.CanEditGroupMd = isManagerOrCreator
		groupResp.CanManageBotAdmin = isManagerOrCreator
	}
	groupResp.SetEffectiveSpaceID(uid, s.db)
	return groupResp, nil
}

func (s *Service) GetGroups(groupNos []string) ([]*InfoResp, error) {
	groups, err := s.db.QueryWithGroupNos(groupNos)
	if err != nil {
		return nil, err
	}

	if len(groups) == 0 {
		return nil, nil
	}
	infoResps := make([]*InfoResp, 0, len(groups))
	for _, group := range groups {
		infoResps = append(infoResps, toInfoResp(group))
	}
	return infoResps, nil
}

func (s *Service) GetUserSupers(uid string) ([]*InfoResp, error) {
	groups, err := s.db.queryUserSupers(uid)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, nil
	}
	infoResps := make([]*InfoResp, 0, len(groups))
	for _, group := range groups {
		infoResps = append(infoResps, toInfoResp(group))
	}
	return infoResps, nil
}

func (s *Service) AddMember(model *AddMemberReq) error {
	err := s.db.InsertMember(&MemberModel{
		GroupNo: model.GroupNo,
		UID:     model.MemberUID,
	})
	return err
}
func (s *Service) GetGroupMemberMaxVersion(groupNo string) (int64, error) {
	version, err := s.db.queryGroupMemberMaxVersion(groupNo)
	return version, err
}

func (s *Service) GetMembers(groupNo string) ([]*MemberResp, error) {
	memberDetails, err := s.db.queryMembersWithGroupNo(groupNo)
	if err != nil {
		return nil, err
	}
	memberResps := make([]*MemberResp, 0, len(memberDetails))
	if len(memberDetails) > 0 {
		for _, memberDetail := range memberDetails {
			memberResps = append(memberResps, newMemberResp(memberDetail))
		}
	}
	return memberResps, nil
}
func (s *Service) GetMember(groupNo, uid string) (*MemberResp, error) {
	memberDetail, err := s.db.queryMemberWithGroupNoAndUID(groupNo, uid)
	if err != nil {
		return nil, err
	}
	if memberDetail == nil || memberDetail.IsDeleted == 1 {
		return nil, nil
	}
	memberResp := newMemberResp(memberDetail)
	return memberResp, nil
}

func (s *Service) GetBlacklistMemberUIDs(groupNo string) ([]string, error) {
	uids, err := s.db.queryBlacklistMemberUIDsWithGroupNo(groupNo)
	if err != nil {
		return nil, err
	}
	return uids, nil
}

func (s *Service) GetMemberUIDsOfManager(groupNo string) ([]string, error) {
	return s.db.QueryGroupManagerOrCreatorUIDS(groupNo)
}

func (s *Service) IsCreatorOrManager(groupNo string, uid string) (bool, error) {
	return s.db.QueryIsGroupManagerOrCreator(groupNo, uid)
}

func (s *Service) GetMemberTotalAndOnlineCount(groupNo string) (int, int, error) {
	var onlineCount, memberCount int64
	var err error
	memberCount, err = s.db.QueryMemberCount(groupNo)
	if err != nil {
		return 0, 0, err
	}
	onlineCount, err = s.db.queryMemberOnlineCount(groupNo)
	if err != nil {
		return 0, 0, err
	}
	return int(memberCount), int(onlineCount), nil
}

func (s *Service) ExistMember(groupNo string, uid string) (bool, error) {
	return s.db.ExistMember(uid, groupNo)
}

func (s *Service) ExistMembers(groupNos []string, uid string) ([]string, error) {
	return s.db.existMembers(groupNos, uid)
}

func (s *Service) GetSettings(groupNos []string, uid string) ([]*SettingResp, error) {
	settings, err := s.settingDB.QuerySettings(groupNos, uid)
	if err != nil {
		return nil, err
	}
	resps := make([]*SettingResp, 0, len(settings))
	if len(settings) > 0 {
		for _, setting := range settings {
			resps = append(resps, toSettingResp(setting))
		}
	}
	return resps, nil
}

// GetSettingsWithUIDs 查询一批用户对某个群的设置
func (s *Service) GetSettingsWithUIDs(groupNo string, uids []string) ([]*SettingResp, error) {
	settings, err := s.settingDB.QuerySettingsWithUIDs(groupNo, uids)
	if err != nil {
		return nil, err
	}
	resps := make([]*SettingResp, 0, len(settings))
	if len(settings) > 0 {
		for _, setting := range settings {
			resps = append(resps, toSettingResp(setting))
		}
	}
	return resps, nil
}

// GetMembersWithUIDAndGroupIds
func (s *Service) GetMembersWithUIDAndGroupIds(uid string, groupNos []string) ([]*MemberResp, error) {
	members, err := s.db.QueryMemberWithUIDAndGroupNos(uid, groupNos)
	if err != nil {
		return nil, err
	}
	list := make([]*MemberResp, 0, len(members))
	if len(members) > 0 {
		for _, member := range members {
			list = append(list, &MemberResp{
				UID:       member.UID,
				GroupNo:   member.GroupNo,
				Role:      member.Role,
				Remark:    member.Remark,
				CreatedAt: time.Time(member.CreatedAt).Unix(),
			})
		}
	}
	return list, err
}

func (s *Service) GetGroupMd(groupNo string) (*GroupMdResult, error) {
	return s.db.QueryGroupMd(groupNo)
}

func (s *Service) UpdateGroupMd(groupNo string, content string, updatedBy string) (int64, error) {
	return s.db.UpdateGroupMd(groupNo, content, updatedBy)
}

func (s *Service) DeleteGroupMd(groupNo string) (int64, error) {
	return s.db.DeleteGroupMd(groupNo)
}

func (s *Service) IsBotAdmin(groupNo string, uid string) (bool, error) {
	return s.db.QueryIsBotAdmin(groupNo, uid)
}

func (s *Service) GetBotMemberUIDs(groupNo string) ([]string, error) {
	return s.db.QueryBotMemberUIDs(groupNo)
}

// AddGroupReq 添加群
type AddGroupReq struct {
	GroupNo string
	Name    string
}

// AddMemberReq 添加群成员
type AddMemberReq struct {
	GroupNo   string
	MemberUID string
}

// InfoResp 群信息
type InfoResp struct {
	GroupNo             string    `json:"group_no"`               // 群编号
	GroupType           GroupType `json:"group_type"`             // 群类型
	Name                string    `json:"name"`                   // 群名称
	Notice              string    `json:"notice"`                 // 群公告
	Creator             string    `json:"creator"`                // 创建者uid
	Status              int       `json:"status"`                 // 群状态
	Forbidden           int       `json:"forbidden"`              // 是否全员禁言
	Invite              int       `json:"invite"`                 // 是否开启邀请确认 0.否 1.是
	ForbiddenAddFriend  int       `json:"forbidden_add_friend"`   //群内禁止加好友
	AllowViewHistoryMsg int       `json:"allow_view_history_msg"` // 是否允许新成员查看历史记录
	CreatedAt           string    `json:"created_at"`
	UpdatedAt           string    `json:"updated_at"`
	Version             int64     `json:"version"`           // 群数据版本
	SpaceID             string    `json:"space_id"`          // Space ID
	IsExternalGroup     int       `json:"is_external_group"` // 是否外部群
}

func toInfoResp(m *Model) *InfoResp {
	return &InfoResp{
		GroupNo:             m.GroupNo,
		GroupType:           GroupType(m.GroupType),
		Name:                m.Name,
		Notice:              m.Notice,
		Creator:             m.Creator,
		Status:              m.Status,
		Forbidden:           m.Forbidden,
		Invite:              m.Invite,
		ForbiddenAddFriend:  m.ForbiddenAddFriend,
		AllowViewHistoryMsg: m.AllowViewHistoryMsg,
		CreatedAt:           m.CreatedAt.String(),
		UpdatedAt:           m.UpdatedAt.String(),
		Version:             m.Version,
		SpaceID:             m.SpaceID,
		IsExternalGroup:     m.IsExternalGroup,
	}
}

type MemberResp struct {
	GroupNo            string // 群编号
	UID                string // 成员uid
	Name               string // 群成员名称
	Remark             string // 成员备注
	Role               int    // 成员角色
	Version            int64
	Vercode            string //验证码
	InviteUID          string // 邀请人uid
	CreatedAt          int64  // 注册时间 10位时间戳
	IsDeleted          int    //是否已删除
	ForbiddenExpirTime int64  // 禁言时长
	Status             int    // 成员状态
}

func newMemberResp(m *MemberDetailModel) *MemberResp {
	return &MemberResp{
		GroupNo:            m.GroupNo,
		UID:                m.UID,
		Name:               m.Name,
		Remark:             m.Remark,
		Role:               m.Role,
		Version:            m.Version,
		Vercode:            m.Vercode,
		InviteUID:          m.InviteUID,
		IsDeleted:          m.IsDeleted,
		ForbiddenExpirTime: m.ForbiddenExpirTime,
		Status:             m.Status,
		CreatedAt:          time.Time(m.CreatedAt).Unix(),
	}
}

// SettingResp 群设置
type SettingResp struct {
	UID             string
	GroupNo         string // 群编号
	Mute            int    // 免打扰
	Top             int    // 置顶
	ShowNick        int    // 显示昵称
	Save            int    // 是否保存
	ChatPwdOn       int    //是否开启聊天密码
	Screenshot      int    //截屏通知
	RevokeRemind    int    //撤回通知
	JoinGroupRemind int    //进群提醒
	Receipt         int    //消息是否回执
	Remark          string // 群备注
	Version         int64  // 版本
}

func toSettingResp(m *Setting) *SettingResp {
	return &SettingResp{
		GroupNo:         m.GroupNo,
		Mute:            m.Mute,
		Top:             m.Top,
		ShowNick:        m.ShowNick,
		Save:            m.Save,
		ChatPwdOn:       m.ChatPwdOn,
		Screenshot:      m.Screenshot,
		RevokeRemind:    m.RevokeRemind,
		JoinGroupRemind: m.JoinGroupRemind,
		Receipt:         m.Receipt,
		Remark:          m.Remark,
		Version:         m.Version,
		UID:             m.UID,
	}
}

type GroupResp struct {
	GroupNo                  string    `json:"group_no"`                    // 群编号
	GroupType                GroupType `json:"group_type"`                  // 群类型
	Category                 string    `json:"category"`                    // 群分类
	Name                     string    `json:"name"`                        // 群名称
	Remark                   string    `json:"remark"`                      // 群备注
	Notice                   string    `json:"notice"`                      // 群公告
	Mute                     int       `json:"mute"`                        // 免打扰
	Top                      int       `json:"top"`                         // 置顶
	ShowNick                 int       `json:"show_nick"`                   // 显示昵称
	Save                     int       `json:"save"`                        // 是否保存
	Forbidden                int       `json:"forbidden"`                   // 是否全员禁言
	Invite                   int       `json:"invite"`                      // 群聊邀请确认
	ChatPwdOn                int       `json:"chat_pwd_on"`                 //是否开启聊天密码
	Screenshot               int       `json:"screenshot"`                  //截屏通知
	RevokeRemind             int       `json:"revoke_remind"`               //撤回提醒
	JoinGroupRemind          int       `json:"join_group_remind"`           //进群提醒
	ForbiddenAddFriend       int       `json:"forbidden_add_friend"`        //群内禁止加好友
	Status                   int       `json:"status"`                      //群状态
	Receipt                  int       `json:"receipt"`                     //消息是否回执
	Flame                    int       `json:"flame"`                       // 阅后即焚
	FlameSecond              int       `json:"flame_second"`                // 阅后即焚秒数
	AllowViewHistoryMsg      int       `json:"allow_view_history_msg"`      // 是否允许新成员查看历史消息
	MemberCount              int       `json:"member_count"`                // 成员数量
	OnlineCount              int       `json:"online_count"`                // 在线数量
	Quit                     int       `json:"quit"`                        // 我是否已退出群聊
	Role                     int       `json:"role"`                        // 我在群聊里的角色
	ForbiddenExpirTime       int64     `json:"forbidden_expir_time"`        // 我在此群的禁言过期时间
	AllowMemberPinnedMessage int       `json:"allow_member_pinned_message"` //是否允许群成员置顶消息
	HasGroupMd               bool      `json:"has_group_md"`                // 是否有GROUP.md
	GroupMdVersion           int64     `json:"group_md_version"`            // GROUP.md版本
	GroupMdUpdatedAt         *string   `json:"group_md_updated_at"`         // GROUP.md最后更新时间
	CanEditGroupMd           bool      `json:"can_edit_group_md"`           // 是否可编辑GROUP.md
	CanManageBotAdmin        bool      `json:"can_manage_bot_admin"`        // 是否可管理Bot管理员
	SpaceID                  string    `json:"space_id"`                    // Space ID
	IsExternalGroup          int       `json:"is_external_group"`           // 是否外部群 0.否 1.是
	CreatedAt                string    `json:"created_at"`
	UpdatedAt                string    `json:"updated_at"`
	Version                  int64     `json:"version"` // 群数据版本
}

func (g *GroupResp) from(model *DetailModel) *GroupResp {
	resp := &GroupResp{
		GroupNo:                  model.GroupNo,
		GroupType:                GroupType(model.GroupType),
		Category:                 model.Category,
		Name:                     model.Name,
		Notice:                   model.Notice,
		Mute:                     model.Mute,
		Top:                      model.Top,
		ShowNick:                 model.ShowNick,
		Save:                     model.Save,
		Remark:                   model.Remark,
		Version:                  model.Version,
		Forbidden:                model.Forbidden,
		Invite:                   model.Invite,
		ChatPwdOn:                model.ChatPwdOn,
		Screenshot:               model.Screenshot,
		RevokeRemind:             model.RevokeRemind,
		JoinGroupRemind:          model.JoinGroupRemind,
		ForbiddenAddFriend:       model.ForbiddenAddFriend,
		Receipt:                  model.Receipt,
		Flame:                    model.Flame,
		FlameSecond:              model.FlameSecond,
		Status:                   model.Status,
		AllowViewHistoryMsg:      model.AllowViewHistoryMsg,
		AllowMemberPinnedMessage: model.AllowMemberPinnedMessage,
		SpaceID:                  model.SpaceID,
		IsExternalGroup:          model.IsExternalGroup,
		HasGroupMd:               model.GroupMd != nil && *model.GroupMd != "",
		GroupMdVersion:           model.GroupMdVersion,
		CreatedAt:                model.CreatedAt.String(),
		UpdatedAt:                model.UpdatedAt.String(),
	}
	if model.GroupMdUpdatedAt != nil {
		t := model.GroupMdUpdatedAt.Format("2006-01-02 15:04:05")
		resp.GroupMdUpdatedAt = &t
	}
	return resp
}

func (g *GroupResp) fromModel(model *Model) *GroupResp {
	resp := &GroupResp{
		GroupNo:                  model.GroupNo,
		GroupType:                GroupType(model.GroupType),
		Category:                 model.Category,
		Name:                     model.Name,
		Notice:                   model.Notice,
		Forbidden:                model.Forbidden,
		Invite:                   model.Invite,
		ForbiddenAddFriend:       model.ForbiddenAddFriend,
		Status:                   model.Status,
		AllowViewHistoryMsg:      model.AllowViewHistoryMsg,
		AllowMemberPinnedMessage: model.AllowMemberPinnedMessage,
		SpaceID:                  model.SpaceID,
		IsExternalGroup:          model.IsExternalGroup,
		HasGroupMd:               model.GroupMd != nil && *model.GroupMd != "",
		GroupMdVersion:           model.GroupMdVersion,
		CreatedAt:                model.CreatedAt.String(),
		UpdatedAt:                model.UpdatedAt.String(),
	}
	if model.GroupMdUpdatedAt != nil {
		t := model.GroupMdUpdatedAt.Format("2006-01-02 15:04:05")
		resp.GroupMdUpdatedAt = &t
	}
	return resp
}

// SetEffectiveSpaceID 对外部群的外部成员替换 SpaceID 为其来源 Space，
// 这样 Web 端依赖 space_id 的会话过滤逻辑无需修改即可自然匹配。
func (g *GroupResp) SetEffectiveSpaceID(loginUID string, db *DB) {
	if g == nil || g.IsExternalGroup == 0 || loginUID == "" {
		return
	}
	sourceSpaceID, err := db.QuerySourceSpaceIDForMember(g.GroupNo, loginUID)
	if err != nil || sourceSpaceID == "" {
		return
	}
	g.SpaceID = sourceSpaceID
}

// SetEffectiveSpaceIDFromMap 与 SetEffectiveSpaceID 等价，但使用调用方预先批量查询的
// groupNo -> sourceSpaceID 映射，避免列表场景下的 N+1 查询。
func (g *GroupResp) SetEffectiveSpaceIDFromMap(externalMap map[string]string) {
	if g == nil || g.IsExternalGroup == 0 || len(externalMap) == 0 {
		return
	}
	if sourceSpaceID, ok := externalMap[g.GroupNo]; ok && sourceSpaceID != "" {
		g.SpaceID = sourceSpaceID
	}
}

// GetGroupMdMaxSize returns the max GROUP.md size from env or default (10240)
func GetGroupMdMaxSize() int {
	val := os.Getenv("TS_GROUPMDMAXSIZE")
	if val != "" {
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			return n
		}
	}
	return 10240
}

// ---------- Service Request/Response types ----------

// CreateGroupServiceReq 创建群请求
type CreateGroupServiceReq struct {
	Creator    string   // 创建者 UID
	Members    []string // 成员 UID 列表（不含创建者，Service 内部会自动加入）
	Name       string   // 群名称（可为空，Service 会自动生成）
	SpaceID    string   // Space ID（可为空）
	BotUID     string   // Bot UID（可为空；非空时自动加入群并设为 bot_admin）
	CategoryID string   // 群聊分组 ID（可为空；非空时自动设置创建者的 group_setting）
}

// CreateGroupServiceResp 创建群响应
type CreateGroupServiceResp struct {
	GroupNo        string   // 群编号
	Name           string   // 群名称
	SkippedMembers []string // 因不在 Space 而被过滤的成员 UID
}

// AddGroupMembersServiceReq 添加群成员请求
type AddGroupMembersServiceReq struct {
	GroupNo      string   // 群编号
	Members      []string // 待添加成员 UID 列表
	OperatorUID  string   // 操作者 UID
	OperatorName string   // 操作者名称
}

// AddGroupMembersServiceResp 添加群成员响应
type AddGroupMembersServiceResp struct {
	Added int // 实际添加成功的数量
}

// RemoveGroupMembersServiceReq 移除群成员请求
type RemoveGroupMembersServiceReq struct {
	GroupNo      string   // 群编号
	Members      []string // 待移除成员 UID 列表
	OperatorUID  string   // 操作者 UID
	OperatorName string   // 操作者名称
}

// RemoveGroupMembersServiceResp 移除群成员响应
type RemoveGroupMembersServiceResp struct {
	Removed     int      // 实际移除数量
	RemovedUIDs []string // 实际移除的 UID 列表
}

// UpdateGroupInfoServiceReq 更新群信息请求
type UpdateGroupInfoServiceReq struct {
	GroupNo      string  // 群编号
	OperatorUID  string  // 操作者 UID
	OperatorName string  // 操作者名称
	Name         *string // 新群名（nil 表示不更新）
	Notice       *string // 新公告（nil 表示不更新）
}

// ---------- Service method implementations ----------

// CreateGroup 创建群（统一入口，Web 和 Bot 共用）
func (s *Service) CreateGroup(req *CreateGroupServiceReq) (*CreateGroupServiceResp, error) {
	if req.Creator == "" {
		return nil, errors.New("creator is required")
	}
	if len(req.Members) == 0 {
		return nil, errors.New("members is required")
	}

	var skippedMembers []string

	// Space 校验
	if req.SpaceID != "" {
		// 校验 Bot 是否属于目标 Space
		if req.BotUID != "" {
			botOk, err := spacepkg.CheckMembership(s.ctx.DB(), req.SpaceID, req.BotUID)
			if err != nil {
				s.Error("check bot space membership failed", zap.Error(err))
				return nil, errors.New("failed to check space membership")
			}
			if !botOk {
				return nil, errors.New("bot is not a member of this space")
			}
		}
		creatorOk, err := spacepkg.CheckMembership(s.ctx.DB(), req.SpaceID, req.Creator)
		if err != nil {
			s.Error("check creator space membership failed", zap.Error(err))
			return nil, errors.New("failed to check space membership")
		}
		if !creatorOk {
			return nil, errors.New("creator is not a member of this space")
		}
		validMembers := make([]string, 0, len(req.Members))
		for _, uid := range req.Members {
			ok, err := spacepkg.CheckMembership(s.ctx.DB(), req.SpaceID, uid)
			if err != nil {
				s.Error("check member space membership failed", zap.Error(err), zap.String("uid", uid))
				return nil, errors.New("failed to check space membership")
			}
			if ok {
				validMembers = append(validMembers, uid)
			} else {
				skippedMembers = append(skippedMembers, uid)
			}
		}
		if len(validMembers) == 0 {
			return nil, errors.New("none of the members belong to this space")
		}
		req.Members = validMembers
	}

	// 查询创建者用户信息
	creatorUser, err := s.userDB.QueryByUID(req.Creator)
	if err != nil {
		s.Error("query creator info failed", zap.Error(err))
		return nil, errors.New("failed to query creator info")
	}
	if creatorUser == nil {
		return nil, errors.New("creator user not found")
	}

	// 成员去重，加入创建者，过滤空值
	allUIDs := make([]string, 0, len(req.Members)+1)
	allUIDs = append(allUIDs, req.Creator)
	seen := map[string]bool{req.Creator: true}
	for _, uid := range req.Members {
		uid = strings.TrimSpace(uid)
		if uid != "" && !seen[uid] {
			seen[uid] = true
			allUIDs = append(allUIDs, uid)
		}
	}

	// 查询成员用户信息
	memberUsers, err := s.userDB.QueryByUIDs(allUIDs)
	if err != nil {
		s.Error("query member info failed", zap.Error(err))
		return nil, errors.New("failed to query member info")
	}
	if len(memberUsers) == 0 {
		return nil, errors.New("no valid member found")
	}

	// 群名生成
	groupName := strings.TrimSpace(req.Name)
	if groupName == "" {
		names := make([]string, 0, len(memberUsers))
		for _, u := range memberUsers {
			names = append(names, u.Name)
		}
		groupName = strings.Join(names, "、")
	}
	nameRunes := []rune(groupName)
	if len(nameRunes) > 20 {
		groupName = string(nameRunes[:20])
	}

	// 生成群编号和版本号
	groupNo := util.GenerUUID()
	version, err := s.ctx.GenSeq(common.GroupSeqKey)
	if err != nil {
		s.Error("generate group version failed", zap.Error(err))
		return nil, errors.New("failed to generate group version")
	}

	// 开启事务
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		s.Error("begin transaction failed", zap.Error(err))
		return nil, errors.New("failed to begin transaction")
	}
	defer tx.RollbackUnlessCommitted()

	// 插入群记录
	err = s.db.InsertTx(&Model{
		GroupNo:             groupNo,
		Name:                groupName,
		Creator:             req.Creator,
		Status:              GroupStatusNormal,
		Version:             version,
		AllowViewHistoryMsg: int(common.GroupAllowViewHistoryMsgEnabled),
		SpaceID:             req.SpaceID,
	}, tx)
	if err != nil {
		s.Error("insert group record failed", zap.Error(err))
		return nil, errors.New("failed to insert group record")
	}

	// 插入成员
	realMemberUIDs := make([]string, 0, len(memberUsers))
	memberVos := make([]*config.UserBaseVo, 0, len(memberUsers))
	for _, memberUser := range memberUsers {
		if memberUser.IsDestroy == 1 {
			continue
		}
		// Bot UID 单独处理（下面添加）
		if req.BotUID != "" && memberUser.UID == req.BotUID {
			continue
		}
		memberVersion, err := s.ctx.GenSeq(common.GroupMemberSeqKey)
		if err != nil {
			s.Error("generate member version failed", zap.Error(err))
			return nil, err
		}
		role := MemberRoleCommon
		if memberUser.UID == req.Creator {
			role = MemberRoleCreator
		}
		err = s.db.InsertMemberTx(&MemberModel{
			GroupNo:   groupNo,
			UID:       memberUser.UID,
			Role:      role,
			Version:   memberVersion,
			InviteUID: req.Creator,
			Robot:     memberUser.Robot,
			Status:    int(common.GroupMemberStatusNormal),
			Vercode:   fmt.Sprintf("%s@%d", util.GenerUUID(), common.GroupMember),
		}, tx)
		if err != nil {
			s.Error("insert member failed", zap.Error(err), zap.String("uid", memberUser.UID))
			return nil, errors.New("failed to insert group member")
		}
		realMemberUIDs = append(realMemberUIDs, memberUser.UID)
		memberVos = append(memberVos, &config.UserBaseVo{UID: memberUser.UID, Name: memberUser.Name})
	}
	if len(realMemberUIDs) == 0 {
		return nil, errors.New("no valid member to add")
	}

	// Bot 加入群
	if req.BotUID != "" {
		botMemberVersion, err := s.ctx.GenSeq(common.GroupMemberSeqKey)
		if err != nil {
			s.Error("generate bot member version failed", zap.Error(err))
			return nil, err
		}
		err = s.db.InsertMemberTx(&MemberModel{
			GroupNo:   groupNo,
			UID:       req.BotUID,
			Role:      MemberRoleCommon,
			Version:   botMemberVersion,
			InviteUID: req.Creator,
			Robot:     1,
			Status:    int(common.GroupMemberStatusNormal),
			Vercode:   fmt.Sprintf("%s@%d", util.GenerUUID(), common.GroupMember),
		}, tx)
		if err != nil {
			s.Error("insert bot member failed", zap.Error(err))
			// Bot 加入失败不阻断建群
		} else {
			realMemberUIDs = append(realMemberUIDs, req.BotUID)
			memberVos = append(memberVos, &config.UserBaseVo{UID: req.BotUID, Name: req.BotUID})
		}
	}

	// 生成群头像事件（事务内）
	n := len(realMemberUIDs)
	if n > 9 {
		n = 9
	}
	avatarMembers := make([]string, n)
	copy(avatarMembers, realMemberUIDs[:n])
	groupAvatarEventID, err := beginAvatarUpdateEvent(s.ctx, s.db, groupNo, avatarMembers, nil, tx)
	if err != nil {
		tx.Rollback()
		s.Error("begin group avatar update event failed", zap.Error(err))
		return nil, err
	}

	// 提交事务
	if err := tx.Commit(); err != nil {
		s.Error("commit transaction failed", zap.Error(err))
		return nil, errors.New("failed to commit transaction")
	}

	// 提交头像生成事件
	if groupAvatarEventID != 0 {
		s.ctx.EventCommit(groupAvatarEventID)
	}

	// 事务提交后设置 Bot 为 bot_admin
	if req.BotUID != "" {
		botAdminVersion, _ := s.ctx.GenSeq(common.GroupMemberSeqKey)
		if err := s.db.UpdateBotAdmin(groupNo, req.BotUID, 1, botAdminVersion); err != nil {
			s.Error("set bot_admin failed", zap.Error(err))
		}
	}

	// 设置创建者的群聊分组（best-effort：失败不阻断建群，与 BotUID 设置同策略）
	if req.CategoryID != "" {
		setting, err := s.settingDB.QuerySetting(groupNo, req.Creator)
		if err != nil {
			s.Error("query group setting for category failed", zap.Error(err))
		} else if setting == nil {
			settingVersion, _ := s.ctx.GenSeq(common.GroupSettingSeqKey)
			_, err = s.ctx.DB().InsertBySql(
				"INSERT INTO group_setting (group_no, uid, category_id, category_sort, revoke_remind, screenshot, receipt, version) VALUES (?, ?, ?, 0, 1, 1, 1, ?)",
				groupNo, req.Creator, req.CategoryID, settingVersion,
			).Exec()
			if err != nil {
				s.Error("insert group setting with category failed", zap.Error(err))
			}
		} else {
			settingVersion, _ := s.ctx.GenSeq(common.GroupSettingSeqKey)
			_, err = s.ctx.DB().Update("group_setting").
				Set("category_id", req.CategoryID).
				Set("category_sort", 0).
				Set("version", settingVersion).
				Where("id=?", setting.Id).Exec()
			if err != nil {
				s.Error("update group setting category failed", zap.Error(err))
			}
		}
	}

	// 创建 IM 频道
	err = s.ctx.IMCreateOrUpdateChannel(&config.ChannelCreateReq{
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Subscribers: realMemberUIDs,
	})
	if err != nil {
		s.Error("create IM channel failed", zap.Error(err))
	}

	// 发送群创建通知
	s.ctx.SendGroupCreate(&config.MsgGroupCreateReq{
		Creator:     req.Creator,
		CreatorName: creatorUser.Name,
		GroupNo:     groupNo,
		Version:     version,
		Members:     memberVos,
	})

	return &CreateGroupServiceResp{
		GroupNo:        groupNo,
		Name:           groupName,
		SkippedMembers: skippedMembers,
	}, nil
}

// AddGroupMembers 添加群成员
func (s *Service) AddGroupMembers(req *AddGroupMembersServiceReq) (*AddGroupMembersServiceResp, error) {
	if req.GroupNo == "" {
		return nil, errors.New("group_no is required")
	}
	if len(req.Members) == 0 {
		return nil, errors.New("members is required")
	}

	// 群存在性 + 状态检查
	groupModel, err := s.db.QueryWithGroupNo(req.GroupNo)
	if err != nil {
		s.Error("query group failed", zap.Error(err))
		return nil, errors.New("failed to query group")
	}
	if groupModel == nil || groupModel.Status == GroupStatusDisband {
		return nil, errors.New("group not found or disbanded")
	}

	// 成员去重、过滤空值
	seen := make(map[string]bool)
	var uniqueUIDs []string
	for _, uid := range req.Members {
		uid = strings.TrimSpace(uid)
		if uid != "" && !seen[uid] {
			seen[uid] = true
			uniqueUIDs = append(uniqueUIDs, uid)
		}
	}
	if len(uniqueUIDs) == 0 {
		return nil, errors.New("no valid members after deduplication")
	}

	// Space 成员校验：群属于某个 Space 时，不在 Space 的成员标记为外部成员。
	// source_space_id 的确定规则：
	//   - 若操作者是外部成员，沿用其 source_space_id（同源 Space 邀请）
	//   - 否则使用被邀请人的默认 Space
	externalMap := make(map[string]bool)
	sourceSpaceMap := make(map[string]string)
	if groupModel.SpaceID != "" {
		var operatorMember *MemberModel
		if req.OperatorUID != "" {
			operatorMember, _ = s.db.QueryMemberWithUID(req.OperatorUID, req.GroupNo)
		}
		for _, uid := range uniqueUIDs {
			ok, err := spacepkg.CheckMembership(s.ctx.DB(), groupModel.SpaceID, uid)
			if err != nil {
				s.Error("check space membership failed", zap.Error(err), zap.String("uid", uid))
				return nil, errors.New("failed to check space membership")
			}
			if ok {
				continue
			}
			externalMap[uid] = true
			if operatorMember != nil && operatorMember.IsExternal == 1 && operatorMember.SourceSpaceID != "" {
				sourceSpaceMap[uid] = operatorMember.SourceSpaceID
			} else {
				sourceSpaceMap[uid] = spacemod.GetUserDefaultSpaceID(s.ctx, uid)
			}
		}
	}

	// 查询用户信息
	memberUsers, err := s.userDB.QueryByUIDs(uniqueUIDs)
	if err != nil {
		s.Error("query member info failed", zap.Error(err))
		return nil, errors.New("failed to query member info")
	}

	// 过滤已在群内的成员
	existingMembers, err := s.db.QueryMembersWithUids(uniqueUIDs, req.GroupNo)
	if err != nil {
		s.Error("query existing members failed", zap.Error(err))
		return nil, errors.New("failed to query existing members")
	}
	existingSet := make(map[string]bool)
	for _, m := range existingMembers {
		if m.IsDeleted == 0 {
			existingSet[m.UID] = true
		}
	}

	// 过滤黑名单
	blacklistMembers, _ := s.db.QueryMembersWithStatus(req.GroupNo, int(common.GroupMemberStatusBlacklist))
	blacklistSet := make(map[string]bool)
	for _, m := range blacklistMembers {
		blacklistSet[m.UID] = true
	}

	// 开启事务
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		s.Error("begin transaction failed", zap.Error(err))
		return nil, errors.New("failed to begin transaction")
	}
	defer tx.RollbackUnlessCommitted()

	var addedUIDs []string
	var addedVos []*config.UserBaseVo
	hasNewExternal := false
	for _, memberUser := range memberUsers {
		if memberUser.IsDestroy == 1 {
			continue
		}
		if existingSet[memberUser.UID] || blacklistSet[memberUser.UID] {
			continue
		}
		memberVersion, err := s.ctx.GenSeq(common.GroupMemberSeqKey)
		if err != nil {
			s.Error("generate member version failed", zap.Error(err))
			return nil, err
		}

		isExt := 0
		srcSpaceID := ""
		if externalMap[memberUser.UID] {
			isExt = 1
			srcSpaceID = sourceSpaceMap[memberUser.UID]
		}

		// 检查是否之前被删除过（需要恢复）
		existDelete, _ := s.db.ExistMemberDelete(memberUser.UID, req.GroupNo)
		newMember := &MemberModel{
			GroupNo:       req.GroupNo,
			UID:           memberUser.UID,
			Role:          MemberRoleCommon,
			Version:       memberVersion,
			Status:        int(common.GroupMemberStatusNormal),
			InviteUID:     req.OperatorUID,
			Robot:         memberUser.Robot,
			Vercode:       fmt.Sprintf("%s@%d", util.GenerUUID(), common.GroupMember),
			IsExternal:    isExt,
			SourceSpaceID: srcSpaceID,
		}
		if existDelete {
			err = s.db.recoverMemberTx(newMember, tx)
		} else {
			err = s.db.InsertMemberTx(newMember, tx)
		}
		if err != nil {
			s.Error("add group member failed", zap.Error(err), zap.String("uid", memberUser.UID))
			continue
		}
		addedUIDs = append(addedUIDs, memberUser.UID)
		addedVos = append(addedVos, &config.UserBaseVo{UID: memberUser.UID, Name: memberUser.Name})
		if isExt == 1 {
			hasNewExternal = true
		}
	}

	// 首次出现外部成员时，在事务内将群标记为外部群，确保成员/群标记一致提交
	markedExternal := false
	if hasNewExternal && groupModel.IsExternalGroup == 0 {
		if updateErr := s.db.UpdateIsExternalGroupTx(req.GroupNo, 1, tx); updateErr != nil {
			s.Error("update is_external_group failed", zap.Error(updateErr), zap.String("group_no", req.GroupNo))
			return nil, errors.New("failed to update external group flag")
		}
		markedExternal = true
	}

	// 提交事务
	if err := tx.Commit(); err != nil {
		s.Error("commit transaction failed", zap.Error(err))
		return nil, errors.New("failed to commit transaction")
	}

	if markedExternal {
		s.ctx.SendChannelUpdateToGroup(req.GroupNo)
	}

	// IM 操作（事务提交之后）
	if len(addedUIDs) > 0 {
		// 添加 IM 订阅
		if err := s.ctx.IMAddSubscriber(&config.SubscriberAddReq{
			ChannelID:   req.GroupNo,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			Subscribers: addedUIDs,
		}); err != nil {
			s.Error("add IM subscriber failed", zap.Error(err))
		}

		// 发布成员添加事件
		s.ctx.SendGroupMemberAdd(&config.MsgGroupMemberAddReq{
			Operator:     req.OperatorUID,
			OperatorName: req.OperatorName,
			GroupNo:      req.GroupNo,
			Members:      addedVos,
		})

		// 发送群成员更新 CMD
		s.ctx.SendCMD(config.MsgCMDReq{
			ChannelID:   req.GroupNo,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			CMD:         common.CMDGroupMemberUpdate,
			Param: map[string]interface{}{
				"group_no": req.GroupNo,
			},
		})

		// 同步新成员到群内所有子区的 IM 订阅（直接 SQL 查 thread 表）
		s.addUsersToGroupThreads(req.GroupNo, addedUIDs)

		// 检查新增成员中是否有 Bot，推送 bot_joined_group 事件
		addedUIDSet := make(map[string]bool, len(addedUIDs))
		for _, uid := range addedUIDs {
			addedUIDSet[uid] = true
		}
		go s.notifyBotJoinedGroup(memberUsers, addedUIDSet, req.GroupNo, req.OperatorUID, req.OperatorName)
	}

	return &AddGroupMembersServiceResp{
		Added: len(addedUIDs),
	}, nil
}

// RemoveGroupMembers 移除群成员
func (s *Service) RemoveGroupMembers(req *RemoveGroupMembersServiceReq) (*RemoveGroupMembersServiceResp, error) {
	if req.GroupNo == "" {
		return nil, errors.New("group_no is required")
	}
	if len(req.Members) == 0 {
		return nil, errors.New("members is required")
	}

	// 群存在性检查
	groupModel, err := s.db.QueryWithGroupNo(req.GroupNo)
	if err != nil {
		s.Error("query group failed", zap.Error(err))
		return nil, errors.New("failed to query group")
	}
	if groupModel == nil || groupModel.Status == GroupStatusDisband {
		return nil, errors.New("group not found or disbanded")
	}

	// 查询待移除成员信息
	targetMembers, err := s.db.QueryMembersWithUids(req.Members, req.GroupNo)
	if err != nil {
		s.Error("query target members failed", zap.Error(err))
		return nil, errors.New("failed to query member info")
	}
	if len(targetMembers) == 0 {
		return nil, errors.New("none of the members are in this group")
	}

	// 过滤：跳过群主、管理员、已删除的成员
	var removableMembers []*MemberModel
	for _, m := range targetMembers {
		if m.IsDeleted == 1 || m.Role == MemberRoleCreator || m.Role == MemberRoleManager {
			continue
		}
		removableMembers = append(removableMembers, m)
	}
	if len(removableMembers) == 0 {
		return &RemoveGroupMembersServiceResp{Removed: 0}, nil
	}

	// 开启事务
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		s.Error("begin transaction failed", zap.Error(err))
		return nil, errors.New("failed to begin transaction")
	}
	defer tx.RollbackUnlessCommitted()

	var removedUIDs []string
	var removedVos []*config.UserBaseVo
	removedExternal := false
	for _, m := range removableMembers {
		memberVersion, err := s.ctx.GenSeq(common.GroupMemberSeqKey)
		if err != nil {
			s.Error("generate member version failed", zap.Error(err))
			return nil, err
		}
		err = s.db.DeleteMemberTx(req.GroupNo, m.UID, memberVersion, tx)
		if err != nil {
			s.Error("delete group member failed", zap.Error(err), zap.String("uid", m.UID))
			continue
		}
		removedUIDs = append(removedUIDs, m.UID)
		if m.IsExternal == 1 {
			removedExternal = true
		}
		// 查询用户名
		memberUser, _ := s.userDB.QueryByUID(m.UID)
		name := m.UID
		if memberUser != nil {
			name = memberUser.Name
		}
		removedVos = append(removedVos, &config.UserBaseVo{UID: m.UID, Name: name})
	}

	// 若移除了外部成员且当前群是外部群，检查剩余外部成员数；为 0 则恢复为普通群
	resetExternalGroup := false
	if removedExternal && groupModel.IsExternalGroup == 1 {
		externalCount, countErr := s.db.QueryExternalMemberCountTx(req.GroupNo, tx)
		if countErr != nil {
			s.Error("query external member count failed", zap.Error(countErr))
		} else if externalCount == 0 {
			if updateErr := s.db.UpdateIsExternalGroupTx(req.GroupNo, 0, tx); updateErr != nil {
				s.Error("update is_external_group failed", zap.Error(updateErr))
			} else {
				resetExternalGroup = true
			}
		}
	}

	// 生成群头像更新事件（best-effort，不阻塞踢人）
	var groupAvatarEventID int64
	if len(removedUIDs) > 0 {
		avatarEventID, avatarErr := beginAvatarUpdateEvent(s.ctx, s.db, req.GroupNo, nil, removedUIDs, tx)
		if avatarErr != nil {
			s.Error("begin group avatar update event failed", zap.Error(avatarErr))
		} else {
			groupAvatarEventID = avatarEventID
		}
	}

	// 提交事务
	if err := tx.Commit(); err != nil {
		s.Error("commit transaction failed", zap.Error(err))
		return nil, errors.New("failed to commit transaction")
	}

	// 外部群标记发生变化时，通知成员刷新频道信息
	if resetExternalGroup {
		s.ctx.SendChannelUpdateToGroup(req.GroupNo)
	}

	// 提交头像生成事件
	if groupAvatarEventID != 0 {
		s.ctx.EventCommit(groupAvatarEventID)
	}

	// IM 操作（事务提交之后）
	if len(removedUIDs) > 0 {
		// 移除 IM 订阅
		if err := s.ctx.IMRemoveSubscriber(&config.SubscriberRemoveReq{
			ChannelID:   req.GroupNo,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			Subscribers: removedUIDs,
		}); err != nil {
			s.Error("remove IM subscriber failed", zap.Error(err))
		}

		// 发送被踢消息
		removeReq := &config.MsgGroupMemberRemoveReq{
			Operator:     req.OperatorUID,
			OperatorName: req.OperatorName,
			GroupNo:      req.GroupNo,
			Members:      removedVos,
		}
		if err := s.ctx.SendGroupMemberBeRemove(removeReq); err != nil {
			s.Error("send group member remove notification failed", zap.Error(err))
		}

		// 发送群成员更新 CMD
		s.ctx.SendCMD(config.MsgCMDReq{
			ChannelID:   req.GroupNo,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			CMD:         common.CMDGroupMemberUpdate,
			Param: map[string]interface{}{
				"group_no": req.GroupNo,
			},
		})

		// 移除被踢用户在该群所有子区的成员身份和置顶（直接 SQL 查 thread 表）
		for _, uid := range removedUIDs {
			s.removeUserFromGroupThreads(req.GroupNo, uid, groupModel.SpaceID)
			// 清理用户在该群的置顶（按 Space 隔离）
			user.RemovePinnedForUserInSpace(uid, groupModel.SpaceID, req.GroupNo, common.ChannelTypeGroup.Uint8())
		}
	}

	return &RemoveGroupMembersServiceResp{
		Removed:     len(removedUIDs),
		RemovedUIDs: removedUIDs,
	}, nil
}

// UpdateGroupInfo 更新群信息
func (s *Service) UpdateGroupInfo(req *UpdateGroupInfoServiceReq) error {
	if req.GroupNo == "" {
		return errors.New("group_no is required")
	}
	if req.Name == nil && req.Notice == nil {
		return errors.New("at least one of name or notice is required")
	}

	// 群存在性 + 状态检查
	groupModel, err := s.db.QueryWithGroupNo(req.GroupNo)
	if err != nil {
		s.Error("query group failed", zap.Error(err))
		return errors.New("failed to query group")
	}
	if groupModel == nil || groupModel.Status == GroupStatusDisband {
		return errors.New("group not found or disbanded")
	}

	// 生成新版本号
	version, err := s.ctx.GenSeq(common.GroupSeqKey)
	if err != nil {
		s.Error("generate group version failed", zap.Error(err))
		return errors.New("failed to generate group version")
	}
	groupModel.Version = version

	// 更新字段
	if req.Name != nil {
		nameRunes := []rune(*req.Name)
		if len(nameRunes) > 20 {
			*req.Name = string(nameRunes[:20])
		}
		groupModel.Name = *req.Name
	}
	if req.Notice != nil {
		groupModel.Notice = *req.Notice
	}

	// 事务更新
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		s.Error("begin transaction failed", zap.Error(err))
		return errors.New("failed to begin transaction")
	}
	defer tx.RollbackUnlessCommitted()

	err = s.db.UpdateTx(groupModel, tx)
	if err != nil {
		s.Error("update group failed", zap.Error(err))
		return errors.New("failed to update group")
	}

	if err := tx.Commit(); err != nil {
		s.Error("commit transaction failed", zap.Error(err))
		return errors.New("failed to commit transaction")
	}

	// 发布群更新事件（name 和 notice 分开发送）
	if req.Name != nil {
		s.ctx.SendGroupUpdate(&config.MsgGroupUpdateReq{
			GroupNo:      req.GroupNo,
			Operator:     req.OperatorUID,
			OperatorName: req.OperatorName,
			Attr:         common.GroupAttrKeyName,
			Data:         map[string]string{common.GroupAttrKeyName: *req.Name},
		})
	}
	if req.Notice != nil {
		s.ctx.SendGroupUpdate(&config.MsgGroupUpdateReq{
			GroupNo:      req.GroupNo,
			Operator:     req.OperatorUID,
			OperatorName: req.OperatorName,
			Attr:         common.GroupAttrKeyNotice,
			Data:         map[string]string{common.GroupAttrKeyNotice: *req.Notice},
		})
	}

	// 通知客户端刷新频道信息
	s.ctx.SendChannelUpdateToGroup(req.GroupNo)

	return nil
}

// ---------- Service internal helpers (thread sync, no thread package import) ----------

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// beginAvatarUpdateEvent 在事务内创建群头像更新事件（公共逻辑）。
// memberUIDs 非空时直接使用（新建群场景）；为空时从事务内查询当前成员。
// excludeUIDs 用于过滤已删除但事务外仍可见的成员。
// 返回 eventID（0 表示无需更新）和 error。
func beginAvatarUpdateEvent(ctx *config.Context, db *DB, groupNo string, memberUIDs []string, excludeUIDs []string, tx *dbr.Tx) (int64, error) {
	if ctx.Event == nil {
		return 0, nil
	}

	// 新建群不需要检查 is_upload_avatar
	if len(memberUIDs) == 0 {
		isUpload, err := db.queryGroupAvatarIsUpload(groupNo)
		if err != nil {
			return 0, nil
		}
		if isUpload == 1 {
			return 0, nil
		}

		members, err := db.QueryMembersFirstNineTx(groupNo, tx)
		if err != nil {
			return 0, nil
		}
		for _, m := range members {
			if !contains(excludeUIDs, m.UID) {
				memberUIDs = append(memberUIDs, m.UID)
			}
		}
	}

	if len(memberUIDs) == 0 {
		return 0, nil
	}

	eventID, err := ctx.EventBegin(&wkevent.Data{
		Event: event.GroupAvatarUpdate,
		Type:  wkevent.CMD,
		Data: &config.CMDGroupAvatarUpdateReq{
			GroupNo: groupNo,
			Members: memberUIDs,
		},
	}, tx)
	if err != nil {
		return 0, fmt.Errorf("begin group avatar update event: %w", err)
	}
	return eventID, nil
}

// removeUserFromGroupThreads 移除用户在某群下所有子区的成员记录、IM 订阅和置顶（直接 SQL）
func (s *Service) removeUserFromGroupThreads(groupNo, uid, spaceID string) {
	type threadInfo struct {
		ShortID string `db:"short_id"`
	}
	var threads []threadInfo
	_, err := s.ctx.DB().Select("thread.short_id").
		From("thread").
		Join("thread_member", "thread.id = thread_member.thread_id").
		Where("thread.group_no=? AND thread_member.uid=? AND thread.status!=3", groupNo, uid).
		Load(&threads)
	if err != nil {
		s.Error("query user threads failed", zap.Error(err), zap.String("groupNo", groupNo), zap.String("uid", uid))
		return
	}
	if len(threads) == 0 {
		return
	}

	// 删除成员记录
	_, err = s.ctx.DB().DeleteFrom("thread_member").
		Where("uid=? AND thread_id IN (SELECT id FROM thread WHERE group_no=?)", uid, groupNo).
		Exec()
	if err != nil {
		s.Error("delete thread member failed", zap.Error(err), zap.String("groupNo", groupNo), zap.String("uid", uid))
		return
	}

	// 移除 IM 订阅和置顶
	for _, t := range threads {
		channelID := groupNo + "____" + t.ShortID
		if rmErr := s.ctx.IMRemoveSubscriber(&config.SubscriberRemoveReq{
			ChannelID:   channelID,
			ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
			Subscribers: []string{uid},
		}); rmErr != nil {
			s.Error("remove thread IM subscriber failed", zap.Error(rmErr), zap.String("channelID", channelID), zap.String("uid", uid))
		}
		// 清理用户在该子区的置顶
		user.RemovePinnedForUserInSpace(uid, spaceID, channelID, common.ChannelTypeCommunityTopic.Uint8())
	}
}

// addUsersToGroupThreads 新成员入群时，将其加入该群所有子区的 IM 订阅（直接 SQL）
func (s *Service) addUsersToGroupThreads(groupNo string, uids []string) {
	if len(uids) == 0 {
		return
	}

	type threadInfo struct {
		ShortID string `db:"short_id"`
	}
	var threads []threadInfo
	_, err := s.ctx.DB().Select("short_id").
		From("thread").
		Where("group_no=? AND status!=3", groupNo).
		Load(&threads)
	if err != nil {
		s.Error("query group threads failed", zap.Error(err), zap.String("groupNo", groupNo))
		return
	}
	if len(threads) == 0 {
		return
	}

	for _, t := range threads {
		channelID := groupNo + "____" + t.ShortID
		if addErr := s.ctx.IMAddSubscriber(&config.SubscriberAddReq{
			ChannelID:   channelID,
			ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
			Subscribers: uids,
		}); addErr != nil {
			s.Error("add thread IM subscriber failed", zap.Error(addErr), zap.String("channelID", channelID), zap.Strings("uids", uids))
		}
	}
}

// notifyBotJoinedGroup 向 Bot 的事件队列推送 bot_joined_group 事件
func (s *Service) notifyBotJoinedGroup(memberUsers []*user.Model, addedUIDSet map[string]bool, groupNo, operator, operatorName string) {
	for _, memberUser := range memberUsers {
		if memberUser.Robot != 1 || !addedUIDSet[memberUser.UID] {
			continue
		}
		robotID := memberUser.UID
		seq, err := s.ctx.GenSeq(fmt.Sprintf("%s%s", common.RobotEventSeqKey, robotID))
		if err != nil {
			s.Error("generate bot event seq failed", zap.Error(err), zap.String("robotID", robotID))
			continue
		}
		eventData := map[string]interface{}{
			"event_id":   seq,
			"event_type": "bot_joined_group",
			"event_data": map[string]interface{}{
				"group_no":      groupNo,
				"operator":      operator,
				"operator_name": operatorName,
			},
		}
		key := fmt.Sprintf("robotEvent:%s", robotID)
		err = s.ctx.GetRedisConn().ZAdd(key, float64(seq), util.ToJson(eventData))
		if err != nil {
			s.Error("push bot_joined_group event failed", zap.Error(err), zap.String("robotID", robotID))
			continue
		}
		s.Info("pushed bot_joined_group event", zap.String("robotID", robotID), zap.String("groupNo", groupNo))
	}
}
