package conversation_ext

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/gocraft/dbr/v2"
)

// target_type constants — kept package-private; callers use the Service API.
const (
	targetTypeDM     uint8 = 1
	targetTypeGroup  uint8 = 2
	targetTypeThread uint8 = 5
)

// threadSeparator is the fixed four-underscore delimiter used in thread
// channel IDs: "{groupNo}____{shortID}".
const threadSeparator = "____"

// ErrThreadForbidden 在 FollowThread 鉴权失败时返回。
// 调用方（HTTP handler）应将此错误翻译为 403。
var ErrThreadForbidden = errors.New("thread follow forbidden: not a member of parent group or thread not visible")

// ErrDMCategoryForbidden 在 FollowDM 指定的 category 不属于当前 uid 或已删除时返回。
// 调用方应将此错误翻译为 400 / 403（按业务约定）。
// PR #21 Round-6 (Jerry-Xin)：DM category 必须由服务端校验归属，否则客户端可写入
// 任意 UUID 让自己的 follow tab 引用不存在的分类（"未分类"渲染）。
var ErrDMCategoryForbidden = errors.New("dm category forbidden: not owned by uid or category deleted")

// ThreadAuthChecker 判定 FollowThread 是否被授权，是一个 narrow interface。
// 为避免对 modules/group / modules/thread 形成循环依赖，采用依赖倒置从外部注入。
//
// AuthorizeThreadFollow 一次性校验：
//   - shortID 对应的 thread 存在，且 status != deleted。
//   - thread.group_no == 入参 groupNo（拒绝跨群引用）。
//   - uid 是 groupNo 的成员。
//
// 鉴权失败返回 ErrThreadForbidden（具体原因由 handler 写日志）。
// 校验通过返回 nil。基础设施错误（DB 错误等）以 wrap 后的形式向上透传。
type ThreadAuthChecker interface {
	AuthorizeThreadFollow(uid, spaceID, groupNo, shortID string) error
}

// Service encapsulates composite operations on user_conversation_ext that
// require a single transaction boundary.  It intentionally avoids importing
// modules/group, modules/user, or modules/thread to prevent circular
// dependencies.
//
// threadAuth 是 FollowThread 的鉴权钩子，由外部模块（在 1module.go 里把
// group/thread 组合起来的实现）在启动时通过 SetThreadAuthChecker 注入。
// 为 nil 时跳过鉴权（仅供测试 / 迁移期使用）。
//
// （历史 DMCategoryChecker 注入点 issue #75 / PR #79 fix 之后已移除——FollowDM
// 鉴权改为事务内 SELECT ... FOR UPDATE，见 authorizeDMCategoryInTx——曾经的
// `dmCatAuth`/`SetDMCategoryChecker` 接口与对应的 message 模块注入也一起清掉。）
type Service struct {
	db          *DB
	session     *dbr.Session
	threadAuth  ThreadAuthChecker
	threadAuthM sync.RWMutex
	log.Log
}

// NewService creates a Service.
func NewService(ctx *config.Context) *Service {
	return &Service{
		db:      NewDB(ctx),
		session: ctx.DB(),
		Log:     log.NewTLog("ConvExtService"),
	}
}

// SetThreadAuthChecker injects the auth checker used by FollowThread.
// Safe for concurrent use; intended to be called once at startup from
// 1module.go after the group / thread modules have initialised.
func (s *Service) SetThreadAuthChecker(c ThreadAuthChecker) {
	s.threadAuthM.Lock()
	s.threadAuth = c
	s.threadAuthM.Unlock()
}

// getThreadAuthChecker returns the currently registered checker (or nil).
func (s *Service) getThreadAuthChecker() ThreadAuthChecker {
	s.threadAuthM.RLock()
	c := s.threadAuth
	s.threadAuthM.RUnlock()
	return c
}

// ---------------------------------------------------------------------------
// Input validation helpers
// ---------------------------------------------------------------------------

func validateBase(uid, spaceID string) error {
	if uid == "" {
		return errors.New("uid must not be empty")
	}
	if spaceID == "" {
		return errors.New("space_id must not be empty")
	}
	return nil
}

// parseThreadChannelID splits a thread channel ID of the form
// "{groupNo}____{shortID}" and returns groupNo, shortID.
// Returns an error if the format is invalid.
func parseThreadChannelID(threadChannelID string) (groupNo, shortID string, err error) {
	parts := strings.SplitN(threadChannelID, threadSeparator, 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("thread_channel_id %q is invalid: expected format {groupNo}____{shortID}", threadChannelID)
	}
	return parts[0], parts[1], nil
}

// threadLikePrefix 构造 "{groupNo}____%" 的 LIKE 前缀，并用 '|' 作为转义符
// （配合 ESCAPE '|' 使用）。集中在一处避免不同调用方对 ESCAPE 字符产生分歧。
func threadLikePrefix(groupNo string) string {
	return escapeLike(groupNo) + escapeLike(threadSeparator) + "%"
}

// escapeLike escapes LIKE special characters for use with ESCAPE '|'.
// The pipe character is chosen as the escape character because it never
// appears in snowflake IDs or our thread channel IDs, avoiding the
// double-backslash quoting problem when passing '\' through the Go MySQL driver.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `|`, `||`)
	s = strings.ReplaceAll(s, `%`, `|%`)
	s = strings.ReplaceAll(s, `_`, `|_`)
	return s
}

// ---------------------------------------------------------------------------
// FollowChannel — clear group-blacklist flag (re-follow a previously unfollowed group)
// ---------------------------------------------------------------------------

// FollowChannel marks the group as followed (group_unfollowed=0) for the given
// user and space.  If no ext row exists it is created with the default values.
//
// PR review (Round 3) Blocking #1/#2 — 关注状态变化时把 user_follow_version +1。
// Upsert 和 Bump 在同一 tx 内执行，这样客户端只要观察 follow_version 就能可靠
// 检测到变化。
func (s *Service) FollowChannel(uid, spaceID, groupNo string) error {
	if err := validateBase(uid, spaceID); err != nil {
		return err
	}
	if groupNo == "" {
		return errors.New("group_no must not be empty")
	}
	zero := int8(0)
	// PR #21 review (lml2468 blocker #2)：所有同时触及 user_follow_version 与
	// user_conversation_ext 的事务必须按相同顺序拿锁，否则与先锁 version
	// 再锁 ext 的 UpdateSort 互锁。把 Bump 放在最前 —— 它通过
	// INSERT ... ON DUPLICATE KEY UPDATE 对 user_follow_version (uid, space_id)
	// 行加 X 锁，使本 tx 在 version 行上有排它后再进入 ext 行操作。
	return s.withTx("FollowChannel", func(tx *dbr.Tx) error {
		if _, err := BumpFollowVersionTx(tx, uid, spaceID); err != nil {
			return fmt.Errorf("FollowChannel bump version: %w", err)
		}
		if err := upsertTx(tx, uid, spaceID, targetTypeGroup, groupNo, ConvExtFields{
			GroupUnfollowed: &zero,
		}); err != nil {
			return fmt.Errorf("FollowChannel upsert: %w", err)
		}
		return nil
	})
}

// withTx wraps fn in a tx with consistent error handling.
func (s *Service) withTx(op string, fn func(tx *dbr.Tx) error) error {
	tx, err := s.session.Begin()
	if err != nil {
		return fmt.Errorf("%s begin tx: %w", op, err)
	}
	defer tx.RollbackUnlessCommitted()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s commit: %w", op, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// UnfollowChannel — blacklist a group + cascade-delete its thread ext rows
// ---------------------------------------------------------------------------

// UnfollowChannel marks the group as unfollowed (group_unfollowed=1) and, in
// the same transaction, deletes all thread (target_type=5) ext rows whose
// target_id starts with "{groupNo}____" for this user+space, and bumps the
// user_follow_version (PR review Round-3 Blocking #1/#2).
func (s *Service) UnfollowChannel(uid, spaceID, groupNo string) error {
	if err := validateBase(uid, spaceID); err != nil {
		return err
	}
	if groupNo == "" {
		return errors.New("group_no must not be empty")
	}
	one := int8(1)
	// PR #21 review (lml2468 blocker #2)：bump 必须先于 ext 行操作，保证与
	// UpdateSort 同序拿锁，避免 (version vs ext) 反向死锁。
	return s.withTx("UnfollowChannel", func(tx *dbr.Tx) error {
		if _, err := BumpFollowVersionTx(tx, uid, spaceID); err != nil {
			return fmt.Errorf("UnfollowChannel bump version: %w", err)
		}
		if err := upsertTx(tx, uid, spaceID, targetTypeGroup, groupNo, ConvExtFields{
			GroupUnfollowed: &one,
		}); err != nil {
			return fmt.Errorf("UnfollowChannel upsert group: %w", err)
		}
		if _, err := tx.DeleteBySql(
			"DELETE FROM "+table+
				" WHERE uid=? AND space_id=? AND target_type=? AND target_id LIKE ? ESCAPE '|'",
			uid, spaceID, targetTypeThread, threadLikePrefix(groupNo),
		).Exec(); err != nil {
			return fmt.Errorf("UnfollowChannel delete threads: %w", err)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// FollowThread — re-follow parent group (implicit) + upsert thread ext row
// ---------------------------------------------------------------------------

// FollowThread creates (or ensures) an ext row for the given thread channel,
// and simultaneously clears the parent group's unfollowed flag so that
// following a specific thread implicitly re-follows its parent group.
//
// threadChannelID must have the format "{groupNo}____{shortID}".
//
// PR review (Round 3) Blocking #3: prior to any DB write, the registered
// ThreadAuthChecker (if any) MUST authorise (uid, groupNo, shortID). Without
// this check FollowThread accepted any syntactically valid channel ID and wrote
// an ext row referencing a thread the user could not see — surfacing unauthorised
// entries on subsequent sidebar queries.  ErrThreadForbidden bubbles up unchanged
// for the handler to translate to a 403 response.
func (s *Service) FollowThread(uid, spaceID, threadChannelID string) error {
	if err := validateBase(uid, spaceID); err != nil {
		return err
	}
	groupNo, shortID, err := parseThreadChannelID(threadChannelID)
	if err != nil {
		return err
	}

	if checker := s.getThreadAuthChecker(); checker != nil {
		if err := checker.AuthorizeThreadFollow(uid, spaceID, groupNo, shortID); err != nil {
			return err
		}
	}

	return s.withTx("FollowThread", func(tx *dbr.Tx) error {
		// PR #21 review (lml2468 blocker #2)：先 bump 后改 ext，与 UpdateSort 同序拿锁。
		if _, err := BumpFollowVersionTx(tx, uid, spaceID); err != nil {
			return fmt.Errorf("FollowThread bump version: %w", err)
		}
		// 1. Clear parent group's unfollowed flag.
		zero := int8(0)
		if err := upsertTx(tx, uid, spaceID, targetTypeGroup, groupNo, ConvExtFields{
			GroupUnfollowed: &zero,
		}); err != nil {
			return fmt.Errorf("FollowThread clear parent group: %w", err)
		}
		// 2. Upsert thread ext row (no additional fields — default values suffice).
		if err := upsertTx(tx, uid, spaceID, targetTypeThread, threadChannelID, ConvExtFields{}); err != nil {
			return fmt.Errorf("FollowThread upsert thread: %w", err)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// UnfollowThread — delete thread ext row only
// ---------------------------------------------------------------------------

// UnfollowThread removes the ext row for the given thread channel.
// It does NOT touch the parent group's unfollowed flag.
//
// threadChannelID must have the format "{groupNo}____{shortID}".
// PR review (Round 3) Blocking #1/#2 — bumps follow_version in same tx.
func (s *Service) UnfollowThread(uid, spaceID, threadChannelID string) error {
	if err := validateBase(uid, spaceID); err != nil {
		return err
	}
	if _, _, err := parseThreadChannelID(threadChannelID); err != nil {
		return err
	}
	// PR #21 review (lml2468 blocker #2)：先 bump 后改 ext，与 UpdateSort 同序拿锁。
	return s.withTx("UnfollowThread", func(tx *dbr.Tx) error {
		if _, err := BumpFollowVersionTx(tx, uid, spaceID); err != nil {
			return fmt.Errorf("UnfollowThread bump version: %w", err)
		}
		if _, err := tx.DeleteFrom(table).
			Where("uid=? AND space_id=? AND target_type=? AND target_id=?",
				uid, spaceID, targetTypeThread, threadChannelID).Exec(); err != nil {
			return fmt.Errorf("UnfollowThread delete: %w", err)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// FollowDM — upsert ext row with followed_dm=1
// ---------------------------------------------------------------------------

// FollowDM marks the DM conversation with peerUID as followed (followed_dm=1).
// If categoryID is non-nil the DM is placed into that group_category UUID.
//
// PR #21 Round-6 (Jerry-Xin)：categoryID 类型由 *int64 改为 *string，与
// group_category.category_id (VARCHAR(32) UUID) 一致；DM 与群共用同一分类 namespace
// 由原型 image-v1.png 证实。
// 校验顺序：
//   - 入参合法（uid/spaceID/peerUID 非空）
//   - 事务内 authorizeDMCategoryInTx 校验 categoryID 属于 uid 且 status==1
//     （issue #75：原 DMCategoryChecker 在事务外校验，存在 TOCTOU 窗口；
//     现在挪进 withTx 并配 SELECT ... FOR UPDATE）
//
// PR review (Round 3) Blocking #1/#2 — bumps follow_version in same tx.
func (s *Service) FollowDM(uid, spaceID, peerUID string, categoryID *string) error {
	if err := validateBase(uid, spaceID); err != nil {
		return err
	}
	if peerUID == "" {
		return errors.New("peer_uid must not be empty")
	}
	if categoryID != nil && *categoryID == "" {
		return errors.New("category_id must not be empty string")
	}
	one := int8(1)
	fields := ConvExtFields{
		FollowedDM:   &one,
		DMCategoryID: categoryID,
	}
	// PR #21 review (lml2468 blocker #2)：先 bump 后改 ext，与 UpdateSort 同序拿锁。
	return s.withTx("FollowDM", func(tx *dbr.Tx) error {
		if categoryID != nil {
			if err := authorizeDMCategoryInTx(tx, uid, spaceID, *categoryID); err != nil {
				return err
			}
		}
		if _, err := BumpFollowVersionTx(tx, uid, spaceID); err != nil {
			return fmt.Errorf("FollowDM bump version: %w", err)
		}
		if err := upsertTx(tx, uid, spaceID, targetTypeDM, peerUID, fields); err != nil {
			return fmt.Errorf("FollowDM upsert: %w", err)
		}
		return nil
	})
}

// authorizeDMCategoryInTx validates the category for a DM follow operation
// inside the caller's transaction, holding an X lock on the row in
// group_category that matches category_id (via the uk_category_id unique
// index; InnoDB also locks the corresponding clustered-index row, so this
// serialises against the delete path). Replaces the former
// AuthorizeDMCategory checker (modules/message/1module.go) which ran
// outside the tx and let a concurrent delete commit between the SELECT and
// the upsert.
//
// Lock predicate is a `WHERE category_id=?` UNIQUE-index equality — in
// REPEATABLE READ this takes only a record lock on a hit, avoiding the
// next-key (gap) lock that a non-unique predicate (e.g. `WHERE status=1`)
// would acquire. Status / owner / space checks live in Go for the same
// reason.
//
// Returns ErrDMCategoryForbidden for:
//   - category missing (dbr.ErrNotFound)
//   - status != 1 (deleted)
//   - uid mismatch (not the category owner)
//   - space_id mismatch (category from a different space)
//
// DB errors are wrapped with the function name to mirror the
// `"<op>: %w"` pattern used by FollowChannel / FollowDM bump sites in
// this file, so call-site logs attribute infra failures correctly.
func authorizeDMCategoryInTx(tx *dbr.Tx, uid, spaceID, categoryID string) error {
	var row struct {
		UID     string `db:"uid"`
		SpaceID string `db:"space_id"`
		Status  int    `db:"status"`
	}
	err := tx.SelectBySql(
		"SELECT uid, space_id, status FROM group_category WHERE category_id=? FOR UPDATE",
		categoryID,
	).LoadOne(&row)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			return ErrDMCategoryForbidden
		}
		return fmt.Errorf("authorizeDMCategoryInTx: %w", err)
	}
	if row.UID != uid || row.SpaceID != spaceID || row.Status != 1 {
		return ErrDMCategoryForbidden
	}
	return nil
}

// ---------------------------------------------------------------------------
// UnfollowDM — delete ext row
// ---------------------------------------------------------------------------

// UnfollowDM removes the ext row for the DM conversation with peerUID.
// Deleting is cleaner than setting followed_dm=0 because it frees the row
// and avoids stale dm_category_id values.
// PR review (Round 3) Blocking #1/#2 — bumps follow_version in same tx.
func (s *Service) UnfollowDM(uid, spaceID, peerUID string) error {
	if err := validateBase(uid, spaceID); err != nil {
		return err
	}
	if peerUID == "" {
		return errors.New("peer_uid must not be empty")
	}
	// PR #21 review (lml2468 blocker #2)：先 bump 后改 ext，与 UpdateSort 同序拿锁。
	return s.withTx("UnfollowDM", func(tx *dbr.Tx) error {
		if _, err := BumpFollowVersionTx(tx, uid, spaceID); err != nil {
			return fmt.Errorf("UnfollowDM bump version: %w", err)
		}
		if _, err := tx.DeleteFrom(table).
			Where("uid=? AND space_id=? AND target_type=? AND target_id=?",
				uid, spaceID, targetTypeDM, peerUID).Exec(); err != nil {
			return fmt.Errorf("UnfollowDM delete: %w", err)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Internal transaction helpers
// ---------------------------------------------------------------------------

// upsertTx is the transaction-scoped counterpart of DB.Upsert.
// It reuses buildUpsertParts so the SQL construction logic stays in one place.
func upsertTx(tx *dbr.Tx, uid, spaceID string, targetType uint8, targetID string, fields ConvExtFields) error {
	extraCols, extraVals, setClauses, setArgs := buildUpsertParts(fields)

	if len(setClauses) == 0 {
		_, err := tx.InsertBySql(
			"INSERT IGNORE INTO "+table+
				" (uid, space_id, target_type, target_id) VALUES (?, ?, ?, ?)",
			uid, spaceID, targetType, targetID,
		).Exec()
		return err
	}

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
	_, err := tx.InsertBySql(query, insertArgs...).Exec()
	return err
}
