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
	// ErrVersionConflict は UpdateSort の CAS に失敗したときに返す。
	// 呼び出し元は errors.Is で判定して再試行を促す。
	ErrVersionConflict = errors.New("version conflict: please retry")
)

// Model は user_conversation_ext テーブルの 1 行に対応する。
type Model struct {
	ID              int64      `db:"id"`
	UID             string     `db:"uid"`
	SpaceID         string     `db:"space_id"`
	TargetType      uint8      `db:"target_type"`
	TargetID        string     `db:"target_id"`
	FollowedDM      int8       `db:"followed_dm"`
	DMCategoryID    *int64     `db:"dm_category_id"`
	GroupUnfollowed int8       `db:"group_unfollowed"`
	FollowSort      int        `db:"follow_sort"`
	Version         int        `db:"version"`
	CreatedAt       time.Time  `db:"created_at"`
	UpdatedAt       time.Time  `db:"updated_at"`
}

// ConvExtFields は Upsert 時に更新可能なフィールドのセットを表す。
// nil ポインタはそのフィールドを更新しないことを意味する。
// ClearDMCategory が true のとき dm_category_id を NULL に更新する
// （DMCategoryID と同時に指定した場合は ClearDMCategory が優先される）。
type ConvExtFields struct {
	FollowedDM      *int8
	DMCategoryID    *int64
	ClearDMCategory bool
	GroupUnfollowed *int8
	FollowSort      *int
}

// SortItem は UpdateSort で渡す並べ替え 1 要素。
type SortItem struct {
	TargetType uint8
	TargetID   string
}

// DB は user_conversation_ext テーブルへのアクセスを提供する。
type DB struct {
	session *dbr.Session
	log.Log
}

// NewDB は DB を生成する。
func NewDB(ctx *config.Context) *DB {
	return &DB{
		session: ctx.DB(),
		Log:     log.NewTLog("ConvExtDB"),
	}
}

const table = "user_conversation_ext"

// Upsert は (uid, space_id, target_type, target_id) を UK として INSERT OR UPDATE する。
// fields に指定された（非 nil の）フィールドをINSERT 時の初期値として設定し、
// 重複キー時にも同じフィールドを UPDATE する。
// すべてのフィールドが nil かつ ClearDMCategory も false の場合は INSERT IGNORE のみ実行する
// （存在すれば何も変えない、存在しなければデフォルト値で挿入）。
func (d *DB) Upsert(uid, spaceID string, targetType uint8, targetID string, fields ConvExtFields) error {
	extraCols, extraVals, setClauses, setArgs := buildUpsertParts(fields)

	if len(setClauses) == 0 {
		// UPDATE すべきフィールドがない場合は INSERT IGNORE のみ
		_, err := d.session.InsertBySql(
			"INSERT IGNORE INTO "+table+
				" (uid, space_id, target_type, target_id) VALUES (?, ?, ?, ?)",
			uid, spaceID, targetType, targetID,
		).Exec()
		return err
	}

	// INSERT ... ON DUPLICATE KEY UPDATE
	// INSERT 側にも同じフィールドを含めることで新規行にも値が設定される。
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

// buildUpsertParts は ConvExtFields から INSERT 用の追加列/値と
// ON DUPLICATE KEY UPDATE 用の SET 句を組み立てる。
// extraCols/extraVals は INSERT 列リストと VALUES プレースホルダへ追加する。
// setClauses/setArgs は ON DUPLICATE KEY UPDATE 句で使う。
// ClearDMCategory の場合のみ SET 句に "dm_category_id = NULL" を追加し、
// INSERT 側では dm_category_id を列に含めない（NULL はデフォルト値と等価）。
func buildUpsertParts(f ConvExtFields) (extraCols []string, extraVals []interface{}, setClauses []string, setArgs []interface{}) {
	if f.FollowedDM != nil {
		extraCols = append(extraCols, "followed_dm")
		extraVals = append(extraVals, *f.FollowedDM)
		setClauses = append(setClauses, "followed_dm = ?")
		setArgs = append(setArgs, *f.FollowedDM)
	}
	switch {
	case f.ClearDMCategory:
		// INSERT 側: 列を追加しない（NULL がデフォルト）
		// UPDATE 側: 明示的に NULL に戻す
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
	return extraCols, extraVals, setClauses, setArgs
}

// Get は単一行を返す。行が存在しない場合は (nil, nil) を返す。
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

// Delete は指定行を削除する。行が存在しない場合もエラーを返さない。
func (d *DB) Delete(uid, spaceID string, targetType uint8, targetID string) error {
	_, err := d.session.DeleteFrom(table).
		Where("uid=? AND space_id=? AND target_type=? AND target_id=?",
			uid, spaceID, targetType, targetID).
		Exec()
	return err
}

// ListFollowedDM は followed_dm=1 の全 DM（target_type=1）行を
// (dm_category_id ASC, follow_sort ASC) 順で返す。
// dm_category_id が NULL の行はソート上先頭扱いになる（NULL first）。
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

// ListUnfollowedGroups は group_unfollowed=1 の群（target_type=2）行を返す。
// 関注 Tab でグループが既に「取り消し関注」済みかどうかを判断するために使う。
func (d *DB) ListUnfollowedGroups(uid, spaceID string) ([]*Model, error) {
	var list []*Model
	_, err := d.session.SelectBySql(
		"SELECT * FROM "+table+
			" WHERE uid=? AND space_id=? AND target_type=2 AND group_unfollowed=1",
		uid, spaceID,
	).Load(&list)
	return list, err
}

// UpdateSort は CAS で follow_sort を一括更新する。
//
// 並行一致性:
//   - BEGIN → SELECT version FOR UPDATE → version == expectedVersion を確認 →
//     バッチ UPDATE follow_sort & version+1 → COMMIT。
//   - version が一致しない場合は ErrVersionConflict を返す（ロールバック済み）。
//   - items が空のとき、何もせず nil を返す。
func (d *DB) UpdateSort(uid, spaceID string, items []SortItem, expectedVersion int) error {
	if len(items) == 0 {
		return nil
	}

	tx, err := d.session.Begin()
	if err != nil {
		return err
	}
	defer tx.RollbackUnlessCommitted()

	// SELECT version FOR UPDATE — REPEATABLE READ 下でも current read を保証する。
	// db_pinned.go の SELECT COUNT(*) FOR UPDATE と同じパターン。
	var currentVersion int
	if _, err = tx.SelectBySql(
		"SELECT version FROM "+table+
			" WHERE uid=? AND space_id=? AND target_type=? AND target_id=? FOR UPDATE",
		uid, spaceID, items[0].TargetType, items[0].TargetID,
	).Load(&currentVersion); err != nil && err != dbr.ErrNotFound {
		return err
	}

	if currentVersion != expectedVersion {
		return ErrVersionConflict
	}

	// バッチ UPDATE: 配列インデックスを follow_sort に使い、同時に version を +1 する。
	nextVersion := currentVersion + 1
	for i, item := range items {
		if _, err = tx.UpdateBySql(
			"UPDATE "+table+
				" SET follow_sort=?, version=?"+
				" WHERE uid=? AND space_id=? AND target_type=? AND target_id=?",
			i+1, nextVersion, uid, spaceID, item.TargetType, item.TargetID,
		).Exec(); err != nil {
			return err
		}
	}

	return tx.Commit()
}
