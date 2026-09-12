package project

import (
	"fmt"
	"strings"

	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gocraft/dbr/v2"
)

// projectMemberCandidateDisplayNameExpr deliberately follows the Space
// directory's display-name chain: user.name, then verified real_name, then a
// stable UID placeholder. Department and email are not projected by this API.
const projectMemberCandidateDisplayNameExpr = "(COALESCE(NULLIF(u.name, ''), NULLIF(uv.real_name, ''), CONCAT('User ', sm.uid)) COLLATE utf8mb4_general_ci)"

func projectMemberCandidatePredicate(projectID, spaceID, keyword string) (string, []interface{}) {
	// The Project tables use utf8mb4_general_ci while the legacy Space/user
	// tables may retain their imported collation. Keep cross-table comparisons
	// explicit at the legacy-to-Project boundary, as the directory queries do.
	query := "FROM `space_member` sm " +
		"INNER JOIN `space` s ON s.space_id = sm.space_id " +
		"INNER JOIN `user` u ON u.uid = sm.uid " +
		"LEFT JOIN `user_verification` uv ON uv.user_id = sm.uid COLLATE utf8mb4_general_ci " +
		"LEFT JOIN `octo_project_member` pm ON pm.project_id = ? AND pm.space_id = ? " +
		"AND pm.uid = sm.uid COLLATE utf8mb4_general_ci " +
		"AND pm.status = ? AND pm.removing = 0 " +
		"WHERE sm.space_id = ? AND s.status = ? AND sm.status = ? " +
		"AND u.robot = 0 AND u.status = 1 AND COALESCE(u.is_destroy, 0) <> 2 " +
		"AND sm.uid NOT IN ?"
	args := []interface{}{projectID, spaceID, MemberStatusActive, spaceID, 1, MemberStatusActive, spacepkg.SystemBotList()}
	if strings.TrimSpace(keyword) != "" {
		query += " AND " + projectMemberCandidateDisplayNameExpr +
			" LIKE CONVERT(? USING utf8mb4) COLLATE utf8mb4_general_ci ESCAPE '!'"
		args = append(args, projectLikePattern(strings.TrimSpace(keyword)))
	}
	return query, args
}

func (d *DB) listProjectMemberCandidatesReadTx(
	tx *dbr.Tx,
	projectID, spaceID, actorUID, keyword string,
	page projectReadPage,
) (*projectMemberCandidatesResult, error) {
	if err := d.ensureReadTx(tx); err != nil {
		return nil, err
	}
	predicate, args := projectMemberCandidatePredicate(projectID, spaceID, keyword)
	var total int64
	if err := tx.SelectBySql("SELECT COUNT(*) "+predicate, args...).LoadOne(&total); err != nil {
		return nil, fmt.Errorf("project: count member candidates: %w", err)
	}

	// SELECT placeholders precede the FROM placeholders in the SQL text, so the
	// actor is prepended to the predicate arguments. The second actor value is
	// used by the stable priority key; it intentionally mirrors the status CASE.
	rowArgs := []interface{}{actorUID, actorUID}
	rowArgs = append(rowArgs, args...)
	rowArgs = append(rowArgs, page.Limit, page.Offset)
	rows := make([]*projectMemberCandidateRow, 0)
	_, err := tx.SelectBySql(
		"SELECT sm.uid, "+projectMemberCandidateDisplayNameExpr+" AS candidate_name, "+
			"CASE WHEN sm.uid = ? THEN '"+memberCandidateStatusCurrentUser+"' "+
			"WHEN pm.uid IS NOT NULL THEN '"+memberCandidateStatusAlready+"' "+
			"ELSE '"+memberCandidateStatusInvitable+"' END AS status, "+
			"CASE WHEN sm.uid = ? THEN 0 ELSE 1 END AS candidate_priority "+
			predicate+
			" ORDER BY candidate_priority ASC, candidate_name ASC, sm.uid ASC LIMIT ? OFFSET ?",
		rowArgs...,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: list member candidates: %w", err)
	}
	return &projectMemberCandidatesResult{Rows: rows, Total: total}, nil
}
