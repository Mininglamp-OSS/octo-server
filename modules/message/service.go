package message

import (
	"strconv"

	"github.com/TangSengDaoDao/TangSengDaoDaoServerLib/common"
	"github.com/TangSengDaoDao/TangSengDaoDaoServerLib/config"
	"github.com/TangSengDaoDao/TangSengDaoDaoServerLib/pkg/log"
)

type IService interface {
	// 查询消息拥有者uid的已删除消息
	GetDeletedMessagesWithUID(uid string, messageIDs []string) ([]*messageUserExtraResp, error)
	// 查询消息的撤回消息
	GetRevokedMessages(messageIDs []string) ([]*messageExtraResp, error)
	// 查询消息的删除消息
	GetDeletedMessages(messageIDs []string) ([]*messageExtraResp, error)
	// 查询用户清空channel消息标记
	GetChannelOffsetWithUID(uid string, channelIDs []string) ([]*channelOffsetResp, error)
	// 删除会话
	DeleteConversation(uid string, channelID string, channelType uint8) error
}

type Service struct {
	ctx *config.Context
	log.Log
	messageExtraDB     *messageExtraDB
	messageUserExtraDB *messageUserExtraDB
	channelOffsetDB    *channelOffsetDB
}

func NewService(ctx *config.Context) *Service {

	return &Service{
		ctx:                ctx,
		Log:                log.NewTLog("message.Service"),
		messageExtraDB:     newMessageExtraDB(ctx),
		messageUserExtraDB: newMessageUserExtraDB(ctx),
		channelOffsetDB:    newChannelOffsetDB(ctx),
	}
}

func (s *Service) GetChannelOffsetWithUID(uid string, channelIDs []string) ([]*channelOffsetResp, error) {
	if len(channelIDs) == 0 {
		return nil, nil
	}
	models, err := s.channelOffsetDB.queryWithUIDAndChannelIDs(uid, channelIDs)
	if err != nil {
		return nil, err
	}
	resps := make([]*channelOffsetResp, 0, len(models))
	for _, model := range models {
		resps = append(resps, &channelOffsetResp{
			ChannelID:   model.ChannelID,
			ChannelType: model.ChannelType,
			MessageSeq:  model.MessageSeq,
		})
	}
	return resps, nil
}

func (s *Service) GetDeletedMessagesWithUID(uid string, messageIDs []string) ([]*messageUserExtraResp, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	models, err := s.messageUserExtraDB.queryDeletedWithMessageIDsAndUID(messageIDs, uid)
	if err != nil {
		return nil, err
	}
	resps := make([]*messageUserExtraResp, 0, len(models))
	for _, model := range models {
		messageID, _ := strconv.ParseInt(model.MessageID, 10, 64)
		resps = append(resps, &messageUserExtraResp{
			MessageID:        messageID,
			MessageIDStr:     model.MessageID,
			ChannelID:        model.ChannelID,
			ChannelType:      model.ChannelType,
			MessageSeq:       model.MessageSeq,
			MessageIsDeleted: model.MessageIsDeleted,
			VoiceReaded:      model.VoiceReaded,
		})
	}
	return resps, nil
}

func newMsgExtraResp(m *messageExtraModel) *messageExtraResp {
	messageID, _ := strconv.ParseInt(m.MessageID, 10, 64)
	return &messageExtraResp{
		MessageID:       messageID,
		MessageIDStr:    m.MessageID,
		Revoke:          m.Revoke,
		Revoker:         m.Revoker,
		IsMutualDeleted: m.IsDeleted,
	}
}
func (s *Service) GetRevokedMessages(messageIDs []string) ([]*messageExtraResp, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	models, err := s.messageExtraDB.queryRevokedWithMessageIDs(messageIDs)
	if err != nil {
		return nil, err
	}
	resps := make([]*messageExtraResp, 0, len(models))
	for _, model := range models {
		resps = append(resps, newMsgExtraResp(model))
	}
	return resps, nil
}

func (s *Service) GetDeletedMessages(messageIDs []string) ([]*messageExtraResp, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	models, err := s.messageExtraDB.queryDeletedWithMessageIDs(messageIDs)
	if err != nil {
		return nil, err
	}
	resps := make([]*messageExtraResp, 0, len(models))
	for _, model := range models {
		resps = append(resps, newMsgExtraResp(model))
	}
	return resps, nil
}

func (s *Service) DeleteConversation(uid string, channelID string, channelType uint8) error {
	err := s.ctx.IMDeleteConversation(config.DeleteConversationReq{
		ChannelID:   channelID,
		ChannelType: uint8(channelType),
		UID:         uid,
	})
	if err != nil {
		return err
	}
	err = s.ctx.SendCMD(config.MsgCMDReq{
		ChannelID:   uid,
		ChannelType: common.ChannelTypePerson.Uint8(),
		CMD:         common.CMDConversationDeleted,
		Param: map[string]interface{}{
			"channel_id":   channelID,
			"channel_type": channelType,
		},
	})
	if err != nil {
		return err
	}

	return nil
}

type messageUserExtraResp struct {
	MessageID        int64  `json:"message_id"`
	MessageIDStr     string `json:"message_id_str"`
	ChannelID        string `json:"channel_id"`
	ChannelType      uint8  `json:"channel_type"`
	MessageSeq       uint32 `json:"message_seq"`
	MessageIsDeleted int    `json:"message_is_deleted,omitempty"`
	VoiceReaded      int    `json:"voice_readed,omitempty"`
}
type channelOffsetResp struct {
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
	MessageSeq  uint32 `json:"message_seq"`
}
