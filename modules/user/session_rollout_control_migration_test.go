package user

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	_ "github.com/go-sql-driver/mysql"
	migrate "github.com/rubenv/sql-migrate"
	"github.com/stretchr/testify/require"
)

func TestSessionRolloutControlMigrationDefinesSingleAuthorityAndAudit(t *testing.T) {
	raw, err := os.ReadFile("sql/20260812000001_session_rollout_control.sql")
	require.NoError(t, err)
	sql := string(raw)
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS `octo_session_rollout_state`",
		"`floor` VARCHAR(16) NOT NULL",
		"`max_per_uid` INT UNSIGNED NOT NULL",
		"`version` BIGINT UNSIGNED NOT NULL",
		"`paused` TINYINT(1) NOT NULL",
		"CREATE TABLE IF NOT EXISTS `octo_session_rollout_advance`",
		"`evidence_json` JSON",
		"`redis_run_id` VARCHAR(64)",
		"`writer_fingerprint` VARCHAR(64)",
	} {
		require.Contains(t, sql, required)
	}
	require.NotContains(t, sql, "octo_session_rollout_marker")
	require.Equal(t, 2, strings.Count(sql, "DROP TABLE IF EXISTS"))
}

func TestSessionRolloutMarkerMigrationRemainsAvailableForUpgradeCompatibility(t *testing.T) {
	raw, err := os.ReadFile("sql/20260811000001_session_rollout_marker.sql")
	require.NoError(t, err)
	require.Contains(t, string(raw), "CREATE TABLE IF NOT EXISTS `octo_session_rollout_marker`")
}

func TestSessionRolloutOldControlMigrationRemainsANoopCompatibilityTombstone(t *testing.T) {
	raw, err := os.ReadFile("sql/20260811000001_session_rollout_control.sql")
	require.NoError(t, err)
	require.Contains(t, string(raw), "Compatibility tombstone")
	require.NotContains(t, string(raw), "CREATE TABLE")
}

func TestSessionRolloutMigrationsUpgradeDatabaseWithAppliedMarker(t *testing.T) {
	db := newSessionRolloutMigrationTestDatabase(t)
	legacy := migrate.MemoryMigrationSource{Migrations: []*migrate.Migration{{
		Id: "20260811000001_session_rollout_marker.sql",
		Up: []string{`CREATE TABLE octo_session_rollout_marker (
			singleton_id TINYINT UNSIGNED NOT NULL,
			initialized_floor VARCHAR(16) NOT NULL DEFAULT '',
			PRIMARY KEY (singleton_id)
		) ENGINE=InnoDB`},
	}}}
	applied, err := migrate.Exec(db, "mysql", legacy, migrate.Up)
	require.NoError(t, err)
	require.Equal(t, 1, applied)

	current := currentSessionRolloutMigrationSource(t)
	applied, err = migrate.Exec(db, "mysql", current, migrate.Up)
	require.NoError(t, err)
	require.Equal(t, 2, applied, "the compatibility tombstone and control migration must run after the marker")
	assertSessionRolloutMigrationTables(t, db)
}

func TestSessionRolloutMigrationsUpgradeDatabaseWithAppliedPRControl(t *testing.T) {
	db := newSessionRolloutMigrationTestDatabase(t)
	legacyControl := parseSessionRolloutMigration(t, "20260812000001_session_rollout_control.sql")
	legacyControl.Id = "20260811000001_session_rollout_control.sql"
	legacy := migrate.MemoryMigrationSource{Migrations: []*migrate.Migration{
		legacyControl,
		{
			Id: "20260811000001_session_rollout_marker.sql",
			Up: []string{`CREATE TABLE octo_session_rollout_marker (
				singleton_id TINYINT UNSIGNED NOT NULL,
				PRIMARY KEY (singleton_id)
			) ENGINE=InnoDB`},
		},
	}}
	applied, err := migrate.Exec(db, "mysql", legacy, migrate.Up)
	require.NoError(t, err)
	require.Equal(t, 2, applied)

	current := currentSessionRolloutMigrationSource(t)
	applied, err = migrate.Exec(db, "mysql", current, migrate.Up)
	require.NoError(t, err)
	require.Equal(t, 1, applied, "only the new control migration should remain pending")
	assertSessionRolloutMigrationTables(t, db)
}

func assertSessionRolloutMigrationTables(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{
		"octo_session_rollout_marker",
		"octo_session_rollout_state",
		"octo_session_rollout_advance",
	} {
		var count int
		err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.TABLES
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table).Scan(&count)
		require.NoError(t, err)
		require.Equal(t, 1, count, "table %s", table)
	}
}

func currentSessionRolloutMigrationSource(t *testing.T) migrate.MemoryMigrationSource {
	t.Helper()
	entries, err := sqlFS.ReadDir("sql")
	require.NoError(t, err)

	migrations := make([]*migrate.Migration, 0, 2)
	for _, entry := range entries {
		if !strings.Contains(entry.Name(), "session_rollout_marker") &&
			!strings.Contains(entry.Name(), "session_rollout_control") {
			continue
		}
		file, err := sqlFS.Open("sql/" + entry.Name())
		require.NoError(t, err)
		reader, ok := file.(io.ReadSeeker)
		require.True(t, ok, "%s must be seekable", entry.Name())
		migration, err := migrate.ParseMigration(entry.Name(), reader)
		require.NoError(t, err)
		require.NoError(t, file.Close())
		migrations = append(migrations, migration)
	}
	require.NotEmpty(t, migrations)
	return migrate.MemoryMigrationSource{Migrations: migrations}
}

func parseSessionRolloutMigration(t *testing.T, name string) *migrate.Migration {
	t.Helper()
	file, err := sqlFS.Open("sql/" + name)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	reader, ok := file.(io.ReadSeeker)
	require.True(t, ok, "%s must be seekable", name)
	migration, err := migrate.ParseMigration(name, reader)
	require.NoError(t, err)
	return migration
}

func newSessionRolloutMigrationTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	databaseName := "octo_session_rollout_" + util.GenerUUID()[:12]
	bootstrap, err := sql.Open("mysql", "root:demo@tcp(127.0.0.1:3306)/information_schema?charset=utf8mb4&parseTime=true")
	require.NoError(t, err)
	require.NoError(t, bootstrap.Ping())
	_, err = bootstrap.Exec("CREATE DATABASE `" + databaseName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, dropErr := bootstrap.Exec("DROP DATABASE IF EXISTS `" + databaseName + "`")
		require.NoError(t, dropErr)
		require.NoError(t, bootstrap.Close())
	})

	db, err := sql.Open("mysql", fmt.Sprintf(
		"root:demo@tcp(127.0.0.1:3306)/%s?charset=utf8mb4&parseTime=true", databaseName,
	))
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}
