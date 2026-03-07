package space

import (
	"github.com/gocraft/dbr/v2"
)

// CheckMembership checks if uid is an active member of the given Space.
func CheckMembership(session *dbr.Session, spaceID string, uid string) (bool, error) {
	if spaceID == "" || uid == "" {
		return false, nil
	}
	var count int
	err := session.Select("COUNT(*)").From("space_member").
		Where("space_id=? AND uid=? AND status=1", spaceID, uid).
		LoadOne(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// CheckBothMembers checks if both uid1 and uid2 are active members of the given Space.
func CheckBothMembers(session *dbr.Session, spaceID string, uid1, uid2 string) (bool, error) {
	if spaceID == "" || uid1 == "" || uid2 == "" {
		return false, nil
	}
	var count int
	err := session.Select("COUNT(DISTINCT uid)").From("space_member").
		Where("space_id=? AND uid IN (?,?) AND status=1", spaceID, uid1, uid2).
		LoadOne(&count)
	if err != nil {
		return false, err
	}
	return count == 2, nil
}
