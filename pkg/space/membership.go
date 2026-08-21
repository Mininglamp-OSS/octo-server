package space

import (
	"github.com/gocraft/dbr/v2"
)

// CheckMembership checks if uid is an active member of the given Space.
// Also verifies the Space itself is active (space.status=1).
func CheckMembership(session *dbr.Session, spaceID string, uid string) (bool, error) {
	if spaceID == "" || uid == "" {
		return false, nil
	}
	var count int
	err := session.SelectBySql(
		"SELECT COUNT(*) FROM space_member sm "+
			"INNER JOIN space s ON s.space_id = sm.space_id AND s.status = 1 "+
			"WHERE sm.uid = ? AND sm.space_id = ? AND sm.status = 1",
		uid, spaceID,
	).LoadOne(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// HaveCommonSpace checks if uid1 and uid2 share at least one active Space membership.
// Used to prevent cross-Space existence probing in user search.
func HaveCommonSpace(session *dbr.Session, uid1, uid2 string) (bool, error) {
	if uid1 == "" || uid2 == "" {
		return false, nil
	}
	if uid1 == uid2 {
		return true, nil
	}
	var count int
	err := session.SelectBySql(
		"SELECT COUNT(*) FROM space_member a "+
			"INNER JOIN space_member b ON a.space_id = b.space_id "+
			"INNER JOIN space s ON s.space_id = a.space_id AND s.status = 1 "+
			"WHERE a.uid=? AND b.uid=? AND a.status=1 AND b.status=1",
		uid1, uid2,
	).LoadOne(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// CheckBothMembers checks if both uid1 and uid2 are active members of the given Space.
// The Space itself must also be active (space.status=1); a disabled Space returns false
// even if both users still have space_member rows, matching CheckMembership's semantics.
func CheckBothMembers(session *dbr.Session, spaceID string, uid1, uid2 string) (bool, error) {
	if spaceID == "" || uid1 == "" || uid2 == "" {
		return false, nil
	}
	var count int
	err := session.SelectBySql(
		"SELECT COUNT(DISTINCT sm.uid) FROM space_member sm "+
			"INNER JOIN space s ON s.space_id = sm.space_id AND s.status = 1 "+
			"WHERE sm.space_id=? AND sm.uid IN (?,?) AND sm.status=1",
		spaceID, uid1, uid2,
	).LoadOne(&count)
	if err != nil {
		return false, err
	}
	return count == 2, nil
}

// SharesActiveSpace 判断两个 uid 是否至少同处一个**活跃** Space。
//
// 谓词与 modules/space 的 queryCoMemberUIDs 完全一致（两侧 sm.status=1 且
// s.status=1）—— 那正是 Person 频道白名单的推导来源，所以任何「这对还能不能私聊」
// 的判断都必须用同一口径，否则服务端拦截与前端展示会互相矛盾。
//
// 注意不要用 modules/space.GetCommonSpaceID 代替：它不校验 space.status，
// 已解散 Space 里的两个人也会被判为共处。
func SharesActiveSpace(session *dbr.Session, uidA, uidB string) (bool, error) {
	if uidA == "" || uidB == "" || uidA == uidB {
		return false, nil
	}
	// EXISTS 而不是 COUNT(*)：LIMIT 对无 GROUP BY 的聚合是无效的（聚合本来就只出
	// 一行），COUNT 会把所有共同 Space 的行数完；EXISTS 命中第一行即短路。
	var found int
	err := session.SelectBySql(
		"SELECT EXISTS(SELECT 1 FROM space_member sm1 "+
			"INNER JOIN space_member sm2 ON sm1.space_id = sm2.space_id "+
			"INNER JOIN space s ON s.space_id = sm1.space_id AND s.status = 1 "+
			"WHERE sm1.uid = ? AND sm2.uid = ? AND sm1.status = 1 AND sm2.status = 1)",
		uidA, uidB,
	).LoadOne(&found)
	if err != nil {
		return false, err
	}
	return found > 0, nil
}

// MembersEverInSpace 返回这批 uid 中「在该 Space 有过成员行」的子集，**不看状态**。
//
// **不看状态**是刻意的：它回答的是「这个人是否属于本 Space 的范畴」，而不是
// 「他现在还是不是成员」。成员移除后的会话面清理必须用前者——按活跃口径筛会在
// 两种情况下静默地什么都不做：
//
//   - 空间被解散：forceDisbandSpace 先把 space.status 置 0，按活跃口径的
//     `INNER JOIN space ... AND s.status = 1` 于是永远返回空集；
//   - 同一批移除多人：A、B 一起被移除后各自的清理工单里，对方的 sm.status 已经是 0，
//     互相被过滤掉，两人之间的私聊永远切不断。
//
// 两种情况下"该清理却没清理"都是隔离失效，所以这里不能带任何 status 谓词。
func MembersEverInSpace(session *dbr.Session, spaceID string, uids []string) (map[string]bool, error) {
	set := make(map[string]bool, len(uids))
	if spaceID == "" || len(uids) == 0 {
		return set, nil
	}
	var found []string
	_, err := session.SelectBySql(
		"SELECT uid FROM space_member WHERE space_id = ? AND uid IN ?",
		spaceID, uids,
	).Load(&found)
	if err != nil {
		return nil, err
	}
	for _, uid := range found {
		set[uid] = true
	}
	return set, nil
}
