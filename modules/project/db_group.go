package project

import "fmt"

// The project-scoped group read view.
//
// # Why this lives in modules/project and reads `group` directly
//
// modules/project must never import modules/group — pkg/project/import_guard_test.go
// pins that at zero, and modules/group already imports THIS package for the P1
// cascade and the P2 all-member-group hooks, so the dependency only has one legal
// direction. Reading the `group` TABLE is not importing the package, and this
// module already does it: the I2/I3 reconcile scans (reconcile_p1.go) and the
// all-member-group DAO (db_all_member_group.go) both join `group` from here.
//
// The alternative — a reverse-registered read hook, the shape
// all_member_group_registry.go uses for writes — is wrong for a synchronous
// request path: an unregistered provider would answer an EMPTY LIST, which is
// indistinguishable from "this project has no groups". A missing write hook
// leaves a repairable gap that a reconcile scan reports; a missing read hook
// silently answers the wrong thing to the client.
//
// # No COLLATE in either statement, deliberately
//
// The module's rule is that a COLLATE belongs exactly where a legacy column
// (`group`, `group_member`, created in 2019 with no explicit collation, measured
// at utf8mb4_0900_ai_ci in production) meets a pinned utf8mb4_general_ci one
// (`octo_project*`). Neither statement here crosses that boundary:
//
//   - `group` ⋈ `group_member` is legacy-to-legacy. Coercing one side would make
//     the predicate non-sargable and give up the index on group_member.group_no,
//     to fix a mismatch that does not exist.
//   - space_id / project_id / uid arrive as BIND PARAMETERS, not as columns of
//     octo_project*. A literal is coercible to the column's own collation, so
//     there is no cross-schema comparison to coerce.
//
// Stated rather than left as an absence, because "this query has no COLLATE" and
// "this query forgot its COLLATE" look identical in a diff.

// projectGroupRow is one row of the project group list.
//
// The Go types mirror modules/group's own Model rather than being re-decided
// here: AvatarColor is a *int because the column is nullable (NULL = derive the
// colour from group_no), IsNamed is a plain int because its migration ends by
// tightening the column to NOT NULL DEFAULT 0.
type projectGroupRow struct {
	GroupNo        string `db:"group_no"`
	Name           string `db:"name"`
	IsNamed        int    `db:"is_named"`
	AvatarText     string `db:"avatar_text"`
	AvatarColor    *int   `db:"avatar_color"`
	IsUploadAvatar int    `db:"is_upload_avatar"`
}

// listMyProjectGroups returns the LIVE groups of one project that uid is an
// active member of, oldest first.
//
// # Membership-scoped, and that is the access control
//
// The list is the caller's own groups, not the project's. I2 is a ceiling, not a
// floor: being an active project member does not put you in any particular group
// of that project, so a project-wide list would hand a member the names of groups
// they hold no seat in — and a group name is the most sensitive field at list
// granularity. There is no self-join path into a group either, so the wider list
// would not even be actionable.
//
// This is also why the handler needs no permission gate beyond projectMiddleware:
// the predicate IS the gate. A Space admin who never joined the project gets an
// empty list rather than a refusal, which is the correct answer and leaks nothing.
//
// # Disbanded groups are excluded, and that filter is load-bearing
//
// Group disband only flips group.status and deliberately leaves group_member rows
// in place — there is no endpoint that cleans them up, so the rows are expected
// rather than a leak. Without this filter every project member would keep seeing
// groups that were disbanded months ago. modules/group's own
// queryProjectGroupNosWithActiveMember carries the same filter for the same
// reason, as does the I2 scan.
//
// # Active member means is_deleted = 0 AND status = 1
//
// That is the repo's canonical predicate (modules/group's ExistMemberActive, and
// both I2 and I4 reconcile scans). Note it is STRICTER than
// queryProjectGroupNosWithActiveMember, which checks is_deleted alone: that one
// feeds a cascade whose job is to remove rows, where over-selecting is harmless.
// A read must not over-select.
//
// # Index
//
// Driven off group_space_project (space_id, project_id). space_id is in the
// predicate rather than inferred from project_id because it is the index's
// LEADING column — without it the index cannot be used at all. The value comes
// from the project row projectMiddleware already verified the caller's Space
// membership against, so it is also the Space isolation boundary, not decoration.
func (d *DB) listMyProjectGroups(spaceID, projectID, uid string, offset, limit int) ([]*projectGroupRow, error) {
	if spaceID == "" || projectID == "" || uid == "" {
		return nil, nil
	}
	var rows []*projectGroupRow
	_, err := d.session.SelectBySql(
		"SELECT g.group_no, g.name, g.is_named, g.avatar_text, g.avatar_color, g.is_upload_avatar "+
			"FROM `group` g "+
			"INNER JOIN `group_member` gm ON gm.group_no = g.group_no "+
			"WHERE g.space_id = ? AND g.project_id = ? AND g.status <> ? "+
			"  AND gm.uid = ? AND gm.is_deleted = 0 AND gm.status = 1 "+
			// g.id ASC is creation order, which puts the all-member group first
			// for free: #855 provisions it with the project, so it is always the
			// project's oldest group. It is also the only ordering available that
			// is total — group_no is a UUID and name is not unique — and a
			// non-total ORDER BY under OFFSET pagination silently drops and
			// duplicates rows between pages.
			"ORDER BY g.id ASC LIMIT ? OFFSET ?",
		spaceID, projectID, groupStatusDisband, uid, limit, offset,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: list my project groups: %w", err)
	}
	return rows, nil
}

// countActiveGroupMembers returns the active member count for each of groupNos.
//
// One grouped query for the whole page, not one query per row. modules/group's
// own group list calls QueryMemberCount inside its loop (api.go's `list`), which
// is N+1 — fine at its sizes, but this endpoint is paginated up to 200 and there
// is no reason to inherit the shape.
//
// Groups with no active members are simply absent from the map; the caller reads
// a missing key as 0. That state is reachable in principle (every member left)
// and is not worth a second query to distinguish from "count is zero".
func (d *DB) countActiveGroupMembers(groupNos []string) (map[string]int, error) {
	if len(groupNos) == 0 {
		return map[string]int{}, nil
	}
	var rows []*struct {
		GroupNo     string `db:"group_no"`
		MemberCount int    `db:"member_count"`
	}
	_, err := d.session.SelectBySql(
		"SELECT group_no, COUNT(*) AS member_count FROM `group_member` "+
			"WHERE group_no IN ? AND is_deleted = 0 AND status = 1 "+
			"GROUP BY group_no",
		groupNos,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: count active group members: %w", err)
	}
	counts := make(map[string]int, len(rows))
	for _, r := range rows {
		counts[r.GroupNo] = r.MemberCount
	}
	return counts, nil
}
