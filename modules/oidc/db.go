package oidc

import (
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/gocraft/dbr/v2"
)

// ErrAlreadyRevoked 旧 RT 已被吊销;RotateRefresh 检测到并发竞争(另一 worker 抢先轮换)时返回
var ErrAlreadyRevoked = errors.New("oidc: refresh token already revoked")

// DB OIDC 模块数据访问层
type DB struct {
	session *dbr.Session
}

// NewDB 构造 DB
func NewDB(ctx *config.Context) *DB {
	return &DB{session: ctx.DB()}
}

// ---------- user_oidc_identity ----------

// QueryIdentityExact 通过 (issuer, subject) 查询绑定关系,并对返回行做**逐字节复核**。
//
// 这是包外唯一可用的入口,原始查询刻意不导出 —— 因为原始查询单独用是不安全的:
// 本表 COLLATE 是 utf8mb4_general_ci,`WHERE subject='ABC'` 会命中一行
// subject='abc' 的记录,于是两个只差大小写的上游主体落到同一个 uid。在登录路径
// 上那是账号接管;在 modules/integration 的认证中间件上更重 —— 它据此签发
// API key。
//
// 曾经复核只装在 modules/oidc 的 adapter 里,而 modules/integration 直连了原始
// 查询,把这个口子开了一半。改成"包外只能拿到复核版"之后,这类绕过是**编译期**
// 挡住的,不依赖谁记得加守卫。
//
// 三种结果必须区分:
//   - 逐字节命中 → 返回该行;
//   - 确实没有行 → (nil, nil),即"首次登录";
//   - ci 命中但不是逐字节相等 → ErrIdentityCaseCollision。
//
// 第三种曾经也报 (nil, nil)。那样 ResolveOrLink 会回 IsNew=true,调用方随即
// IssueSession(CreateUser=true) 把用户行建出来,之后 Insert 才撞 1062,竞态恢复
// 走同一个逐字节查询、同样看不见 —— 每次登录尝试留一个孤立 user 行然后永久失败。
// 现在拒绝落在 ResolveOrLink 里,在任何副作用之前。
// 也就是把静默合并换成响亮拒绝。
func (d *DB) QueryIdentityExact(issuer, subject string) (*IdentityModel, error) {
	row, err := d.queryIdentityByIssuerSubject(issuer, subject)
	if err != nil {
		return nil, err
	}
	if row != nil && !identityRowMatches(row, issuer, subject) {
		// ci 查询命中了,但不是逐字节相等 —— 折叠碰撞。
		//
		// **不能报成"没查到"**:那会让 ResolveOrLink 回 IsNew=true,调用方随即
		// IssueSession(CreateUser=true) 把用户行建出来,之后 identity Insert 才撞上
		// ci 唯一键报 1062,竞态恢复又走同一个逐字节查询、同样看不见,于是拒绝 ——
		// 每次尝试留一个孤立 user 行,永久失败,可无限重复。见 ErrIdentityCaseCollision。
		return nil, fmt.Errorf("%w: issuer/subject collide with an existing row",
			ErrIdentityCaseCollision)
	}
	if row == nil {
		// 真的没有行 —— 首次登录,与碰撞必须区分,否则每个新用户都登不进来。
		return nil, nil
	}
	return row, nil
}

// queryIdentityByIssuerSubject 通过 (issuer, sub) 查询绑定关系。
//
// **不导出**:ci collation 下它会命中大小写不同的行,单独使用不安全。
// 包外走 QueryIdentityExact。
//
// 未命中返回 (nil, nil),与项目其他模块的单条查询语义一致。
// 调用方通过 m == nil && err == nil 判定"记录不存在"。
func (d *DB) queryIdentityByIssuerSubject(issuer, subject string) (*IdentityModel, error) {
	var m *IdentityModel
	if _, err := d.session.Select("*").From("user_oidc_identity").
		Where("issuer=? AND subject=?", issuer, subject).
		Load(&m); err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, fmt.Errorf("oidc: query identity by issuer=%q subject=%q: %w", issuer, subject, err)
	}
	return m, nil
}

// QueryIdentitiesByEmail 通过邮箱查询(用于自动绑定时检测冲突)
func (d *DB) QueryIdentitiesByEmail(issuer, email string) ([]*IdentityModel, error) {
	var list []*IdentityModel
	if _, err := d.session.Select("*").From("user_oidc_identity").
		Where("issuer=? AND email=? AND email<>''", issuer, email).
		Load(&list); err != nil {
		return nil, fmt.Errorf("oidc: query identities by email: %w", err)
	}
	return list, nil
}

// QueryIdentitiesByUID 查询某个 UID 已绑定的所有第三方身份
func (d *DB) QueryIdentitiesByUID(uid string) ([]*IdentityModel, error) {
	var list []*IdentityModel
	if _, err := d.session.Select("*").From("user_oidc_identity").
		Where("uid=?", uid).
		Load(&list); err != nil {
		return nil, fmt.Errorf("oidc: query identities by uid=%q: %w", uid, err)
	}
	return list, nil
}

// InsertIdentity 新增绑定关系
//
// LinkedAt 为零值时主动填上当前时间。util.AttrToUnderscore 会把所有字段塞进
// Columns,Go 的 time.Time 零值是 0001-01-01,会覆盖 SQL 的 CURRENT_TIMESTAMP
// 默认值 — 这里显式补齐才能拿到有意义的时间戳。
func (d *DB) InsertIdentity(m *IdentityModel) error {
	if m.LinkedAt.IsZero() {
		m.LinkedAt = time.Now()
	}
	if _, err := d.session.InsertInto("user_oidc_identity").
		Columns(util.AttrToUnderscore(m)...).
		Record(m).Exec(); err != nil {
		return fmt.Errorf("oidc: insert identity: %w", err)
	}
	return nil
}

// UpdateIdentityLogin 更新最近登录时间与最新 claims 字段
func (d *DB) UpdateIdentityLogin(id int64, email string, emailVerified int, phone string, phoneVerified int) error {
	if _, err := d.session.Update("user_oidc_identity").
		SetMap(map[string]interface{}{
			"email":          email,
			"email_verified": emailVerified,
			"phone":          phone,
			"phone_verified": phoneVerified,
			"last_login_at":  time.Now(),
		}).
		Where("id=?", id).Exec(); err != nil {
		return fmt.Errorf("oidc: update identity login id=%d: %w", id, err)
	}
	return nil
}

// ---------- user_oidc_refresh ----------

// QueryRefreshByHash 通过 token_hash 查询(命中即代表本条 RT 仍有效)
//
// 未命中返回 (nil, nil),调用方通过 m == nil && err == nil 判定"记录不存在"。
func (d *DB) QueryRefreshByHash(hash string) (*RefreshModel, error) {
	var m *RefreshModel
	if _, err := d.session.Select("*").From("user_oidc_refresh").
		Where("token_hash=?", hash).
		Load(&m); err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, fmt.Errorf("oidc: query refresh by hash: %w", err)
	}
	return m, nil
}

// InsertRefresh 新增 RT
func (d *DB) InsertRefresh(m *RefreshModel) error {
	if err := insertRefreshRow(d.session.InsertBySql, m); err != nil {
		return fmt.Errorf("oidc: insert refresh: %w", err)
	}
	return nil
}

// MarkRefreshRevoked 标记吊销,返回真正改动的行数(0 表示已被其他 worker 抢先吊销)。
//
// 幂等语义保留:对已吊销的 id 再次调用不报错。返回 rowsAffected 是为了让多实例
// SyncWorker 区分"我刚把 RT 吊销了"(=1)与"别人抢先一步"(=0)—— 后者表示
// 当前 invalid_grant 只是 IdP 端 RT 旋转后的副产品(其他实例已成功 rotate),
// 不应踢用户,否则会出现"两实例同时跑就乱踢"的假阳性。
func (d *DB) MarkRefreshRevoked(id int64) (int64, error) {
	res, err := d.session.Update("user_oidc_refresh").
		Set("revoked_at", time.Now()).
		Where("id=? AND revoked_at IS NULL", id).Exec()
	if err != nil {
		return 0, fmt.Errorf("oidc: mark refresh revoked id=%d: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RotateRefresh 用新 RT 替换旧 RT(成功刷新后调用)
//
// 旧 RT 的 revoke 走 "WHERE id=? AND revoked_at IS NULL" + RowsAffected 检查,
// 在并发场景下另一 worker 已轮换过该 RT 时返回 ErrAlreadyRevoked,避免重复轮换。
func (d *DB) RotateRefresh(oldID int64, newRT *RefreshModel) error {
	tx, err := d.session.Begin()
	if err != nil {
		return fmt.Errorf("oidc: rotate refresh begin tx: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	res, err := tx.Update("user_oidc_refresh").
		Set("revoked_at", time.Now()).
		Where("id=? AND revoked_at IS NULL", oldID).Exec()
	if err != nil {
		return fmt.Errorf("oidc: rotate refresh revoke old id=%d: %w", oldID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("oidc: rotate refresh rows affected id=%d: %w", oldID, err)
	}
	if affected == 0 {
		return ErrAlreadyRevoked
	}
	if err := insertRefreshRow(tx.InsertBySql, newRT); err != nil {
		return fmt.Errorf("oidc: rotate refresh insert new: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("oidc: rotate refresh commit: %w", err)
	}
	return nil
}

// insertRefreshRow 显式列表插入。
//
// 不能用 util.AttrToUnderscore + Record:它跳 reflect.Struct 字段(time.Time / db.Time
// 都中招),expires_at 没有 schema 默认值会导致 INSERT 失败。集成测试发现的真 bug。
type insertBySql func(query string, value ...interface{}) *dbr.InsertStmt

func insertRefreshRow(insert insertBySql, m *RefreshModel) error {
	_, err := insert(`INSERT INTO user_oidc_refresh
		(identity_id, token_hash, token_ciphertext, expires_at)
		VALUES (?, ?, ?, ?)`,
		m.IdentityID, m.TokenHash, m.TokenCiphertext, m.ExpiresAt,
	).Exec()
	return err
}

// DueRefreshes 拉一批待刷新 RT,JOIN identity 把 uid 一并取出 —— invalid_grant
// 时 worker 直接拿 uid 调踢线,不需要再查一次 identity 表。
//
// 排序:最久未刷新的优先(`COALESCE(last_refreshed_at, created_at)`)。
//
// 索引提示:`COALESCE(...)` 上无 B-tree 索引,filesort 在小数据量(P0 上线初期
// 预计 < 1k active RT)可接受。RT 量明显增长(>10k)后,按需:
//  1. 加复合索引 `(revoked_at, last_refreshed_at, created_at)` 让 WHERE +
//     ORDER BY 局部覆盖;或
//  2. 改成应用层维护"下次该刷新时间"列(避免 COALESCE),用单列索引兜住。
//
// JOIN 到 identity 走主键,负担可忽略。
func (d *DB) DueRefreshes(limit int) ([]*DueRefresh, error) {
	if limit <= 0 {
		return nil, nil
	}
	var list []*DueRefresh
	if _, err := d.session.SelectBySql(`
		SELECT r.id, r.identity_id, i.uid, i.subject,
		       r.token_ciphertext, r.expires_at
		FROM user_oidc_refresh r
		JOIN user_oidc_identity i ON i.id = r.identity_id
		WHERE r.revoked_at IS NULL AND r.expires_at > ?
		ORDER BY COALESCE(r.last_refreshed_at, r.created_at) ASC
		LIMIT ?`,
		time.Now(), limit,
	).Load(&list); err != nil {
		return nil, fmt.Errorf("oidc: due refreshes: %w", err)
	}
	return list, nil
}

// RevokeRefreshByUID 把 uid 名下所有未吊销 RT 标记吊销(logout 用)。
//
// 两步走是因为 dbr 的 Update builder 不支持 JOIN;先查 identity_id 再批量 IN。
// 没有 identity 行时直接返回 0,不触发空 IN(...) 子句。
func (d *DB) RevokeRefreshByUID(uid string) (int64, error) {
	if uid == "" {
		return 0, nil
	}
	var ids []int64
	if _, err := d.session.Select("id").From("user_oidc_identity").
		Where("uid=?", uid).Load(&ids); err != nil {
		return 0, fmt.Errorf("oidc: query identities by uid=%q: %w", uid, err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	res, err := d.session.Update("user_oidc_refresh").
		Set("revoked_at", time.Now()).
		Where("revoked_at IS NULL AND identity_id IN ?", ids).Exec()
	if err != nil {
		return 0, fmt.Errorf("oidc: revoke refresh by uid=%q: %w", uid, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ---------- 跨模块只读查询 ----------

// QueryUIDsByEmail 按邮箱查 dmwork user 表的 uid 列表(用于自动绑定时检测多匹配冲突)。
//
// 过滤条件:
//   - is_destroy=0:排除冷静期(1)和已注销(2),与 user.VerifyPasswordByUID
//     的 IsDestroyDone 检查同源
//   - status<>0:排除被运维封禁/停用的账号。autolink 命中停用 uid 会让 IssueSession
//     在终态拒绝,但 OIDC bind 路径会在 confirm 时已经写完 user_oidc_identity,
//     残留脏数据让该用户后续 OIDC 登录持续失败
//
// TODO: user.IService 后续应暴露 QueryUIDsByEmail,届时本方法迁移到 user 模块,
// oidc 改走接口避免跨模块 SQL 耦合。
func (d *DB) QueryUIDsByEmail(email string) ([]string, error) {
	if email == "" {
		return nil, nil
	}
	var uids []string
	if _, err := d.session.Select("uid").From("user").
		Where("email=? AND email<>'' AND is_destroy=0 AND status<>0", email).
		Load(&uids); err != nil {
		return nil, fmt.Errorf("oidc: query users by email: %w", err)
	}
	return uids, nil
}

// QueryUIDsByPhone 按手机号查 dmwork user 表的 uid 列表(同 QueryUIDsByEmail)。
// 同样过滤 is_destroy=0 AND status<>0 —— 见 QueryUIDsByEmail godoc。
//
// 过渡期必须**合并**盲索引与明文两路结果再去重，不能"盲索引命中就提前返回"：
// 手机号加密第一阶的存量回填是渐进的，(zone, phone) 又不是强唯一约束（历史脏数据可能
// 有重复行，见 bind_service.go 的注释）。若一个重复账号已回填、另一个未回填，
// 提前返回会只给出一个 uid —— 调用方 (BindLocator) 靠 len(uids) 判唯一性，
// 于是"多账号冲突"被误判成"唯一账号"，SMS 绑定会把 OIDC 身份挂到其中一个账号上。
// 回填收敛后（remaining 归零）可以去掉明文那一路。
func (d *DB) QueryUIDsByPhone(zone, phone string) ([]string, error) {
	if phone == "" {
		return nil, nil
	}
	seen := make(map[string]struct{})
	uids := make([]string, 0, 2)
	appendUnique := func(found []string) {
		for _, uid := range found {
			if _, dup := seen[uid]; dup {
				continue
			}
			seen[uid] = struct{}{}
			uids = append(uids, uid)
		}
	}

	if hash, err := user.PhoneBlindHash(zone, phone); err == nil {
		var byHash []string
		if _, err := d.session.Select("uid").From("user").
			Where("phone_hash=? AND is_destroy=0 AND status<>0", hash).
			Load(&byHash); err != nil {
			return nil, fmt.Errorf("oidc: query users by phone hash: %w", err)
		}
		appendUnique(byHash)
	}

	var byPlaintext []string
	if _, err := d.session.Select("uid").From("user").
		Where("zone=? AND phone=? AND phone<>'' AND is_destroy=0 AND status<>0", zone, phone).
		Load(&byPlaintext); err != nil {
		return nil, fmt.Errorf("oidc: query users by phone: %w", err)
	}
	appendUnique(byPlaintext)

	if len(uids) == 0 {
		return nil, nil
	}
	return uids, nil
}

// ---------- oidc_audit_log ----------

// InsertAudit 写入审计日志
func (d *DB) InsertAudit(m *AuditModel) error {
	if _, err := d.session.InsertInto("oidc_audit_log").
		Columns(util.AttrToUnderscore(m)...).
		Record(m).Exec(); err != nil {
		return fmt.Errorf("oidc: insert audit: %w", err)
	}
	return nil
}
