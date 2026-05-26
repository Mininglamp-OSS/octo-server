package space

import (
	"errors"
	"go.uber.org/zap"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/gocraft/dbr/v2"
)

type DB struct {
	ctx     *config.Context
	session *dbr.Session
}

func NewDB(ctx *config.Context) *DB {
	return &DB{
		ctx:     ctx,
		session: ctx.DB(),
	}
}

// isSpaceActive 检查空间是否处于活跃状态
func (d *DB) isSpaceActive(spaceId string) (bool, error) {
	var count int
	_, err := d.session.SelectBySql("SELECT COUNT(*) FROM space WHERE space_id=? AND status=1", spaceId).Load(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// ---------- Space CRUD ----------

func (d *DB) insertSpace(m *SpaceModel, tx *dbr.Tx) error {
	_, err := tx.InsertInto("space").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

func (d *DB) insertSpaceNoTx(m *SpaceModel) error {
	_, err := d.session.InsertInto("space").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

func (d *DB) querySpaceByID(spaceId string) (*SpaceModel, error) {
	var m SpaceModel
	_, err := d.session.Select("*").From("space").Where("space_id=? and status=1", spaceId).Load(&m)
	if m.SpaceId == "" {
		return nil, nil
	}
	return &m, err
}

// updateSpace 用户侧部分更新空间基础信息已迁移到 managerDB.updateSpaceProfile：
// 该 helper 提供事务 + SELECT ... FOR UPDATE + sentinel error 的 TOCTOU 安全语义，
// 用户侧 handler 通过 Space.mdb 直接调用，无需在 DB 层另起一份。

func (d *DB) disbandSpace(spaceId string) error {
	_, err := d.session.Update("space").Set("status", 0).Set("updated_at", time.Now()).Where("space_id=?", spaceId).Exec()
	return err
}

// queryMySpaces 查询用户加入的所有空间（带角色和成员数）
func (d *DB) queryMySpaces(uid string) ([]*SpaceDetailModel, error) {
	var models []*SpaceDetailModel
	_, err := d.session.SelectBySql(`
		SELECT s.*, sm.role,
			(SELECT COUNT(*) FROM space_member WHERE space_id=s.space_id AND status=1) as member_count
		FROM space s
		INNER JOIN space_member sm ON s.space_id = sm.space_id
		WHERE sm.uid=? AND sm.status=1 AND s.status=1
		ORDER BY s.created_at DESC
	`, uid).Load(&models)
	return models, err
}

// querySpaceDetail 查询空间详情（带当前用户角色和成员数）
func (d *DB) querySpaceDetail(spaceId string, uid string) (*SpaceDetailModel, error) {
	var m SpaceDetailModel
	_, err := d.session.SelectBySql(`
		SELECT s.*, IFNULL(sm.role, -1) as role,
			(SELECT COUNT(*) FROM space_member WHERE space_id=s.space_id AND status=1) as member_count
		FROM space s
		LEFT JOIN space_member sm ON s.space_id = sm.space_id AND sm.uid=? AND sm.status=1
		WHERE s.space_id=? AND s.status=1
	`, uid, spaceId).Load(&m)
	if m.SpaceId == "" {
		return nil, nil
	}
	return &m, err
}

// countActiveMembers 查询空间活跃成员数
func (d *DB) countActiveMembers(spaceId string) (int, error) {
	var count int
	_, err := d.session.SelectBySql("SELECT COUNT(*) FROM space_member WHERE space_id=? AND status=1", spaceId).Load(&count)
	return count, err
}

// ---------- Member CRUD ----------

func (d *DB) insertMember(m *MemberModel, tx *dbr.Tx) error {
	_, err := tx.InsertInto("space_member").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

func (d *DB) insertMemberNoTx(m *MemberModel) error {
	_, err := d.session.InsertInto("space_member").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

func (d *DB) queryMember(spaceId string, uid string) (*MemberModel, error) {
	var m MemberModel
	_, err := d.session.Select("*").From("space_member").
		Where("space_id=? and uid=? and status=1", spaceId, uid).Load(&m)
	if m.UID == "" {
		return nil, nil
	}
	return &m, err
}

// IsMember 检查用户是否是 Space 成员
func (d *DB) IsMember(spaceId string, uid string) (bool, error) {
	m, err := d.queryMember(spaceId, uid)
	if err != nil {
		return false, err
	}
	return m != nil, nil
}

// queryMemberIncludeRemoved 查询成员（包括已移除的），用于判断是否曾经加入过
func (d *DB) queryMemberIncludeRemoved(spaceId string, uid string) (*MemberModel, error) {
	var m MemberModel
	_, err := d.session.Select("*").From("space_member").
		Where("space_id=? and uid=?", spaceId, uid).Load(&m)
	if m.UID == "" {
		return nil, nil
	}
	return &m, err
}

func (d *DB) queryMembers(spaceId string, loginUID string, page uint64, limit uint64) ([]*MemberDetailModel, error) {
	var models []*MemberDetailModel
	_, err := d.session.SelectBySql(`
		SELECT sm.*, IFNULL(u.name,'') as name,
			CASE WHEN r.robot_id IS NOT NULL AND r.status=1 THEN 1 ELSE 0 END as robot
		FROM space_member sm
		LEFT JOIN user u ON u.uid=sm.uid
		LEFT JOIN robot r ON r.robot_id=sm.uid
		WHERE sm.space_id=? AND sm.status=1 AND (
			r.robot_id IS NULL
			OR r.creator_uid = ?
		)
		ORDER BY sm.role DESC, sm.created_at ASC
		LIMIT ? OFFSET ?
	`, spaceId, loginUID, limit, (page-1)*limit).Load(&models)
	return models, err
}

func (d *DB) removeMember(spaceId string, uid string) error {
	_, err := d.session.Update("space_member").Set("status", 0).
		Set("updated_at", time.Now()).
		Where("space_id=? and uid=?", spaceId, uid).Exec()
	return err
}

func (d *DB) reactivateMember(spaceId string, uid string, role int) error {
	_, err := d.session.Update("space_member").
		Set("status", 1).Set("role", role).
		Set("updated_at", time.Now()).
		Where("space_id=? and uid=?", spaceId, uid).Exec()
	return err
}

func (d *DB) updateMemberRole(spaceId string, uid string, role int) error {
	_, err := d.session.Update("space_member").Set("role", role).
		Set("updated_at", time.Now()).
		Where("space_id=? and uid=? and status=1", spaceId, uid).Exec()
	return err
}

func (d *DB) updateMemberRoleTx(tx *dbr.Tx, spaceId string, uid string, role int) error {
	_, err := tx.Update("space_member").Set("role", role).
		Set("updated_at", time.Now()).
		Where("space_id=? and uid=? and status=1", spaceId, uid).Exec()
	return err
}

// queryCoMemberUIDs 查询与指定用户同在至少一个空间的所有用户UID
func (d *DB) queryCoMemberUIDs(uid string) ([]string, error) {
	var uids []string
	_, err := d.session.SelectBySql(`
		SELECT DISTINCT sm2.uid
		FROM space_member sm1
		INNER JOIN space_member sm2 ON sm1.space_id = sm2.space_id
		INNER JOIN space s ON s.space_id = sm1.space_id AND s.status=1
		WHERE sm1.uid=? AND sm1.status=1 AND sm2.status=1 AND sm2.uid!=?
	`, uid, uid).Load(&uids)
	return uids, err
}

// GetCoMemberUIDs 包级别函数，供其他模块调用，查询与指定用户同在至少一个空间的所有用户UID
func GetCoMemberUIDs(ctx *config.Context, uid string) ([]string, error) {
	db := NewDB(ctx)
	return db.queryCoMemberUIDs(uid)
}

// ---------- Invitation CRUD ----------

func (d *DB) insertInvitation(m *InvitationModel) error {
	// 显式列写入：dbr 的 Record 反射无法处理 *db.Time（未实现 driver.Valuer）。
	var expires interface{}
	if m.ExpiresAt != nil {
		expires = time.Time(*m.ExpiresAt)
	}
	_, err := d.session.InsertInto("space_invitation").
		Columns("space_id", "invite_code", "creator", "max_uses", "used_count", "expires_at", "status").
		Values(m.SpaceId, m.InviteCode, m.Creator, m.MaxUses, m.UsedCount, expires, m.Status).
		Exec()
	return err
}

// queryInvitationByCode 查询有效邀请码（status=1 且未过期）。
// 过期码与不存在码同等处理，避免公开预览端点（getInviteInfo / getInvitePreview）
// 通过 "有效/无效" 差异泄露"曾经有效"的码（issue #1000 枚举面收敛）。
func (d *DB) queryInvitationByCode(code string) (*InvitationModel, error) {
	var m InvitationModel
	_, err := d.session.Select("*").From("space_invitation").
		Where("invite_code=? AND status=1 AND (expires_at IS NULL OR expires_at > ?)", code, time.Now()).
		Load(&m)
	if m.InviteCode == "" {
		return nil, nil
	}
	return &m, err
}

// incrementInviteUsedCountAtomic atomically increments used_count iff the invite is
// still valid (status=1, not expired, under max_uses). Filter conditions must stay in
// sync with queryInvitationByCode so the read→write path keeps the same validity view,
// closing TOCTOU windows where an admin disables the code (PUT status=0) or TTL elapses
// between SELECT and UPDATE.
// Returns true if the increment was applied, false if the row no longer qualifies.
func (d *DB) incrementInviteUsedCountAtomic(code string) (bool, error) {
	result, err := d.session.UpdateBySql(
		"UPDATE space_invitation SET used_count=used_count+1 "+
			"WHERE invite_code=? AND status=1 "+
			"AND (max_uses=0 OR used_count<max_uses) "+
			"AND (expires_at IS NULL OR expires_at > ?)",
		code, time.Now(),
	).Exec()
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// decrementInviteUsedCountAtomic 原子回滚一次使用计数（used_count>0 时才减）。
// 用于审批后执行加入失败（ErrSpaceFull 等）时撤销已消耗的名额，与 approve 路径的
// incrementInviteUsedCountAtomic 配对，避免"消耗了但没人加入"。
// 不校验 status/expires_at：邀请码可能在消耗后被 owner 禁用或过期，但回滚应无条件进行。
func (d *DB) decrementInviteUsedCountAtomic(code string) (bool, error) {
	result, err := d.session.UpdateBySql(
		"UPDATE space_invitation SET used_count=used_count-1 "+
			"WHERE invite_code=? AND used_count>0",
		code,
	).Exec()
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// BotDetailModel Bot 详情模型
type BotDetailModel struct {
	RobotID string // 机器人ID
	Name    string // 名称
	Avatar  string // 头像
}

// querySpaceBots 查询 Space 内所有有效的 Bot 列表
func (d *DB) querySpaceBots(spaceId string) ([]*BotDetailModel, error) {
	var models []*BotDetailModel
	_, err := d.session.SelectBySql(`
		SELECT r.robot_id, IFNULL(u.name,'') as name, IFNULL(u.avatar,'') as avatar
		FROM space_member sm
		INNER JOIN robot r ON r.robot_id=sm.uid AND r.status=1
		LEFT JOIN user u ON u.uid=sm.uid
		WHERE sm.space_id=? AND sm.status=1
		ORDER BY sm.created_at ASC
	`, spaceId).Load(&models)
	return models, err
}

// updateInvitation 更新邀请码设置
func (d *DB) updateInvitation(code string, maxUses *int, expiresAt *time.Time) error {
	builder := d.session.Update("space_invitation")
	if maxUses != nil {
		builder = builder.Set("max_uses", *maxUses)
	}
	if expiresAt != nil {
		builder = builder.Set("expires_at", *expiresAt)
	}
	builder = builder.Set("updated_at", time.Now())
	_, err := builder.Where("invite_code=? AND status=1", code).Exec()
	return err
}

// GetCommonSpaceID 查找两个用户共同所在的第一个 Space
// 返回 space_id 或空字符串（无共同 Space）
func GetCommonSpaceID(ctx *config.Context, uid1, uid2 string) string {
	var spaceID string
	_, err := ctx.DB().SelectBySql(`
		SELECT sm1.space_id FROM space_member sm1
		INNER JOIN space_member sm2 ON sm1.space_id = sm2.space_id
		WHERE sm1.uid=? AND sm2.uid=? AND sm1.status=1 AND sm2.status=1
		LIMIT 1
	`, uid1, uid2).Load(&spaceID)
	if err != nil {
		ctx.Warn("GetCommonSpaceID query failed", zap.String("uid1", uid1), zap.String("uid2", uid2), zap.Error(err))
	}
	return spaceID
}

// queryInvitationBySpaceAndCode 查询指定 Space 下的邀请码
func (d *DB) queryInvitationBySpaceAndCode(spaceId string, code string) (*InvitationModel, error) {
	var m InvitationModel
	_, err := d.session.Select("*").From("space_invitation").
		Where("space_id=? AND invite_code=? AND status=1", spaceId, code).Load(&m)
	if m.InviteCode == "" {
		return nil, nil
	}
	return &m, err
}

// inviteListFilter 邀请码列表过滤器。
//
//	"active"   —— 仅返回业务有效（status=1 且未过期），与 queryInvitationByCode 的视图一致
//	"disabled" —— 仅返回 status=0 的邀请码（不含"status=1 但已过期"，过期是一种不同的失效）
//	"all"      —— 不过滤，返回空间下全部邀请码
type inviteListFilter string

const (
	inviteListActive   inviteListFilter = "active"
	inviteListDisabled inviteListFilter = "disabled"
	inviteListAll      inviteListFilter = "all"
)

// applyInviteListFilter 把过滤器转成 dbr.Where 片段，复用给 list 和 count。
// now 由调用方快照一次并传入，避免 list/count 双查询各自采样 time.Now() 导致
// 临近过期的邀请码计数不一致（off-by-one 于分页响应）。
func applyInviteListFilter(b *dbr.SelectBuilder, filter inviteListFilter, now time.Time) *dbr.SelectBuilder {
	switch filter {
	case inviteListActive:
		return b.Where("status=1 AND (expires_at IS NULL OR expires_at > ?)", now)
	case inviteListDisabled:
		return b.Where("status=0")
	default:
		return b
	}
}

// queryInvitesBySpace 用户端分页查询空间邀请码。按 created_at 倒序。
func (d *DB) queryInvitesBySpace(spaceId string, filter inviteListFilter, now time.Time, pageSize, pageIndex uint64) ([]*InvitationModel, error) {
	b := d.session.Select("*").From("space_invitation").Where("space_id=?", spaceId)
	b = applyInviteListFilter(b, filter, now)
	var list []*InvitationModel
	_, err := b.OrderDir("created_at", false).
		Limit(pageSize).Offset((pageIndex - 1) * pageSize).
		Load(&list)
	return list, err
}

// countInvitesBySpace 用户端邀请码计数，过滤器语义与 queryInvitesBySpace 一致。
func (d *DB) countInvitesBySpace(spaceId string, filter inviteListFilter, now time.Time) (int64, error) {
	b := d.session.Select("COUNT(*)").From("space_invitation").Where("space_id=?", spaceId)
	b = applyInviteListFilter(b, filter, now)
	var count int64
	_, err := b.Load(&count)
	return count, err
}

// GetUserDefaultSpaceID 获取用户最早加入的 Space（默认 Space）
func GetUserDefaultSpaceID(ctx *config.Context, uid string) string {
	var spaceID string
	_, _ = ctx.DB().SelectBySql(`
		SELECT space_id FROM space_member
		WHERE uid=? AND status=1
		ORDER BY created_at ASC
		LIMIT 1
	`, uid).Load(&spaceID)
	return spaceID
}

// GetSpaceMemberUIDs 获取指定 Space 的所有成员 UID
func GetSpaceMemberUIDs(ctx *config.Context, spaceID string) ([]string, error) {
	var uids []string
	_, err := ctx.DB().SelectBySql(`
		SELECT uid FROM space_member
		WHERE space_id=? AND status=1
	`, spaceID).Load(&uids)
	return uids, err
}

// insertMemberIgnore 插入成员（忽略重复）
func (d *DB) insertMemberIgnore(m *MemberModel) error {
	_, err := d.session.InsertBySql(
		"INSERT IGNORE INTO space_member (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, ?, ?, NOW(), NOW())",
		m.SpaceId, m.UID, m.Role, m.Status,
	).Exec()
	return err
}

// ErrSpaceFull indicates the space has reached its member capacity
var ErrSpaceFull = errors.New("SPACE_FULL")

// ErrAlreadyMember indicates the user is already an active member
var ErrAlreadyMember = errors.New("already_member")

// atomicAddMemberIfNotFull atomically checks capacity and adds a member.
// Uses SELECT ... FOR UPDATE to prevent race conditions.
// Returns ErrSpaceFull if the space has reached its member limit.
func (d *DB) atomicAddMemberIfNotFull(spaceId string, uid string, maxUsers int) error {
	tx, err := d.session.Begin()
	if err != nil {
		return err
	}
	defer tx.RollbackUnlessCommitted()

	// Lock the space row and get current member count atomically
	var count int
	_, err = tx.SelectBySql(`
		SELECT COUNT(*) FROM space_member
		WHERE space_id = ? AND status = 1
		FOR UPDATE
	`, spaceId).Load(&count)
	if err != nil {
		return err
	}

	// Check capacity
	if maxUsers > 0 && count >= maxUsers {
		return ErrSpaceFull
	}

	// Insert new member
	_, err = tx.InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, 0, 1, NOW(), NOW())",
		spaceId, uid,
	).Exec()
	if err != nil {
		return err
	}

	return tx.Commit()
}

// atomicReactivateMemberIfNotFull atomically checks capacity and reactivates a member.
// Returns ErrSpaceFull if the space has reached its member limit.
func (d *DB) atomicReactivateMemberIfNotFull(spaceId string, uid string, maxUsers int) error {
	tx, err := d.session.Begin()
	if err != nil {
		return err
	}
	defer tx.RollbackUnlessCommitted()

	// Lock the space row and get current member count atomically
	var count int
	_, err = tx.SelectBySql(`
		SELECT COUNT(*) FROM space_member
		WHERE space_id = ? AND status = 1
		FOR UPDATE
	`, spaceId).Load(&count)
	if err != nil {
		return err
	}

	// Check capacity
	if maxUsers > 0 && count >= maxUsers {
		return ErrSpaceFull
	}

	// Reactivate member
	_, err = tx.Update("space_member").
		Set("status", 1).Set("role", 0).
		Set("updated_at", time.Now()).
		Where("space_id=? AND uid=?", spaceId, uid).Exec()
	if err != nil {
		return err
	}

	return tx.Commit()
}

// ---------- Admin/Owner Query ----------

// queryAdminsAndOwner 查询 Space 的管理员和拥有者（role >= 1）
func (d *DB) queryAdminsAndOwner(spaceId string) ([]*MemberModel, error) {
	var models []*MemberModel
	_, err := d.session.Select("*").From("space_member").
		Where("space_id=? AND status=1 AND role>=1", spaceId).
		Load(&models)
	return models, err
}

// ---------- Join Apply CRUD ----------

func (d *DB) upsertJoinApply(m *spaceJoinApplyModel) (int64, error) {
	result, err := d.session.InsertBySql(
		"INSERT INTO space_join_apply (space_id, uid, invite_code, status, reviewer_uid) VALUES (?, ?, ?, 0, '') "+
			"ON DUPLICATE KEY UPDATE id=LAST_INSERT_ID(id), status=0, invite_code=VALUES(invite_code), reviewer_uid='', updated_at=NOW()",
		m.SpaceId, m.UID, m.InviteCode,
	).Exec()
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	return id, err
}

func (d *DB) queryJoinApplyByID(id int64) (*spaceJoinApplyModel, error) {
	var m spaceJoinApplyModel
	_, err := d.session.Select("*").From("space_join_apply").
		Where("id=?", id).Load(&m)
	if m.Id == 0 {
		return nil, nil
	}
	return &m, err
}

func (d *DB) queryPendingApplyBySpaceAndUID(spaceId, uid string) (*spaceJoinApplyModel, error) {
	var m spaceJoinApplyModel
	_, err := d.session.Select("*").From("space_join_apply").
		Where("space_id=? AND uid=? AND status=0", spaceId, uid).Load(&m)
	if m.Id == 0 {
		return nil, nil
	}
	return &m, err
}

func (d *DB) queryPendingAppliesBySpace(spaceId string, limit, offset int) ([]*spaceJoinApplyDetailModel, error) {
	var models []*spaceJoinApplyDetailModel
	_, err := d.session.SelectBySql(`
		SELECT a.*, IFNULL(u.name,'') as applicant_name
		FROM space_join_apply a
		LEFT JOIN user u ON u.uid=a.uid
		WHERE a.space_id=? AND a.status=0
		ORDER BY a.created_at DESC
		LIMIT ? OFFSET ?
	`, spaceId, limit, offset).Load(&models)
	return models, err
}

func (d *DB) queryPendingApplyCountBySpace(spaceId string) (int64, error) {
	var count int64
	_, err := d.session.SelectBySql(
		"SELECT COUNT(*) FROM space_join_apply WHERE space_id=? AND status=0", spaceId,
	).Load(&count)
	return count, err
}

func (d *DB) updateJoinApplyStatus(id int64, status int, reviewerUID string) (int64, error) {
	result, err := d.session.Update("space_join_apply").
		Set("status", status).
		Set("reviewer_uid", reviewerUID).
		Set("updated_at", time.Now()).
		Where("id=? AND status=0", id).Exec()
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// updateJoinApplyStatusRaw 无条件更新状态（用于回滚）
func (d *DB) updateJoinApplyStatusRaw(id int64, status int, reviewerUID string) (int64, error) {
	result, err := d.session.Update("space_join_apply").
		Set("status", status).
		Set("reviewer_uid", reviewerUID).
		Set("updated_at", time.Now()).
		Where("id=?", id).Exec()
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
