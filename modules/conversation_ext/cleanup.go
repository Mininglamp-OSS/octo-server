package conversation_ext

import (
	"sort"
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// Global DB singleton — mirrors modules/user/db_pinned.go globalPinnedDB
// ---------------------------------------------------------------------------

var (
	globalConvExtDB     *DB
	globalConvExtDBOnce sync.Once
)

// InitGlobalConvExtDB initialises the package-level *DB singleton.
// It is idempotent: repeated calls after the first are no-ops (sync.Once).
// Called from 1module.go init so the singleton is ready before any cascade-
// cleanup function is invoked by group / thread / user modules.
func InitGlobalConvExtDB(ctx *config.Context) {
	globalConvExtDBOnce.Do(func() {
		globalConvExtDB = NewDB(ctx)
	})
}

// ---------------------------------------------------------------------------
// Cascade-cleanup functions — concrete, no-error-bubble style
// (mirrors RemovePinnedForUserInSpace / RemovePinnedForUser / RemovePinnedForChannel
// in modules/user/db_pinned.go)
// ---------------------------------------------------------------------------

// RemoveConvExtForUserInSpace cleans up a user's ext rows for a specific channel
// and all its child threads within a given space.  Intended for use when a user
// leaves a group or is kicked: call once for the group channel (channelType=2),
// and optionally once per thread (channelType=5).
//
// When channelType equals targetTypeGroup (2) the function also deletes, in the
// same space, every thread row whose target_id begins with "{channelID}____",
// mirroring the cascade logic in service.UnfollowChannel.
//
// Atomicity (PR review Blocking #2): when the cascade applies (channelType=2)
// the channel-row DELETE and the thread-cascade DELETE are wrapped in a single
// transaction so a partial failure cannot leave orphaned thread rows.  Errors
// are still logged as warnings and never propagated so the caller's main flow
// is not interrupted; on failure the transaction rolls back and a subsequent
// retry can re-run the same cleanup cleanly.
func RemoveConvExtForUserInSpace(uid, spaceID, channelID string, channelType uint8) {
	if globalConvExtDB == nil {
		return
	}
	db := globalConvExtDB

	// PR review (Round 3) Blocking #1/#2 — cascade cleanup also alters the user's
	// follow set, so user_follow_version must be bumped in the same tx so the
	// client can detect the change on the next sidebar sync.
	tx, err := db.session.Begin()
	if err != nil {
		db.Warn("RemoveConvExtForUserInSpace: Begin 失败",
			zap.String("uid", uid),
			zap.String("spaceID", spaceID),
			zap.String("channelID", channelID),
			zap.Error(err))
		return
	}
	defer tx.RollbackUnlessCommitted()

	// PR #21 review (lml2468 blocker #2)：先 bump 后 delete，与 UpdateSort 同序拿锁。
	if _, err := BumpFollowVersionTx(tx, uid, spaceID); err != nil {
		db.Warn("RemoveConvExtForUserInSpace: bump follow_version 失败（事务回滚）",
			zap.String("uid", uid),
			zap.String("spaceID", spaceID),
			zap.Error(err))
		return
	}

	if _, err := tx.DeleteFrom(table).
		Where("uid=? AND space_id=? AND target_type=? AND target_id=?",
			uid, spaceID, channelType, channelID).
		Exec(); err != nil {
		db.Warn("RemoveConvExtForUserInSpace: 删除频道 ext 行失败（事务回滚）",
			zap.String("uid", uid),
			zap.String("spaceID", spaceID),
			zap.String("channelID", channelID),
			zap.Uint8("channelType", channelType),
			zap.Error(err))
		return
	}

	// Cascade thread rows only for group cleanup.
	if channelType == targetTypeGroup {
		if _, err := tx.DeleteBySql(
			"DELETE FROM "+table+
				" WHERE uid=? AND space_id=? AND target_type=? AND target_id LIKE ? ESCAPE '|'",
			uid, spaceID, targetTypeThread, threadLikePrefix(channelID),
		).Exec(); err != nil {
			db.Warn("RemoveConvExtForUserInSpace: 级联删除子区 ext 行失败（事务回滚）",
				zap.String("uid", uid),
				zap.String("spaceID", spaceID),
				zap.String("channelID", channelID),
				zap.Error(err))
			return
		}
	}

	if err := tx.Commit(); err != nil {
		db.Warn("RemoveConvExtForUserInSpace: Commit 失败",
			zap.String("uid", uid),
			zap.String("spaceID", spaceID),
			zap.String("channelID", channelID),
			zap.Error(err))
	}
}

// RemoveConvExtForUser cleans up all DM ext rows (target_type=1) from uid toward
// peerUID across every space.  Intended for use when two users delete each other
// as friends.  Errors are logged as warnings and never propagated.
//
// PR review (Round 3) Blocking #1/#2 — affected (uid, spaceID) pairs have their
// user_follow_version bumped in the same transaction so the next sidebar sync
// observes the removal.
func RemoveConvExtForUser(uid, peerUID string) {
	if globalConvExtDB == nil {
		return
	}
	db := globalConvExtDB

	tx, err := db.session.Begin()
	if err != nil {
		db.Warn("RemoveConvExtForUser: Begin 失败", zap.String("uid", uid), zap.Error(err))
		return
	}
	defer tx.RollbackUnlessCommitted()

	// 1. 先采集受影响的 (uid, space_id) 集合。
	var spaces []string
	if _, err := tx.SelectBySql(
		"SELECT DISTINCT space_id FROM "+table+
			" WHERE uid=? AND target_type=? AND target_id=?",
		uid, targetTypeDM, peerUID,
	).Load(&spaces); err != nil {
		db.Warn("RemoveConvExtForUser: SELECT spaces 失败",
			zap.String("uid", uid), zap.String("peerUID", peerUID), zap.Error(err))
		return
	}
	// 2. 按 space_id 字典序排序后再依次 bump follow_version：
	//    - PR #21 review (yujiawei P2-4)：避免两个并发 cleanup 在重叠 space 上
	//      以相反顺序拿锁导致 MySQL 死锁；
	//    - PR #21 review (lml2468 blocker #2)：bump 也必须先于 ext DELETE，
	//      与 UpdateSort 同序拿 (version → ext) 锁。
	sort.Strings(spaces)
	for _, sp := range spaces {
		if _, err := BumpFollowVersionTx(tx, uid, sp); err != nil {
			db.Warn("RemoveConvExtForUser: bump follow_version 失败（事务回滚）",
				zap.String("uid", uid), zap.String("spaceID", sp), zap.Error(err))
			return
		}
	}
	// 3. DELETE。
	if _, err := tx.DeleteBySql(
		"DELETE FROM "+table+" WHERE uid=? AND target_type=? AND target_id=?",
		uid, targetTypeDM, peerUID,
	).Exec(); err != nil {
		db.Warn("RemoveConvExtForUser: 删除 DM ext 行失败（事务回滚）",
			zap.String("uid", uid), zap.String("peerUID", peerUID), zap.Error(err))
		return
	}
	if err := tx.Commit(); err != nil {
		db.Warn("RemoveConvExtForUser: Commit 失败", zap.String("uid", uid), zap.Error(err))
	}
}

// RemoveConvExtForChannel removes all ext rows for a given channel across every
// user.  Intended for use when a group is disbanded or a thread is deleted.
//
// When channelType equals targetTypeGroup (2) the function also deletes all child
// thread rows (target_type=5) whose target_id begins with "{channelID}____",
// so that a single call cleans up both the group and every thread in it.
//
// Atomicity (PR review Blocking #2): when the cascade applies (channelType=2)
// the channel-row DELETE and the thread-cascade DELETE are wrapped in a single
// transaction so a partial failure cannot leave orphaned thread rows.  Errors
// are logged as warnings and never propagated.
func RemoveConvExtForChannel(channelID string, channelType uint8) {
	if globalConvExtDB == nil {
		return
	}
	db := globalConvExtDB

	tx, err := db.session.Begin()
	if err != nil {
		db.Warn("RemoveConvExtForChannel: Begin 失败",
			zap.String("channelID", channelID), zap.Error(err))
		return
	}
	defer tx.RollbackUnlessCommitted()

	// 1. 先采集受影响的 (uid, space_id) 集合。
	//    group 情况下要把父 channel + 子 thread 的 owner 一并采集，
	//    所以用一次 SELECT DISTINCT 合并查。
	type owner struct {
		UID     string `db:"uid"`
		SpaceID string `db:"space_id"`
	}
	var owners []owner
	if channelType == targetTypeGroup {
		if _, err := tx.SelectBySql(
			"SELECT DISTINCT uid, space_id FROM "+table+
				" WHERE (target_type=? AND target_id=?)"+
				" OR (target_type=? AND target_id LIKE ? ESCAPE '|')",
			channelType, channelID, targetTypeThread, threadLikePrefix(channelID),
		).Load(&owners); err != nil {
			db.Warn("RemoveConvExtForChannel: SELECT owners 失败",
				zap.String("channelID", channelID), zap.Error(err))
			return
		}
	} else {
		if _, err := tx.SelectBySql(
			"SELECT DISTINCT uid, space_id FROM "+table+
				" WHERE target_type=? AND target_id=?",
			channelType, channelID,
		).Load(&owners); err != nil {
			db.Warn("RemoveConvExtForChannel: SELECT owners 失败",
				zap.String("channelID", channelID), zap.Error(err))
			return
		}
	}

	// 2. 按 (uid, space_id) 字典序排序后依次 bump follow_version：
	//    - PR #21 review (yujiawei P2-4)：相同顺序拿锁，避免并发 cleanup 死锁；
	//    - PR #21 review (lml2468 blocker #2)：bump 必须先于 ext DELETE，
	//      与 UpdateSort 同序拿 (version → ext) 锁。
	sort.Slice(owners, func(i, j int) bool {
		if owners[i].UID != owners[j].UID {
			return owners[i].UID < owners[j].UID
		}
		return owners[i].SpaceID < owners[j].SpaceID
	})
	for _, o := range owners {
		if _, err := BumpFollowVersionTx(tx, o.UID, o.SpaceID); err != nil {
			db.Warn("RemoveConvExtForChannel: bump follow_version 失败（事务回滚）",
				zap.String("uid", o.UID), zap.String("spaceID", o.SpaceID),
				zap.String("channelID", channelID), zap.Error(err))
			return
		}
	}

	// 3. 主 DELETE（group 时还要级联 thread）。
	if _, err := tx.DeleteFrom(table).
		Where("target_type=? AND target_id=?", channelType, channelID).Exec(); err != nil {
		db.Warn("RemoveConvExtForChannel: 删除频道 ext 行失败（事务回滚）",
			zap.String("channelID", channelID), zap.Uint8("channelType", channelType), zap.Error(err))
		return
	}
	if channelType == targetTypeGroup {
		if _, err := tx.DeleteBySql(
			"DELETE FROM "+table+
				" WHERE target_type=? AND target_id LIKE ? ESCAPE '|'",
			targetTypeThread, threadLikePrefix(channelID),
		).Exec(); err != nil {
			db.Warn("RemoveConvExtForChannel: 级联删除子区 ext 行失败（事务回滚）",
				zap.String("channelID", channelID), zap.Error(err))
			return
		}
	}

	if err := tx.Commit(); err != nil {
		db.Warn("RemoveConvExtForChannel: Commit 失败",
			zap.String("channelID", channelID), zap.Error(err))
	}
}
