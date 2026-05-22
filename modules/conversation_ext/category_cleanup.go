package conversation_ext

import (
	"fmt"

	"github.com/gocraft/dbr/v2"
)

// UnfollowGroupsTx 在传入 tx 内把一组 (uid, spaceID) 下的群批量标记为取消关注
// （group_unfollowed=1），并级联删除每个群下的 thread ext 行。
//
// 语义与 service.UnfollowChannel 一致，区别仅在于：
//   - 作用于多个群，而非单个；
//   - 由调用方持有 tx，并由调用方负责 BumpFollowVersionTx（必须在本函数之前 bump，
//     与 UpdateSort 同序拿 (version → ext) 锁，避免死锁，详见 PR #21 Round-3 blocker #2）。
//
// groupNos 为空时 no-op。
func UnfollowGroupsTx(tx *dbr.Tx, uid, spaceID string, groupNos []string) error {
	if len(groupNos) == 0 {
		return nil
	}
	one := int8(1)
	zero := int8(0)
	for _, groupNo := range groupNos {
		if groupNo == "" {
			continue
		}
		// 必须同时清 auto_follow_threads —— 与 service.UnfollowChannel 同语义：否则
		// 后续 OnThreadCreated 会按 auto_follow_threads=1 把已取关的用户当 fanout 目标。
		if err := upsertTx(tx, uid, spaceID, targetTypeGroup, groupNo, ConvExtFields{
			GroupUnfollowed:   &one,
			AutoFollowThreads: &zero,
		}); err != nil {
			return fmt.Errorf("UnfollowGroupsTx upsert group %q: %w", groupNo, err)
		}
		if _, err := tx.DeleteBySql(
			"DELETE FROM "+table+
				" WHERE uid=? AND space_id=? AND target_type=? AND target_id LIKE ? ESCAPE '|'",
			uid, spaceID, targetTypeThread, threadLikePrefix(groupNo),
		).Exec(); err != nil {
			return fmt.Errorf("UnfollowGroupsTx delete threads for %q: %w", groupNo, err)
		}
	}
	return nil
}

// UnfollowDMsByCategoryTx 在传入 tx 内删除 (uid, spaceID) 下 dm_category_id=categoryID
// 的全部 DM ext 行。
//
// 由调用方负责事先 BumpFollowVersionTx（同 UnfollowGroupsTx 的锁序约束）。
// categoryID 为空时 no-op。
func UnfollowDMsByCategoryTx(tx *dbr.Tx, uid, spaceID, categoryID string) error {
	if categoryID == "" {
		return nil
	}
	if _, err := tx.DeleteBySql(
		"DELETE FROM "+table+
			" WHERE uid=? AND space_id=? AND target_type=? AND dm_category_id=?",
		uid, spaceID, targetTypeDM, categoryID,
	).Exec(); err != nil {
		return fmt.Errorf("UnfollowDMsByCategoryTx delete DMs: %w", err)
	}
	return nil
}
