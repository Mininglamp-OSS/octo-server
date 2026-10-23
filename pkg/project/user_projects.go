package project

import (
	"context"
	"database/sql"

	"github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/Mininglamp-OSS/octo-server/pkg/user"
	"github.com/gocraft/dbr/v2"
)

// UserProject is one Project the target user holds an active seat in, reduced to
// the list's six fields: no epoch, counts or personal preference. role 0 is a
// real (member) value and is never omitted.
type UserProject struct {
	ProjectID   string `db:"project_id" json:"project_id"`
	SpaceID     string `db:"space_id" json:"space_id"`
	Name        string `db:"name" json:"name"`
	Description string `db:"description" json:"description"`
	Logo        string `db:"logo" json:"logo"`
	Role        int    `db:"role" json:"role"`
}

// userProjectSeat keeps the actual seat identity out of the response DTO.
// Legacy and Project collations can disagree on UID and Space equality, so both
// the seat and Project must bind to the qualified identities through FoldID.
type userProjectSeat struct {
	UserProject
	SeatUID     string `db:"seat_uid"`
	SeatSpaceID string `db:"seat_space_id"`
}

// UserProjectsInSpace returns every activated Project of spaceID that uid has
// joined, newest first (octo_project.id DESC, unpaginated); an empty non-nil
// slice means none.
//
// A row qualifies only when the account is live (pkg/user.ActiveAccounts), the
// Space is active and uid holds an active seat in it (pkg/space.ActiveMembers),
// and the Project is status = 1 with activated_at set while uid's seat row is
// status = 1, removing = 0 and carries the Project's own space_id. All reads share
// one read-only REPEATABLE READ transaction.
//
// The private seat UID and both Project/seat Space IDs are matched with ASCII-only
// FoldID before projection; SQL collation equality alone does not establish identity.
//
// Legacy tables are reached only through pkg/user and pkg/space in separate
// legacy-only statements; joining them to octo_* mixes utf8mb4_0900_ai_ci with
// utf8mb4_general_ci and fails with MySQL error 1267 in production. Any failure
// returns (nil, err), never a partial or empty answer.
func UserProjectsInSpace(ctx context.Context, session *dbr.Session, spaceID, uid string) ([]UserProject, error) {
	out := make([]UserProject, 0)

	tx, err := session.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()
	tx.Timeout = membershipReadTimeout

	live, err := user.ActiveAccounts(tx, []string{uid})
	if err != nil {
		return nil, err
	}
	if !FoldedHas(live, uid) {
		return out, nil
	}

	qualifiedSpaceID, found, err := space.ResolveSpaceID(tx, spaceID)
	if err != nil {
		return nil, err
	}
	if !found {
		return out, nil
	}

	seats, err := space.ActiveMembers(tx, qualifiedSpaceID, []string{uid})
	if err != nil {
		return nil, err
	}
	if !FoldedHas(seats, uid) {
		return out, nil
	}

	var seatRows []userProjectSeat
	if _, err := tx.SelectBySql(
		"SELECT p.project_id, p.space_id, p.name, p.description, p.logo, pm.role, pm.uid AS seat_uid, pm.space_id AS seat_space_id "+
			"FROM `octo_project_member` pm "+
			"INNER JOIN `octo_project` p "+
			"ON p.project_id = pm.project_id AND p.space_id = pm.space_id "+
			"WHERE pm.space_id = ? AND pm.uid = ? "+
			"  AND pm.status = 1 AND pm.removing = 0 "+
			"  AND p.status = 1 AND p.activated_at IS NOT NULL "+
			"ORDER BY p.id DESC",
		qualifiedSpaceID, uid,
	).Load(&seatRows); err != nil {
		return nil, err
	}

	// Bind actual identities without merging distinct legacy accounts or Spaces.
	uidFold := FoldID(uid)
	spaceFold := FoldID(qualifiedSpaceID)
	out = make([]UserProject, 0, len(seatRows))
	for _, seat := range seatRows {
		if FoldID(seat.SeatUID) != uidFold ||
			FoldID(seat.SpaceID) != spaceFold || FoldID(seat.SeatSpaceID) != spaceFold {
			continue
		}
		out = append(out, seat.UserProject)
	}
	return out, nil
}

// ProjectMetadata is the six-field projection returned by an internal lookup
// that has no user-role context. Role is deliberately nil and is encoded as JSON
// null; it must not be interpreted as a membership answer.
type ProjectMetadata struct {
	ProjectID   string `db:"project_id" json:"project_id"`
	SpaceID     string `db:"space_id" json:"space_id"`
	Name        string `db:"name" json:"name"`
	Description string `db:"description" json:"description"`
	Logo        string `db:"logo" json:"logo"`
	Role        *int   `json:"role"`
}

// ProjectMetadataInSpace returns the matching active, activated Project rows
// without consulting any user or membership tables. The Space is resolved and
// checked in the same read-only REPEATABLE READ transaction as the Project read.
func ProjectMetadataInSpace(ctx context.Context, session *dbr.Session, spaceID string, projectIDs []string) ([]ProjectMetadata, error) {
	tx, err := session.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()
	tx.Timeout = membershipReadTimeout

	qualifiedSpaceID, found, err := space.ResolveSpaceID(tx, spaceID)
	if err != nil {
		return nil, err
	}
	if !found {
		return []ProjectMetadata{}, nil
	}
	active, err := space.IsActiveSpace(tx, qualifiedSpaceID)
	if err != nil {
		return nil, err
	}
	if !active {
		return []ProjectMetadata{}, nil
	}

	var rows []ProjectMetadata
	_, err = tx.SelectBySql(
		"SELECT project_id, space_id, name, description, logo "+
			"FROM `octo_project` "+
			"WHERE space_id = ? AND project_id IN ? "+
			"  AND status = 1 AND activated_at IS NOT NULL",
		qualifiedSpaceID, projectIDs,
	).Load(&rows)
	if err != nil {
		return nil, err
	}

	spaceFold := FoldID(qualifiedSpaceID)
	requested := make(map[string]struct{}, len(projectIDs))
	for _, projectID := range projectIDs {
		requested[FoldID(projectID)] = struct{}{}
	}
	if rows == nil {
		rows = []ProjectMetadata{}
	}
	out := rows[:0]
	for _, row := range rows {
		if FoldID(row.SpaceID) != spaceFold {
			continue
		}
		if _, ok := requested[FoldID(row.ProjectID)]; !ok {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}
