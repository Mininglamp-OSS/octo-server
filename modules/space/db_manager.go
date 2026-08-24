package space

import (
	"errors"
	"fmt"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
)

// escapeLike 转义 LIKE 模式中的通配符：反斜杠、%、_ 都需要 escape。
// 必须先替换反斜杠，否则后续加的转义会被二次转义。
// 注意：SQL 侧 LIKE 表达式必须配合 `ESCAPE '\\'` 子句使用（见 likeEscapeClause），
// 否则在 sql_mode 包含 NO_BACKSLASH_ESCAPES 的实例上默认不会把 `\` 当作转义字符。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	s = strings.ReplaceAll(s, "_", `\_`)
	return s
}

// likeEscapeClause LIKE 显式声明转义字符，避免 sql_mode=NO_BACKSLASH_ESCAPES 时 `\` 失效。
const likeEscapeClause = ` ESCAPE '\\'`

// buildLikePattern 组装 "%keyword%" 形式，已对通配符做转义，防止 foo_bar 误匹配 foobar。
func buildLikePattern(keyword string) string {
	return "%" + escapeLike(keyword) + "%"
}

// memberSearchColumns 管理端成员模糊搜索覆盖的列。
// email/username 对 SSO / 邮箱登录用户尤为关键：这类用户 username 可能为空，
// 只能靠 email 定位（与 user 模块 queryUserListWithPageAndKeyword 的取向一致）。
// u.* 来自 LEFT JOIN 的 user 表，sm.uid 来自 space_member 自身。
var memberSearchColumns = []string{"u.name", "u.username", "u.email", "u.phone", "sm.uid"}

// memberSearchWhere 按 keyword 组装跨列 OR LIKE 条件及其占位参数。
// list / count 两处共用，避免搜索范围漂移导致"列表与总数样本不一致"的分页错位。
func memberSearchWhere(keyword string) (string, []interface{}) {
	like := buildLikePattern(keyword)
	clauses := make([]string, len(memberSearchColumns))
	args := make([]interface{}, len(memberSearchColumns))
	for i, col := range memberSearchColumns {
		clauses[i] = col + " LIKE ?" + likeEscapeClause
		args[i] = like
	}
	return strings.Join(clauses, " OR "), args
}

// memberSearchActiveColumns 空间侧 members/search 端点的检索列。与管理端
// memberSearchColumns 的区别：
//   - email 明文匹配、明文返回（工作邮箱，无需掩码）；
//   - phone 仅匹配后 4 位，使「可检索粒度 == 可见粒度」
//     （响应仅显示 138****5678），admin 无法通过子串查询逐位探测/重建完整号码。
//
// 后 4 位的取值优先用 user.phone_last4（手机号加密第一阶新增的低敏列，
// 见 modules/user/phone_crypto.go），未回填的存量行回退到 RIGHT(u.phone,4)。
// 这个过渡表达式让检索随回填进度逐行切换，不必等回填全部跑完才敢改；回填收敛后
// （GET /v1/manager/user/phone_shadow_backfill 的 remaining 归零）应简化成直接用
// u.phone_last4，届时 RIGHT(u.phone,4) 这个明文读取方即可彻底移除。
//
// 前端注意：phone 检索只匹配后 4 位，传完整号码不会命中——按手机号查找请用后 4 位。
var memberSearchActiveColumns = []string{
	"u.name", "u.username", "u.email",
	"COALESCE(NULLIF(u.phone_last4,''), RIGHT(u.phone,4))",
	"sm.uid",
}

// memberSearchActiveWhere 为空间侧 members/search 组装跨列 OR LIKE 条件。
// list / count 共用同一条件，避免搜索范围漂移导致分页错位。
func memberSearchActiveWhere(keyword string) (string, []interface{}) {
	like := buildLikePattern(keyword)
	clauses := make([]string, len(memberSearchActiveColumns))
	args := make([]interface{}, len(memberSearchActiveColumns))
	for i, col := range memberSearchActiveColumns {
		clauses[i] = col + " LIKE ?" + likeEscapeClause
		args[i] = like
	}
	return strings.Join(clauses, " OR "), args
}

// placeholders 生成 "?, ?, ?" 形式 placeholder 字符串，n 必须大于 0。
func placeholders(n int) string {
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}

// managerSpaceModel 管理侧空间列表模型（带成员数和创建者名称）
type managerSpaceModel struct {
	SpaceModel
	CreatorName string // 创建者名称
	MemberCount int    // 活跃成员数
}

// managerMemberModel 管理侧成员列表模型（带用户名）
type managerMemberModel struct {
	MemberModel
	Name string // 用户名
}

// managerDB 管理后台专用查询
type managerDB struct {
	session *dbr.Session
}

func newManagerDB(session *dbr.Session) *managerDB {
	return &managerDB{session: session}
}

// memberCountJoin 成员数的 LEFT JOIN 派生表，预聚合 status=1 的活跃成员数。
// 相比以往的相关子查询 (SELECT COUNT(*) ... WHERE space_id=s.space_id AND status=1)，
// 派生表只对 space_member 扫一次并按 space_id 分组，再 LEFT JOIN 回 space，
// 和 spacemember_spaceid_status 复合索引配合可走索引覆盖扫描。
const memberCountJoin = `LEFT JOIN (SELECT space_id, COUNT(*) AS cnt FROM space_member WHERE status=1 GROUP BY space_id) mc ON mc.space_id = s.space_id`

// querySpaces 按关键字 + 状态分页查询空间。
// statuses 为空时不按状态过滤，非空时 WHERE s.status IN (statuses...)。
func (d *managerDB) querySpaces(keyword string, statuses []int, pageSize, pageIndex uint64) ([]*managerSpaceModel, error) {
	where, args := buildSpaceListFilter(keyword, statuses)
	query := fmt.Sprintf(`
		SELECT s.*, IFNULL(u.name, '') AS creator_name, IFNULL(mc.cnt, 0) AS member_count
		FROM space s
		LEFT JOIN user u ON u.uid = s.creator
		%s
		WHERE %s
		ORDER BY s.created_at DESC
		LIMIT ? OFFSET ?`, memberCountJoin, where)
	args = append(args, pageSize, (pageIndex-1)*pageSize)

	var list []*managerSpaceModel
	_, err := d.session.SelectBySql(query, args...).Load(&list)
	return list, err
}

// countSpaces 空间总数（与 querySpaces 共用过滤器）
func (d *managerDB) countSpaces(keyword string, statuses []int) (int64, error) {
	where, args := buildSpaceListFilter(keyword, statuses)
	query := "SELECT COUNT(*) FROM space s WHERE " + where
	var count int64
	_, err := d.session.SelectBySql(query, args...).Load(&count)
	return count, err
}

// buildSpaceListFilter 组装 querySpaces / countSpaces 的 WHERE 片段与参数，
// keyword 走 escapeLike，防止 _/%/\ 被当作通配符误匹配。
func buildSpaceListFilter(keyword string, statuses []int) (string, []interface{}) {
	clauses := []string{"1=1"}
	args := make([]interface{}, 0, len(statuses)+3)
	if len(statuses) > 0 {
		clauses = append(clauses, "s.status IN ("+placeholders(len(statuses))+")")
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	if keyword != "" {
		clauses = append(clauses, "(s.name LIKE ?"+likeEscapeClause+" OR s.space_id LIKE ?"+likeEscapeClause+" OR s.creator LIKE ?"+likeEscapeClause+")")
		like := buildLikePattern(keyword)
		args = append(args, like, like, like)
	}
	return strings.Join(clauses, " AND "), args
}

// querySpaceIncludeDisbanded 查询空间（不过滤 status，后台可看已解散空间）。
// err 优先于"未找到"返回，调用方能区分"DB 错误"和"空间不存在"。
//
// 单行查询这里保留相关子查询（而不是共用 querySpaces 的派生表 JOIN），
// 原因：派生表 GROUP BY 必须先物化全量 space_member，对单行查询是浪费；
// 而相关子查询配合 spacemember_spaceid_status 复合索引只扫一个 space_id，更优。
func (d *managerDB) querySpaceIncludeDisbanded(spaceId string) (*managerSpaceModel, error) {
	var m managerSpaceModel
	_, err := d.session.Select(
		"s.*",
		"IFNULL(u.name, '') as creator_name",
		"(SELECT COUNT(*) FROM space_member WHERE space_id=s.space_id AND status=1) as member_count",
	).From(dbr.I("space").As("s")).
		LeftJoin(dbr.I("user").As("u"), "u.uid=s.creator").
		Where("s.space_id=?", spaceId).
		Load(&m)
	if err != nil {
		return nil, err
	}
	if m.SpaceId == "" {
		return nil, nil
	}
	return &m, nil
}

// isUserExists 校验 user 表是否存在该 uid，供管理端代建空间拦截不存在的 creator_uid。
func (d *managerDB) isUserExists(uid string) (bool, error) {
	if uid == "" {
		return false, nil
	}
	var count int
	_, err := d.session.SelectBySql("SELECT COUNT(*) FROM `user` WHERE uid=?", uid).Load(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// forceDisbandSpace 管理员强制解散：标记 space 状态为 0，同时将所有成员置为已移除
// 返回本次真正被移除的成员 UID，供调用方逐个做缓存失效与事件广播。
func (d *managerDB) forceDisbandSpace(spaceId string, operatorUID string) ([]string, error) {
	tx, err := d.session.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()

	now := time.Now()
	// 先用 FOR UPDATE 锁定并读出活跃成员，再动任何 UPDATE。
	//
	// 顺序和锁都是必要的：普通 SELECT 在 REPEATABLE READ 下建立的是快照读，而后面
	// 的 UPDATE 是当前读。两者之间若有成员并发加入，UPDATE 会把他一并置 0，但他不在
	// 快照名单里 —— 于是没有清理工单、没有缓存失效，人被移出了一个已解散的空间，
	// 却还留在该空间的所有群里、IM 群订阅也原封不动。加锁读挡住的是这一种。
	//
	// 它**没有**关掉整个「解散期间并发加入」窗口：gap lock 只是让那次插入等到本事务
	// 提交，之后它照样往一个 status=0 的空间里写一行 status=1，而且没有清理工单
	// （joinPresetGroups 也不复核成员身份，人还可能落进已解散空间的预设群）。
	// 那个孤儿过不了 SpaceMiddleware（谓词带 space.status），所以影响限于群内。
	// 彻底关闭要让加入侧的事务在锁内复核 space.status，记在 follow-up。
	uids, err := lockActiveMemberUIDsTx(tx, spaceId)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Update("space").Set("status", 0).Set("updated_at", now).
		Where("space_id=?", spaceId).Exec(); err != nil {
		return nil, err
	}
	if _, err = tx.Update("space_member").Set("status", 0).Set("updated_at", now).
		Where("space_id=? AND status=1", spaceId).Exec(); err != nil {
		return nil, err
	}
	// 批量入队：本事务正握着 space_member 的 FOR UPDATE 范围锁，逐条 INSERT 会把
	// 上万次往返都压在锁内，期间所有并发加入路径全部阻塞。
	if err := enqueueMemberRemovalCleanupBatchTx(tx, spaceId, uids, operatorUID, MemberRemoveReasonSpaceDisbanded); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return uids, nil
}

// queryMembersAdmin 管理后台查询空间成员（含已移除，支持 keyword）
func (d *managerDB) queryMembersAdmin(spaceId, keyword string, pageSize, pageIndex uint64) ([]*managerMemberModel, error) {
	builder := d.session.Select("sm.*", "IFNULL(u.name,'') as name").
		From(dbr.I("space_member").As("sm")).
		LeftJoin(dbr.I("user").As("u"), "u.uid=sm.uid").
		Where("sm.space_id=?", spaceId)
	if keyword != "" {
		clause, args := memberSearchWhere(keyword)
		builder = builder.Where(clause, args...)
	}
	var list []*managerMemberModel
	_, err := builder.
		OrderDir("sm.role", false).
		OrderAsc("sm.created_at").
		Limit(pageSize).Offset((pageIndex - 1) * pageSize).
		Load(&list)
	return list, err
}

// countMembersAdmin 空间成员总数（含已移除，支持 keyword）
func (d *managerDB) countMembersAdmin(spaceId, keyword string) (int64, error) {
	builder := d.session.Select("COUNT(*)").
		From(dbr.I("space_member").As("sm")).
		LeftJoin(dbr.I("user").As("u"), "u.uid=sm.uid").
		Where("sm.space_id=?", spaceId)
	if keyword != "" {
		clause, args := memberSearchWhere(keyword)
		builder = builder.Where(clause, args...)
	}
	var count int64
	_, err := builder.Load(&count)
	return count, err
}

// updateSpaceStatus 更新空间状态
func (d *managerDB) updateSpaceStatus(spaceId string, status int) error {
	_, err := d.session.Update("space").
		Set("status", status).
		Set("updated_at", time.Now()).
		Where("space_id=?", spaceId).Exec()
	return err
}

// ErrSpaceNotFound 空间不存在（事务内 SELECT FOR UPDATE 未命中）
var ErrSpaceNotFound = errors.New("space not found")

// ErrSpaceDisbandedForUpdate 事务内发现空间已解散，禁止更新基础信息
var ErrSpaceDisbandedForUpdate = errors.New("space already disbanded")

// ErrSpaceBannedForUpdate 事务内发现空间已封禁，且调用方未授权对封禁空间执行更新。
// 用户端 PUT 应映射为 4xx；管理端调用 updateSpaceProfile 时 allowBanned=true，
// 该 sentinel 不会被触发。
var ErrSpaceBannedForUpdate = errors.New("space is banned and caller disallowed banned updates")

// updateSpaceProfile 管理端部分更新空间基础字段。
//
// 用 SELECT ... FOR UPDATE 在事务内锁定 space 行并原子校验存在性 + 非 Disbanded 状态，
// 关闭 handler 层 guard 与 UPDATE 之间的 TOCTOU 窗口：
// 即便 forceDisbandSpace 在 handler 通过 guard 后并发执行，它会阻塞到本事务结束，
// 或本事务的 SELECT 看到 status=Disbanded 并直接返回 ErrSpaceDisbandedForUpdate。
//
// 存在性 / 已解散用 sentinel error 表达，**不依赖 RowsAffected**：
// MySQL 默认 affected_rows 是「真正变更的行数」，对于"新值与旧值完全相同"的幂等请求
// 会返回 0，与"行不存在"无法区分。强制走事务 + 显式校验消除歧义。
//
// 返回 tx 内锁定时刻读到的 pre-update 快照，供调用方做"旧值→新值"的审计日志；
// 由于读取与 UPDATE 在同一事务内串行化，并发更新场景下的 from 值不会 stale。
//
// nil 参数不变更；调用方需保证至少有一个非 nil（否则 no-op，但仍返回快照）。
//
// presetGroupIds 与其他字段一致用 *string 表达"是否变更"，传入字符串作为整体写入
// preset_group_ids 列（运行期解析见 api.go 的 joinPresetGroups）；
// 该参数仅由用户侧 PUT /v1/space/:space_id 使用，管理端目前传 nil。
//
// 状态守卫契约（事务内强制，关闭 handler 层 guard 与 UPDATE 之间的 TOCTOU 窗口）：
//   - SpaceStatusDisbanded 永远拒绝（ErrSpaceDisbandedForUpdate）
//   - SpaceStatusBanned 由 allowBanned 控制：
//     allowBanned=true（管理端）  → 放行，允许对封禁空间执行修复性更新
//     allowBanned=false（用户端）→ 拒绝（ErrSpaceBannedForUpdate）
//
// 用户端 handler 必须传 allowBanned=false：仅在入口用 checkSpaceActive 挡 banned 不够，
// 入口检查与事务之间存在 race 窗口（manager 并发 ban），事务侧必须再挡一次才闭环。
func (d *managerDB) updateSpaceProfile(
	spaceId string,
	name *string,
	description *string,
	logo *string,
	joinMode *int,
	maxUsers *int,
	presetGroupIds *string,
	allowBanned bool,
) (*SpaceModel, error) {
	tx, err := d.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	// SELECT ... FOR UPDATE 锁定整行并取得稳定快照（供审计 from 字段使用）。
	var before SpaceModel
	found, err := tx.SelectBySql(
		"SELECT * FROM space WHERE space_id=? FOR UPDATE",
		spaceId,
	).Load(&before)
	if err != nil {
		return nil, fmt.Errorf("lock space row: %w", err)
	}
	if found == 0 {
		return nil, ErrSpaceNotFound
	}
	if before.Status == SpaceStatusDisbanded {
		return nil, ErrSpaceDisbandedForUpdate
	}
	if !allowBanned && before.Status == SpaceStatusBanned {
		return nil, ErrSpaceBannedForUpdate
	}

	builder := tx.Update("space")
	changed := false
	if name != nil {
		builder = builder.Set("name", *name)
		changed = true
	}
	if description != nil {
		builder = builder.Set("description", *description)
		changed = true
	}
	if logo != nil {
		builder = builder.Set("logo", *logo)
		changed = true
	}
	if joinMode != nil {
		builder = builder.Set("join_mode", *joinMode)
		changed = true
	}
	if maxUsers != nil {
		builder = builder.Set("max_users", *maxUsers)
		changed = true
	}
	if presetGroupIds != nil {
		builder = builder.Set("preset_group_ids", *presetGroupIds)
		changed = true
	}
	if !changed {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return &before, nil
	}
	builder = builder.Set("updated_at", time.Now())
	if _, err := builder.Where("space_id=?", spaceId).Exec(); err != nil {
		return nil, fmt.Errorf("update space profile: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &before, nil
}

// upsertMembers 批量添加/重新激活成员（单一事务，部分失败则全部回滚）
func (d *managerDB) upsertMembers(spaceId string, uids []string) error {
	if len(uids) == 0 {
		return nil
	}
	tx, err := d.session.Begin()
	if err != nil {
		return err
	}
	defer tx.RollbackUnlessCommitted()
	for _, uid := range uids {
		if _, err := tx.InsertBySql(
			"INSERT INTO space_member (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, 0, 1, NOW(), NOW()) "+
				"ON DUPLICATE KEY UPDATE status=1, updated_at=NOW()",
			spaceId, uid,
		).Exec(); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ErrCannotRemoveOwner 拦截删除 owner 的请求，调用方需先转让所有权
var ErrCannotRemoveOwner = errors.New("cannot remove space owner; transfer ownership first")

// spaceMutatorMaxAttempts 限定 space_member 写路径遇死锁时的重试次数。
const spaceMutatorMaxAttempts = 5

// isRetryableTxErr 判定是否为可重试的瞬时事务错误：InnoDB 死锁(1213)或锁等待超时(1205)。
// 与 modules/common space_welcome_config_store.go 的同名判定保持一致。
func isRetryableTxErr(err error) bool {
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Number == 1213 || myErr.Number == 1205
	}
	return false
}

// retryOnDeadlock 对**整事务、全有全无**的写操作做有界重试。
//
// 为什么用重试而不是让每条路径统一加锁顺序（PR #804 round-8 review，两名 reviewer
// 各自在真实 MySQL 上实测确认）：space_member 上有多条会一次锁多行的写路径——
// removeMembersLocked（批量移除）、removeMembersForce（超管强制移除）、
// transferOwnerAdminLocked（转让：**先单行锁住目标、再范围扫描降级**，两段式、非单调）。
// 它们各自的加锁顺序由各自的索引/结构决定，从任何一个调用点都无法统一**对方**的顺序，
// 所以跨路径 AB-BA 死锁靠「我这条走对索引」根治不了——FORCE INDEX 只收窄了锁面、关掉了
// 批量 vs 批量，对批量 vs 转让实测仍会死锁（目标落在批次内时甚至是确定性的）。
//
// 死锁时 InnoDB 已把被选中的事务整体回滚、没有半提交状态，把整个事务重跑一遍即可；
// 幸存的那一方已经完成，重试通常第二次就成功。只能用于幂等或全有全无的操作：这三条
// 写路径都在单事务里全有全无地提交，符合。领域错误（ErrCannotRemoveOwner 等）
// isRetryableTxErr 判否、原样透传，不影响调用方的 errors.Is。
func retryOnDeadlock(fn func() error) error {
	var err error
	for attempt := 1; attempt <= spaceMutatorMaxAttempts; attempt++ {
		if err = fn(); !isRetryableTxErr(err) {
			return err
		}
	}
	return fmt.Errorf("space_member 写操作重试 %d 次仍死锁: %w", spaceMutatorMaxAttempts, err)
}

// removeMembersForce 在单一事务中强制移除成员。
//
// 用 SELECT ... FOR UPDATE 锁定目标行并原子校验 role，若任一 uid 当前是 owner 则整体回滚。
// 这封住了「handler 查询 owner 状态 → DB 更新」之间的 TOCTOU：
// 如果并发的 transferOwnerAdmin 想把 [uids] 中某个成员提升为 owner，它的 UPDATE 会阻塞到本事务结束。
//
// 反向窗口（先本事务删除 → 再被并发 transfer 提升为 owner）由 transferOwnerAdmin 内部的
// `AND status=1` 守卫关掉：本事务 commit 后该 uid 的 status=0，后续 transfer 的 UPDATE 影响 0 行。
// 返回本次真正被移除的 uid，供调用方精确地做失效缓存与事件广播。
func (d *managerDB) removeMembersForce(spaceId string, uids []string, operatorUID string) ([]string, error) {
	var removed []string
	err := retryOnDeadlock(func() error {
		var e error
		removed, e = d.removeMembersForceOnce(spaceId, uids, operatorUID)
		return e
	})
	return removed, err
}

// removeMembersForceOnce 是 removeMembersForce 的单次尝试；死锁重试见调用方。
func (d *managerDB) removeMembersForceOnce(spaceId string, uids []string, operatorUID string) ([]string, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	tx, err := d.session.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()

	var ownerCount int
	if _, err = tx.SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid IN ? AND role=2 AND status=1 FOR UPDATE",
		spaceId, uids,
	).Load(&ownerCount); err != nil {
		return nil, err
	}
	if ownerCount > 0 {
		return nil, ErrCannotRemoveOwner
	}

	now := time.Now()
	removed := make([]string, 0, len(uids))
	for _, uid := range uids {
		result, err := tx.Update("space_member").
			Set("status", 0).
			Set("updated_at", now).
			Where("space_id=? AND uid=? AND status=1", spaceId, uid).Exec()
		if err != nil {
			return nil, err
		}
		// 只给真正被改动的成员行入队。此前这里没有 status=1 谓词，对已移除 / 不存在
		// 的 uid 也会"更新"一次；无谓词地入队会产出永远无事可做的工单，还会让
		// 一次误传的 uid 触发一遍别人的会话面清理。
		affected, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if affected == 0 {
			continue
		}
		if err := enqueueMemberRemovalCleanupTx(tx, spaceId, uid, operatorUID, MemberRemoveReasonForceRemoved); err != nil {
			return nil, err
		}
		removed = append(removed, uid)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return removed, nil
}

// ErrTransferTargetMissing 目标成员不存在或已被移除，不能成为新 owner
var ErrTransferTargetMissing = errors.New("transfer target not found or already removed")

// transferOwnerAdmin 管理端转让所有权，见 transferOwnerAdminLocked。
func (d *managerDB) transferOwnerAdmin(spaceId, newOwnerUID string) error {
	return transferOwnerAdminLocked(d.session, spaceId, newOwnerUID)
}

// transferOwnerAdminLocked 将 newOwner 置为 owner(2)，将当前所有 owner 降为 admin(1)。
// 管理端与用户侧转让共用此原语（PR #339 review：用户侧内联事务缺行锁，
// 目标被并发移除后 UPDATE 影响 0 行仍降级 owner，产生无主空间）。
//
// 事务开始时先用 SELECT ... FOR UPDATE 锁定目标行并确认其 status=1，
// 避免「先降老 owner → 目标被并发 remove → 后续 UPDATE 影响 0 行」导致空间无主。
// 若目标不存在 / 已被移除，整个事务回滚并返回 ErrTransferTargetMissing。
func transferOwnerAdminLocked(sess *dbr.Session, spaceId, newOwnerUID string) error {
	return retryOnDeadlock(func() error {
		return transferOwnerAdminLockedOnce(sess, spaceId, newOwnerUID)
	})
}

// transferOwnerAdminLockedOnce 是 transferOwnerAdminLocked 的单次尝试；死锁重试见调用方。
func transferOwnerAdminLockedOnce(sess *dbr.Session, spaceId, newOwnerUID string) error {
	tx, err := sess.Begin()
	if err != nil {
		return err
	}
	defer tx.RollbackUnlessCommitted()

	var targetCount int
	if _, err = tx.SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1 FOR UPDATE",
		spaceId, newOwnerUID,
	).Load(&targetCount); err != nil {
		return err
	}
	if targetCount == 0 {
		return ErrTransferTargetMissing
	}

	now := time.Now()
	// 先把现有所有 owner 降为 admin（通常只有一个，但防御式写法）
	if _, err = tx.Update("space_member").
		Set("role", 1).Set("updated_at", now).
		Where("space_id=? AND role=2 AND status=1", spaceId).Exec(); err != nil {
		return err
	}
	// 再把目标用户提升为 owner
	if _, err = tx.Update("space_member").
		Set("role", 2).Set("updated_at", now).
		Where("space_id=? AND uid=? AND status=1", spaceId, newOwnerUID).Exec(); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrRemoveHierarchy 操作者未严格高于目标角色，不能移除目标成员
var ErrRemoveHierarchy = errors.New("operator does not outrank removal target")

// removeMemberLocked 在单事务内锁定目标行并重读角色后移除成员。
//
// 目标 role 必须在锁内重读：pre-check 读到非 owner 后，目标可能被并发转让
// 升为 owner，裸 UPDATE 仍会把它移除，产生无主空间——与 transferOwnerAdminLocked
// 防御的是同源的对称竞态（PR #339 review）。
//   - 目标行不存在 / 已移除 → 幂等返回 nil（pre-check 与事务之间被并发移除）；
//   - 目标 role == 2 → ErrCannotRemoveOwner；
//   - 目标 role >= rejectRoleAtOrAbove → ErrRemoveHierarchy
//     （removeMembers 传操作者角色，实现「仅可移除更低角色」；自助退出传 2，仅拦 owner）。
//
// 返回值 removed 表示这次是否真的改动了成员行。调用方据此决定要不要做失效缓存 /
// 广播事件 / 触发清理 —— 「行本来就不存在」与「真的移除了」都返回 nil error，
// 只看 error 分不出来，会对着非成员空跑一整套收尾。
//
// 真正改动了成员行时，会在**同一事务内**写出会话面清理工单
// （transactional outbox，见 enqueueMemberRemovalCleanupTx）：这样成员行提交与
// 清理任务落库同生共死，进程在两者之间崩溃也不会留下"已移除但没清理"的成员。
// 提前返回的三条分支（行不存在 / owner / 角色不够）都没有改动成员行，因此不入队。
func removeMemberLocked(sess *dbr.Session, spaceId, uid string, rejectRoleAtOrAbove int, operatorUID, reason string) (bool, error) {
	tx, err := sess.Begin()
	if err != nil {
		return false, err
	}
	defer tx.RollbackUnlessCommitted()

	var roles []int
	if _, err = tx.SelectBySql(
		"SELECT role FROM space_member WHERE space_id=? AND uid=? AND status=1 FOR UPDATE",
		spaceId, uid,
	).Load(&roles); err != nil {
		return false, err
	}
	if len(roles) == 0 {
		return false, nil
	}
	if roles[0] == 2 {
		return false, ErrCannotRemoveOwner
	}
	if roles[0] >= rejectRoleAtOrAbove {
		return false, ErrRemoveHierarchy
	}
	if _, err = tx.Update("space_member").
		Set("status", 0).Set("updated_at", time.Now()).
		Where("space_id=? AND uid=?", spaceId, uid).Exec(); err != nil {
		return false, err
	}
	if err = enqueueMemberRemovalCleanupTx(tx, spaceId, uid, operatorUID, reason); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// selectMembersForRemovalForUpdateSQL 是 removeMembersLocked 的整批锁定查询。
//
// FORCE INDEX 是**语义性**的，不是性能调优：不 FORCE 时优化器在真实规模上会选
// (space_id, status)，锁住整个空间的活跃行、锁序也乱；FORCE 到 (space_id, uid) 把锁面
// 收敛到目标本身、并让批量 vs 批量同序（同一条语句同一个计划）。
//
// ⚠️ 它**不能**单独消除批量 vs 群主转让的跨路径死锁：transferOwnerAdminLocked 是
// 先单行锁目标、再扫描降级的两段式，非单调，从本调用点无法统一它的顺序（round-8
// 两名 reviewer 实测：FORCE 后目标落在批次内仍确定性死锁）。跨路径死锁由三条写路径
// 共用的 retryOnDeadlock 兜底，见其注释。FORCE 仍然值得留：它把锁面和阻塞窗口收窄了，
// 也让死锁少发一档。
//
// TestRemoveMembersLockedForcesUniqueIndexPlan 用同一个常量断言计划，一旦有人删掉
// FORCE、优化器回退，那条测试立刻变红。
const selectMembersForRemovalForUpdateSQL = "SELECT uid, role FROM space_member " +
	"FORCE INDEX (spacemember_spaceid_uid) " +
	"WHERE space_id=? AND uid IN ? AND status=1 FOR UPDATE"

// spaceMemberRoleRow 承接 removeMembersLocked 的整批锁定查询：uid + 当前 role。
type spaceMemberRoleRow struct {
	UID  string `db:"uid"`
	Role int    `db:"role"`
}

// removeMembersLocked 是 removeMemberLocked 的整批版本：**一个事务**里逐个锁定、
// 重读角色、翻转成员行，最后一次性入队全部清理工单，单次提交。
// 返回真正被改动的 uid 列表，语义与逐个调用 removeMemberLocked 收集 ok=true 一致。
//
// 为什么必须是一个事务，而不是循环调用 removeMemberLocked（PR #804 round-4/5 review）：
//
// 群侧的群主交接通告要靠 HasPendingRemovalCleanup 判断「继任者是不是也在这一批里」
// 来收敛连锁通告，而它只看得见**已提交**的行。逐个提交时，清理 worker 每 10s 一轮、
// 新工单 next_attempt_at=now 立即可认领，一次 tick 落在循环中途就会认领已提交的前缀，
// 而后面几个 uid 的工单行尚不存在 —— 检查把「马上要被移除的继任者」读成「不在队列」，
// 于是发出一条写下时就已作废的「已成为新群主」，NoPersist=0 永久留在群历史里。
// 实测（reason=kicked，群内连续元老 C/S2/S3）：整批预先可见 → 1 条；逐条提交且每轮
// 都被 tick 命中 → 3 条；逐条提交且只有首次提交后一个 tick → 2 条。
//
// 更糟的是它会与另一条既有缺口复合：若最终继任者自己挂着一条 pending 工单（重试退避
// 中的普通状态），最后一环反而被抑制，于是群历史里**最后**一条群主消息指向一个已经
// 不在群里的人，而真正的群主从未被通告 —— 正是本次通告机制要消灭的东西。
//
// 整批一个事务后，同批所有工单行在任何 worker 能认领之前就已全部可见，链条收敛回
// 一条，且通告的是最终群主。这与 removeMembersForce（管理端强制移除）已经在用的形状
// 相同，只是多了 removeMemberLocked 的角色层级校验。
//
// 代价是部分失败语义变了：以前中途 DB 出错会留下已提交的前缀，现在整批回滚。这对
// transactional outbox 反而更干净（成员行与工单同生共死这一点不变），调用方也不必再
// 为「前缀已提交」做补偿收尾。owner 与同级及更高角色仍是**静默跳过**，不是错误。
func removeMembersLocked(sess *dbr.Session, spaceId string, uids []string, rejectRoleAtOrAbove int, operatorUID, reason string) ([]string, error) {
	var removed []string
	err := retryOnDeadlock(func() error {
		var e error
		removed, e = removeMembersLockedOnce(sess, spaceId, uids, rejectRoleAtOrAbove, operatorUID, reason)
		return e
	})
	return removed, err
}

// removeMembersLockedOnce 是 removeMembersLocked 的单次尝试；死锁重试见调用方
// （FORCE INDEX 收窄锁面并关掉批量 vs 批量，但批量 vs 转让/强制移除的跨路径死锁靠
//
//	retryOnDeadlock 兜底，见 retryOnDeadlock 的注释）。
func removeMembersLockedOnce(sess *dbr.Session, spaceId string, uids []string, rejectRoleAtOrAbove int, operatorUID, reason string) ([]string, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	tx, err := sess.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()

	// 一条语句锁住整批，且**强制走唯一索引 (space_id, uid)**（FORCE INDEX）。
	//
	// 为什么必须 FORCE：space_member 上有两条候选索引 —— 唯一键
	// spacemember_spaceid_uid(space_id, uid) 与 spacemember_spaceid_status(space_id,
	// status)。不加 FORCE 时优化器是按代价选的，实测（MySQL 8.0 + ANALYZE）在本端点
	// 真实规模上（250 人空间、200 uid 批）选的是 **spacemember_spaceid_status**：
	// 于是锁按 (space_id, status, id) 顺序、且把该空间**所有** status=1 的行都锁住
	// （250 把，而非 200 个目标），扩大了锁面（挡住并发 joinSpace/leaveSpace/transferOwner）。
	// FORCE INDEX 把计划钉成 range on (space_id, uid)：只锁 200 个目标、按 uid 序，锁面
	// 收敛到目标本身，并让**批量 vs 批量**同序（同一条语句同一个计划，无法互相成环）。
	//   - 只 ORDER BY uid **不行**：锁在扫描期就获取，排序发生在其后。
	//   - 逐个 FOR UPDATE 也不行：那会引回按调用方顺序持多把锁的批次间 AB-BA。
	// SQL 抽成常量 selectMembersForRemovalForUpdateSQL，与计划断言测试共用，防止漂移。
	//
	// ⚠️ FORCE INDEX **不足以**消除批量 vs 群主转让的跨路径死锁：transferOwnerAdminLocked
	// 先单行锁目标、再扫描降级，是两段式非单调获取，目标落在批次内时仍确定性死锁
	// （round-8 两名 reviewer 实测）。那一类由外层 retryOnDeadlock 兜底。这里 FORCE 的
	// 价值是收窄锁面/阻塞窗口、并关掉批量 vs 批量，不是根治跨路径。
	//
	// 角色重读仍在锁内：这条 SELECT 把每个成员的当前 role 连同锁一起取回，随后在 Go
	// 里做层级判定，语义与 removeMemberLocked 逐个重读一致（防止 pre-check 之后目标被
	// 并发转让升为 owner 仍被移除）。
	//
	// 对不存在的 uid 仍会在唯一索引上取间隙锁，与逐个 FOR UPDATE 同量，单语句只是一次取全。
	var locked []spaceMemberRoleRow
	if _, err = tx.SelectBySql(selectMembersForRemovalForUpdateSQL, spaceId, uids).Load(&locked); err != nil {
		return nil, err
	}
	roleByUID := make(map[string]int, len(locked))
	for _, r := range locked {
		roleByUID[r.UID] = r.Role
	}

	now := time.Now()
	removed := make([]string, 0, len(uids))
	seen := make(map[string]struct{}, len(uids))
	for _, uid := range uids {
		// 重复 uid 只处理一次，与老实现「第二次读不到已翻转的行」等价。
		if _, dup := seen[uid]; dup {
			continue
		}
		seen[uid] = struct{}{}
		role, active := roleByUID[uid]
		// 三种「不该动」的情形静默跳过，与既有 removeMembers 的观察行为一致
		// （那边是 ErrCannotRemoveOwner / ErrRemoveHierarchy 被调用方 continue 掉、
		// ok=false 不计入 removed）：不在空间、owner、同级及更高。
		if !active || role == 2 || role >= rejectRoleAtOrAbove {
			continue
		}
		// 更新的是上面 FOR UPDATE 已经锁住的行，不额外获取新锁，不影响获取顺序。
		if _, err = tx.Update("space_member").
			Set("status", 0).Set("updated_at", now).
			Where("space_id=? AND uid=?", spaceId, uid).Exec(); err != nil {
			return nil, err
		}
		removed = append(removed, uid)
	}

	// 只给真正被改动的成员行入队，且与那些翻转同事务提交（transactional outbox）。
	// 用多值 INSERT 的批量版本而不是逐条 enqueueMemberRemovalCleanupTx：锁内往返
	// 从 N 次压到 N/200 次，理由与 enqueueMemberRemovalCleanupBatchTx 自身的注释相同。
	if len(removed) > 0 {
		if err = enqueueMemberRemovalCleanupBatchTx(tx, spaceId, removed, operatorUID, reason); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return removed, nil
}

// queryInvitesAdmin 分页查询空间所有邀请码（含已禁用）
func (d *managerDB) queryInvitesAdmin(spaceId string, pageSize, pageIndex uint64) ([]*InvitationModel, error) {
	var list []*InvitationModel
	_, err := d.session.Select("*").From("space_invitation").
		Where("space_id=?", spaceId).
		OrderDir("created_at", false).
		Limit(pageSize).Offset((pageIndex - 1) * pageSize).
		Load(&list)
	return list, err
}

// countInvitesAdmin 空间邀请码总数
func (d *managerDB) countInvitesAdmin(spaceId string) (int64, error) {
	var count int64
	_, err := d.session.Select("COUNT(*)").From("space_invitation").
		Where("space_id=?", spaceId).Load(&count)
	return count, err
}

// disableInvitation 将邀请码置为无效
func (d *managerDB) disableInvitation(spaceId, code string) (int64, error) {
	result, err := d.session.Update("space_invitation").
		Set("status", 0).Set("updated_at", time.Now()).
		Where("space_id=? AND invite_code=?", spaceId, code).Exec()
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// updateInvitationAdmin 管理端可修改 max_uses / expires_at / status，nil 字段不变更。
// 返回 affected rows，0 表示记录不存在。
//
// 有意设计：WHERE 不限制 status，管理员可以对已禁用（status=0）的邀请码执行 PUT，
// 包括通过 {"status": 1} 重新启用——这是管理操作的必要能力（如误禁恢复）。
// 若要禁止重新启用，应在 API 层决策，不在此函数加 AND status=1。
func (d *managerDB) updateInvitationAdmin(spaceId, code string, maxUses *int, expiresAt *time.Time, status *int) (int64, error) {
	builder := d.session.Update("space_invitation")
	changed := false
	if maxUses != nil {
		builder = builder.Set("max_uses", *maxUses)
		changed = true
	}
	if expiresAt != nil {
		builder = builder.Set("expires_at", *expiresAt)
		changed = true
	}
	if status != nil {
		builder = builder.Set("status", *status)
		changed = true
	}
	if !changed {
		return 0, nil
	}
	builder = builder.Set("updated_at", time.Now())
	result, err := builder.Where("space_id=? AND invite_code=?", spaceId, code).Exec()
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// queryJoinAppliesAdmin 管理后台查询申请列表，status<0 表示不过滤
func (d *managerDB) queryJoinAppliesAdmin(spaceId string, status int, pageSize, pageIndex uint64) ([]*spaceJoinApplyDetailModel, error) {
	builder := d.session.Select("a.*", "IFNULL(u.name,'') as applicant_name").
		From(dbr.I("space_join_apply").As("a")).
		LeftJoin(dbr.I("user").As("u"), "u.uid=a.uid").
		Where("a.space_id=?", spaceId)
	if status >= 0 {
		builder = builder.Where("a.status=?", status)
	}
	var list []*spaceJoinApplyDetailModel
	_, err := builder.
		OrderDir("a.created_at", false).
		Limit(pageSize).Offset((pageIndex - 1) * pageSize).
		Load(&list)
	return list, err
}

// countJoinAppliesAdmin 申请总数
func (d *managerDB) countJoinAppliesAdmin(spaceId string, status int) (int64, error) {
	builder := d.session.Select("COUNT(*)").From("space_join_apply").Where("space_id=?", spaceId)
	if status >= 0 {
		builder = builder.Where("status=?", status)
	}
	var count int64
	_, err := builder.Load(&count)
	return count, err
}
