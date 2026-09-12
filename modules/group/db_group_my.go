package group

import "github.com/Mininglamp-OSS/octo-lib/common"

// queryGroupsWithMemberUIDAndRoles returns groups where memberUID currently has
// one of the requested native management roles. It deliberately has a
// different, stricter predicate from the legacy no-role queries: role-filtered
// results are limited to active native members of live product groups and to
// Spaces where the caller still has an active seat.
func (d *DB) queryGroupsWithMemberUIDAndRoles(memberUID, spaceID string, roles []int) ([]*Model, error) {
	if memberUID == "" || len(roles) == 0 {
		return make([]*Model, 0), nil
	}

	builder := d.session.
		Select("distinct `group`.*").
		From("`group`").
		Join("group_member", "`group`.group_no=group_member.group_no").
		Where("group_member.uid=? AND group_member.is_deleted=0 AND group_member.status=? AND group_member.is_external=0 AND group_member.role IN ? AND `group`.status<>? AND `group`.purpose=''",
			memberUID, common.GroupMemberStatusNormal, roles, GroupStatusDisband).
		Where("(`group`.space_id='' OR EXISTS (SELECT 1 FROM space_member sm INNER JOIN space s ON s.space_id=sm.space_id AND s.status=1 WHERE sm.space_id=`group`.space_id AND sm.uid=? AND sm.status=1))", memberUID)
	if spaceID != "" {
		builder = builder.Where("`group`.space_id=?", spaceID)
	}

	var models []*Model
	_, err := builder.Load(&models)
	return models, err
}

// queryGroupMyRoles returns the stored native role for every requested group
// in one query. The legacy list modes already decide which rows are visible;
// this lookup only backfills the role field and therefore intentionally keeps
// their is_deleted-only semantics.
func (d *DB) queryGroupMyRoles(memberUID string, groupNos []string) (map[string]int, error) {
	roles := make(map[string]int, len(groupNos))
	if memberUID == "" || len(groupNos) == 0 {
		return roles, nil
	}

	var rows []struct {
		GroupNo string `db:"group_no"`
		Role    int    `db:"role"`
	}
	_, err := d.session.
		Select("group_no", "role").
		From("group_member").
		Where("uid=? AND group_no IN ? AND is_deleted=0", memberUID, groupNos).
		Load(&rows)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		roles[row.GroupNo] = row.Role
	}
	return roles, nil
}

// queryGroupMemberCounts returns member counts for all requested groups in one
// grouped query, preserving QueryMemberCount's is_deleted-only inclusion rule.
func (d *DB) queryGroupMemberCounts(groupNos []string) (map[string]int64, error) {
	counts := make(map[string]int64, len(groupNos))
	if len(groupNos) == 0 {
		return counts, nil
	}

	var rows []struct {
		GroupNo string `db:"group_no"`
		Count   int64  `db:"member_count"`
	}
	if _, err := d.session.
		Select("group_no", "COUNT(*) AS member_count").
		From("group_member").
		Where("group_no IN ? AND is_deleted=0", groupNos).
		GroupBy("group_no").
		Load(&rows); err != nil {
		return counts, err
	}
	for _, row := range rows {
		counts[row.GroupNo] = row.Count
	}
	return counts, nil
}
