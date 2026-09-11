package project

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
)

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
// # No COLLATE in either legacy membership statement, deliberately
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

// sqlListMyProjectGroups is the statement listMyProjectGroups runs.
//
// A named constant so the plan guard (TestTheProjectGroupListReachesItsRowsByAnIndex)
// can EXPLAIN the string production actually executes rather than a copy of it. A
// copy passes forever once the two drift, which is the failure mode that makes a
// plan assertion worse than none. #855 established the shape.
//

// g.id ASC is creation order. It is the only TOTAL ordering available
// — group_no is a UUID and name is not unique — and a non-total
// ORDER BY under OFFSET pagination silently drops and duplicates rows
// between pages, so that property is the reason for the choice.
//
// It USUALLY puts the all-member group first, because #855 provisions
// it with the project. Usually, not always: ensureAllMemberGroup
// rebuilds it on a later write path when the first provisioning failed
// or the group was disbanded or detached, and the rebuilt group takes a
// fresh, higher id. So a project that hit that path lists its 全员群
// wherever it now sorts. Nothing here compensates: the client labels it
// by comparing group_no against the all_member_group_no it already has
// from the project detail, which is right in both cases. Position is a
// convenience, never the contract.
const projectGroupSelectColumns = "g.group_no, g.name, g.is_named, g.avatar_text, " +
	"g.avatar_color, g.is_upload_avatar"

const projectGroupFrom = " FROM `group` g " +
	"INNER JOIN `group_member` gm ON gm.group_no = g.group_no "

const projectGroupActiveMembershipPredicate = " AND g.status <> ? " +
	"AND gm.uid = ? AND gm.is_deleted = 0 AND gm.status = ? "

const sqlListMyProjectGroups = "SELECT " + projectGroupSelectColumns + projectGroupFrom +
	"WHERE g.space_id = ? AND g.project_id = ?" + projectGroupActiveMembershipPredicate +
	"ORDER BY g.id ASC LIMIT ? OFFSET ?"

// projectGroupRow is one row of the project group list.
//
// The Go types mirror modules/group's own Model rather than being re-decided
// here: AvatarColor is a *int because the column is nullable (NULL = derive the
// colour from group_no), IsNamed is a plain int because its migration ends by
// tightening the column to NOT NULL DEFAULT 0.
type projectGroupRow struct {
	ProjectID      string `db:"project_id"`
	GroupNo        string `db:"group_no"`
	Name           string `db:"name"`
	IsNamed        int    `db:"is_named"`
	AvatarText     string `db:"avatar_text"`
	AvatarColor    *int   `db:"avatar_color"`
	IsUploadAvatar int    `db:"is_upload_avatar"`
}

const sqlListMyProjectGroupsByProjectIDs = "SELECT g.project_id, " + projectGroupSelectColumns + projectGroupFrom +
	"WHERE g.space_id = ? AND g.project_id IN ?" + projectGroupActiveMembershipPredicate +
	"ORDER BY g.project_id ASC, g.id ASC"

const sqlListProjectGroupRelationsByProjectIDs = "SELECT g.project_id, g.group_no, g.name, g.project_linked_by, " +
	"IFNULL(s.pinned, 0) AS pinned " +
	"FROM `group` g LEFT JOIN `octo_project_group_user_setting` s ON " +
	"s.space_id = g.space_id COLLATE utf8mb4_general_ci " +
	"AND s.project_id = g.project_id COLLATE utf8mb4_general_ci " +
	"AND s.group_no = g.group_no COLLATE utf8mb4_general_ci " +
	"AND s.uid = ? " +
	"WHERE g.space_id = ? AND g.project_id IN ? AND g.status <> ? " +
	"ORDER BY g.project_id ASC, IFNULL(s.pinned, 0) DESC, " +
	"CASE WHEN s.pinned = 1 THEN s.pinned_at END DESC, " +
	"g.id ASC, g.group_no ASC"

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
// The relation endpoint and unified sidebar use the Project-wide relation
// projection below. This legacy helper remains native-membership-scoped for
// callers that render actual chat rooms.
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
// # Active member means is_deleted = 0 AND status = GroupMemberStatusNormal
//
// That is the repo's canonical predicate (modules/group's ExistMemberActive, and
// both I2 and I4 reconcile scans). It is STRICTER than
// queryProjectGroupNosWithActiveMember, which checks is_deleted alone: that one
// feeds a cascade whose job is to remove rows, where over-selecting is harmless.
//
// # What the strictness actually decides: BLACKLISTED members
//
// Spelled out because the abstract argument above hides the only case that
// reaches it. Removal sets is_deleted = 1, so the two predicates agree there. The
// one reachable state where they disagree is the group blacklist, which sets
// status = GroupMemberStatusBlacklist and deliberately LEAVES is_deleted = 0 —
// the blacklist branch of modules/group's ExistMemberActiveInternal spells that
// out. So this endpoint hides a project group from a member that group has
// blacklisted, and two older surfaces still show it to them:
// GET /v1/group/my (queryGroupsWithMemberUIDAndSpaceID filters is_deleted alone)
// and the group detail.
//
// That divergence is chosen, not inherited. Blacklisting is how a group denies
// someone access: ExistMemberActive is the hardening line #343/#345 put in front
// of group and thread reads for exactly this uid, so listing the group here would
// advertise a room they cannot open. The two looser surfaces are the ones behind —
// both are named in P1's "read-path hardening" out-of-scope list, which is a
// separate task. Pinned by TestListProjectGroupsHidesAGroupThatBlacklistedMe, so
// a later change to the predicate has to argue with a test rather than a comment.
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
	_, err := d.session.SelectBySql(sqlListMyProjectGroups,
		spaceID, projectID, groupStatusDisband, uid, int(common.GroupMemberStatusNormal), limit, offset,
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
//
// # This number can differ from modules/group's member_count for the same group
//
// modules/group's own QueryMemberCount filters is_deleted alone, so it counts
// blacklisted members; this one does not. A group holding one blacklisted member
// therefore reads N here and N+1 on the group header.
//
// Matching QueryMemberCount instead was the alternative and is worse: the count
// would then include people the list beside it treats as non-members, so the
// endpoint would contradict ITSELF rather than contradict another endpoint. A
// count that means the same thing as the list it sits in is the one property
// worth keeping; the cross-surface difference is bounded by how rare blacklisting
// is, and it disappears when the older surfaces are hardened.
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
			"WHERE group_no IN ? AND is_deleted = 0 AND status = ? "+
			"GROUP BY group_no",
		groupNos, int(common.GroupMemberStatusNormal),
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

// listMyProjectGroupResponses owns the complete response construction for the
// legacy native-membership-scoped projection. Keeping the mapper beside the
// membership-scoped query prevents those callers from drifting on fields or
// member counts; the relation-only endpoint and sidebar use ProjectGroupRelation.
func (d *DB) listMyProjectGroupResponses(spaceID, projectID, uid string, offset, limit int) ([]*GroupResp, error) {
	rows, err := d.listMyProjectGroups(spaceID, projectID, uid, offset, limit)
	if err != nil {
		return nil, err
	}
	groupNos := make([]string, 0, len(rows))
	for _, row := range rows {
		groupNos = append(groupNos, row.GroupNo)
	}
	counts, err := d.countActiveGroupMembers(groupNos)
	if err != nil {
		return nil, err
	}
	result := make([]*GroupResp, 0, len(rows))
	for _, row := range rows {
		result = append(result, projectGroupResponse(row, counts[row.GroupNo]))
	}
	return result, nil
}

func projectGroupResponse(row *projectGroupRow, memberCount int) *GroupResp {
	return &GroupResp{
		GroupNo:        row.GroupNo,
		Name:           row.Name,
		IsNamed:        row.IsNamed,
		AvatarText:     row.AvatarText,
		AvatarColor:    row.AvatarColor,
		IsUploadAvatar: row.IsUploadAvatar,
		MemberCount:    memberCount,
	}
}

// listMyProjectGroupResponsesByProjectIDs returns the same legacy
// membership-scoped projection as listMyProjectGroupResponses for multiple
// Projects. It executes one group query and one grouped member-count query
// regardless of Project count, then applies the per-Project page limit in memory.
func (d *DB) listMyProjectGroupResponsesByProjectIDs(spaceID, uid string, projectIDs []string, limit int) (map[string][]*GroupResp, error) {
	result := make(map[string][]*GroupResp, len(projectIDs))
	for _, projectID := range projectIDs {
		result[projectID] = make([]*GroupResp, 0)
	}
	if spaceID == "" || uid == "" || len(projectIDs) == 0 || limit <= 0 {
		return result, nil
	}

	var rows []*projectGroupRow
	_, err := d.session.SelectBySql(
		sqlListMyProjectGroupsByProjectIDs,
		spaceID, projectIDs, groupStatusDisband, uid, int(common.GroupMemberStatusNormal),
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: list my project groups in batch: %w", err)
	}

	selected := make(map[string][]*projectGroupRow, len(projectIDs))
	groupNos := make([]string, 0, len(rows))
	for _, row := range rows {
		if _, requested := result[row.ProjectID]; !requested || len(selected[row.ProjectID]) >= limit {
			continue
		}
		selected[row.ProjectID] = append(selected[row.ProjectID], row)
		groupNos = append(groupNos, row.GroupNo)
	}
	counts, err := d.countActiveGroupMembers(groupNos)
	if err != nil {
		return nil, err
	}

	for projectID, projectRows := range selected {
		groups := make([]*GroupResp, 0, len(projectRows))
		for _, row := range projectRows {
			groups = append(groups, projectGroupResponse(row, counts[row.GroupNo]))
		}
		result[projectID] = groups
	}
	return result, nil
}

// ListMyProjectGroups is the in-process legacy native-membership-scoped
// projection for callers that render actual chat-room surfaces. It is
// intentionally not the relation-only GET /v1/projects/:project_id/groups
// endpoint or the unified sidebar projection.
//
// It follows the legacy default page, including its 50-row bound.
func ListMyProjectGroups(ctx *config.Context, spaceID, projectID, uid string) ([]*GroupResp, error) {
	if ctx == nil || spaceID == "" || projectID == "" || uid == "" {
		return []*GroupResp{}, nil
	}
	db := NewDB(ctx)
	return db.listMyProjectGroupResponses(spaceID, projectID, uid, 0, projectDefaultPageLimit)
}

// ListMyProjectGroupsByProjectIDs is the batched legacy native-membership
// projection. The unified sidebar uses ListProjectGroupRelationsByProjectIDs
// instead, while this helper remains available for legacy GroupResp callers.
func ListMyProjectGroupsByProjectIDs(ctx *config.Context, spaceID, uid string, projectIDs []string) (map[string][]*GroupResp, error) {
	if ctx == nil {
		return map[string][]*GroupResp{}, nil
	}
	db := NewDB(ctx)
	return db.listMyProjectGroupResponsesByProjectIDs(spaceID, uid, projectIDs, projectDefaultPageLimit)
}

// ProjectGroupRelation is the relation-only projection used by
// GET /v1/projects/:project_id/groups. It deliberately carries no native
// membership, ACL, or chat fields.
type ProjectGroupRelation struct {
	GroupNo   string  `json:"group_no" db:"group_no"`
	Name      string  `json:"name" db:"name"`
	ProjectID string  `json:"project_id" db:"project_id"`
	LinkedBy  *string `json:"linked_by" db:"project_linked_by"`
	Pinned    bool    `json:"pinned" db:"pinned"`
}

type projectGroupRelationRow struct {
	GroupNo       string  `db:"group_no"`
	Name          string  `db:"name"`
	ProjectID     string  `db:"project_id"`
	ProjectLinked *string `db:"project_linked_by"`
	Pinned        bool    `db:"pinned"`
}

func projectGroupRelationFromRow(row *projectGroupRelationRow) ProjectGroupRelation {
	return ProjectGroupRelation{
		GroupNo:   row.GroupNo,
		Name:      row.Name,
		ProjectID: row.ProjectID,
		LinkedBy:  normalizedProjectLinkedBy(row.ProjectLinked),
		Pinned:    row.Pinned,
	}
}

// projectGroupLike escapes LIKE metacharacters while keeping the query's
// collation literal. The endpoint's keyword is a literal substring, not a
// pattern supplied by the caller.
func projectGroupLike(keyword string) string {
	escaped := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(keyword)
	return "%" + escaped + "%"
}

func (d *DB) listProjectGroupRelations(
	ctx context.Context, projectID, spaceID, actorUID, keyword string, offset, limit int,
) ([]ProjectGroupRelation, int64, error) {
	if d == nil || d.session == nil {
		return nil, 0, fmt.Errorf("project: group relation database unavailable")
	}
	projectID = strings.TrimSpace(projectID)
	spaceID = strings.TrimSpace(spaceID)
	actorUID = strings.TrimSpace(actorUID)
	keyword = strings.TrimSpace(keyword)
	if projectID == "" || spaceID == "" || actorUID == "" {
		return nil, 0, ErrGroupProjectInvalid
	}
	if utf8.RuneCountInString(keyword) > 30 {
		return nil, 0, fmt.Errorf("%w: keyword", ErrGroupProjectInvalid)
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = projectDefaultPageLimit
	}
	tx, err := d.session.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("%w: begin Project group list: %v", ErrGroupProjectDependency, err)
	}
	defer tx.RollbackUnlessCommitted()

	access, err := AuthorizeGroupProjectReadTx(tx, actorUID, projectID, spaceID)
	if err != nil {
		return nil, 0, err
	}
	countWhere := "g.space_id = ? AND g.project_id = ? AND g.status <> ?"
	countArgs := []interface{}{access.SpaceID, access.ProjectID, groupStatusDisband}
	if keyword != "" {
		countWhere += " AND g.name LIKE CONVERT(? USING utf8mb4) COLLATE utf8mb4_general_ci ESCAPE '!'"
		countArgs = append(countArgs, []byte(projectGroupLike(keyword)))
	}
	var total int64
	if err := tx.SelectBySql("SELECT COUNT(*) FROM `group` g WHERE "+countWhere, countArgs...).LoadOne(&total); err != nil {
		return nil, 0, fmt.Errorf("%w: count Project groups: %v", ErrGroupProjectDependency, err)
	}

	query := "SELECT g.group_no, g.name, g.project_id, g.project_linked_by, " +
		"IFNULL(s.pinned, 0) AS pinned " +
		"FROM `group` g LEFT JOIN `octo_project_group_user_setting` s ON " +
		"s.space_id = g.space_id COLLATE utf8mb4_general_ci " +
		"AND s.project_id = g.project_id COLLATE utf8mb4_general_ci " +
		"AND s.group_no = g.group_no COLLATE utf8mb4_general_ci " +
		"AND s.uid = ? " +
		"WHERE " + countWhere +
		" ORDER BY IFNULL(s.pinned, 0) DESC, " +
		"CASE WHEN s.pinned = 1 THEN s.pinned_at END DESC, " +
		"g.id ASC, g.group_no ASC LIMIT ? OFFSET ?"
	pageArgs := append([]interface{}{actorUID}, countArgs...)
	pageArgs = append(pageArgs, limit, offset)
	var rows []*projectGroupRelationRow
	if _, err := tx.SelectBySql(query, pageArgs...).Load(&rows); err != nil {
		return nil, 0, fmt.Errorf("%w: list Project groups: %v", ErrGroupProjectDependency, err)
	}
	result := make([]ProjectGroupRelation, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		result = append(result, projectGroupRelationFromRow(row))
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, fmt.Errorf("%w: commit Project group list: %v", ErrGroupProjectDependency, err)
	}
	return result, total, nil
}

// listProjectGroupRelationsByProjectIDs returns the relation-only projection used
// by the sidebar for every requested Project. Only Projects where the caller has
// an active seat receive groups; a visible Space-listed Project pinned without a
// Project seat remains an empty entry.
//
// The authorization predicates are evaluated once for the caller and the active
// Project set, while each Project keeps the relation endpoint's default page
// bound and pinned-first ordering.
func (d *DB) listProjectGroupRelationsByProjectIDs(
	ctx context.Context, spaceID, actorUID string, projectIDs []string, limit int,
) (map[string][]ProjectGroupRelation, error) {
	result := make(map[string][]ProjectGroupRelation, len(projectIDs))
	for _, projectID := range projectIDs {
		result[projectID] = make([]ProjectGroupRelation, 0)
	}
	if d == nil || d.session == nil {
		return nil, fmt.Errorf("project: group relation database unavailable")
	}
	spaceID = strings.TrimSpace(spaceID)
	actorUID = strings.TrimSpace(actorUID)
	if spaceID == "" || actorUID == "" || len(projectIDs) == 0 {
		return result, nil
	}
	if limit <= 0 {
		limit = projectDefaultPageLimit
	}

	tx, err := d.session.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: begin sidebar Project group list: %v", ErrGroupProjectDependency, err)
	}
	defer tx.RollbackUnlessCommitted()

	activeSeat, err := queryGroupProjectSpaceSeatTx(tx, spaceID, actorUID)
	if err != nil {
		return nil, fmt.Errorf("%w: check Space seat: %v", ErrGroupProjectDependency, err)
	}
	if !activeSeat {
		return result, nil
	}
	activeUser, err := queryGroupProjectUserEligibleTx(tx, actorUID)
	if err != nil {
		return nil, fmt.Errorf("%w: check user: %v", ErrGroupProjectDependency, err)
	}
	if !activeUser {
		return result, nil
	}

	var activeProjectIDs []string
	_, err = tx.SelectBySql(
		"SELECT p.project_id FROM `octo_project` p "+
			"INNER JOIN `octo_project_member` pm ON pm.project_id = p.project_id "+
			"AND pm.space_id = p.space_id "+
			"WHERE p.space_id = ? AND p.status = ? AND pm.uid = ? "+
			"AND pm.status = ? AND pm.removing = 0 AND p.project_id IN ? "+
			"ORDER BY p.project_id ASC",
		spaceID, StatusNormal, actorUID, MemberStatusActive, projectIDs,
	).Load(&activeProjectIDs)
	if err != nil {
		return nil, fmt.Errorf("%w: list sidebar Project memberships: %v", ErrGroupProjectDependency, err)
	}
	if len(activeProjectIDs) == 0 {
		return result, nil
	}

	query := sqlListProjectGroupRelationsByProjectIDs
	var rows []*projectGroupRelationRow
	if _, err := tx.SelectBySql(query, actorUID, spaceID, activeProjectIDs, groupStatusDisband).Load(&rows); err != nil {
		return nil, fmt.Errorf("%w: list sidebar Project groups: %v", ErrGroupProjectDependency, err)
	}
	for _, row := range rows {
		if row == nil {
			continue
		}
		groups, ok := result[row.ProjectID]
		if !ok || len(groups) >= limit {
			continue
		}
		groups = append(groups, projectGroupRelationFromRow(row))
		result[row.ProjectID] = groups
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%w: commit sidebar Project group list: %v", ErrGroupProjectDependency, err)
	}
	return result, nil
}

// ListProjectGroupRelationsByProjectIDs is the batched relation-only projection
// used by the category sidebar. A requested Project without an active seat for
// uid receives an empty slice, matching the sidebar's pinned Space-listed view.
func ListProjectGroupRelationsByProjectIDs(
	ctx *config.Context, spaceID, uid string, projectIDs []string,
) (map[string][]ProjectGroupRelation, error) {
	result := make(map[string][]ProjectGroupRelation, len(projectIDs))
	for _, projectID := range projectIDs {
		result[projectID] = make([]ProjectGroupRelation, 0)
	}
	if ctx == nil {
		return result, nil
	}
	db := NewDB(ctx)
	return db.listProjectGroupRelationsByProjectIDs(
		context.Background(), spaceID, uid, projectIDs, projectDefaultPageLimit,
	)
}

func normalizedProjectLinkedBy(value *string) *string {
	if value == nil {
		return nil
	}
	v := strings.TrimSpace(*value)
	if v == "" {
		return nil
	}
	return &v
}
