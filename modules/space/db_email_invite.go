package space

import (
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/db"
	"github.com/gocraft/dbr/v2"
)

// insertEmailInvite 写入一条邮件邀请，返回自增 ID。
// 同时把 m.Id / m.CreatedAt / m.UpdatedAt 回填进 m，便于调用方直接序列化响应（避免零值时间）。
func (d *DB) insertEmailInvite(m *spaceEmailInviteModel) (int64, error) {
	var expires interface{}
	if m.ExpiresAt != nil {
		expires = time.Time(*m.ExpiresAt)
	}
	now := time.Now()
	result, err := d.session.InsertInto("space_email_invite").
		Columns(
			"token_hash", "invite_type", "email", "space_id", "role",
			"planned_name", "planned_description", "planned_logo",
			"planned_max_users", "planned_join_mode",
			"status", "expires_at", "created_by", "created_at", "updated_at",
		).
		Values(
			m.TokenHash, m.InviteType, m.Email, m.SpaceId, m.Role,
			m.PlannedName, m.PlannedDescription, m.PlannedLogo,
			m.PlannedMaxUsers, m.PlannedJoinMode,
			m.Status, expires, m.CreatedBy, now, now,
		).Exec()
	if err != nil {
		return 0, fmt.Errorf("insert space_email_invite: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert space_email_invite last_insert_id: %w", err)
	}
	m.Id = id
	stamp := db.Time(now)
	m.CreatedAt = stamp
	m.UpdatedAt = stamp
	return id, nil
}

// queryEmailInviteByTokenHash 按 token_hash 精确查找。
func (d *DB) queryEmailInviteByTokenHash(tokenHash string) (*spaceEmailInviteModel, error) {
	var m spaceEmailInviteModel
	_, err := d.session.Select("*").From("space_email_invite").
		Where("token_hash=?", tokenHash).Load(&m)
	if err != nil {
		return nil, fmt.Errorf("query space_email_invite by token_hash: %w", err)
	}
	if m.Id == 0 {
		return nil, nil
	}
	return &m, nil
}

// queryEmailInviteByID 按主键查找。
func (d *DB) queryEmailInviteByID(id int64) (*spaceEmailInviteModel, error) {
	var m spaceEmailInviteModel
	_, err := d.session.Select("*").From("space_email_invite").
		Where("id=?", id).Load(&m)
	if err != nil {
		return nil, fmt.Errorf("query space_email_invite by id: %w", err)
	}
	if m.Id == 0 {
		return nil, nil
	}
	return &m, nil
}

// listEmailInvitesByCreator 列出某发起人创建的邀请（带类型 + 可选状态过滤）。
// status 传 -1 表示不过滤。
func (d *DB) listEmailInvitesByCreator(createdBy string, inviteType, status, limit, offset int) ([]*spaceEmailInviteModel, int64, error) {
	return d.listEmailInvites(
		"created_by=? AND invite_type=?", []interface{}{createdBy, inviteType},
		status, limit, offset,
	)
}

// listEmailInvitesBySpace 列出某空间的 member 类型邀请（可选状态过滤）。
func (d *DB) listEmailInvitesBySpace(spaceId string, status, limit, offset int) ([]*spaceEmailInviteModel, int64, error) {
	return d.listEmailInvites(
		"space_id=? AND invite_type=?", []interface{}{spaceId, EmailInviteTypeMember},
		status, limit, offset,
	)
}

func (d *DB) listEmailInvites(whereSQL string, whereArgs []interface{}, status, limit, offset int) ([]*spaceEmailInviteModel, int64, error) {
	args := append([]interface{}{}, whereArgs...)
	if status >= 0 {
		whereSQL += " AND status=?"
		args = append(args, status)
	}

	var count int64
	countSQL := "SELECT COUNT(*) FROM space_email_invite WHERE " + whereSQL
	if _, err := d.session.SelectBySql(countSQL, args...).Load(&count); err != nil {
		return nil, 0, fmt.Errorf("count space_email_invite: %w", err)
	}

	var models []*spaceEmailInviteModel
	listArgs := append([]interface{}{}, args...)
	listArgs = append(listArgs, limit, offset)
	listSQL := "SELECT * FROM space_email_invite WHERE " + whereSQL + " ORDER BY id DESC LIMIT ? OFFSET ?"
	if _, err := d.session.SelectBySql(listSQL, listArgs...).Load(&models); err != nil {
		return nil, 0, fmt.Errorf("list space_email_invite: %w", err)
	}
	return models, count, nil
}

// revokeEmailInvite 仅允许将 pending 邀请置为 revoked；返回受影响行数。
func (d *DB) revokeEmailInvite(id int64) (int64, error) {
	result, err := d.session.Update("space_email_invite").
		Set("status", EmailInviteStatusRevoked).
		Set("updated_at", time.Now()).
		Where("id=? AND status=?", id, EmailInviteStatusPending).Exec()
	if err != nil {
		return 0, fmt.Errorf("revoke space_email_invite: %w", err)
	}
	return result.RowsAffected()
}

// queryUserEmail 读取用户邮箱（按 uid）。不存在返回空串。
func (d *DB) queryUserEmail(uid string) (string, error) {
	var email string
	_, err := d.session.SelectBySql("SELECT IFNULL(email,'') FROM `user` WHERE uid=?", uid).Load(&email)
	if err != nil {
		return "", fmt.Errorf("query user email: %w", err)
	}
	return email, nil
}

// queryUserName 读取用户名（按 uid）。不存在返回空串。
func (d *DB) queryUserName(uid string) (string, error) {
	var name string
	_, err := d.session.SelectBySql("SELECT IFNULL(name,'') FROM `user` WHERE uid=?", uid).Load(&name)
	if err != nil {
		return "", fmt.Errorf("query user name: %w", err)
	}
	return name, nil
}

// queryActiveMemberCount 统计 space 当前活跃成员数。
func (d *DB) queryActiveMemberCount(spaceId string) (int, error) {
	var count int
	_, err := d.session.SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND status=1", spaceId,
	).Load(&count)
	if err != nil {
		return 0, fmt.Errorf("count active members: %w", err)
	}
	return count, nil
}

// rollbackConsumedEmailInvite 把已 consumed 的邀请回滚到 pending，并清空 consumed_by/consumed_at。
// 仅用于 member accept 路径在 join 失败时的最终一致性补偿。WHERE 含 consumed_by 防止误回滚他人。
func (d *DB) rollbackConsumedEmailInvite(id int64, consumedBy string) error {
	_, err := d.session.UpdateBySql(
		"UPDATE space_email_invite SET status=?, consumed_by='', consumed_at=NULL, updated_at=NOW() "+
			"WHERE id=? AND status=? AND consumed_by=?",
		EmailInviteStatusPending, id, EmailInviteStatusConsumed, consumedBy,
	).Exec()
	if err != nil {
		return fmt.Errorf("rollback consumed space_email_invite: %w", err)
	}
	return nil
}

// consumeEmailInviteTx 在事务内原子地将 pending 邀请消费，附带过期检查；返回受影响行数。
// 上层需要根据返回的行数决定是继续后续创建/加入流程，还是回滚事务。
func (d *DB) consumeEmailInviteTx(tx *dbr.Tx, id int64, consumedBy string) (int64, error) {
	now := time.Now()
	result, err := tx.UpdateBySql(
		"UPDATE space_email_invite SET status=?, consumed_by=?, consumed_at=?, updated_at=? "+
			"WHERE id=? AND status=? AND (expires_at IS NULL OR expires_at > ?)",
		EmailInviteStatusConsumed, consumedBy, now, now,
		id, EmailInviteStatusPending, now,
	).Exec()
	if err != nil {
		return 0, fmt.Errorf("consume space_email_invite: %w", err)
	}
	return result.RowsAffected()
}
