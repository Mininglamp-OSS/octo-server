package thread

import (
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
	ShortID         string `json:"short_id"`
	GroupNo         string `json:"group_no"`
	Name            string `json:"name"`
	CreatorUID      string `json:"creator_uid"`
	SourceMessageID *int64 `json:"source_message_id"`
	Status          int    `json:"status"`
	Version         int64  `json:"version"`
	db.BaseModel
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

// QueryByGroupNo 查询群下的子区（默认限制 100 条）
func (d *DB) QueryByGroupNo(groupNo string) ([]*Model, error) {
	var models []*Model
	_, err := d.session.Select("*").From("thread").
		Where("group_no=? AND status=?", groupNo, ThreadStatusActive).
		OrderDir("created_at", false).
		Limit(100).
		Load(&models)
	return models, err
}

// QueryByGroupNoWithStatus 查询群下指定状态的子区（默认限制 100 条）
func (d *DB) QueryByGroupNoWithStatus(groupNo string, status int) ([]*Model, error) {
	var models []*Model
	_, err := d.session.Select("*").From("thread").
		Where("group_no=? AND status=?", groupNo, status).
		OrderDir("created_at", false).
		Limit(100).
		Load(&models)
	return models, err
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
