package oidc

import (
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
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

// QueryIdentityByIssuerSubject 通过 (issuer, sub) 查询绑定关系
//
// 未命中返回 (nil, nil),与项目其他模块的单条查询语义一致。
// 调用方通过 m == nil && err == nil 判定"记录不存在"。
func (d *DB) QueryIdentityByIssuerSubject(issuer, subject string) (*IdentityModel, error) {
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

// QueryRefreshDueForSync 拉取需要刷新的 RT(未吊销 + 即将过期或最久未刷新)
//
// 当前 ORDER BY COALESCE(last_refreshed_at, created_at) 无覆盖索引,小数据量
// 下 filesort 可接受。当 active RT 行数明显增长后(P1.3 上线后量化),按需添加
// idx_sync_order(revoked_at, last_refreshed_at, created_at) 复合索引,或在
// 应用层缓存 cursor 做增量轮询。本期 scaffold 不预先下注。
func (d *DB) QueryRefreshDueForSync(limit int) ([]*RefreshModel, error) {
	if limit <= 0 {
		// 防御负数 / 零:负数 uint64 转换会得到极大值导致全表扫描
		return nil, nil
	}
	var list []*RefreshModel
	if _, err := d.session.Select("*").From("user_oidc_refresh").
		Where("revoked_at IS NULL AND expires_at > ?", time.Now()).
		OrderAsc("COALESCE(last_refreshed_at, created_at)").
		Limit(uint64(limit)).
		Load(&list); err != nil {
		return nil, fmt.Errorf("oidc: query refresh due for sync: %w", err)
	}
	return list, nil
}

// InsertRefresh 新增 RT
func (d *DB) InsertRefresh(m *RefreshModel) error {
	if _, err := d.session.InsertInto("user_oidc_refresh").
		Columns(util.AttrToUnderscore(m)...).
		Record(m).Exec(); err != nil {
		return fmt.Errorf("oidc: insert refresh: %w", err)
	}
	return nil
}

// MarkRefreshRevoked 标记吊销
//
// 幂等语义:对已吊销的 id 再次调用不报错(WHERE 加 revoked_at IS NULL 过滤),
// 与 logout / 异常清理路径的"多次调用"诉求一致。需要严格"是否真正吊销"语义
// 的并发竞态检测见 RotateRefresh + ErrAlreadyRevoked。
func (d *DB) MarkRefreshRevoked(id int64) error {
	if _, err := d.session.Update("user_oidc_refresh").
		Set("revoked_at", time.Now()).
		Where("id=? AND revoked_at IS NULL", id).Exec(); err != nil {
		return fmt.Errorf("oidc: mark refresh revoked id=%d: %w", id, err)
	}
	return nil
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
	if _, err := tx.InsertInto("user_oidc_refresh").
		Columns(util.AttrToUnderscore(newRT)...).
		Record(newRT).Exec(); err != nil {
		return fmt.Errorf("oidc: rotate refresh insert new: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("oidc: rotate refresh commit: %w", err)
	}
	return nil
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
