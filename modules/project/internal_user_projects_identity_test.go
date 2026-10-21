package project

import (
	"context"
	"testing"

	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"github.com/stretchr/testify/require"
)

// TestUserProjectsInSpaceRejectsDistinctLegacySeatIdentity runs against the
// production collation shape (iupProbeSession): `user` / `space_member` at
// utf8mb4_0900_ai_ci, `octo_project_member` at utf8mb4_general_ci. Under the
// former `s` and `ß` are DISTINCT accounts; under the latter they are EQUAL
// spellings, so `pm.uid = 's'` also returns ß's seat rows and the six-field
// projection cannot tell whose role it is about to serve.
//
// The case pins both directions of the fix:
//
//   - the listed row must belong to the account that QUALIFIED (live `user` row
//     plus active Space seat), not merely to a spelling general_ci equates with
//     it — s must never inherit ß's Owner seat, and activating ß must not hand
//     either account the other's Project;
//   - a genuine identity is still served, including when the caller's spelling
//     differs only by ASCII case, which both collations treat as the same uid.
func TestUserProjectsInSpaceRejectsDistinctLegacySeatIdentity(t *testing.T) {
	sess := iupProbeSession(t)
	for _, statement := range []string{
		"INSERT INTO `user` (uid,name,short_no,status) VALUES ('s','Active','active-s',1),('ß','Banned','banned-sharp-s',0)",
		"INSERT INTO space (space_id,name,creator,status) VALUES ('identity-scope','Scope','s',1)",
		"INSERT INTO space_member (space_id,uid,role,status) VALUES ('identity-scope','s',0,1),('identity-scope','ß',0,1)",
		// Inserted first, so it holds the LOWER id and sorts last under the
		// contract's id DESC order when a leaked row slips through.
		"INSERT INTO octo_project (project_id,space_id,name,creator,status,activated_at,created_at,updated_at) VALUES ('legacy-private','identity-scope','Private project','ß',1,NOW(3),NOW(3),NOW(3))",
		"INSERT INTO octo_project_member (project_id,space_id,uid,role,status,removing,created_at,updated_at) VALUES ('legacy-private','identity-scope','ß',2,1,0,NOW(3),NOW(3))",
	} {
		_, err := sess.Exec(statement)
		require.NoError(t, err)
	}

	query := func(t *testing.T, uid string) []projectpkg.UserProject {
		t.Helper()
		rows, err := projectpkg.UserProjectsInSpace(context.Background(), sess, "identity-scope", uid)
		require.NoError(t, err)
		require.NotNil(t, rows)
		return rows
	}

	require.Empty(t, query(t, "s"),
		"a user with no Project seat must not receive the distinct banned account's Owner seat")

	for _, statement := range []string{
		"INSERT INTO octo_project (project_id,space_id,name,description,creator,status,activated_at,created_at,updated_at) VALUES ('ascii-own','identity-scope','Own project','Own description','s',1,NOW(3),NOW(3),NOW(3))",
		"INSERT INTO octo_project_member (project_id,space_id,uid,role,status,removing,created_at,updated_at) VALUES ('ascii-own','identity-scope','S',1,1,0,NOW(3),NOW(3))",
	} {
		_, err := sess.Exec(statement)
		require.NoError(t, err)
	}

	own := []projectpkg.UserProject{{
		ProjectID:   "ascii-own",
		SpaceID:     "identity-scope",
		Name:        "Own project",
		Description: "Own description",
		Logo:        "",
		Role:        1,
	}}
	private := []projectpkg.UserProject{{
		ProjectID:   "legacy-private",
		SpaceID:     "identity-scope",
		Name:        "Private project",
		Description: "",
		Logo:        "",
		Role:        2,
	}}

	// The live account sees its own seat only; the banned account's Owner row is
	// a general_ci match for `s` but not the same identity, so it is dropped.
	require.Equal(t, own, query(t, "s"),
		"an active account must not inherit the distinct banned account's Project seat or Owner role")
	require.Equal(t, own, query(t, "S"),
		"ASCII case is the database's own notion of the same uid and must stay served")

	require.Empty(t, query(t, "ß"),
		"a banned account holds no live identity, so its seat is not listed even for its own spelling")

	// Restoring ß's account liveness restores exactly ONE list: its own. The
	// pm.uid predicate is still a general_ci match for both seats, so this also
	// proves the identity binding is not an artifact of the SQL narrowing.
	_, err := sess.Exec("UPDATE `user` SET status = 1 WHERE uid = 'ß'")
	require.NoError(t, err)

	require.Equal(t, private, query(t, "ß"),
		"the distinct account, once qualified, must be served its own Project with its real role")
	require.Equal(t, own, query(t, "s"),
		"activating ß must not leak its seat to s, nor s's Project to ß")
}
