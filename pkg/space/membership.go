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

// CheckMembershipForCleanup answers a different question from CheckMembership:
// "does uid still hold their seat in this Space, so removal cleanup must SKIP?"
//
// It differs on exactly one axis — a **banned** Space (status=2) still counts.
// Membership there is real: Manager.addMembers rejects only SpaceStatusDisbanded
// (modules/space/api_manager.go:638), so adding people to a banned Space is allowed,
// and the cleanup pipeline must not tear an active member out of every group merely
// because their Space was banned. A **disbanded** Space (status=0) does not count:
// the Space is gone, so a surviving space_member row is a join-vs-disband orphan and
// cleanup must proceed.
//
// Both removal-cleanup gates MUST use this rather than CheckMembership, so the two
// layers answer the same question:
//   - the worker gate      (modules/space/member_removal.go)
//   - the group cascade step (modules/group/space_member_removal.go)
//
// **Do NOT use this for authorization.** Access control is CheckMembership's job and
// requires space.status=1 — a banned Space must never pass an auth gate. CheckMembership
// has 37 non-test call sites including SpaceMiddleware; relaxing it instead of adding
// this second predicate would admit banned Spaces across the whole authenticated API
// (Mininglamp-OSS/octo-server#797).
//
// The status literal is spelled out rather than referencing modules/space's
// SpaceStatusDisbanded: modules/space imports this package, so the constant is
// unreachable here without an import cycle.
func CheckMembershipForCleanup(session *dbr.Session, spaceID string, uid string) (bool, error) {
	if spaceID == "" || uid == "" {
		return false, nil
	}
	var count int
	err := session.SelectBySql(
		"SELECT COUNT(*) FROM space_member sm "+
			"INNER JOIN space s ON s.space_id = sm.space_id AND s.status <> 0 "+
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
