package message

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/modules/channel"
	chservice "github.com/Mininglamp-OSS/octo-server/modules/channel/service"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
	"go.uber.org/zap"
)

// ReactionAction 决定 reaction 写入语义。
type ReactionAction uint8

const (
	// ReactionToggle 翻转 is_deleted —— 用户态 POST /v1/reactions 的历史语义（盲翻转，
	// 供 Web 乐观更新对账）。
	ReactionToggle ReactionAction = iota
	// ReactionAdd 幂等点亮（is_deleted=0）—— Bot 显式 add，重试安全。
	ReactionAdd
	// ReactionRemove 幂等取消（is_deleted=1）—— Bot 显式 remove，重试安全。
	ReactionRemove
)

// PersonSpaceContext 携带 DM(私聊) 的 Space 隔离上下文。仅当调用方经过 SpaceMiddleware
// 且 opt-in（spaceID 非空）时传入；nil 表示跳过隔离判定（Bot 态 / 非 Space 模式 /
// 老客户端兼容）。字段与用户态 addOrCancelReaction 的 DM 隔离块一一对应。
type PersonSpaceContext struct {
	SpaceID        string
	DefaultSpaceID string
	IsSystemBot    bool
}

// ReactionWriteReq 是一次 reaction 写入的全部输入。身份（UID/Name）由调用方解析后传入：
// 用户态传登录用户，Bot 态传 robotID + bot 展示名。MessageID 传原始串，WriteReaction
// 内部用 parsePositiveMessageID 归一，保证与 message / reaction_users 表的规范十进制串一致。
type ReactionWriteReq struct {
	ChannelID   string
	ChannelType uint8
	MessageID   string
	UID         string
	Name        string
	Emoji       string
	Action      ReactionAction
	PersonSpace *PersonSpaceContext
}

// ReactionWriteResult 是写入成功后的最终态，供调用方组装响应。
type ReactionWriteResult struct {
	MessageID   string
	ChannelID   string
	ChannelType uint8
	Emoji       string
	Seq         int64
	IsDeleted   int
}

// ReactionFailure 表示一次业务校验/写入失败，携带要下发的 errcode（+ request-invalid 的
// field 明细）。内部 DB/系统错误已在 WriteReaction 内经 zap 记录，调用方只需把 Code 映射
// 到 httperr.ResponseErrorL 即可 —— 使用户态与 Bot 态共用同一套错误语义（含防枚举归并）。
type ReactionFailure struct {
	Code  codes.Code
	Field string
}

func invalidReactionField(field string) *ReactionFailure {
	return &ReactionFailure{Code: errcode.ErrMessageRequestInvalid, Field: field}
}

func reactionFail(code codes.Code) *ReactionFailure {
	return &ReactionFailure{Code: code}
}

// ReactionService 是 reaction 写入的唯一权威：承载成员/可见性/纯文本/子区/解散/DM-Space
// 隔离全链路校验 + 写入 + 同步 CMD 派发。用户态 addOrCancelReaction 与 Bot API
// botMessageReaction 均委托它，避免安全校验分叉（对齐 #603 加固）。它持有 reaction 相关
// 依赖子集（与 message.New 同构造），故可被 bot_api 独立构造复用，而不必再造一个会重复
// 注册事件监听的 *Message。
type ReactionService struct {
	ctx *config.Context
	log.Log
	reactionDB         *messageReactionDB
	db                 *DB
	messageExtraDB     *messageExtraDB
	messageUserExtraDB *messageUserExtraDB
	channelOffsetDB    *channelOffsetDB
	channelService     chservice.IService
	groupService       group.IService
	userService        user.IService
	threadDB           *thread.DB
}

// NewReactionService 构造一个自足的 reaction 写入服务。
func NewReactionService(ctx *config.Context) *ReactionService {
	return &ReactionService{
		ctx:                ctx,
		Log:                log.NewTLog("message.ReactionService"),
		reactionDB:         newMessageReactionDB(ctx),
		db:                 NewDB(ctx),
		messageExtraDB:     newMessageExtraDB(ctx),
		messageUserExtraDB: newMessageUserExtraDB(ctx),
		channelOffsetDB:    newChannelOffsetDB(ctx),
		channelService:     channel.NewService(ctx),
		groupService:       group.NewService(ctx),
		userService:        user.NewService(ctx),
		threadDB:           thread.NewDB(ctx),
	}
}

// WriteReaction 执行一次校验后的 reaction 写入。校验顺序与语义与用户态历史实现严格一致：
// 成员/好友鉴权 → 子区未删 + 父群未解散 → 群未解散 → 目标消息存在 → 对调用者可见 →
// DM 跨 Space 隔离 → 纯文本类型门 → 生成 seq → 写入（toggle/add/remove）→ 派发同步 CMD。
// 可见性/DM-Space 不匹配一律归并到 404（防枚举，不泄露"存在但越权/类型不支持"信号）。
func (s *ReactionService) WriteReaction(req *ReactionWriteReq) (*ReactionWriteResult, *ReactionFailure) {
	channelID := strings.TrimSpace(req.ChannelID)
	emoji := strings.TrimSpace(req.Emoji)
	if channelID == "" {
		return nil, invalidReactionField("channel_id")
	}
	messageID, ok := parsePositiveMessageID(strings.TrimSpace(req.MessageID))
	if !ok {
		return nil, invalidReactionField("message_id")
	}
	// 规范化：parsePositiveMessageID 接受 "+910000"/"0910000" 等非规范整数写法，但
	// message_id 在 message / reaction_users 表里以规范十进制串存储。归一后所有后续查询
	// （queryMessageByID、set/toggleReaction、可见性 idList）用同一个规范串。
	normMessageID := strconv.FormatInt(messageID, 10)
	if emoji == "" {
		return nil, invalidReactionField("emoji")
	}
	// emoji 存 varchar(20)：按 rune 上限拦截，避免超长输入被 DB 静默截断/报错。
	if utf8.RuneCountInString(emoji) > maxReactionEmojiRunes {
		return nil, invalidReactionField("emoji")
	}

	fakeChannelID := channelID

	// 频道成员鉴权（对 UID —— 用户或 bot 同构）。
	switch req.ChannelType {
	case common.ChannelTypeGroup.Uint8():
		isMember, err := s.groupService.ExistMemberActive(channelID, req.UID)
		if err != nil {
			s.Error("查询群成员关系错误", zap.Error(err))
			return nil, reactionFail(errcode.ErrMessageQueryFailed)
		}
		if !isMember {
			return nil, reactionFail(errcode.ErrMessageChannelAccessDenied)
		}
	case common.ChannelTypePerson.Uint8():
		fakeChannelID = common.GetFakeChannelIDWith(channelID, req.UID)
		if channelID != req.UID {
			isFriend, err := s.userService.IsFriend(req.UID, channelID)
			if err != nil {
				s.Error("查询好友关系错误", zap.Error(err))
				return nil, reactionFail(errcode.ErrMessageQueryFailed)
			}
			if !isFriend {
				return nil, reactionFail(errcode.ErrMessageChannelAccessDenied)
			}
		}
	case common.ChannelTypeCommunityTopic.Uint8():
		parentGroupNo, shortID, perr := thread.ParseChannelID(channelID)
		if perr != nil || parentGroupNo == "" {
			s.Error("解析子区频道ID失败（reaction）", zap.Error(perr), zap.String("channelID", channelID))
			return nil, invalidReactionField("channel_id")
		}
		isMember, err := s.groupService.ExistMemberActive(parentGroupNo, req.UID)
		if err != nil {
			s.Error("查询父群成员关系错误", zap.Error(err), zap.String("groupNo", parentGroupNo))
			return nil, reactionFail(errcode.ErrMessageQueryFailed)
		}
		if !isMember {
			return nil, reactionFail(errcode.ErrMessageChannelAccessDenied)
		}
		// 已删除子区 fail-closed（与 getThreadMessage 一致）：thread 不存在/已删除一律
		// 404，防枚举。
		okThread, terr := reactionThreadNotDeleted(s.threadDB, parentGroupNo, shortID)
		if terr != nil {
			s.Error("查询子区状态失败（reaction）", zap.Error(terr), zap.String("channelID", channelID))
			return nil, reactionFail(errcode.ErrMessageQueryFailed)
		}
		if !okThread {
			return nil, reactionFail(errcode.ErrMessageNotFound)
		}
		// 父群解散守卫（企业微信式只读）：与下面 Group 分支同语义，收在这里避免再解析一次。
		disbanded, err := s.isGroupDisbanded(parentGroupNo)
		if err != nil {
			s.Error("查询父群是否已解散错误", zap.Error(err))
			return nil, reactionFail(errcode.ErrMessageQueryFailed)
		}
		if disbanded {
			return nil, reactionFail(errcode.ErrMessageGroupDisbanded)
		}
	default:
		return nil, invalidReactionField("channel_type")
	}

	// 解散守卫（企业微信式只读）：群解散后禁止添加/取消回应。CommunityTopic 的父群解散
	// 判定已在上面就地完成，这里只处理 Group。
	if req.ChannelType == common.ChannelTypeGroup.Uint8() {
		disbanded, err := s.isGroupDisbanded(channelID)
		if err != nil {
			s.Error("查询群是否已解散错误", zap.Error(err))
			return nil, reactionFail(errcode.ErrMessageQueryFailed)
		}
		if disbanded {
			return nil, reactionFail(errcode.ErrMessageGroupDisbanded)
		}
	}

	msgModel, err := s.db.queryMessageByID(fakeChannelID, req.ChannelType, normMessageID)
	if err != nil {
		s.Error("查询 reaction 目标消息失败", zap.Error(err), zap.String("channel_id", fakeChannelID), zap.String("message_id", normMessageID))
		return nil, reactionFail(errcode.ErrMessageQueryFailed)
	}
	if msgModel == nil {
		return nil, reactionFail(errcode.ErrMessageNotFound)
	}
	visible, err := s.reactionTargetVisibleToViewer(msgModel, messageID, req.UID)
	if err != nil {
		s.Error("校验 reaction 目标消息可见性失败", zap.Error(err), zap.String("channel_id", fakeChannelID), zap.String("message_id", normMessageID))
		return nil, reactionFail(errcode.ErrMessageQueryFailed)
	}
	if !visible {
		return nil, reactionFail(errcode.ErrMessageNotFound)
	}

	// Person(DM)Space 隔离：DM 是跨 Space 共享的同一物理频道，禁止给不属于当前已校验
	// Space 的 DM 消息点回应。不匹配按 404 归并（防枚举，与不可见同码）。放在类型门之前，
	// 避免"跨 Space 非文本"泄露类型信号。PersonSpace==nil（未 opt-in / Bot 态）时跳过。
	if req.ChannelType == common.ChannelTypePerson.Uint8() && req.PersonSpace != nil {
		if !personSpaceAllows(payloadSpaceIDFromRaw(msgModel.Payload), req.PersonSpace.IsSystemBot, req.PersonSpace.SpaceID, req.PersonSpace.DefaultSpaceID) {
			return nil, reactionFail(errcode.ErrMessageNotFound)
		}
	}

	// 消息类型门：当前仅纯文本消息（common.Text=1）允许 reaction。放在可见性校验之后，
	// 保证不可见/越权消息仍归并到 404，只有对调用者可见的非文本消息才返回 unsupported_type。
	if !payloadIsPlainText(msgModel.Payload) {
		return nil, reactionFail(errcode.ErrMessageReactionUnsupportedType)
	}

	seq, err := s.genMessageReactionSeq(fakeChannelID)
	if err != nil {
		s.Error("生成消息回应序列号失败", zap.Error(err))
		return nil, reactionFail(errcode.ErrMessageStoreFailed)
	}

	model := &reactionModel{
		ChannelID:   fakeChannelID,
		ChannelType: req.ChannelType,
		UID:         req.UID,
		Name:        req.Name,
		MessageID:   normMessageID,
		Emoji:       emoji,
		Seq:         seq,
	}
	var isDeleted int
	switch req.Action {
	case ReactionAdd:
		isDeleted, err = s.reactionDB.setReaction(model, 0)
	case ReactionRemove:
		isDeleted, err = s.reactionDB.setReaction(model, 1)
	default: // ReactionToggle
		isDeleted, err = s.reactionDB.toggleReaction(model)
	}
	if err != nil {
		s.Error("写入消息回应失败", zap.Error(err), zap.String("channel_id", fakeChannelID), zap.String("message_id", normMessageID))
		return nil, reactionFail(errcode.ErrMessageStoreFailed)
	}

	// 发送同步消息 cmd。param 带 message_id/emoji/seq，让 Web 精确定位消息 + emoji，并用
	// seq 做乱序丢弃；无 param 也可降级为频道级失效 + 拉 /v1/reaction/sync 增量。
	if err := s.ctx.SendCMD(config.MsgCMDReq{
		NoPersist:   true,
		ChannelID:   channelID,
		ChannelType: req.ChannelType,
		CMD:         common.CMDSyncMessageReaction,
		FromUID:     req.UID,
		Param: map[string]interface{}{
			"message_id":   normMessageID,
			"channel_id":   channelID,
			"channel_type": req.ChannelType,
			"emoji":        emoji,
			"seq":          seq,
		},
	}); err != nil {
		s.Error("发送同步命令失败", zap.Error(err))
		return nil, reactionFail(errcode.ErrMessageNotifyFailed)
	}

	return &ReactionWriteResult{
		MessageID:   normMessageID,
		ChannelID:   channelID,
		ChannelType: req.ChannelType,
		Emoji:       emoji,
		Seq:         seq,
		IsDeleted:   isDeleted,
	}, nil
}

// isGroupDisbanded 判定群是否已解散。ReactionService 自有一份（与 *Message.isGroupDisbanded
// 同实现），避免依赖 *Message 实例。
func (s *ReactionService) isGroupDisbanded(groupNo string) (bool, error) {
	info, err := s.groupService.GetGroupWithGroupNo(groupNo)
	if err != nil {
		return false, err
	}
	return info.Status == group.GroupStatusDisband, nil
}

func (s *ReactionService) genMessageReactionSeq(channelID string) (int64, error) {
	return s.ctx.GenSeq(fmt.Sprintf("%s:%s", common.MessageReactionSeqKey, channelID))
}

// reactionTargetVisibleToViewer 判定目标消息对 loginUID(用户或 bot) 是否可见。与
// *Message.reactionTargetVisibleToViewer 同规则（撤回/删除/清理偏移/过期/visibles 白名单），
// 复用包级纯谓词 messageVisibleToViewer，两处判定不分叉。
func (s *ReactionService) reactionTargetVisibleToViewer(msgModel *messageModel, messageID int64, loginUID string) (bool, error) {
	idList := []string{strconv.FormatInt(messageID, 10)}
	var extra *messageExtraDetailModel
	extras, err := s.messageExtraDB.queryWithMessageIDsAndUID(idList, loginUID)
	if err != nil {
		return false, err
	}
	if len(extras) > 0 {
		extra = extras[0]
	}
	var userExtra *messageUserExtraModel
	userExtras, err := s.messageUserExtraDB.queryWithMessageIDsAndUID(idList, loginUID)
	if err != nil {
		return false, err
	}
	if len(userExtras) > 0 {
		userExtra = userExtras[0]
	}
	var userOffsetSeq uint32
	userOffset, err := s.channelOffsetDB.queryWithUIDAndChannel(loginUID, msgModel.ChannelID, msgModel.ChannelType)
	if err != nil {
		return false, err
	}
	if userOffset != nil {
		userOffsetSeq = userOffset.MessageSeq
	}
	channelOffsetSeq, err := reactionChannelOffsetSeqWith(s.channelService, msgModel.ChannelID, msgModel.ChannelType, loginUID)
	if err != nil {
		return false, err
	}
	return messageVisibleToViewer(msgModel, extra, userExtra, userOffsetSeq, channelOffsetSeq, loginUID), nil
}

// reactionThreadNotDeleted 校验子区本身未被删除（成员/父群鉴权由调用方前置完成）。返回
// (ok, err)：err=DB 错误；ok=false 表示子区不存在或已删除（调用方按 404 处理，防枚举）。
// 与 *Message.threadNotDeleted 同实现，抽成自由函数供 ReactionService 复用而不依赖 *Message。
func reactionThreadNotDeleted(tdb *thread.DB, groupNo, shortID string) (bool, error) {
	t, err := tdb.QueryByGroupNoAndShortID(groupNo, shortID)
	if err != nil {
		return false, err
	}
	if t == nil || t.Status == thread.ThreadStatusDeleted {
		return false, nil
	}
	return true, nil
}

// reactionChannelOffsetSeqWith 取频道级清理偏移 seq（与 *Message.reactionChannelOffsetSeq
// 同实现），抽成自由函数供 ReactionService 的可见性判定复用。
func reactionChannelOffsetSeqWith(chSvc chservice.IService, channelID string, channelType uint8, loginUID string) (uint32, error) {
	lookupID := channelID
	if channelType == common.ChannelTypePerson.Uint8() && !fakeChannelContainsUID(channelID, loginUID) {
		lookupID = common.GetFakeChannelIDWith(channelID, loginUID)
	}
	settings, err := chSvc.GetChannelSettings([]string{lookupID})
	if err != nil {
		return 0, err
	}
	if len(settings) > 0 && settings[0].OffsetMessageSeq > 0 {
		return settings[0].OffsetMessageSeq, nil
	}
	return 0, nil
}
