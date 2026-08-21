package space

import (
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

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

// queryMySpace 查询用户加入的指定空间（带角色和成员数）。可见性条件与 queryMySpaces
// 完全一致（sm.status=1 AND s.status=1），只是把范围收窄到单个 space_id，供只应报告
// 自身绑定租户的非会话凭据使用。不存在 / 已停用 / 已退出均返回 (nil, nil)。
func (d *DB) queryMySpace(uid string, spaceID string) (*SpaceDetailModel, error) {
	var models []*SpaceDetailModel
	_, err := d.session.SelectBySql(`
		SELECT s.*, sm.role,
			(SELECT COUNT(*) FROM space_member WHERE space_id=s.space_id AND status=1) as member_count
		FROM space s
		INNER JOIN space_member sm ON s.space_id = sm.space_id
		WHERE sm.uid=? AND sm.space_id=? AND sm.status=1 AND s.status=1
	`, uid, spaceID).Load(&models)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, nil
	}
	return models[0], nil
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

// queryMember 查询活跃成员。
//
// 读失败先于"未命中"返回。注意破坏性写这条理由并不成立：resetApprovedApplyForRejoin
// 自己的 WHERE 里带了 NOT EXISTS(活跃成员)，所以一次假的 nil 只会让重置命中 0 行，
// 不会误改数据。真正的代价是**回错话**——joinSpace 会据此回 already_member 或
// PENDING，把一次 DB 抖动讲成一个确定的业务结论（推送前审查 D 更正了原本的说法）。
func (d *DB) queryMember(spaceId string, uid string) (*MemberModel, error) {
	var m MemberModel
	_, err := d.session.Select("*").From("space_member").
		Where("space_id=? and uid=? and status=1", spaceId, uid).Load(&m)
	if err != nil {
		return nil, err
	}
	if m.UID == "" {
		return nil, nil
	}
	return &m, nil
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
	if err != nil {
		return nil, err
	}
	if m.UID == "" {
		return nil, nil
	}
	return &m, nil
}

func (d *DB) queryMembers(spaceId string, loginUID string, page uint64, limit uint64) ([]*MemberDetailModel, error) {
	var models []*MemberDetailModel
	// name 兜底链（issue #344）：u.name 为空时回退 user_verification.real_name，
	// 二者皆空时由 mapping 层（MemberDetailModel.DisplayName）给稳定占位符。
	// 这里只多挂一个只读 LEFT JOIN uv；Space + bot 归属过滤（WHERE 子句）保持不变，
	// 不放宽任何隔离边界。禁止用 short_no / username 兜底（privacy-gated）。
	_, err := d.session.SelectBySql(`
		SELECT sm.*, IFNULL(u.name,'') as name,
			IFNULL(uv.real_name,'') as real_name,
			CASE WHEN r.robot_id IS NOT NULL AND r.status=1 THEN 1 ELSE 0 END as robot
		FROM space_member sm
		LEFT JOIN user u ON u.uid=sm.uid
		LEFT JOIN user_verification uv ON uv.user_id=sm.uid
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

func (d *DB) searchMembers(spaceId, keyword string, pageIndex, pageSize int) ([]*memberSearchModel, error) {
	builder := d.session.Select(
		"sm.*",
		"IFNULL(u.name,'') as name",
		"IFNULL(u.username,'') as username",
		"IFNULL(u.email,'') as email",
		"IFNULL(u.phone,'') as phone",
		"CASE WHEN r.robot_id IS NOT NULL AND r.status=1 THEN 1 ELSE 0 END as robot",
	).From(dbr.I("space_member").As("sm")).
		LeftJoin(dbr.I("user").As("u"), "u.uid=sm.uid").
		LeftJoin(dbr.I("robot").As("r"), "r.robot_id=sm.uid").
		Where("sm.space_id=? AND sm.status=1", spaceId)
	if keyword != "" {
		clause, args := memberSearchActiveWhere(keyword)
		builder = builder.Where(clause, args...)
	}
	var list []*memberSearchModel
	_, err := builder.
		OrderDir("sm.role", false).
		OrderAsc("sm.created_at").
		OrderAsc("sm.uid").
		Limit(uint64(pageSize)).
		Offset(uint64((pageIndex - 1) * pageSize)).
		Load(&list)
	return list, err
}

func (d *DB) countSearchMembers(spaceId, keyword string) (int64, error) {
	builder := d.session.Select("COUNT(*)").
		From(dbr.I("space_member").As("sm")).
		LeftJoin(dbr.I("user").As("u"), "u.uid=sm.uid").
		Where("sm.space_id=? AND sm.status=1", spaceId)
	if keyword != "" {
		clause, args := memberSearchActiveWhere(keyword)
		builder = builder.Where(clause, args...)
	}
	var count int64
	_, err := builder.Load(&count)
	return count, err
}

// removeMemberLocked 锁内重读角色后移除成员，防并发转让产生无主空间，
// 见 db_manager.go removeMemberLocked。
func (d *DB) removeMemberLocked(spaceId, uid string, rejectRoleAtOrAbove int, operatorUID, reason string) error {
	return removeMemberLocked(d.session, spaceId, uid, rejectRoleAtOrAbove, operatorUID, reason)
}

func (d *DB) reactivateMember(spaceId string, uid string, role int) error {
	_, err := d.session.Update("space_member").
		Set("status", 1).Set("role", role).
		Set("updated_at", time.Now()).
		Where("space_id=? and uid=?", spaceId, uid).Exec()
	return err
}

// updateMemberRole 更新成员角色，仅用于非 owner 角色（0/1）的变更。
//
// WHERE 带 role <> 2 守卫（PR #339 review F1）：调用方的 pre-check 都在事务外，
// 目标可能在 check 后被并发转让升为 owner，裸 UPDATE 会把新 owner 降级产生
// 无主空间。命中守卫时影响 0 行、静默幂等——显式的 owner 降级已被各调用方
// pre-check 拒绝，竞态命中时保持 owner 角色就是正确结果。
// 设 role=2 必须走 transferOwnerAdminLocked，禁止经本函数产生第二个 owner。
func (d *DB) updateMemberRole(spaceId string, uid string, role int) error {
	_, err := d.session.Update("space_member").Set("role", role).
		Set("updated_at", time.Now()).
		Where("space_id=? and uid=? and status=1 and role <> 2", spaceId, uid).Exec()
	return err
}

// transferOwnerAdmin 用户侧转让所有权，复用管理端的行锁原语，
// 防止目标被并发移除后仍把当前 owner 降级产生无主空间。
func (d *DB) transferOwnerAdmin(spaceId, newOwnerUID string) error {
	return transferOwnerAdminLocked(d.session, spaceId, newOwnerUID)
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

// queryInvitationByCodeUnfiltered 查询邀请码，不过滤 status / 过期时间。
//
// 仅用于审批消耗失败后判定失效原因（次数用尽 / 被禁用 / 已过期）并写入日志：
// incrementInviteUsedCountAtomic 把三种条件合并在一条 WHERE 里，返回 false 时无法
// 区分是哪一种。绝不可用于准入判定——那必须继续走 queryInvitationByCode 的过滤视图。
// 读失败必须先于"未命中"返回：本函数存在的唯一目的就是把精确失效原因写进日志，
// 若沿用同文件里"先判空再返回 err"的写法，一次 DB 故障会被记成"邀请码已不存在"
// 并对外报 invite_code_invalid，正好污染它要产出的那条诊断信息（PR #684 review）。
func (d *DB) queryInvitationByCodeUnfiltered(code string) (*InvitationModel, error) {
	var m InvitationModel
	_, err := d.session.Select("*").From("space_invitation").
		Where("invite_code=?", code).
		Load(&m)
	if err != nil {
		return nil, err
	}
	if m.InviteCode == "" {
		return nil, nil
	}
	return &m, nil
}

// consumeInviteSQL 消耗一次邀请码名额的唯一写法。参数顺序：invite_code, now。
//
// 条件比 queryInvitationByCode 多一条 max_uses——两者本就**不**相同，也不该相同：
// 查询是"这个码此刻能不能被看到/被提交"，消耗是"这个码此刻还能不能再分出一个名额"。
// 共享的是 status=1 与未过期这两条，改动其中任意一条时两处都要一起看
// （原注释声称二者"必须一致"，那是错的——推送前审查 F）。
//
// 审批事务 (approveJoinApplyAtomic) 与直接加入路径 (incrementInviteUsedCountAtomic)
// 共用本常量——此前两处各写了一份逐字节相同的谓词，正是下一次漂移的起点
// （PR #684 review round 4 P2-3）。
const consumeInviteSQL = "UPDATE space_invitation SET used_count=used_count+1 " +
	"WHERE invite_code=? AND status=1 " +
	"AND (max_uses=0 OR used_count<max_uses) " +
	"AND (expires_at IS NULL OR expires_at > ?)"

// incrementInviteUsedCountAtomic atomically increments used_count iff the invite is
// still valid (status=1, not expired, under max_uses), closing TOCTOU windows where an
// admin disables the code (PUT status=0) or the TTL elapses between SELECT and UPDATE.
// The predicate itself lives in consumeInviteSQL — see there for how it relates to
// queryInvitationByCode's view.
// Returns true if the increment was applied, false if the row no longer qualifies.
func (d *DB) incrementInviteUsedCountAtomic(code string) (bool, error) {
	result, err := d.session.UpdateBySql(consumeInviteSQL, code, time.Now()).Exec()
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

// HasActiveCommonSpace 判定两个用户是否同属一个**正常状态**的 Space（双方在籍
// status=1，且 space.status=SpaceStatusNormal）。
//
// 与 GetCommonSpaceID 的关键差别是 JOIN 了 space 表校验父实体活性：封禁 / 解散
// Space 走的是 updateSpaceStatus（db_manager.go），它**只翻 space.status、不清
// space_member 行**（与 forceDisbandSpace 不同）。因此只查关联表的判定会让封禁
// Space 的两个成员仍被视为"同 Space 可达"，绕过封禁冻结——与
// api_member_search.go 里已经处理过的同一类问题。
//
// 授权判定必须用本函数；GetCommonSpaceID 保留给展示/兼容用途（它有十余个调用方，
// 语义不在本次范围内变更）。
func HasActiveCommonSpace(ctx *config.Context, uid1, uid2 string) (bool, error) {
	if uid1 == "" || uid2 == "" {
		return false, nil
	}
	var exists int
	_, err := ctx.DB().SelectBySql(`
		SELECT 1 FROM space_member sm1
		INNER JOIN space_member sm2 ON sm1.space_id = sm2.space_id
		INNER JOIN space s ON s.space_id = sm1.space_id
		WHERE sm1.uid=? AND sm2.uid=? AND sm1.status=1 AND sm2.status=1
		  AND s.status=?
		LIMIT 1
	`, uid1, uid2, SpaceStatusNormal).Load(&exists)
	if err != nil {
		return false, err
	}
	return exists > 0, nil
}

// GetCommonSpaceID 查找两个用户共同所在的第一个 Space
// 返回 space_id 或空字符串（无共同 Space）
//
// 注意：本函数**不校验 Space 活性**（不 JOIN space 表），仅供展示用途。
// 授权判定请用 HasActiveCommonSpace。
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
//
// Deprecated: 内部吞掉 DB 错误，调用方无法区分"用户没默认 Space"和"查询失败"。
// 对于"authoritative-empty 契约"敏感的路径（如 /v1/conversation/sync 的
// space_memberships sideband），请改用 GetUserDefaultSpaceIDE 拿到 error
// 后 fail-closed 返回非 200。
func GetUserDefaultSpaceID(ctx *config.Context, uid string) string {
	spaceID, _ := GetUserDefaultSpaceIDE(ctx, uid)
	return spaceID
}

// GetUserDefaultSpaceIDE 是 GetUserDefaultSpaceID 的 error-returning 变体。
// 用户没默认 Space 时返回 ("", nil)；DB 查询失败时返回 ("", err)。
func GetUserDefaultSpaceIDE(ctx *config.Context, uid string) (string, error) {
	var spaceID string
	_, err := ctx.DB().SelectBySql(`
		SELECT space_id FROM space_member
		WHERE uid=? AND status=1
		ORDER BY created_at ASC
		LIMIT 1
	`, uid).Load(&spaceID)
	if err != nil {
		return "", fmt.Errorf("query user default space failed: %w", err)
	}
	return spaceID, nil
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

// upsertJoinApply 创建申请，或在同一 (space_id, uid) 已有记录时重置为待审批。
//
// created_at 一并刷新：uk_space_uid 决定一个申请人在一个 Space 只有一行，被拒后
// 重新申请会复用该行，而两条列表链路都把 created_at 当"申请时间"排序与展示
// （db.go queryPendingAppliesBySpace / db_manager.go queryJoinAppliesAdmin）。
// 不刷新会让重新申请仍显示首次申请日期（issue #683）。
//
// 已通过（status=1）的记录一律不改写。审批先把状态翻成 1，随后才写入成员行，
// 中间存在一个窗口：此时申请人重新提交，成员校验查不到成员行、待审批查询也因
// 只看 status=0 而查不到这条记录，于是落到这里。无条件改写会把已通过的申请打回
// 待审批并清空 reviewer_uid，留下"人已在 Space 里、申请却显示待审批"的矛盾状态，
// 之后 rejectJoinApply 还能把它标记为已拒绝且不撤销成员资格（PR #684 review）。
//
// MySQL 的 ON DUPLICATE KEY UPDATE 按书写顺序从左到右求值，后面的赋值能看到前面
// 赋过的新值，所以 status 必须放在最后 —— 其余各列的 IF 都要读到它的原值。
func (d *DB) upsertJoinApply(m *spaceJoinApplyModel) (int64, error) {
	result, err := d.session.InsertBySql(
		"INSERT INTO space_join_apply (space_id, uid, invite_code, status, reviewer_uid) VALUES (?, ?, ?, 0, '') "+
			"ON DUPLICATE KEY UPDATE id=LAST_INSERT_ID(id), "+
			"invite_code=IF(status=1, invite_code, VALUES(invite_code)), "+
			"reviewer_uid=IF(status=1, reviewer_uid, ''), "+
			"created_at=IF(status=1, created_at, NOW()), "+
			"updated_at=IF(status=1, updated_at, NOW()), "+
			"status=IF(status=1, 1, 0)",
		m.SpaceId, m.UID, m.InviteCode,
	).Exec()
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	return id, err
}

// refreshPendingApplyInvite 让一条仍在待审批的申请改用申请人本次提交的邀请码，
// 并把申请时间刷新为现在。
//
// WHERE 必须保留 status=0：管理员审批与申请人重申可能并发，审批会先把状态改成 1
// 再消耗名额，此时若无条件更新会把"审批中/已通过"的记录打回待审批。返回 0 行即表示
// 状态已被并发改动，由调用方重新判定。
//
// 这只是重申写入申请行的两条路径之一；另一条是待审批查询落空后走的 upsertJoinApply，
// 它有自己的 status=1 守卫（见该函数注释）。两条都堵上才算完整。
//
// 反过来的交错（先读 apply、申请人改码、再按旧码消耗名额）曾经存在，现已消除：
// 审批走 approveJoinApplyAtomic，在 CAS 锁住该行之后于事务内重新读取邀请码，
// 记账对象一定是记录上当时的那个码。updateJoinApplyStatus 现在只用于拒绝路径。
//
// 刻意不触碰 used_count：提交申请从来不占用邀请码名额，名额在审批时才消耗。
func (d *DB) refreshPendingApplyInvite(id int64, inviteCode string) (int64, error) {
	result, err := d.session.UpdateBySql(
		"UPDATE space_join_apply SET invite_code=?, created_at=NOW(), updated_at=NOW() WHERE id=? AND status=0",
		inviteCode, id,
	).Exec()
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// approveOutcome 描述一次原子审批的终态。
type approveOutcome int

const (
	// approveFailed 是零值，专门留给"返回了 error"的情形。让零值等于成功，会让
	// 任何一处漏检 err 的调用把失败的审批当成通过（PR #684 review round 4 P2-9）。
	approveFailed         approveOutcome = iota
	approveOK                            // 已加入或已重新激活
	approveAlreadyHandled                // CAS 落空：已被其他审批者处理
	approveInviteUnusable                // 邀请码无法消耗
	approveSpaceFull                     // 空间已满
	approveAlreadyMember                 // 申请人本就是活跃成员
)

// approveJoinApplyAtomic 在**同一个事务**里完成审批的三件事：把申请 CAS 成已通过、
// 消耗该行此刻绑定的邀请码名额、以及在容量锁内写入或重新激活成员行。
//
// 这是本模块最关键的不变量来源：status=1 只会与活跃成员行**一起提交**，因此
// "已通过但不是成员"这个状态对其它事务永远不可见。此前三者是三个独立事务，
// 中间任何一步失败或进程被杀，都会留下一条 status=1 却没有成员的孤儿行——
// 待审批查询看不到它（只查 status=0）、审批/拒绝都以"已处理"拒绝、重新申请又被
// upsert 守卫挡住，申请人被永久锁死且无人能救（PR #684 review round 3 P1）。
//
// 事务化同时消灭了原来的补偿逻辑：失败时整笔回滚，不再需要把状态写回 0，
// 也不再需要退还名额——那些补偿写本身失败时会把系统留在坏状态。
//
// 邀请码在事务内**重新读取**，而不是沿用调用方在 CAS 之前读到的那份快照：
// 并发重申可能已经把这行改成了另一个码，沿用旧快照会记账到错码上（round 2 P2-1）。
func (d *DB) approveJoinApplyAtomic(applyID int64, reviewerUID, spaceId string, maxUsers int) (approveOutcome, string, error) {
	tx, err := d.session.Begin()
	if err != nil {
		return approveFailed, "", err
	}
	defer tx.RollbackUnlessCommitted()

	// 1. CAS 成已通过；落空说明已被并发处理
	result, err := tx.UpdateBySql(
		"UPDATE space_join_apply SET status=1, reviewer_uid=?, updated_at=NOW() WHERE id=? AND status=0",
		reviewerUID, applyID,
	).Exec()
	if err != nil {
		return approveFailed, "", err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return approveFailed, "", err
	}
	if affected == 0 {
		return approveAlreadyHandled, "", nil
	}

	// 2. CAS 已锁住该行，此处读到的邀请码就是审批真正针对的那个
	var row struct {
		UID        string
		InviteCode string
	}
	if _, err = tx.SelectBySql(
		"SELECT uid, invite_code FROM space_join_apply WHERE id=?", applyID,
	).Load(&row); err != nil {
		return approveFailed, "", err
	}

	// 3. 容量锁：与既有 atomicAddMemberIfNotFull 取同一把锁，避免并发审批超员。
	//
	// 本事务的加锁顺序是 space_join_apply → space_member → space_invitation。
	// 目前仓库里只有这一个事务同时锁其中两张表，所以不存在反序。若将来把直接加入
	// (api.go joinSpace 直接模式) 或邮件邀请接受也合并成单事务，务必沿用同一顺序：
	// 那两条路径现在是"先消耗邀请码、再写成员、失败退还"，直接融合会得到
	// space_invitation → space_member，与此处相反，构成死锁（round 4 P2-6）。
	var count int
	if _, err = tx.SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND status=1 FOR UPDATE", spaceId,
	).Load(&count); err != nil {
		return approveFailed, "", err
	}

	// 必须是加锁读。本事务在上面读 space_join_apply 时已经钉住了 REPEATABLE READ 的
	// 一致性读视图，普通读会从那个旧快照取值：并发的 removeMemberLocked 提交后，
	// FOR UPDATE 的计数是当前值、普通读到的成员状态却仍是陈旧的 1，于是审批会走进
	// "已是成员"分支、提交 status=1 却一行成员都没写——正是本文件声称不可能出现的
	// 状态（PR #684 review round 4 P1-A，已实测复现读视图差异）。
	// 加锁读取当前值，并与 removeMemberLocked 自身的 FOR UPDATE 相互串行。
	var member struct {
		UID    string
		Status int
	}
	memberRows, err := tx.SelectBySql(
		"SELECT uid, status FROM space_member WHERE space_id=? AND uid=? FOR UPDATE", spaceId, row.UID,
	).Load(&member)
	if err != nil {
		return approveFailed, "", err
	}

	// 本就是活跃成员：审批照常落库（status=1 与活跃成员并存，不变量成立），
	// 但不占用名额——没有新增成员就不该消耗。
	if memberRows > 0 && member.Status == 1 {
		if err = tx.Commit(); err != nil {
			return approveFailed, "", err
		}
		return approveAlreadyMember, row.InviteCode, nil
	}

	// 4. 消耗名额。过滤条件必须与 queryInvitationByCode 的有效性视图一致。
	if row.InviteCode != "" {
		result, err = tx.UpdateBySql(consumeInviteSQL, row.InviteCode, time.Now()).Exec()
		if err != nil {
			return approveFailed, "", err
		}
		if affected, err = result.RowsAffected(); err != nil {
			return approveFailed, "", err
		}
		if affected == 0 {
			return approveInviteUnusable, row.InviteCode, nil
		}
	}

	// 5. 容量校验（在锁内读到的 count 上判断）
	if maxUsers > 0 && count >= maxUsers {
		return approveSpaceFull, row.InviteCode, nil
	}

	// 6. 写入或重新激活成员行
	if memberRows > 0 {
		_, err = tx.Update("space_member").
			Set("status", 1).Set("role", 0).Set("updated_at", time.Now()).
			Where("space_id=? AND uid=?", spaceId, row.UID).Exec()
	} else {
		_, err = tx.InsertBySql(
			"INSERT INTO space_member (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, 0, 1, NOW(), NOW())",
			spaceId, row.UID,
		).Exec()
	}
	if err != nil {
		return approveFailed, "", err
	}

	if err = tx.Commit(); err != nil {
		return approveFailed, "", err
	}
	return approveOK, row.InviteCode, nil
}

// resetApprovedApplyForRejoin 把一条陈旧的已通过申请重置为待审批，让当事人可以
// 重新走审批流程。
//
// 仅当"status=1 且当前没有活跃成员行"时生效。由于 approveJoinApplyAtomic 保证
// status=1 与活跃成员一起提交，这个组合唯一对应"审批通过之后成员资格又结束了"，
// 不存在与"审批进行中"混淆的可能——正因如此这里不需要任何时间阈值。
//
// "没有活跃成员"这个条件必须写进 SQL 本身，而不是由调用方先查一次再来调用：
// 两者之间存在窗口，管理员在此刻把该用户重新拉回空间，就会留下"人已在空间里、
// 却凭空多出一条待审批申请"的矛盾状态（PR #684 review round 3）。
// 判定与写入分离、写入不带谓词，正是本 PR 前几轮反复栽跟头的同一类错误。
func (d *DB) resetApprovedApplyForRejoin(id int64, inviteCode string) (int64, error) {
	result, err := d.session.UpdateBySql(
		"UPDATE space_join_apply ja SET ja.status=0, ja.invite_code=?, ja.reviewer_uid='', "+
			"ja.created_at=NOW(), ja.updated_at=NOW() "+
			"WHERE ja.id=? AND ja.status=1 AND NOT EXISTS ("+
			"SELECT 1 FROM space_member sm WHERE sm.space_id=ja.space_id AND sm.uid=ja.uid AND sm.status=1)",
		inviteCode, id,
	).Exec()
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// queryJoinApplyByID 按 ID 读取申请。读失败先于"未命中"返回：本函数的返回值现在
// 参与守卫判定，把 DB 故障吞成"记录不存在"会让调用方走进错误分支
// （PR #684 review round 3 P2-2）。
func (d *DB) queryJoinApplyByID(id int64) (*spaceJoinApplyModel, error) {
	var m spaceJoinApplyModel
	_, err := d.session.Select("*").From("space_join_apply").
		Where("id=?", id).Load(&m)
	if err != nil {
		return nil, err
	}
	if m.Id == 0 {
		return nil, nil
	}
	return &m, nil
}

// queryPendingApplyBySpaceAndUID 查询该用户在该 Space 的待审批申请。
//
// 读失败先于"未命中"返回：joinSpace 用本函数的结果在"更新已有待审批申请"和
// "走 upsert 新建"两条分支之间做选择，把 DB 故障吞成"没有待审批申请"会让请求
// 静默走错分支（PR #684 review round 3）。同 queryJoinApplyByID。
func (d *DB) queryPendingApplyBySpaceAndUID(spaceId, uid string) (*spaceJoinApplyModel, error) {
	var m spaceJoinApplyModel
	_, err := d.session.Select("*").From("space_join_apply").
		Where("space_id=? AND uid=? AND status=0", spaceId, uid).Load(&m)
	if err != nil {
		return nil, err
	}
	if m.Id == 0 {
		return nil, nil
	}
	return &m, nil
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
