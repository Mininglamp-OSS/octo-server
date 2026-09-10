package space

// This regression deliberately recreates the legacy/migration collation split
// instead of inheriting CI's all-general_ci schema. The directory owner query
// combines user.name, user_verification.real_name, and space_member.uid for
// keyword matching, so it must remain executable when those source tables do
// not share a table collation.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/require"
)

const directoryCollationProbeDB = "octo_space_directory_collation_probe"

// These are the tables queryDirectoryOwners touches on its keyword path. The
// legacy tables inherit utf8mb4_0900_ai_ci in the deployed drift shape, while
// user_verification remains explicitly utf8mb4_general_ci.
var directoryCollationProbeTables = []struct {
	name   string
	legacy bool
}{
	{"space_member", true},
	{"user", true},
	{"robot", true},
	{"user_verification", false},
}

func newDirectoryCollationProbe(t *testing.T) *DB {
	t.Helper()
	addr := os.Getenv("OCTO_TEST_MYSQL_ADDR")
	if addr == "" {
		addr = testCtx.GetConfig().DB.MySQLAddr
	}
	slash := strings.LastIndex(addr, "/")
	require.Positive(t, slash, "unexpected MySQL DSN shape")
	opts := ""
	if q := strings.Index(addr[slash:], "?"); q >= 0 {
		opts = addr[slash+q:]
	}
	adminDSN := addr[:slash+1] + opts
	probeDSN := addr[:slash+1] + directoryCollationProbeDB + opts

	admin, err := dbr.Open("mysql", adminDSN, nil)
	require.NoError(t, err)
	adminSess := admin.NewSession(nil)
	exec := func(sess *dbr.Session, stmt string) {
		_, execErr := sess.UpdateBySql(stmt).Exec()
		require.NoError(t, execErr, stmt)
	}
	exec(adminSess, "DROP DATABASE IF EXISTS `"+directoryCollationProbeDB+"`")
	exec(adminSess, "CREATE DATABASE `"+directoryCollationProbeDB+
		"` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")

	conn, err := dbr.Open("mysql", probeDSN, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = adminSess.UpdateBySql("DROP DATABASE IF EXISTS `" + directoryCollationProbeDB + "`").Exec()
		_ = conn.Close()
		_ = admin.Close()
	})
	sess := conn.NewSession(nil)

	var sourceSchema string
	require.NoError(t, testCtx.DB().SelectBySql("SELECT DATABASE()").LoadOne(&sourceSchema))
	require.NotEmpty(t, sourceSchema)
	for _, table := range directoryCollationProbeTables {
		exec(sess, "CREATE TABLE `"+table.name+"` LIKE `"+sourceSchema+"`.`"+table.name+"`")
		if table.legacy {
			exec(sess, "ALTER TABLE `"+table.name+"` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
		}
	}
	return &DB{session: sess}
}

func TestDirectoryKeywordSurvivesCollationDrift(t *testing.T) {
	_, _, err := setup(t)
	require.NoError(t, err)
	probe := newDirectoryCollationProbe(t)

	const spaceID = "sp-directory-collation"
	type owner struct {
		uid      string
		name     string
		realName string
		keyword  string
	}
	owners := []owner{
		{uid: "owner-name", name: "Alice Drift", keyword: "Alice Drift"},
		{uid: "owner-real-name", realName: "Bob Drift", keyword: "Bob Drift"},
		{uid: "owner-placeholder", keyword: memberDisplayNamePlaceholderPrefix + "owner-placeholder"},
	}
	for _, owner := range owners {
		_, err = probe.session.InsertBySql(
			"INSERT INTO `user` (uid, name, robot, status, is_destroy) VALUES (?, ?, 0, 1, 0)",
			owner.uid, owner.name,
		).Exec()
		require.NoError(t, err)
		_, err = probe.session.InsertBySql(
			"INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)",
			spaceID, owner.uid,
		).Exec()
		require.NoError(t, err)
		if owner.realName != "" {
			_, err = probe.session.InsertBySql(
				"INSERT INTO user_verification (user_id, real_name, source, source_sub) VALUES (?, ?, 'test', 'test')",
				owner.uid, owner.realName,
			).Exec()
			require.NoError(t, err)
		}
	}

	for _, owner := range owners {
		t.Run(owner.uid, func(t *testing.T) {
			got, queryErr := probe.queryDirectoryOwners(context.Background(), spaceID, owner.keyword)
			require.NoError(t, queryErr)
			require.Len(t, got, 1)
			require.Equal(t, owner.uid, got[0].UID)
		})
	}
}
