package group

// queryGroupsWithMemberUIDAndRoles returns groups in which memberUID currently
// has one of the requested native group roles. This is intentionally separate
// from the legacy no-role queries: callers without a role filter retain their
// historical membership predicates and saved-group behavior.
func (d *DB) queryGroupsWithMemberUIDAndRoles(memberUID, spaceID string, roles []int) ([]*Model, error) {
	if memberUID == "" || len(roles) == 0 {
		return make([]*Model, 0), nil
	}

	builder := d.session.
		Select("distinct `group`.*").
		From("`group`").
		Join("group_member", "`group`.group_no=group_member.group_no").
		Where("group_member.uid=? and group_member.is_deleted=0 and group_member.status=1 and group_member.role in ?", memberUID, roles)
	if spaceID != "" {
		builder = builder.Where("`group`.space_id=?", spaceID)
	}

	var models []*Model
	_, err := builder.Load(&models)
	return models, err
}

// queryGroupMyRoles returns the native role for each group in groupNos. The
// schema guarantees one member row per (group, uid), so the query deliberately
// projects that row rather than inventing an aggregate role.
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
		Where("uid=? and group_no in ? and is_deleted=0", memberUID, groupNos).
		Load(&rows)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		roles[row.GroupNo] = row.Role
	}
	return roles, nil
}

// queryGroupMemberCounts returns all requested group member counts in one
// grouped query, preserving QueryMemberCount's inclusion of soft-deleted rows
// only when active (is_deleted=0).
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
