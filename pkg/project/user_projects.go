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

	seats, err := space.ActiveMembers(tx, spaceID, []string{uid})
	if err != nil {
		return nil, err
	}
	if !FoldedHas(seats, uid) {
		return out, nil
	}

	var rows []UserProject
	if _, err := tx.SelectBySql(
		"SELECT p.project_id, p.space_id, p.name, p.description, p.logo, pm.role "+
			"FROM `octo_project_member` pm "+
			"INNER JOIN `octo_project` p "+
			"ON p.project_id = pm.project_id AND p.space_id = pm.space_id "+
			"WHERE pm.space_id = ? AND pm.uid = ? "+
			"  AND pm.status = 1 AND pm.removing = 0 "+
			"  AND p.status = 1 AND p.activated_at IS NOT NULL "+
			"ORDER BY p.id DESC",
		spaceID, uid,
	).Load(&rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return out, nil
	}
	return rows, nil
}
