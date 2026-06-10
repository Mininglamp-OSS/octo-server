package thread

import (
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
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

// QueryByGroupNo 分页查询群下的活跃子区。
// 为兼容旧调用方保留 active-only 语义；如需按其他状态查询请用 QueryByGroupNoWithStatus。
func (d *DB) QueryByGroupNo(groupNo string, offset, limit int64) ([]*Model, error) {
	return d.QueryByGroupNoWithStatus(groupNo, []int{ThreadStatusActive}, offset, limit)
}

// QueryByGroupNoWithStatus 分页查询群下指定 status 集合的子区。
// statuses 为空时返回空列表，避免误拉所有数据（含 deleted）。
func (d *DB) QueryByGroupNoWithStatus(groupNo string, statuses []int, offset, limit int64) ([]*Model, error) {
	if limit <= 0 || len(statuses) == 0 {
		return []*Model{}, nil
	}
	if offset < 0 {
		offset = 0
	}
	var models []*Model
	_, err := d.session.Select("*").From("thread").
		Where("group_no=? AND status IN ?", groupNo, statuses).
		OrderBy("created_at DESC, id DESC").
		Offset(uint64(offset)).
		Limit(uint64(limit)).
		Load(&models)
	return models, err
}

// CountByGroupNo 统计群下活跃子区总数。
func (d *DB) CountByGroupNo(groupNo string) (int64, error) {
	return d.CountByGroupNoWithStatus(groupNo, []int{ThreadStatusActive})
}

// CountByGroupNoWithStatus 统计群下指定 status 集合的子区总数。
// statuses 为空时返回 0，与 QueryByGroupNoWithStatus 对齐。
func (d *DB) CountByGroupNoWithStatus(groupNo string, statuses []int) (int64, error) {
	if len(statuses) == 0 {
		return 0, nil
	}
	var count int64
	err := d.session.Select("count(*)").From("thread").
		Where("group_no=? AND status IN ?", groupNo, statuses).
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

// QueryByShortIDs 批量查询子区，返回 map[shortID]*Model。
// 用于 sidebar 聚合接口补齐子区的 last_message_at。
//
// Deprecated: 仅按 short_id 不带 group_no/status 过滤会跨群命中且包含
// deleted 行。新代码应使用 QueryActiveByGroupShortIDs（PR review Round-3
// Blocking #3 / Important #4）。残留调用方清零后再行删除。
func (d *DB) QueryByShortIDs(shortIDs []string) (map[string]*Model, error) {
	result := make(map[string]*Model)
	if len(shortIDs) == 0 {
		return result, nil
	}
	var models []*Model
	_, err := d.session.Select("*").From("thread").
		Where("short_id IN ?", shortIDs).
		Load(&models)
	if err != nil {
		return nil, err
	}
	for _, m := range models {
		result[m.ShortID] = m
	}
	return result, nil
}

// ShortRef 表示 (group_no, short_id) 二元组的轻量引用。
// 供 QueryActiveByGroupShortIDs 与 auth checker 使用。
type ShortRef struct {
	GroupNo string
	ShortID string
}

// ThreadLite 只保留 sidebar 聚合 / auth check 必需的最小字段集。
// 不读 thread.Model 的全部列（包括 thread_md 等大文本字段），以最小化 I/O
// （PR review Round-3 Important #4）。
type ThreadLite struct {
	GroupNo       string     `db:"group_no"`
	ShortID       string     `db:"short_id"`
	Status        int        `db:"status"`
	LastMessageAt *time.Time `db:"last_message_at"`
}

// QueryActiveByGroupShortIDs 按 (group_no, short_id) 批量查询子区。
// 只返回 status != ThreadStatusDeleted 的行，SELECT 限定最小列集合。
// 返回 map 的键是 "{groupNo}____{shortID}"（thread channel ID 形式）。
// 无对应行的键不会出现在 map 中，调用方按零值判定。
//
// PR review Round-3：
//   - Blocking #3：只按 short_id 匹配可能撞上其他 group 的同名 short_id，
//     从鉴权角度是重大隐患，必须带上 group_no。
//   - Important #4：sidebar enrich 路径原先用 SELECT *，这里收窄到最小列。
func (d *DB) QueryActiveByGroupShortIDs(refs []ShortRef) (map[string]*ThreadLite, error) {
	result := make(map[string]*ThreadLite)
	if len(refs) == 0 {
		return result, nil
	}

	// 拼接 len(refs) 个 (?, ?) 占位符。
	placeholders := make([]string, len(refs))
	args := make([]interface{}, 0, 1+len(refs)*2)
	for i, r := range refs {
		placeholders[i] = "(?, ?)"
		args = append(args, r.GroupNo, r.ShortID)
	}
	args = append(args, ThreadStatusDeleted)

	var rows []*ThreadLite
	_, err := d.session.SelectBySql(
		"SELECT group_no, short_id, status, last_message_at FROM thread"+
			" WHERE (group_no, short_id) IN ("+strings.Join(placeholders, ", ")+")"+
			" AND status != ?",
		args...,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("query active threads by (group, short): %w", err)
	}
	for _, r := range rows {
		key := r.GroupNo + "____" + r.ShortID
		result[key] = r
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

// QueryActiveShortIDs 批量查询 status=active 的子区 shortID。
// 用于 /v1/conversation/sync 过滤路径：archived/deleted 子区都不返回给客户端。
// archived 子区由 server-side cron (#1376) 维护；收到消息时通过
// RecordMessageAndReactivate 自动复活为 active，重新出现在 sync 列表里。
func (d *DB) QueryActiveShortIDs(shortIDs []string) ([]string, error) {
	if len(shortIDs) == 0 {
		return []string{}, nil
	}
	var result []string
	_, err := d.session.Select("short_id").From("thread").
		Where("short_id IN ? AND status=?", shortIDs, ThreadStatusActive).
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

// ArchiveStaleBatch 批量把 status=ThreadStatusActive 且 last_message_at < threshold
// 的子区切到 ThreadStatusArchived，单次最多 batchSize 行，返回实际归档行数。
//
// SQL 关键点：
//   - WHERE 加 version < ? 防赛跑：cron 拿到版本 V 后，若任何人（手动 archive/unarchive、
//     收消息 auto-unarchive）把同一行的版本号推到 >= V，本批不再触它，避免 cron 用旧
//     版本号覆盖更新的版本号，让 sync 客户端漏拉。
//   - ORDER BY last_message_at, id：保证 MySQL 复制（statement-based / mixed）确定性，
//     且和 idx_status_last_msg_id 三列索引同序，避免 filesort。
//   - last_message_at IS NULL 的子区（从未发过消息）一律保留，避免误归档新建空子区。
//
// 整批共享同一个 version（来自 caller 的 GenSeq）：sync API 按 version 单调递增拉取，
// 一批同 version 不影响 cursor 推进。
func (d *DB) ArchiveStaleBatch(threshold time.Time, batchSize int, version int64) (int64, error) {
	if batchSize <= 0 {
		return 0, nil
	}
	result, err := d.session.UpdateBySql(
		"UPDATE thread SET status=?, version=?, updated_at=? "+
			"WHERE status=? AND last_message_at IS NOT NULL AND last_message_at < ? "+
			"AND version < ? "+
			"ORDER BY last_message_at, id "+
			"LIMIT ?",
		ThreadStatusArchived, version, time.Now(),
		ThreadStatusActive, threshold,
		version,
		batchSize,
	).Exec()
	if err != nil {
		return 0, fmt.Errorf("archive stale threads: %w", err)
	}
	return result.RowsAffected()
}

// versionRetryAttempts 是 CAS 写路径的最大重试次数。
// 任一手动写要"输给"3 次并发更高版本号的写入才会失败，实际上单行很难触达。
const versionRetryAttempts = 3

// CAS 写路径的 sentinel errors，让 service 层能精确区分"行不存在"、"行被并发删了"、
// "行不在期望状态"等场景，而不是把所有"无变更"都当成成功。
var (
	// ErrThreadNotFound 子区不存在（从未被创建或被物理删除）。
	ErrThreadNotFound = errors.New("thread not found")
	// ErrThreadDeleted 子区当前 status=ThreadStatusDeleted。不允许 archive/unarchive/改名。
	ErrThreadDeleted = errors.New("thread deleted")
	// ErrThreadStatusMismatch 行当前 status 与期望不符且不是目标状态。
	// 例如 ArchiveThread 期望 active，但行已被并发改为 archived/deleted。
	ErrThreadStatusMismatch = errors.New("thread status mismatch")
	// ErrThreadCASExhausted CAS 重试次数耗尽。基本不会触发。
	ErrThreadCASExhausted = errors.New("thread CAS retry exhausted")
)

// UpdateStatusFrom 把 status 从 expectedStatus 原子地切换到 newStatus，带 version
// CAS guard 和重试。WHERE 同时校验 short_id / status==expectedStatus / version<新版本。
//
// 返回值语义：
//   - nil：成功写入
//   - nil（特殊）：当前 status 已经是 newStatus（重复操作幂等成功）
//   - ErrThreadNotFound：行不存在
//   - ErrThreadDeleted：行已被并发删除
//   - ErrThreadStatusMismatch：行 status 与 expected 不符，也不是 newStatus
//   - ErrThreadCASExhausted：CAS 三连败（基本不会发生）
//
// 比起锁外读 status 再调 UpdateStatus 的旧路径，这里把状态判定整合进 UPDATE 的 WHERE，
// 闭掉了"读完 status 之后被 delete/cron 改写"的窗口。
func (d *DB) UpdateStatusFrom(shortID string, expectedStatus, newStatus int, newVersion func() (int64, error)) error {
	for attempt := 0; attempt < versionRetryAttempts; attempt++ {
		version, err := newVersion()
		if err != nil {
			return fmt.Errorf("update status: gen version: %w", err)
		}
		result, err := d.session.UpdateBySql(
			"UPDATE thread SET status=?, version=?, updated_at=? "+
				"WHERE short_id=? AND status=? AND version<?",
			newStatus, version, time.Now(),
			shortID, expectedStatus, version,
		).Exec()
		if err != nil {
			return fmt.Errorf("update status: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("update status: rows affected: %w", err)
		}
		if affected > 0 {
			return nil
		}
		// 0 行：行可能不存在 / 已删 / 已是 newStatus / status 还在 expected 但被抢版本。
		actual, ok, perr := d.probeStatus(shortID)
		if perr != nil {
			return fmt.Errorf("update status: probe: %w", perr)
		}
		if !ok {
			return ErrThreadNotFound
		}
		switch actual {
		case ThreadStatusDeleted:
			return ErrThreadDeleted
		case newStatus:
			return nil // 已经是目标状态，幂等成功
		case expectedStatus:
			// 仍在期望状态，但 version 被并发抢先：换更大 GenSeq 重试
		default:
			return ErrThreadStatusMismatch
		}
	}
	return ErrThreadCASExhausted
}

// MarkDeleted 把任何非 deleted 状态的子区切到 deleted。幂等：已删除直接返回 nil。
func (d *DB) MarkDeleted(shortID string, newVersion func() (int64, error)) error {
	for attempt := 0; attempt < versionRetryAttempts; attempt++ {
		version, err := newVersion()
		if err != nil {
			return fmt.Errorf("mark deleted: gen version: %w", err)
		}
		result, err := d.session.UpdateBySql(
			"UPDATE thread SET status=?, version=?, updated_at=? "+
				"WHERE short_id=? AND status!=? AND version<?",
			ThreadStatusDeleted, version, time.Now(),
			shortID, ThreadStatusDeleted, version,
		).Exec()
		if err != nil {
			return fmt.Errorf("mark deleted: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("mark deleted: rows affected: %w", err)
		}
		if affected > 0 {
			return nil
		}
		actual, ok, perr := d.probeStatus(shortID)
		if perr != nil {
			return fmt.Errorf("mark deleted: probe: %w", perr)
		}
		if !ok {
			return ErrThreadNotFound
		}
		if actual == ThreadStatusDeleted {
			return nil // 已删除，幂等
		}
		// 行存在且非 deleted：被抢版本，重试
	}
	return ErrThreadCASExhausted
}

// UpdateName 改名。不允许在已删除的子区上改名（返回 ErrThreadDeleted）。
func (d *DB) UpdateName(shortID string, name string, newVersion func() (int64, error)) error {
	for attempt := 0; attempt < versionRetryAttempts; attempt++ {
		version, err := newVersion()
		if err != nil {
			return fmt.Errorf("update name: gen version: %w", err)
		}
		result, err := d.session.UpdateBySql(
			"UPDATE thread SET name=?, version=?, updated_at=? "+
				"WHERE short_id=? AND status!=? AND version<?",
			name, version, time.Now(),
			shortID, ThreadStatusDeleted, version,
		).Exec()
		if err != nil {
			return fmt.Errorf("update name: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("update name: rows affected: %w", err)
		}
		if affected > 0 {
			return nil
		}
		actual, ok, perr := d.probeStatus(shortID)
		if perr != nil {
			return fmt.Errorf("update name: probe: %w", perr)
		}
		if !ok {
			return ErrThreadNotFound
		}
		if actual == ThreadStatusDeleted {
			return ErrThreadDeleted
		}
		// 行存在且非 deleted：被抢版本，重试
	}
	return ErrThreadCASExhausted
}

// probeStatus 读当前 status。返回 (status, exists, err)。
func (d *DB) probeStatus(shortID string) (int, bool, error) {
	var status int
	n, err := d.session.SelectBySql("SELECT status FROM thread WHERE short_id=?", shortID).Load(&status)
	if err != nil {
		return 0, false, err
	}
	return status, n > 0, nil
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
	ID       int64  `json:"id"`
	ThreadID int64  `json:"thread_id"`
	UID      string `json:"uid"`
	Role     int    `json:"role"` // 0=普通成员, 1=创建者
	Version  int64  `json:"version"`
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

// RecordMessageAndReactivate 收到消息时的事务路径：在行锁内决定是否解档。
//
// 流程：BEGIN → SELECT ... FOR UPDATE → 看当前 status 决定是否解档 → UPDATE → COMMIT。
// 关键点：
//   - GenSeq（即 newVersion 回调）只在锁内、且确认当前确实 archived 时才调用。
//     避免 listener 在拿锁前预生成的版本号低于 cron 在它前面拿到的版本号，
//     从而把 thread.version 写"回退"——这是 sync 游标按 version 单调推进的前提。
//   - active 子区收消息不再消耗 GenSeq，热路径无写放大。
//   - status=deleted 的行直接 no-op，不被消息复活。
//
// 与 ArchiveStaleBatch 的并发收敛：
//   - cron 先拿锁 → status=archived → 我们 SELECT 读到 archived → 取新版本 → 解档为
//     active（新版本号严格 > cron 的版本号，因为 GenSeq 是全局单调）。
//   - 我们先拿锁 → last_message_at=NOW → cron 的 WHERE last_message_at<cutoff 不匹配
//     → cron 跳过本行。
func (d *DB) RecordMessageAndReactivate(shortID, content, senderUID string, newVersion func() (int64, error)) error {
	tx, err := d.session.Begin()
	if err != nil {
		return fmt.Errorf("record message: begin tx: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	var status int
	loaded, err := tx.SelectBySql(
		"SELECT status FROM thread WHERE short_id=? AND status!=? FOR UPDATE",
		shortID, ThreadStatusDeleted,
	).Load(&status)
	if err != nil {
		return fmt.Errorf("record message: lock thread: %w", err)
	}
	if loaded == 0 {
		// 行不存在或已删除：消息到达不该复活已删的子区，直接放弃，但事务正常 commit。
		return tx.Commit()
	}

	now := time.Now()
	if status == ThreadStatusArchived {
		version, gerr := newVersion()
		if gerr != nil {
			return fmt.Errorf("record message: gen version: %w", gerr)
		}
		if _, err := tx.UpdateBySql(
			"UPDATE thread SET status=?, version=?, last_message_at=?, "+
				"message_count = message_count + 1, last_message_content=?, "+
				"last_message_sender_uid=?, updated_at=? "+
				"WHERE short_id=?",
			ThreadStatusActive, version, now, content, senderUID, now, shortID,
		).Exec(); err != nil {
			return fmt.Errorf("record message: reactivate update: %w", err)
		}
		return tx.Commit()
	}

	// status == active：仅更新统计，不动 version。
	if _, err := tx.UpdateBySql(
		"UPDATE thread SET last_message_at=?, message_count = message_count + 1, "+
			"last_message_content=?, last_message_sender_uid=?, updated_at=? "+
			"WHERE short_id=?",
		now, content, senderUID, now, shortID,
	).Exec(); err != nil {
		return fmt.Errorf("record message: stats update: %w", err)
	}
	return tx.Commit()
}

// UpdateMessageStats 原子更新消息统计（收到消息时调用）
func (d *DB) UpdateMessageStats(shortID string, content string, senderUID string) error {
	_, err := d.session.Update("thread").SetMap(map[string]interface{}{
		"message_count":           dbr.Expr("message_count + 1"),
		"last_message_at":         time.Now(),
		"last_message_content":    content,
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
