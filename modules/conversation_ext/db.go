package conversation_ext

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/gocraft/dbr/v2"
)

// 错误定义
var (
	// ErrVersionConflict 在 UpdateSort 的 CAS 失败时返回。
	// 调用方应用 errors.Is 判定并重试。
	ErrVersionConflict = errors.New("version conflict: please retry")
	// ErrSortTargetNotFound 在 UpdateSort 指定的 target 任一不存在时返回。
	// 任一 SELECT ... FOR UPDATE 落空都意味着 CAS 的"能锁住所有目标行"前提崩塌，
	// 不能静默成功，必须显式拒绝。
	ErrSortTargetNotFound = errors.New("sort target not found")
)

// Model 对应 user_conversation_ext 表的一行。
type Model struct {
	ID         int64  `db:"id"`
	UID        string `db:"uid"`
	SpaceID    string `db:"space_id"`
	TargetType uint8  `db:"target_type"`
	TargetID   string `db:"target_id"`
	FollowedDM int8   `db:"followed_dm"`
	// DMCategoryID 是 group_category.category_id（VARCHAR(32) UUID），DM 与群
	// 共用同一分类 namespace（PR #21 Round-6，原型 image-v1.png 印证）。
	// NULL 表示未分类。
	DMCategoryID    *string `db:"dm_category_id"`
	GroupUnfollowed int8    `db:"group_unfollowed"`
	FollowSort      int     `db:"follow_sort"`
	// AutoFollowThreads = 1 表示关注 target_type=2 群后，新建子区时自动给该 (uid, space_id) 物化 thread ext 行。
	// 仅在 target_type=2 行上有意义；其他 target_type 写 0 保持。
	AutoFollowThreads int8      `db:"auto_follow_threads"`
	CreatedAt         time.Time `db:"created_at"`
	UpdatedAt         time.Time `db:"updated_at"`
}

// ConvExtFields 描述 Upsert 时可更新的字段集合。
// nil 指针表示该字段不参与更新。
// ClearDMCategory 为 true 时把 dm_category_id 更新为 NULL
// （与 DMCategoryID 同时指定时 ClearDMCategory 优先）。
type ConvExtFields struct {
	FollowedDM *int8
	// DMCategoryID 是 group_category.category_id（VARCHAR(32) UUID）。
	DMCategoryID    *string
	ClearDMCategory bool
	GroupUnfollowed *int8
	FollowSort      *int
	// AutoFollowThreads 仅在 target_type=2 行有意义，nil 表示不更新该字段。
	AutoFollowThreads *int8
}

// SortItem 是传给 UpdateSort 的单条排序项。
type SortItem struct {
	TargetType uint8
	TargetID   string
}

// DB 提供对 user_conversation_ext 表的访问。
type DB struct {
	session *dbr.Session
	log.Log
}

// NewDB 构造 DB。
func NewDB(ctx *config.Context) *DB {
	return &DB{
		session: ctx.DB(),
		Log:     log.NewTLog("ConvExtDB"),
	}
}

const table = "user_conversation_ext"

// Upsert 以 (uid, space_id, target_type, target_id) 为 UK 做 INSERT OR UPDATE。
// fields 中（非 nil 的）字段在 INSERT 时作为初值写入，
// 命中重复键时同样用这些字段 UPDATE。
// 当所有字段都为 nil 且 ClearDMCategory=false 时仅执行 INSERT IGNORE
// （存在则不变，不存在则按默认值插入）。
func (d *DB) Upsert(uid, spaceID string, targetType uint8, targetID string, fields ConvExtFields) error {
	extraCols, extraVals, setClauses, setArgs := buildUpsertParts(fields)

	if len(setClauses) == 0 {
		// 没有需要 UPDATE 的字段时只跑 INSERT IGNORE
		_, err := d.session.InsertBySql(
			"INSERT IGNORE INTO "+table+
				" (uid, space_id, target_type, target_id) VALUES (?, ?, ?, ?)",
			uid, spaceID, targetType, targetID,
		).Exec()
		return err
	}

	// INSERT ... ON DUPLICATE KEY UPDATE
	// 在 INSERT 侧也带上同样字段，新行就能拿到对应初值。
	colsSQL := "uid, space_id, target_type, target_id"
	if len(extraCols) > 0 {
		colsSQL += ", " + strings.Join(extraCols, ", ")
	}
	placeholders := "?, ?, ?, ?"
	if len(extraVals) > 0 {
		placeholders += strings.Repeat(", ?", len(extraVals))
	}
	setSQL := strings.Join(setClauses, ", ")
	query := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s) ON DUPLICATE KEY UPDATE %s",
		table, colsSQL, placeholders, setSQL,
	)
	insertArgs := append([]interface{}{uid, spaceID, targetType, targetID}, extraVals...)
	insertArgs = append(insertArgs, setArgs...)
	_, err := d.session.InsertBySql(query, insertArgs...).Exec()
	return err
}

// buildUpsertParts 从 ConvExtFields 构造 INSERT 的追加列/值
// 以及 ON DUPLICATE KEY UPDATE 的 SET 子句。
// extraCols/extraVals 用于追加到 INSERT 的列名列表和 VALUES 占位符。
// setClauses/setArgs 用于 ON DUPLICATE KEY UPDATE 子句。
// 仅 ClearDMCategory 情况下 SET 中加入 "dm_category_id = NULL"，
// INSERT 侧不把 dm_category_id 列出来（NULL 等价于默认值）。
func buildUpsertParts(f ConvExtFields) (extraCols []string, extraVals []interface{}, setClauses []string, setArgs []interface{}) {
	if f.FollowedDM != nil {
		extraCols = append(extraCols, "followed_dm")
		extraVals = append(extraVals, *f.FollowedDM)
		setClauses = append(setClauses, "followed_dm = ?")
		setArgs = append(setArgs, *f.FollowedDM)
	}
	switch {
	case f.ClearDMCategory:
		// INSERT 侧：不加列（NULL 即默认值）
		// UPDATE 侧：显式置回 NULL
		setClauses = append(setClauses, "dm_category_id = NULL")
	case f.DMCategoryID != nil:
		extraCols = append(extraCols, "dm_category_id")
		extraVals = append(extraVals, *f.DMCategoryID)
		setClauses = append(setClauses, "dm_category_id = ?")
		setArgs = append(setArgs, *f.DMCategoryID)
	}
	if f.GroupUnfollowed != nil {
		extraCols = append(extraCols, "group_unfollowed")
		extraVals = append(extraVals, *f.GroupUnfollowed)
		setClauses = append(setClauses, "group_unfollowed = ?")
		setArgs = append(setArgs, *f.GroupUnfollowed)
	}
	if f.FollowSort != nil {
		extraCols = append(extraCols, "follow_sort")
		extraVals = append(extraVals, *f.FollowSort)
		setClauses = append(setClauses, "follow_sort = ?")
		setArgs = append(setArgs, *f.FollowSort)
	}
	if f.AutoFollowThreads != nil {
		extraCols = append(extraCols, "auto_follow_threads")
		extraVals = append(extraVals, *f.AutoFollowThreads)
		setClauses = append(setClauses, "auto_follow_threads = ?")
		setArgs = append(setArgs, *f.AutoFollowThreads)
	}
	return extraCols, extraVals, setClauses, setArgs
}

// Get 返回单行。行不存在时返回 (nil, nil)。
func (d *DB) Get(uid, spaceID string, targetType uint8, targetID string) (*Model, error) {
	var m Model
	err := d.session.SelectBySql(
		"SELECT * FROM "+table+
			" WHERE uid=? AND space_id=? AND target_type=? AND target_id=?",
		uid, spaceID, targetType, targetID,
	).LoadOne(&m)
	if err == dbr.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// Delete 删除指定行。行不存在时也不返回错误。
func (d *DB) Delete(uid, spaceID string, targetType uint8, targetID string) error {
	_, err := d.session.DeleteFrom(table).
		Where("uid=? AND space_id=? AND target_type=? AND target_id=?",
			uid, spaceID, targetType, targetID).
		Exec()
	return err
}

// ListFollowedDM 返回所有 followed_dm=1 的 DM 行（target_type=1），
// 按 (dm_category_id ASC, follow_sort ASC) 排序。
// dm_category_id 为 NULL 的行排在最前（NULL first）。
func (d *DB) ListFollowedDM(uid, spaceID string) ([]*Model, error) {
	var list []*Model
	_, err := d.session.SelectBySql(
		"SELECT * FROM "+table+
			" WHERE uid=? AND space_id=? AND target_type=1 AND followed_dm=1"+
			" ORDER BY dm_category_id ASC, follow_sort ASC",
		uid, spaceID,
	).Load(&list)
	return list, err
}

// ListGroupExts 返回 target_type=2（群）的全部 ext 行（无视 group_unfollowed 标志）。
//
// Issue #41：sidebar follow tab 需要按 follow_sort 排序群条目，而 follow_sort 写在
// user_conversation_ext 上。已关注的群在用户从未拖拽前不一定存在 ext 行，缺失时
// 上层视为 FollowSort=0；存在 group_unfollowed=1 的行由 ListUnfollowedGroups 单独
// 过滤，本方法不再次区分以避免漏读已设置过 follow_sort 的群。
func (d *DB) ListGroupExts(uid, spaceID string) ([]*Model, error) {
	var list []*Model
	_, err := d.session.SelectBySql(
		"SELECT * FROM "+table+
			" WHERE uid=? AND space_id=? AND target_type=2",
		uid, spaceID,
	).Load(&list)
	return list, err
}

// ListUnfollowedGroups 返回 group_unfollowed=1 的群行（target_type=2）。
// 用于关注 Tab 判断某个群是否已"取消关注"。
func (d *DB) ListUnfollowedGroups(uid, spaceID string) ([]*Model, error) {
	var list []*Model
	_, err := d.session.SelectBySql(
		"SELECT * FROM "+table+
			" WHERE uid=? AND space_id=? AND target_type=2 AND group_unfollowed=1",
		uid, spaceID,
	).Load(&list)
	return list, err
}

// ListThreadExts 返回 target_type=5（子区）的 ext 行，
// 按 (follow_sort ASC) 排序。
// 用于 follow tab 里子区独立条目的构造。
func (d *DB) ListThreadExts(uid, spaceID string) ([]*Model, error) {
	var list []*Model
	_, err := d.session.SelectBySql(
		"SELECT * FROM "+table+
			" WHERE uid=? AND space_id=? AND target_type=5"+
			" ORDER BY follow_sort ASC",
		uid, spaceID,
	).Load(&list)
	return list, err
}

// UpdateSort 通过 CAS 一次性更新 follow_sort。
//
// PR review Round-3 Blocking #1/#2/#5 修正后的并发一致性：
//   - BEGIN → 用 FOR UPDATE 锁 user_follow_version 的 (uid, space_id) 行取当前值。
//   - cur != expectedVersion → ErrVersionConflict（回滚）。
//   - 用 (target_type, target_id) 升序 FOR UPDATE 锁 user_conversation_ext 全部目标行。
//     返回行数 != len(items) → ErrSortTargetNotFound（回滚）。
//   - 对每一行 UPDATE follow_sort。RowsAffected ∈ {0,1}：0=新值与旧值相同（无变化，
//     仍视为成功），1=正常更新；>1 不可达（WHERE 含逻辑主键），仅作防御性守卫。
//   - 最后在同 tx 内把 user_follow_version +1。
//   - items 为空时什么也不做，返回 nil。
//
// 旧实现的漏洞：
//
//	(1) 只锁 items[0] → items[1..] 缺失会被 0 行 UPDATE 静默吞掉。
//	(2) 不同首 item 的并发调用没有共享锁行，完全交错执行。
//	(3) 把 per-row version 当成用户级 CAS 的锚，但新关注的行 version=0，
//	    与既存行不一致，导致 UpdateSort 要么硬通过要么永远失败。
//
// 修复是引入 user_follow_version 表 (uid, space_id, version) 作为 CAS 的单一根，
// 任何 follow 状态变化都对它 +1，从根上解决问题。
func (d *DB) UpdateSort(uid, spaceID string, items []SortItem, expectedVersion int64) error {
	if len(items) == 0 {
		return nil
	}

	tx, err := d.session.Begin()
	if err != nil {
		return fmt.Errorf("update sort: begin tx: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	// 1. 锁 follow_version 行并取当前值。行不存在则初始化（version=0）。
	if err := ensureFollowVersionRowTx(tx, uid, spaceID); err != nil {
		return fmt.Errorf("update sort: %w", err)
	}
	var cur int64
	if err := tx.SelectBySql(
		"SELECT version FROM "+followVersionTable+
			" WHERE uid=? AND space_id=? FOR UPDATE",
		uid, spaceID,
	).LoadOne(&cur); err != nil {
		return fmt.Errorf("update sort: lock follow_version: %w", err)
	}
	if cur != expectedVersion {
		return ErrVersionConflict
	}

	// 2. 用 SELECT ... FOR UPDATE 按确定顺序锁住全部 ext 行。
	pairPlaceholders := make([]string, len(items))
	selectArgs := make([]interface{}, 0, 2+len(items)*2)
	selectArgs = append(selectArgs, uid, spaceID)
	for i, it := range items {
		pairPlaceholders[i] = "(?, ?)"
		selectArgs = append(selectArgs, it.TargetType, it.TargetID)
	}
	type lockedRow struct {
		TargetType uint8  `db:"target_type"`
		TargetID   string `db:"target_id"`
	}
	var locked []lockedRow
	if _, err = tx.SelectBySql(
		"SELECT target_type, target_id FROM "+table+
			" WHERE uid=? AND space_id=?"+
			" AND (target_type, target_id) IN ("+strings.Join(pairPlaceholders, ", ")+")"+
			" ORDER BY target_type, target_id FOR UPDATE",
		selectArgs...,
	).Load(&locked); err != nil {
		return fmt.Errorf("update sort: lock rows: %w", err)
	}

	// All items must already exist as ext rows.  Default-followed groups
	// (target_type=2 with group_setting.category_id set but no ext row) must
	// have been materialized upstream by Service.AuthorizeAndMaterializeDefaultFollowedGroups
	// before this DB call.  Putting that materialization inside the same tx
	// would require trusting client-supplied group IDs (issue #151 code review #1
	// — unauthorized materialization risk), which the DB layer is not able to
	// authorize without importing modules/group / modules/message.
	if len(locked) != len(items) {
		return ErrSortTargetNotFound
	}

	// 3. 逐行 UPDATE follow_sort。RowsAffected ∈ {0,1}（见函数注释）：0 仅意味着
	//    新值等于旧值，仍是成功；>1 不可达，作为防御性守卫报错。
	for i, item := range items {
		res, err := tx.UpdateBySql(
			"UPDATE "+table+" SET follow_sort=?"+
				" WHERE uid=? AND space_id=? AND target_type=? AND target_id=?",
			i+1, uid, spaceID, item.TargetType, item.TargetID,
		).Exec()
		if err != nil {
			return fmt.Errorf("update sort: update row (%d,%s): %w", item.TargetType, item.TargetID, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("update sort: rows affected: %w", err)
		}
		// MySQL 驱动默认走 rows-changed 语义：新值等于旧值时 affected=0。
		// 行的存在性已由前面的 SELECT ... FOR UPDATE + len(locked) 校验保证，
		// 所以 affected ∈ {0, 1}，0 仅意味着无需变更，不是 conflict。
		if affected > 1 {
			return fmt.Errorf("update sort: unexpected rows affected=%d for (%d,%s)", affected, item.TargetType, item.TargetID)
		}
	}

	// 4. 同 tx 内把 follow_version +1。
	if _, err := BumpFollowVersionTx(tx, uid, spaceID); err != nil {
		return fmt.Errorf("update sort: bump follow_version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("update sort: commit: %w", err)
	}
	return nil
}

// RestoreAutoFollowThreadsTx is the move-back counterpart of
// ClearAutoFollowThreadsTx.  It sets auto_follow_threads=1 on the
// (uid, space_id, target_type=2, group_no) ext rows in groupNos that exist,
// silently skipping any that don't.  Idempotent; safe to call when no row
// has been materialized (sidebar.Sync's later MaterializeDefaultFollowedGroups
// call will create the row with auto_follow_threads=1 anyway).
//
// Why it exists (issue #151 review #4 by an9xyz): the lifecycle is symmetric:
// materialize on first follow-tab read sets =1; move-out clears to 0;
// move-back-into-a-category must restore =1, otherwise the sidebar
// materialization branch sees the existing groupExts entry (with =0) and
// skips its INSERT IGNORE.  The user re-categorizes the group, the group
// re-appears in the follow tab via buildFollowItems, but OnThreadCreated's
// fan-out filter (auto_follow_threads=1 AND group_unfollowed=0) still
// excludes them — phantom missing fan-out.
//
// Like Clear, this helper does NOT touch group_unfollowed (the user's
// explicit unfollow choice is preserved across category churn) and does NOT
// bump user_follow_version (caller's surrounding category change already
// bumps once).
//
// Callers in scope: modules/category moveGroupToCategory move-in branch
// (req.CategoryID != ""), including first-time categorize (no row yet,
// no-op here, sidebar materializes later) and A→B category move (row exists
// with =1 already, no-op effectively).  The single common path simplifies
// reasoning and removes the temptation to branch on subcases.
func RestoreAutoFollowThreadsTx(tx *dbr.Tx, uid, spaceID string, groupNos []string) error {
	if uid == "" || spaceID == "" || len(groupNos) == 0 {
		return nil
	}
	_, err := tx.UpdateBySql(
		"UPDATE "+table+" SET auto_follow_threads=1"+
			" WHERE uid=? AND space_id=? AND target_type=? AND target_id IN ?",
		uid, spaceID, targetTypeGroup, groupNos,
	).Exec()
	if err != nil {
		return fmt.Errorf("restore auto_follow_threads: %w", err)
	}
	return nil
}

// ClearAutoFollowThreadsTx is the cleanup counterpart of
// MaterializeDefaultFollowedGroups.  It sets auto_follow_threads=0 on the
// (uid, space_id, target_type=2, group_no) ext rows in groupNos that exist,
// silently skipping any that don't.  Idempotent; safe to call when no row
// has been materialized.
//
// Why it exists (issue #151 review #3 by yujiawei): MaterializeDefault-
// FollowedGroups creates ext rows with auto_follow_threads=1 for groups that
// are followed *implicitly* via group_setting.category_id.  When that
// implicit follow is revoked (user moves the group out of any category, or
// the category is deleted), the row remains and selectEligibleForFanoutTx
// keeps treating the user as eligible for new-thread fan-out — even though
// buildFollowItems now drops the group from the follow tab because
// CategoryID is nil.  Before this PR no row existed so the cleanup was
// implicit ("no row = no fanout"); once we materialize, the cleanup must
// be made explicit at every site that clears group_setting.category_id.
//
// What it does NOT do:
//   - Does not delete the ext row (preserves group_unfollowed flag and any
//     follow_sort the user explicitly chose).
//   - Does not touch thread (target_type=5) ext rows — existing thread
//     subscriptions remain valid; we only stop auto-following NEW threads.
//   - Does not bump user_follow_version — the caller (category handler)
//     already bumps once for the surrounding group_setting change; bumping
//     again here would force two client reloads for one logical action.
//
// Callers in scope: modules/category moveGroupToCategory move-out branch
// (req.CategoryID == "").  modules/category deleteCategory does NOT need
// this helper — it already calls UnfollowGroupsTx which sets
// group_unfollowed=1 AND auto_follow_threads=0 (stronger semantic: explicit
// unfollow on category delete per PM contract).  Future code that mutates
// group_setting.category_id from non-NULL to NULL without setting
// group_unfollowed=1 MUST call this helper in the same transaction to keep
// the read/write contracts in sync.
func ClearAutoFollowThreadsTx(tx *dbr.Tx, uid, spaceID string, groupNos []string) error {
	if uid == "" || spaceID == "" || len(groupNos) == 0 {
		return nil
	}
	_, err := tx.UpdateBySql(
		"UPDATE "+table+" SET auto_follow_threads=0"+
			" WHERE uid=? AND space_id=? AND target_type=? AND target_id IN ?",
		uid, spaceID, targetTypeGroup, groupNos,
	).Exec()
	if err != nil {
		return fmt.Errorf("clear auto_follow_threads: %w", err)
	}
	return nil
}

// MaterializeDefaultFollowedGroups writes a fresh ext row (auto_follow_threads=1,
// group_unfollowed=0) for each (uid, space_id, target_type=2, group_no) tuple
// that does not yet have one.  It is the data-layer entry point used by:
//
//   - Sidebar.Sync: after buildFollowItems decides which groups belong in the
//     follow tab (gated by group_setting.category_id IS NOT NULL), passes the
//     missing ones here so OnThreadCreated fans out new threads going forward.
//   - Service.AuthorizeAndMaterializeDefaultFollowedGroups: pre-flight step
//     before UpdateSort, after DefaultFollowedGroupGuard has filtered the
//     client-supplied payload down to groups the caller genuinely has in a
//     category.
//
// SECURITY contract: this function does NOT authorize the (uid, group_no)
// pair.  Callers MUST verify upstream that the user is allowed to follow
// each group (member + visible, AND the group has category_id set, per
// issue #151 code review #1).  Without that gate a malicious client can
// piggy-back arbitrary group IDs and start receiving thread fan-outs for
// groups they are not in — leaking thread metadata.
//
// Fail-open contract for sidebar callers: a failure here must not block the
// sidebar response; the caller logs and continues.  Pre-flight callers
// (UpdateSort path) propagate the error to the client.
func (d *DB) MaterializeDefaultFollowedGroups(uid, spaceID string, groupNos []string) error {
	if len(groupNos) == 0 {
		return nil
	}
	items := make([]SortItem, len(groupNos))
	for i, no := range groupNos {
		items[i] = SortItem{TargetType: targetTypeGroup, TargetID: no}
	}
	tx, err := d.session.Begin()
	if err != nil {
		return fmt.Errorf("materialize default-followed groups: begin tx: %w", err)
	}
	defer tx.RollbackUnlessCommitted()
	if err := materializeDefaultFollowedGroupsTx(tx, uid, spaceID, items); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("materialize default-followed groups: commit: %w", err)
	}
	return nil
}

// materializeDefaultFollowedGroupsTx INSERTs an ext row for each group the
// caller listed that did not yet have one.  Default-followed groups (in a
// category, never touched by the user) end up here on the first drag (via
// UpdateSort) or on the first sidebar/sync (via MaterializeDefaultFollowedGroups
// — issue #151 symptom #2 path).
//
// The newly-inserted row is intentionally configured so that downstream paths
// already coded against ext-row presence start working immediately:
//
//   - group_unfollowed=0 — the group is followed.
//   - auto_follow_threads=1 — selectEligibleForFanoutTx will include this user
//     when OnThreadCreated fires for the group (closes symptom #2).
//   - follow_sort=0 — UpdateSort's step 3 re-assigns the real sort position;
//     the standalone read-path entry (MaterializeDefaultFollowedGroups) is fine
//     leaving 0 because sidebar item ordering for default-followed groups
//     already defaults to 0 in buildFollowItems.
//
// INSERT IGNORE rather than ON DUPLICATE KEY UPDATE: we want a strict no-op
// when the row already exists — never overwrite a user's existing choices for
// group_unfollowed or auto_follow_threads.  INSERT IGNORE also avoids burning
// AUTO_INCREMENT id values on duplicate-key hits (innodb_autoinc_lock_mode=2
// default on MySQL 8 would otherwise bump the counter even on the no-op
// branch), matching the existing pattern in bulkInsertThreadExtForFanoutUsersTx.
//
// Concurrency: callers do NOT need to hold a SELECT ... FOR UPDATE on the
// target (uid, space_id, target_type, target_id) tuple beforehand.  The unique
// key uk(uid, space_id, target_type, target_id) plus INSERT IGNORE's
// "skip duplicates" semantics make the operation safe under any interleaving
// with FollowChannel / UnfollowChannel / OnThreadCreated fanout — whichever
// committed first wins, the no-op preserves that row.
func materializeDefaultFollowedGroupsTx(tx *dbr.Tx, uid, spaceID string, missing []SortItem) error {
	if len(missing) == 0 {
		return nil
	}
	tupleHolders := make([]string, len(missing))
	args := make([]interface{}, 0, len(missing)*4)
	for i, it := range missing {
		tupleHolders[i] = "(?, ?, ?, ?, 0, 1)"
		args = append(args, uid, spaceID, it.TargetType, it.TargetID)
	}
	_, err := tx.InsertBySql(
		"INSERT IGNORE INTO "+table+
			" (uid, space_id, target_type, target_id, group_unfollowed, auto_follow_threads) VALUES "+
			strings.Join(tupleHolders, ", "),
		args...,
	).Exec()
	if err != nil {
		return fmt.Errorf("insert default-followed group rows: %w", err)
	}
	return nil
}
