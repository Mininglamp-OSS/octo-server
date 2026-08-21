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

// ActiveMemberSet 批量判定一组 uid 是否是 spaceID 的活跃成员，返回命中集合。
//
// 与 CheckMembership 同口径（sm.status=1 且 s.status=1），只是把 N 次单点查询
// 折成一次 IN 查询。给「拿着一批候选 uid 收窄到本 Space 成员」的场景用，
// 避免为此把整个 Space 的成员名单拉进内存。
// 空输入或空 spaceID 直接返回空集合，不查库。
func ActiveMemberSet(session *dbr.Session, spaceID string, uids []string) (map[string]bool, error) {
	set := make(map[string]bool, len(uids))
	if spaceID == "" || len(uids) == 0 {
		return set, nil
	}
	var found []string
	_, err := session.SelectBySql(
		"SELECT sm.uid FROM space_member sm "+
			"INNER JOIN space s ON s.space_id = sm.space_id AND s.status = 1 "+
			"WHERE sm.space_id = ? AND sm.status = 1 AND sm.uid IN ?",
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
	var count int
	err := session.SelectBySql(
		"SELECT COUNT(*) FROM space_member sm1 "+
			"INNER JOIN space_member sm2 ON sm1.space_id = sm2.space_id "+
			"INNER JOIN space s ON s.space_id = sm1.space_id AND s.status = 1 "+
			"WHERE sm1.uid = ? AND sm2.uid = ? AND sm1.status = 1 AND sm2.status = 1 "+
			"LIMIT 1",
		uidA, uidB,
	).LoadOne(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}
