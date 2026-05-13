package conversation_ext

import (
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/gocraft/dbr/v2"
)

// followVersionTable 是 user_follow_version 表名。
const followVersionTable = "user_follow_version"

// FollowVersionDB 提供对 user_follow_version 表的访问。
//
// PR review (Round 3) Blocking #1/#2 — 这个用户级单调序列号取代了
// 原先的 per-row user_conversation_ext.version，
// 用来表达 follow 状态的全部变化（follow / unfollow / sort / category）。
type FollowVersionDB struct {
	session *dbr.Session
	log.Log
}

// NewFollowVersionDB 构造 FollowVersionDB。
func NewFollowVersionDB(ctx *config.Context) *FollowVersionDB {
	return &FollowVersionDB{
		session: ctx.DB(),
		Log:     log.NewTLog("FollowVersionDB"),
	}
}

// Get 返回 (uid, space_id) 当前的 version。行不存在时返回 0。
// 只读路径，不取锁。
func (d *FollowVersionDB) Get(uid, spaceID string) (int64, error) {
	var v int64
	err := d.session.SelectBySql(
		"SELECT version FROM "+followVersionTable+" WHERE uid=? AND space_id=?",
		uid, spaceID,
	).LoadOne(&v)
	if err == dbr.ErrNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("follow_version get: %w", err)
	}
	return v, nil
}

// LockTx 在 tx 内对 (uid, space_id) 行执行 SELECT ... FOR UPDATE。
// 行不存在时先 INSERT IGNORE 初始化（version=0）再锁。
// 用于 UpdateSort 的 CAS 判定。
func (d *FollowVersionDB) LockTx(tx *dbr.Tx, uid, spaceID string) (int64, error) {
	if err := ensureFollowVersionRowTx(tx, uid, spaceID); err != nil {
		return 0, err
	}
	var v int64
	if err := tx.SelectBySql(
		"SELECT version FROM "+followVersionTable+
			" WHERE uid=? AND space_id=? FOR UPDATE",
		uid, spaceID,
	).LoadOne(&v); err != nil {
		return 0, fmt.Errorf("follow_version lock: %w", err)
	}
	return v, nil
}

// BumpFollowVersionTx 在 tx 内把 (uid, space_id) 的 version +1。
// 行不存在时以 version=1 创建。返回新 version。
//
// 任何 follow 状态变更（Follow/Unfollow/Sort/Category/Cleanup）都必须在
// 同一 tx 里调用本函数，这样客户端只要观察 follow_version 就能感知到
// "自己的 follow 列表发生了变化"。
func BumpFollowVersionTx(tx *dbr.Tx, uid, spaceID string) (int64, error) {
	// LAST_INSERT_ID(expr) 是 MySQL 的特殊用法：把 expr 记录下来，
	// 后续 SELECT LAST_INSERT_ID() 即可读出。
	// 新插入时走 VALUES 侧的 LAST_INSERT_ID(1)；冲突时走 ON DUPLICATE KEY UPDATE
	// 侧的 LAST_INSERT_ID(version+1)。无论哪个分支，一个 round-trip 就能拿到新 version。
	if _, err := tx.InsertBySql(
		"INSERT INTO "+followVersionTable+" (uid, space_id, version)"+
			" VALUES (?, ?, LAST_INSERT_ID(1))"+
			" ON DUPLICATE KEY UPDATE version = LAST_INSERT_ID(version + 1)",
		uid, spaceID,
	).Exec(); err != nil {
		return 0, fmt.Errorf("follow_version bump: %w", err)
	}
	var v int64
	if err := tx.SelectBySql("SELECT LAST_INSERT_ID()").LoadOne(&v); err != nil {
		return 0, fmt.Errorf("follow_version bump read last_insert_id: %w", err)
	}
	return v, nil
}

// ensureFollowVersionRowTx 在 tx 内保证 (uid, space_id) 行存在（初始 version=0）。
// 不修改已存在的行。供 LockTx 使用。
func ensureFollowVersionRowTx(tx *dbr.Tx, uid, spaceID string) error {
	if _, err := tx.InsertBySql(
		"INSERT IGNORE INTO "+followVersionTable+
			" (uid, space_id, version) VALUES (?, ?, 0)",
		uid, spaceID,
	).Exec(); err != nil {
		return fmt.Errorf("follow_version ensure row: %w", err)
	}
	return nil
}
