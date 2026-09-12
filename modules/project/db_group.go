package project

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/config"
	aiteampkg "github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
)

// sqlListProjectGroupRelationsByProjectIDs is the single batched relation query
// used by the unified sidebar. The window row number applies the 50-row
// per-Project cap before rows are returned to Go, while retaining the endpoint's
// pinned-first and total ordering.

const sqlListProjectGroupRelationsByProjectIDs = "SELECT project_id, group_no, name, project_linked_by, pinned " +
	"FROM (" +
	"SELECT g.project_id, g.group_no, g.name, g.project_linked_by, " +
	"IFNULL(s.pinned, 0) AS pinned, s.pinned_at, g.id AS group_id, " +
	"ROW_NUMBER() OVER (PARTITION BY g.project_id ORDER BY IFNULL(s.pinned, 0) DESC, " +
	"CASE WHEN s.pinned = 1 THEN s.pinned_at END DESC, g.id ASC, g.group_no ASC) AS project_row_num " +
	"FROM `group` g LEFT JOIN `octo_project_group_user_setting` s ON " +
	"s.space_id = g.space_id COLLATE utf8mb4_general_ci " +
	"AND s.project_id = g.project_id COLLATE utf8mb4_general_ci " +
	"AND s.group_no = g.group_no COLLATE utf8mb4_general_ci " +
	"AND s.uid = ? " +
	"WHERE g.space_id = ? AND g.project_id IN ? AND g.status <> ? AND g.purpose <> ?" +
	") AS ranked " +
	"WHERE project_row_num <= ? " +
	"ORDER BY project_id ASC, pinned DESC, " +
	"CASE WHEN pinned = 1 THEN pinned_at END DESC, group_id ASC, group_no ASC"

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
	countWhere := "g.space_id = ? AND g.project_id = ? AND g.status <> ? AND g.purpose <> ?"
	countArgs := []interface{}{access.SpaceID, access.ProjectID, groupStatusDisband, aiteampkg.GroupPurpose}
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
// an active Project seat receive groups. A non-member Project is not rendered by
// the sidebar, even when a historical pin preference remains.
//
// The authorization predicates are evaluated once for the caller and the active
// Project set, while each Project keeps a hard SQL-side 50-row bound and
// pinned-first ordering. The small in-memory bound below is defensive only; the
// window predicate prevents the database from materializing an unbounded Project.
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
	if limit <= 0 || limit > projectDefaultPageLimit {
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
	if _, err := tx.SelectBySql(query, actorUID, spaceID, activeProjectIDs, groupStatusDisband, aiteampkg.GroupPurpose, limit).Load(&rows); err != nil {
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
// used by the category sidebar. A requested Project without an active Project
// seat for uid receives an empty slice; pin preferences do not widen this gate.
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
