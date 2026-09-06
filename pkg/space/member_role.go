package space

import (
	"github.com/gocraft/dbr/v2"
)

// Space member roles. Mirrors modules/space's MemberRole* constants, spelled out
// here rather than imported: modules/space imports this package, so referencing
// its constants would be an import cycle. Same reason CheckMembershipForCleanup
// spells out its status literal.
const (
	// MemberRoleCommon is an ordinary Space member.
	MemberRoleCommon = 0
	// MemberRoleAdmin is a Space administrator.
	MemberRoleAdmin = 1
	// MemberRoleOwner is the Space owner.
	MemberRoleOwner = 2
)

// MemberRole returns uid's role in spaceID. ok=false means uid is NOT an active
// member of an active Space, and callers MUST treat that as "no role at all"
// rather than falling back to MemberRoleCommon — the zero value of an int is a
// valid role, so a caller that ignores ok silently grants ordinary-member rights
// to a non-member.
//
// The predicate is CheckMembership's (space_member.status=1 AND space.status=1),
// deliberately NOT CheckMembershipForCleanup's relaxed variant: this answers an
// authorization question, and a banned Space must never pass an auth gate
// (Mininglamp-OSS/octo-server#797).
func MemberRole(session *dbr.Session, spaceID string, uid string) (int, bool, error) {
	if spaceID == "" || uid == "" {
		return 0, false, nil
	}
	var roles []int
	rows, err := session.SelectBySql(
		"SELECT sm.role FROM space_member sm "+
			"INNER JOIN space s ON s.space_id = sm.space_id AND s.status = 1 "+
			"WHERE sm.uid = ? AND sm.space_id = ? AND sm.status = 1 LIMIT 1",
		uid, spaceID,
	).Load(&roles)
	if err != nil {
		return 0, false, err
	}
	if rows == 0 || len(roles) == 0 {
		return 0, false, nil
	}
	return roles[0], true, nil
}
