package conversation_ext

import (
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
// Errors are logged as warnings and never propagated so that the caller's main
// flow is not interrupted.
func RemoveConvExtForUserInSpace(uid, spaceID, channelID string, channelType uint8) {
	if globalConvExtDB == nil {
		return
	}
	db := globalConvExtDB

	// Delete the channel row itself.
	if _, err := db.session.DeleteFrom(table).
		Where("uid=? AND space_id=? AND target_type=? AND target_id=?",
			uid, spaceID, channelType, channelID).
		Exec(); err != nil {
		db.Warn("RemoveConvExtForUserInSpace: 删除频道 ext 行失败",
			zap.String("uid", uid),
			zap.String("spaceID", spaceID),
			zap.String("channelID", channelID),
			zap.Uint8("channelType", channelType),
			zap.Error(err))
	}

	// When the channel is a group (target_type=2), also cascade-delete all its
	// child thread rows (target_type=5, target_id LIKE '{channelID}_____%').
	if channelType == targetTypeGroup {
		prefix := escapeLike(channelID) + threadSeparator + "%"
		if _, err := db.session.DeleteBySql(
			"DELETE FROM "+table+
				" WHERE uid=? AND space_id=? AND target_type=? AND target_id LIKE ? ESCAPE '|'",
			uid, spaceID, targetTypeThread, prefix,
		).Exec(); err != nil {
			db.Warn("RemoveConvExtForUserInSpace: 级联删除子区 ext 行失败",
				zap.String("uid", uid),
				zap.String("spaceID", spaceID),
				zap.String("channelID", channelID),
				zap.Error(err))
		}
	}
}

// RemoveConvExtForUser cleans up all DM ext rows (target_type=1) from uid toward
// peerUID across every space.  Intended for use when two users delete each other
// as friends.  Errors are logged as warnings and never propagated.
func RemoveConvExtForUser(uid, peerUID string) {
	if globalConvExtDB == nil {
		return
	}
	db := globalConvExtDB
	if _, err := db.session.DeleteFrom(table).
		Where("uid=? AND target_type=? AND target_id=?",
			uid, targetTypeDM, peerUID).
		Exec(); err != nil {
		db.Warn("RemoveConvExtForUser: 删除 DM ext 行失败",
			zap.String("uid", uid),
			zap.String("peerUID", peerUID),
			zap.Error(err))
	}
}

// RemoveConvExtForChannel removes all ext rows for a given channel across every
// user.  Intended for use when a group is disbanded or a thread is deleted.
//
// When channelType equals targetTypeGroup (2) the function also deletes all child
// thread rows (target_type=5) whose target_id begins with "{channelID}____",
// so that a single call cleans up both the group and every thread in it.
//
// Errors are logged as warnings and never propagated.
func RemoveConvExtForChannel(channelID string, channelType uint8) {
	if globalConvExtDB == nil {
		return
	}
	db := globalConvExtDB

	// Delete all rows for this channel (across all users and spaces).
	if _, err := db.session.DeleteFrom(table).
		Where("target_type=? AND target_id=?", channelType, channelID).
		Exec(); err != nil {
		db.Warn("RemoveConvExtForChannel: 删除频道 ext 行失败",
			zap.String("channelID", channelID),
			zap.Uint8("channelType", channelType),
			zap.Error(err))
	}

	// Cascade: when the channel is a group, also delete all child thread rows.
	if channelType == targetTypeGroup {
		prefix := escapeLike(channelID) + threadSeparator + "%"
		if _, err := db.session.DeleteBySql(
			"DELETE FROM "+table+
				" WHERE target_type=? AND target_id LIKE ? ESCAPE '|'",
			targetTypeThread, prefix,
		).Exec(); err != nil {
			db.Warn("RemoveConvExtForChannel: 级联删除子区 ext 行失败",
				zap.String("channelID", channelID),
				zap.Error(err))
		}
	}
}
