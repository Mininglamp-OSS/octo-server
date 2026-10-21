package project

// Behavioural coverage for pkg/project.UserProjectsInSpace, which backs
// GET /v1/internal/projects.
//
// The properties that matter here are all database facts, so every case runs
// against the real engine with real fixtures rather than a stub: activation is a
// latch on octo_project, isolation is a conjunction of space_id on two tables,
// and the snapshot is a property of the transaction the function opens. A fake
// store would agree with any implementation.
//
// The one thing a single-shot test cannot observe is the SNAPSHOT across the
// qualification reads and the list read: both orderings return the same answer
// unless a write lands in between. TestUserProjectsInSpacePinsOneSnapshot...
// drives that interleaving deterministically through dbr's query event stream —
// no production hook is added for it.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/require"
)

// ---------- fixtures ----------

// iupProject inserts one octo_project row. `activated` selects between the
// two-phase create's waiting state (NULL) and the confirmed latch; nothing else
// in the fixture writes that column.
func iupProject(t *testing.T, projectID, spaceID, creator, name, description, logo string, status int, activated bool) {
	t.Helper()
	activatedAt := "NULL"
	if activated {
		activatedAt = "UTC_TIMESTAMP(3)"
	}
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO octo_project "+
			"(project_id, space_id, name, description, logo, creator, status, activated_at, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, "+activatedAt+", UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))",
		projectID, spaceID, name, description, logo, creator, status,
	).Exec()
	require.NoError(t, err)
}

// iupSeat inserts one octo_project_member row with every flag explicit, so a
// case that means "seat closing" cannot accidentally inherit the active default.
func iupSeat(t *testing.T, projectID, uid, spaceID string, role, status, removing int) {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO octo_project_member "+
			"(project_id, uid, space_id, role, status, removing, invite_uid, created_at, joined_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))",
		projectID, uid, spaceID, role, status, removing, uid,
	).Exec()
	require.NoError(t, err)
}

// iupUserProjects is the call under test, with the contract's "success is never
// nil" assertion hoisted so every case pins it.
func iupUserProjects(t *testing.T, spaceID, uid string) []projectpkg.UserProject {
	t.Helper()
	rows, err := projectpkg.UserProjectsInSpace(context.Background(), testCtx.DB(), spaceID, uid)
	require.NoError(t, err)
	require.NotNil(t, rows, "a successful query must return a non-nil slice (the wire answer is [] not null)")
	return rows
}

func iupProjectIDs(rows []projectpkg.UserProject) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ProjectID)
	}
	return ids
}

// ---------- happy path: roles, stored fields, ordering ----------

// TestUserProjectsInSpaceReturnsStoredFieldsAndEveryRole pins acceptance 1 and
// the projection: member/admin/Owner all appear with their real role, the six
// fields are the stored values (including an empty logo and a 500-character
// description), and a creator with no seat gets nothing.
func TestUserProjectsInSpaceReturnsStoredFieldsAndEveryRole(t *testing.T) {
	setup(t)
	const uid = "iup-role-user"
	seedUserBare(t, uid)
	seedSpace(t, spaceA, 1)
	// role 2 = Space OWNER. Holding the highest Space role must not add a project;
	// the answer is governed by the PROJECT roster alone.
	seedSpaceMember(t, spaceA, uid, 2, 1)

	// Insertion order is id order, and the answer is id DESC, so each expectation
	// below is the reverse of this list.
	// octo_project.description is TEXT; the fixture goes well past the historical
	// 500-character limit so a truncation regression is visible.
	longDescription := strings.Repeat("描", 1024)
	iupProject(t, "iup-member", spaceA, "someone-else", "Member project", "", "", 1, true)
	iupProject(t, "iup-admin", spaceA, "someone-else", "Admin project", "admin description", "https://cdn.test/logo.png", 1, true)
	iupProject(t, "iup-owner", spaceA, uid, "Owner project", longDescription, "", 1, true)
	// uid CREATED this one but holds no seat. Space ownership and creation do not
	// imply project membership, so it must not appear.
	iupProject(t, "iup-creator-only", spaceA, uid, "Creator but not a member", "", "", 1, true)

	iupSeat(t, "iup-member", uid, spaceA, 0, 1, 0)
	iupSeat(t, "iup-admin", uid, spaceA, 1, 1, 0)
	iupSeat(t, "iup-owner", uid, spaceA, 2, 1, 0)

	require.Equal(t, []projectpkg.UserProject{
		{ProjectID: "iup-owner", SpaceID: spaceA, Name: "Owner project", Description: longDescription, Logo: "", Role: 2},
		{ProjectID: "iup-admin", SpaceID: spaceA, Name: "Admin project", Description: "admin description", Logo: "https://cdn.test/logo.png", Role: 1},
		{ProjectID: "iup-member", SpaceID: spaceA, Name: "Member project", Description: "", Logo: "", Role: 0},
	}, iupUserProjects(t, spaceA, uid))
}

// ---------- activation, seat lifecycle, project status ----------

// TestUserProjectsInSpaceTracksActivationSeatStateAndProjectStatus pins
// acceptance 3: the activation latch excludes a waiting project and admits it
// once set; a closing/closed seat and a disbanded project drop out; a rejoin
// restores the project with its CURRENT role.
func TestUserProjectsInSpaceTracksActivationSeatStateAndProjectStatus(t *testing.T) {
	setup(t)
	const uid = "iup-lifecycle-user"
	seedUserBare(t, uid)
	seedSpace(t, spaceA, 1)
	seedSpaceMember(t, spaceA, uid, 0, 1)

	// Insertion order chosen so the pending project carries the HIGHEST id: the
	// activation step below then visibly moves it to the front of an id-DESC answer,
	// rather than merely appending to it.
	iupProject(t, "iup-disbanded", spaceA, "someone-else", "Disbanded", "", "", 0, true)
	iupProject(t, "iup-closing", spaceA, "someone-else", "Closing seat", "", "", 1, true)
	iupProject(t, "iup-removed", spaceA, "someone-else", "Removed seat", "", "", 1, true)
	iupProject(t, "iup-active", spaceA, "someone-else", "Active", "", "", 1, true)
	iupProject(t, "iup-pending", spaceA, "someone-else", "Awaiting activation", "", "", 1, false)

	iupSeat(t, "iup-disbanded", uid, spaceA, 0, 1, 0)
	iupSeat(t, "iup-closing", uid, spaceA, 0, 1, 1)
	iupSeat(t, "iup-removed", uid, spaceA, 0, 0, 0)
	iupSeat(t, "iup-pending", uid, spaceA, 0, 1, 0)
	iupSeat(t, "iup-active", uid, spaceA, 1, 1, 0)

	require.Equal(t, []string{"iup-active"}, iupProjectIDs(iupUserProjects(t, spaceA, uid)),
		"a project awaiting activation is answered as absent; passing seats on a closing, closed or disbanded project are not memberships")

	// The subsystem confirms the container: the latch is set and only then does the
	// project become visible.
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE octo_project SET activated_at = UTC_TIMESTAMP(3) WHERE project_id = ?", "iup-pending").Exec()
	require.NoError(t, err)
	require.Equal(t, []string{"iup-pending", "iup-active"}, iupProjectIDs(iupUserProjects(t, spaceA, uid)),
		"activation must admit the project, newest first")

	// A seat entering the removal window is not a member, exactly as everywhere
	// else in this package.
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE octo_project_member SET removing = 1 WHERE project_id = ? AND uid = ?", "iup-active", uid).Exec()
	require.NoError(t, err)
	require.Equal(t, []string{"iup-pending"}, iupProjectIDs(iupUserProjects(t, spaceA, uid)),
		"removing = 1 must drop the project")

	// Removal completes.
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE octo_project_member SET status = 0, removing = 0 WHERE project_id = ? AND uid = ?", "iup-active", uid).Exec()
	require.NoError(t, err)
	require.Equal(t, []string{"iup-pending"}, iupProjectIDs(iupUserProjects(t, spaceA, uid)),
		"an inactive seat must stay dropped")

	// Rejoin, with a different role. The row is updated rather than re-inserted,
	// because that is what the real re-admission does.
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE octo_project_member SET status = 1, removing = 0, role = 2 WHERE project_id = ? AND uid = ?",
		"iup-active", uid).Exec()
	require.NoError(t, err)
	rows := iupUserProjects(t, spaceA, uid)
	require.Equal(t, []string{"iup-pending", "iup-active"}, iupProjectIDs(rows))
	require.Equal(t, 2, rows[1].Role, "a rejoin must report the CURRENT role, not the one from before the removal")

	// Disbanding the project removes it even though the seat is active again.
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE octo_project SET status = 0 WHERE project_id = ?", "iup-active").Exec()
	require.NoError(t, err)
	require.Equal(t, []string{"iup-pending"}, iupProjectIDs(iupUserProjects(t, spaceA, uid)))
}

// ---------- Space isolation ----------

// TestUserProjectsInSpaceIsolatesSpacesAndSeatSpaceMismatch pins acceptance 2:
// a Project of another Space never appears, and a seat whose denormalized
// space_id disagrees with its Project is absent from BOTH Spaces — matching the
// seat's Space cannot rescue it, because the join requires the two to agree.
func TestUserProjectsInSpaceIsolatesSpacesAndSeatSpaceMismatch(t *testing.T) {
	setup(t)
	const uid = "iup-scope-user"
	seedUserBare(t, uid)
	seedSpace(t, spaceA, 1)
	seedSpace(t, spaceB, 1)
	seedSpaceMember(t, spaceA, uid, 0, 1)
	seedSpaceMember(t, spaceB, uid, 0, 1)

	iupProject(t, "iup-in-a", spaceA, "someone-else", "Project in A", "", "", 1, true)
	iupProject(t, "iup-in-b", spaceB, "someone-else", "Project in B", "", "", 1, true)
	// The Project lives in spaceA; the seat's copy claims spaceB.
	iupProject(t, "iup-mismatch", spaceA, "someone-else", "Drifted seat", "", "", 1, true)

	iupSeat(t, "iup-in-a", uid, spaceA, 0, 1, 0)
	iupSeat(t, "iup-in-b", uid, spaceB, 0, 1, 0)
	iupSeat(t, "iup-mismatch", uid, spaceB, 0, 1, 0)

	require.Equal(t, []projectpkg.UserProject{
		{ProjectID: "iup-in-a", SpaceID: spaceA, Name: "Project in A", Role: 0},
	}, iupUserProjects(t, spaceA, uid))

	require.Equal(t, []projectpkg.UserProject{
		{ProjectID: "iup-in-b", SpaceID: spaceB, Name: "Project in B", Role: 0},
	}, iupUserProjects(t, spaceB, uid), "the mismatched seat must not be served under the Space it claims either")
}

// ---------- qualification: account, Space, Space seat ----------

// TestUserProjectsInSpaceRequiresLiveAccountActiveSpaceAndSeat pins acceptance
// 4 and 5. Every case keeps a fully active Project seat, so only the
// qualification read can produce the empty answer.
func TestUserProjectsInSpaceRequiresLiveAccountActiveSpaceAndSeat(t *testing.T) {
	const caseUID = "iup-qual-user"

	cases := []struct {
		name       string
		mutate     func(t *testing.T)
		querySpace string
		queryUID   string
		wantRows   int
	}{
		{
			name:     "baseline: live account, active Space, active Space seat",
			wantRows: 1,
		},
		{
			name: "banned account keeps every seat row",
			mutate: func(t *testing.T) {
				_, err := testCtx.DB().UpdateBySql("UPDATE `user` SET status = 0 WHERE uid = ?", caseUID).Exec()
				require.NoError(t, err)
			},
			wantRows: 0,
		},
		{
			name: "fully destroyed account",
			mutate: func(t *testing.T) {
				_, err := testCtx.DB().UpdateBySql("UPDATE `user` SET is_destroy = 2 WHERE uid = ?", caseUID).Exec()
				require.NoError(t, err)
			},
			wantRows: 0,
		},
		{
			// A cooling-off destroy request is reversible and the account still acts;
			// only the terminal state counts as gone.
			name: "cooling-off account is still live",
			mutate: func(t *testing.T) {
				_, err := testCtx.DB().UpdateBySql("UPDATE `user` SET is_destroy = 1 WHERE uid = ?", caseUID).Exec()
				require.NoError(t, err)
			},
			wantRows: 1,
		},
		{
			name: "banned Space",
			mutate: func(t *testing.T) {
				_, err := testCtx.DB().UpdateBySql("UPDATE `space` SET status = 2 WHERE space_id = ?", spaceA).Exec()
				require.NoError(t, err)
			},
			wantRows: 0,
		},
		{
			name: "disbanded Space",
			mutate: func(t *testing.T) {
				_, err := testCtx.DB().UpdateBySql("UPDATE `space` SET status = 0 WHERE space_id = ?", spaceA).Exec()
				require.NoError(t, err)
			},
			wantRows: 0,
		},
		{
			name: "Space seat revoked",
			mutate: func(t *testing.T) {
				_, err := testCtx.DB().UpdateBySql(
					"UPDATE space_member SET status = 0 WHERE space_id = ? AND uid = ?", spaceA, caseUID).Exec()
				require.NoError(t, err)
			},
			wantRows: 0,
		},
		{
			name: "Space seat deleted",
			mutate: func(t *testing.T) {
				_, err := testCtx.DB().DeleteBySql(
					"DELETE FROM space_member WHERE space_id = ? AND uid = ?", spaceA, caseUID).Exec()
				require.NoError(t, err)
			},
			wantRows: 0,
		},
		{
			name:     "unknown uid",
			queryUID: "iup-nobody",
			wantRows: 0,
		},
		{
			name:       "unknown Space",
			querySpace: "space_nowhere",
			wantRows:   0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)
			seedUserBare(t, caseUID)
			seedSpace(t, spaceA, 1)
			seedSpaceMember(t, spaceA, caseUID, 0, 1)
			iupProject(t, "iup-qual-project", spaceA, "someone-else", "Qualification project", "", "", 1, true)
			iupSeat(t, "iup-qual-project", caseUID, spaceA, 0, 1, 0)
			if tc.mutate != nil {
				tc.mutate(t)
			}

			spaceID, uid := tc.querySpace, tc.queryUID
			if spaceID == "" {
				spaceID = spaceA
			}
			if uid == "" {
				uid = caseUID
			}

			rows := iupUserProjects(t, spaceID, uid)
			require.Len(t, rows, tc.wantRows)
			if tc.wantRows == 1 {
				require.Equal(t, "iup-qual-project", rows[0].ProjectID)
			}
			// The fixture must keep both membership rows active, or the empty answer
			// above could be produced by the list query instead of by qualification.
			if tc.wantRows == 0 {
				var seats int
				require.NoError(t, testCtx.DB().SelectBySql(
					"SELECT COUNT(*) FROM octo_project_member WHERE project_id = ? AND uid = ? AND status = 1 AND removing = 0",
					"iup-qual-project", caseUID).LoadOne(&seats))
				require.Equal(t, 1, seats, "the project seat must still be open so the denial isolates the qualification read")
			}
		})
	}
}

// ---------- completeness: beyond the user-state page cap, order frozen ----------

// TestUserProjectsInSpaceReturnsEveryMatchBeyondTheUserListPageCap pins
// acceptance 6 and 7 together, because they share one fixture: more matching
// projects than the user-facing list's MAXIMUM page (200), returned in full,
// in id DESC order, with a pinned preference on the OLDEST project proving the
// preference does not reorder (or filter) this answer.
func TestUserProjectsInSpaceReturnsEveryMatchBeyondTheUserListPageCap(t *testing.T) {
	setup(t)
	const uid = "iup-full-user"
	seedUserBare(t, uid)
	seedSpace(t, spaceA, 1)
	seedSpaceMember(t, spaceA, uid, 0, 1)

	total := projectMaxPageLimit + 5 // strictly more than any single user-state page
	for i := range total {
		projectID := fmt.Sprintf("iup-full-%03d", i)
		iupProject(t, projectID, spaceA, "someone-else", "Project "+projectID, "", "", 1, true)
		iupSeat(t, projectID, uid, spaceA, i%3, 1, 0)
	}

	// The oldest project (lowest id) is the one pinned, so any list that honoured
	// pinned DESC would move it to the FRONT.
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO octo_project_user_setting (project_id, uid, pinned, pinned_at, created_at, updated_at) "+
			"VALUES (?, ?, 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))",
		"iup-full-000", uid,
	).Exec()
	require.NoError(t, err)

	rows := iupUserProjects(t, spaceA, uid)
	require.Len(t, rows, total, "the answer is complete by contract: no page parameter, no cap, no truncation")

	// Expected order is the reverse of the insertion order, constructed here rather
	// than read back from SQL: auto-increment assigns ids in insert order, so this
	// pins id DESC without restating the query under test as its own oracle.
	wantOrder := make([]string, 0, total)
	for i := total - 1; i >= 0; i-- {
		wantOrder = append(wantOrder, fmt.Sprintf("iup-full-%03d", i))
	}
	require.Equal(t, wantOrder, iupProjectIDs(rows), "ordering must be octo_project.id DESC")

	require.Equal(t, "iup-full-204", rows[0].ProjectID)
	require.Equal(t, "iup-full-000", rows[len(rows)-1].ProjectID,
		"a pinned preference must not lift the oldest project to the front; this read does not consult it")
	require.Equal(t, 0, rows[len(rows)-1].Role, "role 0 must survive as a value")
}

// ---------- identifier matching follows the database, not Go ----------

// TestUserProjectsInSpaceMatchesIdentifiersCaseInsensitively pins acceptance
// 10's collation half: every table involved compares ids case-insensitively, so
// a caller using a differently-cased (but equal) uid/Space must be served —
// which is only true if the qualification results are intersected through
// FoldID rather than by exact Go string equality — and the response keeps the
// DATABASE's spelling of the ids.
func TestUserProjectsInSpaceMatchesIdentifiersCaseInsensitively(t *testing.T) {
	setup(t)
	const (
		storedUID   = "CaseUser"
		storedSpace = "Space-Case"
		storedPID   = "Project-Case"
	)
	seedUserBare(t, storedUID)
	seedSpace(t, storedSpace, 1)
	seedSpaceMember(t, storedSpace, storedUID, 0, 1)
	iupProject(t, storedPID, storedSpace, "someone-else", "Cased project", "cased description", "", 1, true)
	iupSeat(t, storedPID, storedUID, storedSpace, 1, 1, 0)

	rows := iupUserProjects(t, strings.ToLower(storedSpace), strings.ToLower(storedUID))
	require.Equal(t, []projectpkg.UserProject{{
		ProjectID:   storedPID,
		SpaceID:     storedSpace,
		Name:        "Cased project",
		Description: "cased description",
		Logo:        "",
		Role:        1,
	}}, rows, "the database matched these rows case-insensitively; Go must not reject them with an exact comparison, and the stored spelling must be returned unchanged")
}

// ---------- the production collation drift ----------

const iupCollationProbeDB = "octo_project_user_list_collation_probe"

// iupProbeSession builds an isolated database in production's measured shape:
// the octo_* tables keep the collation their migrations declare
// (utf8mb4_general_ci) while the dump-imported legacy tables are converted to
// utf8mb4_0900_ai_ci, the server default they inherited there. CI's own database
// is general_ci everywhere, so a comparison between the two families that fails
// with MySQL 1267 in production is GREEN here. This is the only shape in which
// that defect is observable, and the drift is built by conversion rather than
// assumed.
//
// CREATE TABLE ... LIKE rather than hand-written DDL, so the fixture cannot drift
// from the schema the migrations produced — the same device
// reconcile_p1_collation_test.go uses for the same reason.
func iupProbeSession(t *testing.T) *dbr.Session {
	t.Helper()
	addr := testCtx.GetConfig().DB.MySQLAddr
	slash := strings.LastIndex(addr, "/")
	require.Positive(t, slash, "unexpected DSN shape: %s", addr)
	opts := ""
	if q := strings.Index(addr[slash:], "?"); q >= 0 {
		opts = addr[slash+q:]
	}

	admin, err := dbr.Open("mysql", addr[:slash+1]+opts, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	adminSess := admin.NewSession(nil)
	exec := func(sess *dbr.Session, stmt string) {
		_, e := sess.UpdateBySql(stmt).Exec()
		require.NoError(t, e, stmt)
	}

	exec(adminSess, "DROP DATABASE IF EXISTS `"+iupCollationProbeDB+"`")
	// Created general_ci, exactly like CI does — so the drift comes from the
	// TABLES, which is how it arises in production.
	exec(adminSess, "CREATE DATABASE `"+iupCollationProbeDB+
		"` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")

	conn, err := dbr.Open("mysql", addr[:slash+1]+iupCollationProbeDB+opts, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = adminSess.UpdateBySql("DROP DATABASE IF EXISTS `" + iupCollationProbeDB + "`").Exec()
		_ = conn.Close()
	})
	sess := conn.NewSession(nil)

	src := currentSchemaName(t)
	// The COLLATION of each compared id column is set EXPLICITLY rather than
	// inherited, so the fixture's drift does not depend on the server's default
	// collation. Every column below is VARCHAR(40) NOT NULL DEFAULT '' in the
	// migrated schema; MODIFY is used instead of a whole-table CONVERT so
	// octo_project's generated column is never rewritten.
	for _, tbl := range []struct {
		name      string
		columns   []string
		collation string
	}{
		{"octo_project", []string{"project_id", "space_id"}, "utf8mb4_general_ci"},
		{"octo_project_member", []string{"project_id", "uid", "space_id"}, "utf8mb4_general_ci"},
		{"space", []string{"space_id"}, "utf8mb4_0900_ai_ci"},
		{"space_member", []string{"space_id", "uid"}, "utf8mb4_0900_ai_ci"},
		{"user", []string{"uid"}, "utf8mb4_0900_ai_ci"},
	} {
		exec(sess, "CREATE TABLE `"+tbl.name+"` LIKE `"+src+"`.`"+tbl.name+"`")
		mods := make([]string, 0, len(tbl.columns))
		for _, column := range tbl.columns {
			mods = append(mods, "MODIFY `"+column+"` VARCHAR(40) NOT NULL DEFAULT '' COLLATE "+tbl.collation)
		}
		exec(sess, "ALTER TABLE `"+tbl.name+"` "+strings.Join(mods, ", "))
	}
	return sess
}

// TestUserProjectsInSpaceSurvivesTheProductionCollationDrift pins acceptance
// 10's mixed-collation half with a real hostile engine. The function must reach
// the legacy tables ONLY through pkg/user and pkg/space as separate single-table
// statements; a join between an octo_* table and `user`/`space` is MySQL error
// 1267 on this shape, so adding one turns this case into an error rather than a
// wrong list.
//
// The fixture's hostility is ASSERTED rather than claimed: the probe query at the
// end is exactly the join the implementation must not write, and it must fail.
// Without that, the case would pass on a database that was never actually
// drifted.
func TestUserProjectsInSpaceSurvivesTheProductionCollationDrift(t *testing.T) {
	sess := iupProbeSession(t)
	const (
		uid     = "iup-drift-user"
		spaceID = "space-drift"
		pid     = "project-drift"
	)

	_, err := sess.InsertBySql(
		"INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)", uid, "drift-user", uid).Exec()
	require.NoError(t, err)
	_, err = sess.InsertBySql(
		"INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, 'creator', 1)",
		spaceID, spaceID).Exec()
	require.NoError(t, err)
	_, err = sess.InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)", spaceID, uid).Exec()
	require.NoError(t, err)
	_, err = sess.InsertBySql(
		"INSERT INTO octo_project (project_id, space_id, name, creator, status, activated_at, created_at, updated_at) "+
			"VALUES (?, ?, 'Drifted project', 'someone-else', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))",
		pid, spaceID).Exec()
	require.NoError(t, err)
	_, err = sess.InsertBySql(
		"INSERT INTO octo_project_member (project_id, uid, space_id, role, status, removing, created_at, updated_at) "+
			"VALUES (?, ?, ?, 2, 1, 0, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))", pid, uid, spaceID).Exec()
	require.NoError(t, err)

	rows, err := projectpkg.UserProjectsInSpace(context.Background(), sess, spaceID, uid)
	require.NoError(t, err,
		"the function must not compare an octo_* column with a legacy one; that is MySQL 1267 on this shape")
	require.Equal(t, []projectpkg.UserProject{{
		ProjectID: pid,
		SpaceID:   spaceID,
		Name:      "Drifted project",
		Role:      2,
	}}, rows)

	// The fixture really is hostile: this is the join the implementation must not
	// use, and on this database it fails for the COLLATION reason rather than some
	// unrelated SQL error.
	var probe int
	err = sess.SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member` pm INNER JOIN `user` u ON u.uid = pm.uid").LoadOne(&probe)
	require.Error(t, err, "the legacy fixture must be drifted (utf8mb4_0900_ai_ci vs utf8mb4_general_ci), "+
		"otherwise this case proves nothing")
	require.Contains(t, err.Error(), "1267", "the probe join must trip MySQL's illegal-mix-of-collations: %v", err)

	// And the legacy-LEGACY pair the qualification read does use still works, because
	// both sides drifted together — that is why pkg/space may join them and this
	// function must not mix them with octo_*.
	require.NoError(t, sess.SelectBySql(
		"SELECT COUNT(*) FROM space_member sm INNER JOIN space s ON s.space_id = sm.space_id").LoadOne(&probe))

	// A failing LIST statement must surface as (nil, error), not as a truthful empty
	// answer. The probe database is the safe place for it: renaming its octo_project
	// breaks only the list query (qualification reads still succeed) and leaves the
	// shared test schema untouched.
	_, err = sess.UpdateBySql("RENAME TABLE `octo_project` TO `octo_project_offline`").Exec()
	require.NoError(t, err)
	rows, err = projectpkg.UserProjectsInSpace(context.Background(), sess, spaceID, uid)
	require.Error(t, err, "a failing list statement must be reported, not answered as []")
	require.Nil(t, rows, "no empty or partial answer may escape a failed statement")

	_, err = sess.UpdateBySql("RENAME TABLE `octo_project_offline` TO `octo_project`").Exec()
	require.NoError(t, err)
	rows, err = projectpkg.UserProjectsInSpace(context.Background(), sess, spaceID, uid)
	require.NoError(t, err)
	require.Len(t, rows, 1, "restoring the table must restore the answer")
}

// ---------- failure is an error, never an empty answer ----------

// TestUserProjectsInSpaceReturnsErrorsInsteadOfEmptyResults pins the other half
// of acceptance 10 at the layer that owns the reads: a transaction that cannot
// be opened, and a caller whose context is already cancelled, both surface as
// errors with NO result — never as the truthful-negative empty slice a consumer
// would read as "this user belongs to nothing".
func TestUserProjectsInSpaceReturnsErrorsInsteadOfEmptyResults(t *testing.T) {
	setup(t)

	// A closed pool fails at BeginTx. Using the real DSN keeps the door open for a
	// statement-level failure too; this is the cheapest non-destructive way to
	// prove the error path is not swallowed into an empty slice.
	conn, err := dbr.Open("mysql", testCtx.GetConfig().DB.MySQLAddr, nil)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	session := conn.NewSession(nil)

	rows, err := projectpkg.UserProjectsInSpace(context.Background(), session, spaceA, "iup-any-user")
	require.Error(t, err, "a transaction that cannot open must be reported, not answered as []")
	require.Nil(t, rows, "no partial result may escape a failed transaction")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	rows, err = projectpkg.UserProjectsInSpace(cancelled, testCtx.DB(), spaceA, "iup-any-user")
	require.Error(t, err, "a cancelled caller must not be answered as if the user had no projects")
	require.Nil(t, rows)
}

// ---------- one snapshot across qualification and list ----------

// iupListReadSeam is a dbr EventReceiver whose SpanStart fires a callback
// immediately before the FIRST statement touching octo_project_member — the
// list read. That is exactly the point between the qualification reads and the
// list read, and it needs no production code: dbr copies the session's
// EventReceiver onto the transaction it begins, and every Load/LoadOne goes
// through it.
//
// Only the list statement names octo_project_member; the qualification reads go
// through pkg/user and pkg/space and touch `user` / `space_member` instead.
type iupListReadSeam struct {
	once  sync.Once
	fire  func()
	fired bool
}

func (s *iupListReadSeam) Event(string)                       {}
func (s *iupListReadSeam) EventKv(string, map[string]string)  {}
func (s *iupListReadSeam) EventErr(_ string, err error) error { return err }
func (s *iupListReadSeam) EventErrKv(_ string, err error, _ map[string]string) error {
	return err
}
func (s *iupListReadSeam) Timing(string, int64)                      {}
func (s *iupListReadSeam) TimingKv(string, int64, map[string]string) {}

func (s *iupListReadSeam) SpanStart(ctx context.Context, eventName, query string) context.Context {
	if strings.Contains(query, "octo_project_member") {
		s.once.Do(func() {
			s.fired = true
			s.fire()
		})
	}
	return ctx
}

func (s *iupListReadSeam) SpanError(context.Context, error) {}
func (s *iupListReadSeam) SpanFinish(context.Context)       {}

// TestUserProjectsInSpacePinsOneSnapshotAcrossQualificationAndList pins
// acceptance 9 with the connection default REMOVED rather than inherited.
//
// Each case commits a change on a SEPARATE connection at the exact point between
// the qualification reads and the list read, then asserts two things:
//
//   - the in-flight request still describes the OLD snapshot, and
//   - a request that starts afterwards reflects the NEW state.
//
// The session the function runs on has transaction_isolation = READ-COMMITTED, so
// no case can pass by inheriting the deployment's level. Which case actually
// DISCRIMINATES on that is worth stating, because it is not all of them:
//
//   - role change — the discriminating case. The role is read by the LIST query,
//     which runs after the seam. Under READ COMMITTED the list would take a fresh
//     view and report the NEW role; it can only report the old one if the whole
//     function shares one snapshot, so this case fails if the isolation pin is
//     removed (or if the transaction is dropped altogether).
//   - Space seat revoked / account disabled — the qualification reads. Those run
//     before the seam in either shape, so they assert the ORDERING contract
//     (a revocation that commits after qualification belongs to the next answer,
//     while the next request must see it) rather than the isolation level. They
//     are here because acceptance 9 names exactly this window for revocation.
func TestUserProjectsInSpacePinsOneSnapshotAcrossQualificationAndList(t *testing.T) {
	const uid = "iup-snapshot-user"

	cases := []struct {
		name string
		// mutate runs on a different connection, inside the seam.
		mutate func(t *testing.T)
		// assertFirst inspects the in-flight answer (the old snapshot).
		assertFirst func(t *testing.T, rows []projectpkg.UserProject)
		// assertSecond inspects a fresh call (the new state).
		assertSecond func(t *testing.T, rows []projectpkg.UserProject)
	}{
		{
			name: "role change",
			mutate: func(t *testing.T) {
				_, err := testCtx.DB().UpdateBySql(
					"UPDATE octo_project_member SET role = 2 WHERE project_id = ? AND uid = ?", "iup-snapshot", uid).Exec()
				require.NoError(t, err)
			},
			assertFirst: func(t *testing.T, rows []projectpkg.UserProject) {
				require.Len(t, rows, 1)
				require.Equal(t, 0, rows[0].Role, "the in-flight answer must describe the snapshot the qualification reads established, not the role the seam just committed")
			},
			assertSecond: func(t *testing.T, rows []projectpkg.UserProject) {
				require.Len(t, rows, 1)
				require.Equal(t, 2, rows[0].Role)
			},
		},
		{
			name: "Space seat revoked",
			mutate: func(t *testing.T) {
				res, err := testCtx.DB().UpdateBySql(
					"UPDATE space_member SET status = 0 WHERE space_id = ? AND uid = ?", spaceA, uid).Exec()
				require.NoError(t, err)
				n, err := res.RowsAffected()
				require.NoError(t, err)
				require.EqualValues(t, 1, n, "the seam must actually have closed the Space seat")
			},
			assertFirst: func(t *testing.T, rows []projectpkg.UserProject) {
				require.Len(t, rows, 1, "a revocation committing after the first read belongs to the NEXT answer")
			},
			assertSecond: func(t *testing.T, rows []projectpkg.UserProject) {
				require.Empty(t, rows)
			},
		},
		{
			name: "account disabled",
			mutate: func(t *testing.T) {
				_, err := testCtx.DB().UpdateBySql("UPDATE `user` SET status = 0 WHERE uid = ?", uid).Exec()
				require.NoError(t, err)
			},
			assertFirst: func(t *testing.T, rows []projectpkg.UserProject) {
				require.Len(t, rows, 1)
			},
			assertSecond: func(t *testing.T, rows []projectpkg.UserProject) {
				require.Empty(t, rows)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)
			seedUserBare(t, uid)
			seedSpace(t, spaceA, 1)
			seedSpaceMember(t, spaceA, uid, 0, 1)
			iupProject(t, "iup-snapshot", spaceA, "someone-else", "Snapshot project", "", "", 1, true)
			iupSeat(t, "iup-snapshot", uid, spaceA, 0, 1, 0)

			// A session whose OWN default is READ COMMITTED. One pooled connection, so
			// the transaction necessarily runs on the connection that was made
			// hostile; driver BeginTx applies SET TRANSACTION ISOLATION LEVEL to the
			// next transaction only, so this measures the code's pin rather than the
			// server default.
			conn, err := dbr.Open("mysql", testCtx.GetConfig().DB.MySQLAddr, nil)
			require.NoError(t, err)
			defer conn.Close()
			conn.SetMaxOpenConns(1)
			conn.SetMaxIdleConns(1)
			session := conn.NewSession(nil)
			_, err = session.DB.Exec("SET SESSION transaction_isolation = 'READ-COMMITTED'")
			require.NoError(t, err)
			require.Equal(t, "READ-COMMITTED", sessionIsolation(t, session),
				"the fixture must actually be hostile, or this case proves nothing")

			seam := &iupListReadSeam{fire: func() { tc.mutate(t) }}
			session.EventReceiver = seam

			rows, err := projectpkg.UserProjectsInSpace(context.Background(), session, spaceA, uid)
			require.NoError(t, err)
			require.True(t, seam.fired, "the seam must have fired, or no change landed mid-request")
			tc.assertFirst(t, rows)

			// The pin is scoped to the transaction: the pooled connection must still
			// report READ-COMMITTED to whoever borrows it next.
			require.Equal(t, "READ-COMMITTED", sessionIsolation(t, session),
				"UserProjectsInSpace must not leave its isolation level behind on the pooled connection")

			tc.assertSecond(t, iupUserProjects(t, spaceA, uid))
		})
	}
}
