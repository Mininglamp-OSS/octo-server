package message

import (
	"context"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/Mininglamp-OSS/octo-server/pkg/readreceipt"
	"github.com/gocraft/dbr/v2"
)

type memberReadedDB struct {
	ctx     *config.Context
	session *dbr.Session
}

func newMemberReadedDB(ctx *config.Context) *memberReadedDB {
	return &memberReadedDB{
		ctx:     ctx,
		session: ctx.DB(),
	}
}

// batchInsertOrUpdate shares the final normalization/transaction boundary with Bot receipts.
func (m *memberReadedDB) batchInsertOrUpdate(ctx context.Context, models []*memberReadedModel) error {
	rows := make([]readreceipt.Row, 0, len(models))
	for _, model := range models {
		rows = append(rows, readreceipt.Row{
			MessageID: model.MessageID, ChannelID: model.ChannelID,
			ChannelType: model.ChannelType, UID: model.UID,
		})
	}
	return readreceipt.Write(ctx, m.session, rows)
}

// 查询消息已读数量
func (m *memberReadedDB) queryCountWithMessageIDs(channelID string, channelType uint8, messageIDs []string) (map[int64]int, error) {
	if len(messageIDs) <= 0 {
		return nil, nil
	}
	var respCountModels []struct {
		MessageID int64
		Num       int
	}
	_, err := m.session.Select("member_readed.message_id,count(*) num").From("member_readed").Where("member_readed.channel_id=? and member_readed.channel_type=? and member_readed.message_id in ?", channelID, channelType, messageIDs).GroupBy("member_readed.message_id", "member_readed.channel_id", "member_readed.channel_type").Load(&respCountModels)
	if err != nil {
		return nil, err
	}
	resultMap := map[int64]int{}
	if len(respCountModels) > 0 {
		for _, respCountModel := range respCountModels {
			resultMap[respCountModel.MessageID] = respCountModel.Num
		}
	}
	return resultMap, nil
}

// 查询已读列表
func (m *memberReadedDB) queryWithMessageIDAndPage(messageID string, pIndex, pSize uint64) ([]*memberReadedModel, error) {
	var models []*memberReadedModel
	_, err := m.session.Select("*").From("member_readed").Where("member_readed.message_id=?", messageID).OrderDesc("created_at").Limit(pSize).Offset((pIndex - 1) * pSize).Load(&models)
	return models, err
}

type memberReadedModel struct {
	CloneNo     string // TODO: 此字段作废
	MessageID   int64
	ChannelID   string
	ChannelType uint8
	UID         string
	db.BaseModel
}
