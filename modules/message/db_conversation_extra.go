package message

import (
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/gocraft/dbr/v2"
)

type conversationExtraDB struct {
	ctx     *config.Context
	session *dbr.Session
}

func newConversationExtraDB(ctx *config.Context) *conversationExtraDB {

	return &conversationExtraDB{
		ctx:     ctx,
		session: ctx.DB(),
	}
}

// insertOrUpdate writes the original conversation extension fields in one
// upsert statement. manual_unread is deliberately omitted from both the
// insert values and duplicate-key assignments: a new row gets the column's
// default value, while an existing marker is preserved atomically.
func (c *conversationExtraDB) insertOrUpdate(model *conversationExtraModel) (bool, error) {
	result, err := c.session.InsertBySql(`
INSERT INTO conversation_extra
    (uid,channel_id,channel_type,browse_to,keep_message_seq,keep_offset_y,draft,version)
VALUES (?,?,?,?,?,?,?,?)
ON DUPLICATE KEY UPDATE
    browse_to=IF(VALUES(version)>version AND VALUES(browse_to)>browse_to,VALUES(browse_to),browse_to),
    keep_message_seq=IF(VALUES(version)>version,VALUES(keep_message_seq),keep_message_seq),
    keep_offset_y=IF(VALUES(version)>version,VALUES(keep_offset_y),keep_offset_y),
    draft=IF(VALUES(version)>version,VALUES(draft),draft),
    version=GREATEST(version,VALUES(version))`,
		model.UID,
		model.ChannelID,
		model.ChannelType,
		model.BrowseTo,
		model.KeepMessageSeq,
		model.KeepOffsetY,
		model.Draft,
		model.Version,
	).Exec()
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

func (c *conversationExtraDB) maxVersion(uid string) (int64, error) {
	var version int64
	err := c.session.SelectBySql(
		"SELECT COALESCE(MAX(version),0) FROM conversation_extra WHERE uid=?",
		uid,
	).LoadOne(&version)
	return version, err
}

func (c *conversationExtraDB) sync(uid string, version int64) ([]*conversationExtraModel, error) {
	var models []*conversationExtraModel
	_, err := c.session.Select("*").From("conversation_extra").Where("uid=? and version>?", uid, version).Load(&models)
	return models, err
}

func (c *conversationExtraDB) queryWithChannelIDs(uid string, channelIDs []string) ([]*conversationExtraModel, error) {
	if len(channelIDs) == 0 {
		return nil, nil
	}
	var models []*conversationExtraModel
	_, err := c.session.Select("*").From("conversation_extra").Where("uid=? and channel_id in ?", uid, channelIDs).Load(&models)
	return models, err
}

func (c *conversationExtraDB) queryManualUnread(uid string, groupChannelIDs, topicChannelIDs []string) ([]*conversationExtraModel, error) {
	conditions := make([]dbr.Builder, 0, 2)
	if len(groupChannelIDs) > 0 {
		conditions = append(conditions, dbr.And(
			dbr.Eq("channel_type", common.ChannelTypeGroup.Uint8()),
			dbr.Eq("channel_id", groupChannelIDs),
		))
	}
	if len(topicChannelIDs) > 0 {
		conditions = append(conditions, dbr.And(
			dbr.Eq("channel_type", common.ChannelTypeCommunityTopic.Uint8()),
			dbr.Eq("channel_id", topicChannelIDs),
		))
	}
	if len(conditions) == 0 {
		return nil, nil
	}

	var models []*conversationExtraModel
	_, err := c.session.Select("channel_id", "channel_type").From("conversation_extra").
		Where(dbr.And(
			dbr.Eq("uid", uid),
			dbr.Eq("manual_unread", 1),
			dbr.Or(conditions...),
		)).
		Load(&models)
	return models, err
}

func (c *conversationExtraDB) queryOne(uid string, channelID string, channelType uint8) (*conversationExtraModel, error) {
	var models []*conversationExtraModel
	_, err := c.session.Select("*").From("conversation_extra").Where("uid=? and channel_id=? and channel_type=?", uid, channelID, channelType).Load(&models)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, nil
	}
	return models[0], nil
}

// setManualUnread changes only the manual-unread state and its sync version.
// It intentionally does not use insertOrUpdate, because that method also
// writes draft/read-position fields and would overwrite them with zero values
// when called by the dedicated manual-unread endpoint.
func (c *conversationExtraDB) setManualUnread(uid string, channelID string, channelType uint8, version int64) (bool, error) {
	result, err := c.session.InsertBySql(`
INSERT INTO conversation_extra (uid,channel_id,channel_type,version,manual_unread)
VALUES (?,?,?,?,1)
ON DUPLICATE KEY UPDATE
    manual_unread=IF(VALUES(version)>version,VALUES(manual_unread),manual_unread),
    version=GREATEST(version,VALUES(version))`,
		uid,
		channelID,
		channelType,
		version,
	).Exec()
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

// clearManualUnread changes only the manual-unread state and its sync version.
// The caller must verify that the row is currently marked unread before
// generating the version and invoking this method.
func (c *conversationExtraDB) clearManualUnread(uid string, channelID string, channelType uint8, version int64) (bool, error) {
	result, err := c.session.Update("conversation_extra").
		Set("manual_unread", false).
		Set("version", version).
		Where("uid=? and channel_id=? and channel_type=? and manual_unread=1 and version<?", uid, channelID, channelType, version).
		Exec()
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

type conversationExtraModel struct {
	UID            string
	ChannelID      string
	ChannelType    uint8
	BrowseTo       uint32
	KeepMessageSeq uint32
	KeepOffsetY    int
	Draft          string // 草稿
	Version        int64
	db.BaseModel
	ManualUnread bool
}
