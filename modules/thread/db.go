package thread

import (
	"errors"
	"fmt"
	"hash/crc32"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/gocraft/dbr/v2"
)

// DB 数据库操作
type DB struct {
	ctx     *config.Context
	session *dbr.Session
}

// NewDB 创建数据库操作实例
func NewDB(ctx *config.Context) *DB {
	return &DB{
		ctx:     ctx,
		session: ctx.DB(),
	}
}

// Model 子区数据模型
type Model struct {
	ShortID              string     `json:"short_id"`
	GroupNo              string     `json:"group_no"`
	Name                 string     `json:"name"`
	CreatorUID           string     `json:"creator_uid"`
	SourceMessageID      *int64     `json:"source_message_id"`
	Status               int        `json:"status"`
	Version              int64      `json:"version"`
	MessageCount         int64      `json:"message_count"`
	LastMessageAt        *time.Time `json:"last_message_at"`
	LastMessageContent   string     `json:"last_message_content"`
	LastMessageSenderUID string     `json:"last_message_sender_uid"`
	// GROUP.md 相关字段
	ThreadMd          *string    `json:"thread_md"`
	ThreadMdVersion   int64      `json:"thread_md_version"`
	ThreadMdUpdatedAt *time.Time `json:"thread_md_updated_at"`
	ThreadMdUpdatedBy string     `json:"thread_md_updated_by"`
	db.BaseModel
}

// ThreadMdResult 子区 GROUP.md 查询结果
type ThreadMdResult struct {
	Content   string     `json:"content"`
	Version   int64      `json:"version"`
	UpdatedAt *time.Time `json:"updated_at"`
	UpdatedBy string     `json:"updated_by"`
}

// Insert 插入子区
func (d *DB) Insert(m *Model) error {
	_, err := d.session.InsertInto("thread").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

// InsertTx 事务插入子区
func (d *DB) InsertTx(m *Model, tx *dbr.Tx) error {
	_, err := tx.InsertInto("thread").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

// InsertTxReturningID 事务插入子区并返回 ID
func (d *DB) InsertTxReturningID(m *Model, tx *dbr.Tx) (int64, error) {
	result, err := tx.InsertInto("thread").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// QueryByShortID 根据 shortID 查询子区
func (d *DB) QueryByShortID(shortID string) (*Model, error) {
	var model *Model
	_, err := d.session.Select("*").From("thread").Where("short_id=?", shortID).Load(&model)
	return model, err
}

// QueryByGroupNoAndShortID 根据群编号和 shortID 查询子区
func (d *DB) QueryByGroupNoAndShortID(groupNo, shortID string) (*Model, error) {
	var model *Model
	_, err := d.session.Select("*").From("thread").Where("group_no=? AND short_id=?", groupNo, shortID).Load(&model)
	return model, err
}

// QueryByGroupNo 分页查询群下的活跃子区
func (d *DB) QueryByGroupNo(groupNo string, offset, limit int64) ([]*Model, error) {
	if limit <= 0 {
		return []*Model{}, nil
	}
	if offset < 0 {
		offset = 0
	}
	var models []*Model
	_, err := d.session.Select("*").From("thread").
		Where("group_no=? AND status=?", groupNo, ThreadStatusActive).
		OrderBy("created_at DESC, id DESC").
		Offset(uint64(offset)).
		Limit(uint64(limit)).
		Load(&models)
	return models, err
}

// CountByGroupNo 统计群下活跃子区总数
func (d *DB) CountByGroupNo(groupNo string) (int64, error) {
	var count int64
	err := d.session.Select("count(*)").From("thread").
		Where("group_no=? AND status=?", groupNo, ThreadStatusActive).
		LoadOne(&count)
	return count, err
}

// ThreadMetaRow 子区元数据（用于会话列表批量查询）
type ThreadMetaRow struct {
	ShortID         string `json:"short_id"`
	SourceMessageID *int64 `json:"source_message_id"`
	MessageCount    int64  `json:"message_count"`
}

// QueryThreadMetaByShortIDs 批量查询子区元数据（source_message_id, message_count）
func (d *DB) QueryThreadMetaByShortIDs(shortIDs []string) (map[string]*ThreadMetaRow, error) {
	result := make(map[string]*ThreadMetaRow)
	if len(shortIDs) == 0 {
		return result, nil
	}
	var rows []*ThreadMetaRow
	_, err := d.session.Select("short_id", "source_message_id", "message_count").From("thread").
		Where("short_id IN ? AND status != ?", shortIDs, ThreadStatusDeleted).
		Load(&rows)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.ShortID] = row
	}
	return result, nil
}

// QueryNonDeletedShortIDs 批量查询未删除的子区 shortID
func (d *DB) QueryNonDeletedShortIDs(shortIDs []string) ([]string, error) {
	if len(shortIDs) == 0 {
		return []string{}, nil
	}
	var result []string
	_, err := d.session.Select("short_id").From("thread").
		Where("short_id IN ? AND status != ?", shortIDs, ThreadStatusDeleted).
		Load(&result)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// QuerySourceMessageIDsByShortIDs 批量查询子区的 source_message_id
// 返回 map[shortID]*int64，nil 值表示无源消息
func (d *DB) QuerySourceMessageIDsByShortIDs(shortIDs []string) (map[string]*int64, error) {
	meta, err := d.QueryThreadMetaByShortIDs(shortIDs)
	if err != nil {
		return nil, err
	}
	result := make(map[string]*int64, len(meta))
	for shortID, row := range meta {
		result[shortID] = row.SourceMessageID
	}
	return result, nil
}


// UpdateStatus 更新子区状态
func (d *DB) UpdateStatus(shortID string, status int, version int64) error {
	_, err := d.session.Update("thread").SetMap(map[string]interface{}{
		"status":     status,
		"version":    version,
		"updated_at": time.Now(),
	}).Where("short_id=?", shortID).Exec()
	return err
}

// UpdateName 更新子区名称
func (d *DB) UpdateName(shortID string, name string, version int64) error {
	_, err := d.session.Update("thread").SetMap(map[string]interface{}{
		"name":       name,
		"version":    version,
		"updated_at": time.Now(),
	}).Where("short_id=?", shortID).Exec()
	return err
}

// Update 更新子区信息
func (d *DB) Update(m *Model) error {
	_, err := d.session.Update("thread").SetMap(map[string]interface{}{
		"name":       m.Name,
		"status":     m.Status,
		"version":    m.Version,
		"updated_at": time.Now(),
	}).Where("short_id=?", m.ShortID).Exec()
	return err
}

// ExistByShortID 检查子区是否存在
func (d *DB) ExistByShortID(shortID string) (bool, error) {
	var count int
	_, err := d.session.Select("count(*)").From("thread").
		Where("short_id=? AND status!=?", shortID, ThreadStatusDeleted).
		Load(&count)
	return count > 0, err
}

// ExistByGroupNoAndShortID 检查群下的子区是否存在
func (d *DB) ExistByGroupNoAndShortID(groupNo, shortID string) (bool, error) {
	var count int
	_, err := d.session.Select("count(*)").From("thread").
		Where("group_no=? AND short_id=? AND status!=?", groupNo, shortID, ThreadStatusDeleted).
		Load(&count)
	return count > 0, err
}

// QueryByID 根据 ID 查询子区
func (d *DB) QueryByID(id int64) (*Model, error) {
	var model *Model
	_, err := d.session.Select("*").From("thread").Where("id=?", id).Load(&model)
	return model, err
}

// QueryThreadsByGroupNoAndUID 查询用户在某群下加入的所有子区
func (d *DB) QueryThreadsByGroupNoAndUID(groupNo, uid string) ([]*Model, error) {
	var models []*Model
	_, err := d.session.Select("t.*").
		From(dbr.I("thread").As("t")).
		Join(dbr.I("thread_member").As("tm"), "t.id = tm.thread_id").
		Where("t.group_no=? AND tm.uid=? AND t.status!=?", groupNo, uid, ThreadStatusDeleted).
		Load(&models)
	return models, err
}

// DeleteMembersByGroupNoAndUIDTx 事务中删除用户在某群下所有子区的成员记录
func (d *DB) DeleteMembersByGroupNoAndUIDTx(groupNo, uid string, tx *dbr.Tx) error {
	_, err := tx.DeleteFrom("thread_member").
		Where("uid=? AND thread_id IN (SELECT id FROM thread WHERE group_no=?)", uid, groupNo).
		Exec()
	return err
}

// DeleteSettingsByGroupNoAndUIDTx 事务中删除用户在某群下所有子区的个人设置
func (d *DB) DeleteSettingsByGroupNoAndUIDTx(groupNo, uid string, tx *dbr.Tx) error {
	_, err := tx.DeleteFrom("thread_setting").
		Where("group_no=? AND uid=?", groupNo, uid).Exec()
	return err
}

// MemberModel 子区成员数据模型
type MemberModel struct {
	ID        int64  `json:"id"`
	ThreadID  int64  `json:"thread_id"`
	UID       string `json:"uid"`
	Role      int    `json:"role"` // 0=普通成员, 1=创建者
	Version   int64  `json:"version"`
	db.BaseModel
}

// InsertMember 添加子区成员
func (d *DB) InsertMember(m *MemberModel) error {
	_, err := d.session.InsertInto("thread_member").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

// InsertMemberTx 事务添加子区成员
func (d *DB) InsertMemberTx(m *MemberModel, tx *dbr.Tx) error {
	_, err := tx.InsertInto("thread_member").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

// DeleteMember 删除子区成员
func (d *DB) DeleteMember(threadID int64, uid string) error {
	_, err := d.session.DeleteFrom("thread_member").Where("thread_id=? AND uid=?", threadID, uid).Exec()
	return err
}

// QueryMembers 查询子区成员
func (d *DB) QueryMembers(threadID int64) ([]*MemberModel, error) {
	var models []*MemberModel
	_, err := d.session.Select("*").From("thread_member").
		Where("thread_id=?", threadID).
		OrderDir("created_at", true).
		Load(&models)
	return models, err
}

// QueryMemberUIDs 查询子区成员 UID 列表
func (d *DB) QueryMemberUIDs(threadID int64) ([]string, error) {
	var uids []string
	_, err := d.session.Select("uid").From("thread_member").
		Where("thread_id=?", threadID).
		Load(&uids)
	return uids, err
}

// ExistMember 检查是否是子区成员
func (d *DB) ExistMember(threadID int64, uid string) (bool, error) {
	var count int
	_, err := d.session.Select("count(*)").From("thread_member").
		Where("thread_id=? AND uid=?", threadID, uid).
		Load(&count)
	return count > 0, err
}

// QueryThreadIDByShortID 根据 shortID 查询子区 ID
func (d *DB) QueryThreadIDByShortID(shortID string) (int64, error) {
	var id int64
	_, err := d.session.Select("id").From("thread").Where("short_id=?", shortID).Load(&id)
	return id, err
}

// CountMembers 统计子区成员数量
func (d *DB) CountMembers(threadID int64) (int, error) {
	var count int
	_, err := d.session.Select("count(*)").From("thread_member").
		Where("thread_id=?", threadID).
		Load(&count)
	return count, err
}

// MemberCountResult 成员数量结果
type MemberCountResult struct {
	ThreadID int64 `db:"thread_id"`
	Count    int   `db:"count"`
}

// CountMembersBatch 批量统计子区成员数量
func (d *DB) CountMembersBatch(threadIDs []int64) (map[int64]int, error) {
	if len(threadIDs) == 0 {
		return make(map[int64]int), nil
	}

	var results []MemberCountResult
	_, err := d.session.Select("thread_id", "count(*) as count").
		From("thread_member").
		Where("thread_id IN ?", threadIDs).
		GroupBy("thread_id").
		Load(&results)
	if err != nil {
		return nil, err
	}

	countMap := make(map[int64]int, len(results))
	for _, r := range results {
		countMap[r.ThreadID] = r.Count
	}
	return countMap, nil
}

// UpdateMessageStats 原子更新消息统计（收到消息时调用）
func (d *DB) UpdateMessageStats(shortID string, content string, senderUID string) error {
	_, err := d.session.Update("thread").SetMap(map[string]interface{}{
		"message_count":          dbr.Expr("message_count + 1"),
		"last_message_at":        time.Now(),
		"last_message_content":   content,
		"last_message_sender_uid": senderUID,
	}).Where("short_id=?", shortID).Exec()
	return err
}

// QueryThreadMd 查询子区 GROUP.md 内容
func (d *DB) QueryThreadMd(groupNo, shortID string) (*ThreadMdResult, error) {
	var result *ThreadMdResult
	_, err := d.session.Select(
		"IFNULL(thread_md,'') as content",
		"thread_md_version as version",
		"thread_md_updated_at as updated_at",
		"thread_md_updated_by as updated_by",
	).From("thread").
		Where("group_no=? AND short_id=? AND status!=?", groupNo, shortID, ThreadStatusDeleted).
		Load(&result)
	return result, err
}

// UpdateThreadMd 更新子区 GROUP.md 内容，返回新版本号
func (d *DB) UpdateThreadMd(groupNo, shortID, content, updatedBy string) (int64, error) {
	tx, err := d.session.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.RollbackUnlessCommitted()

	result, err := tx.UpdateBySql(
		"UPDATE `thread` SET thread_md=?, thread_md_version=LAST_INSERT_ID(thread_md_version+1), thread_md_updated_at=NOW(), thread_md_updated_by=? WHERE group_no=? AND short_id=? AND status!=?",
		content, updatedBy, groupNo, shortID, ThreadStatusDeleted,
	).Exec()
	if err != nil {
		return 0, err
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		return 0, errors.New("thread not found or already deleted")
	}

	var newVersion int64
	_, err = tx.SelectBySql("SELECT LAST_INSERT_ID()").Load(&newVersion)
	if err != nil {
		return 0, err
	}
	return newVersion, tx.Commit()
}

// DeleteThreadMd 删除子区 GROUP.md 内容，保留删除者 UID，返回新版本号
func (d *DB) DeleteThreadMd(groupNo, shortID, deletedBy string) (int64, error) {
	tx, err := d.session.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.RollbackUnlessCommitted()

	result, err := tx.UpdateBySql(
		"UPDATE `thread` SET thread_md=NULL, thread_md_version=LAST_INSERT_ID(thread_md_version+1), thread_md_updated_at=NOW(), thread_md_updated_by=? WHERE group_no=? AND short_id=? AND status!=?",
		deletedBy, groupNo, shortID, ThreadStatusDeleted,
	).Exec()
	if err != nil {
		return 0, err
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		return 0, errors.New("thread not found or already deleted")
	}

	var newVersion int64
	_, err = tx.SelectBySql("SELECT LAST_INSERT_ID()").Load(&newVersion)
	if err != nil {
		return 0, err
	}
	return newVersion, tx.Commit()
}

// QueryMessageFromUID 根据 channelID 和 messageID 查询消息发送者
func (d *DB) QueryMessageFromUID(channelID string, messageID int64) (string, error) {
	table := d.getMessageTable(channelID)
	var fromUID string
	_, err := d.session.Select("from_uid").From(table).
		Where("message_id=? AND channel_id=?", messageID, channelID).
		Load(&fromUID)
	return fromUID, err
}

// SettingModel 子区用户设置
type SettingModel struct {
	GroupNo string
	ShortID string
	UID     string
	Mute    int
	Version int64
	db.BaseModel
}

// QuerySetting 按 (groupNo, shortID, uid) 查询单条设置
func (d *DB) QuerySetting(groupNo, shortID, uid string) (*SettingModel, error) {
	var m *SettingModel
	_, err := d.session.Select("*").From("thread_setting").
		Where("group_no=? AND short_id=? AND uid=?", groupNo, shortID, uid).Load(&m)
	return m, err
}

// QuerySettingsWithUIDs 批量查询一批用户对某子区的设置
func (d *DB) QuerySettingsWithUIDs(groupNo, shortID string, uids []string) ([]*SettingModel, error) {
	if len(uids) == 0 {
		return []*SettingModel{}, nil
	}
	var settings []*SettingModel
	_, err := d.session.Select("*").From("thread_setting").
		Where("group_no=? AND short_id=? AND uid IN ?", groupNo, shortID, uids).Load(&settings)
	return settings, err
}

// UpsertSetting 按 (group_no, short_id, uid) 幂等写入,避免并发 read-then-write 竞态
func (d *DB) UpsertSetting(m *SettingModel) error {
	_, err := d.session.InsertBySql(
		"INSERT INTO thread_setting (group_no, short_id, uid, mute, version) VALUES (?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE mute=VALUES(mute), version=VALUES(version)",
		m.GroupNo, m.ShortID, m.UID, m.Mute, m.Version,
	).Exec()
	return err
}

func (d *DB) getMessageTable(channelID string) string {
	tableCount := d.ctx.GetConfig().TablePartitionConfig.MessageTableCount
	if tableCount <= 0 {
		return "message"
	}
	tableIndex := crc32.ChecksumIEEE([]byte(channelID)) % uint32(tableCount)
	if tableIndex == 0 {
		return "message"
	}
	return fmt.Sprintf("message%d", tableIndex)
}
