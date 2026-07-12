package message

import (
	"fmt"
	"hash/crc32"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// DB DB
type DB struct {
	session *dbr.Session
	ctx     *config.Context
}

// NewDB NewDB
func NewDB(ctx *config.Context) *DB {
	return &DB{
		session: ctx.DB(),
		ctx:     ctx,
	}
}

// queryMessageByID 按 (channelID, channelType, messageID) 在分表里查单条消息正文。
// 返回 nil 表示未命中（消息不存在或软删除）；err 仅用于真正的 DB 错误。
//
// messageID 必须以字符串形式传入：底层 message[N] 表的 message_id 列是
// VARCHAR(20) 且 UNIQUE 索引建在该列上。MySQL 在字符串列与数值参数比较时会做
// 隐式类型转换，导致索引无法使用，EXPLAIN 退化成 type=ALL 全表扫。
// 调用方在解析 path 后用 strconv.FormatInt 转一道。
func (d *DB) queryMessageByID(channelID string, channelType uint8, messageID string) (*messageModel, error) {
	var model *messageModel
	_, err := d.session.Select("*").From(d.getTable(channelID)).
		Where("channel_id=? AND channel_type=? AND message_id=? AND is_deleted=0", channelID, channelType, messageID).
		Load(&model)
	if err != nil {
		return nil, err
	}
	return model, nil
}

// queryMessagesByIDs 按 (channelID, channelType, message_id IN ...) 批量查同一频道下的
// 多条消息正文（is_deleted=0）。message_id 列为 VARCHAR(20)，必须以字符串切片绑定才能
// 命中索引（同 queryMessageByID 的隐式类型转换说明）。同一频道所有消息落同一分表
// (getTable 按 channelID 取模)，故一次查询即可。
func (d *DB) queryMessagesByIDs(channelID string, channelType uint8, messageIDs []string) ([]*messageModel, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	var models []*messageModel
	_, err := d.session.Select("*").From(d.getTable(channelID)).
		Where("channel_id=? AND channel_type=? AND message_id in ? AND is_deleted=0", channelID, channelType, messageIDs).
		Load(&models)
	if err != nil {
		return nil, err
	}
	return models, nil
}

func (d *DB) queryMaxMessageSeq(channelID string, channelType uint8) (uint32, error) {
	var maxMessageSeq uint32
	err := d.session.Select("IFNULL(max(message_seq),0)").From(d.getTable(channelID)).Where("channel_id=? and channel_type=?", channelID, channelType).LoadOne(&maxMessageSeq)
	return maxMessageSeq, err
}

func (d *DB) queryProhibitWordsWithVersion(version int64) ([]*ProhibitWordModel, error) {
	var list []*ProhibitWordModel
	_, err := d.session.Select("*").From("prohibit_words").Where("`version` > ?", version).Load(&list)
	return list, err
}

// 新增消息
func (d *DB) insertMessage(m *messageModel) error {
	_, err := d.session.InsertInto(d.getTable(m.ChannelID)).Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

// 通过频道ID获取表
func (d *DB) getTable(channelID string) string {
	count := d.ctx.GetConfig().TablePartitionConfig.MessageTableCount
	if count <= 0 {
		log.Warn("MessageTableCount is not configured or invalid, using default table", zap.Int("count", count))
		return "message"
	}
	tableIndex := crc32.ChecksumIEEE([]byte(channelID)) % uint32(count)
	if tableIndex == 0 {
		return "message"
	}
	return fmt.Sprintf("message%d", tableIndex)
}

// ProhibitWordModel 违禁词model
type ProhibitWordModel struct {
	Content   string
	IsDeleted int
	Version   int64
	db.BaseModel
}

// Model 消息model
type messageModel struct {
	MessageID   int64
	MessageSeq  uint32
	ClientMsgNo string
	Header      string
	Setting     uint8
	FromUID     string
	ChannelID   string
	ChannelType uint8
	Timestamp   int64
	// Type        int
	Payload   []byte
	IsDeleted int
	Signal    int
	Expire    uint32
	db.BaseModel
}
