package base

import (
	"bytes"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	migrate "github.com/rubenv/sql-migrate"
	"github.com/stretchr/testify/require"
)

// searchetlMigrationFile is the migration whose Up DDL this test exercises. Kept as a named
// constant so the test fails loudly (file-not-found) if the migration is ever renamed.
const searchetlMigrationFile = "20260614000001_searchetl_init.sql"

// Test_Issue528_SearchetlMigrationIdempotentWhenTableExists reproduces the octo-server#528
// boot panic: octo_etl_es_cursor is owned by the standalone octo-search-indexer, which shares
// this database. When the indexer starts first it creates the table, but octo-server may not
// yet have recorded this migration ID in gorp_migrations (new DB / first boot) — so sql-migrate
// executes the Up DDL against an already-existing table.
//
// We reproduce that "indexer-first" world by pre-creating the table ourselves, then run this
// migration's OWN Up statements verbatim (parsed from the embedded sql FS, exactly what
// rubenv/sql-migrate runs in production). A bare `CREATE TABLE` fails with Error 1050 (RED,
// pre-fix); `CREATE TABLE IF NOT EXISTS` is a no-op and succeeds (GREEN, post-fix). Reading the
// DDL from the real file keeps the test honest if the migration ever drifts.
func Test_Issue528_SearchetlMigrationIdempotentWhenTableExists(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	// Never leave the shared test schema in a half-created state for later tests.
	defer func() {
		_, _ = ctx.DB().UpdateBySql("DROP TABLE IF EXISTS `octo_etl_es_cursor`").Exec()
	}()

	// Arrange: reproduce the octo-search-indexer-first world where the table already exists
	// before octo-server applies this migration ID. (NewTestServer's migration run may already
	// have created it; DROP + CREATE makes the precondition deterministic regardless of setup.)
	_, _ = ctx.DB().UpdateBySql("DROP TABLE IF EXISTS `octo_etl_es_cursor`").Exec()
	_, err := ctx.DB().UpdateBySql(
		"CREATE TABLE `octo_etl_es_cursor` (" +
			"`shard_table` VARCHAR(64) NOT NULL, " +
			"`last_id` BIGINT NOT NULL DEFAULT 0, " +
			"`updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP, " +
			"PRIMARY KEY (`shard_table`)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
	).Exec()
	require.NoError(t, err, "scaffold: pre-create octo_etl_es_cursor as octo-search-indexer would")

	// Act: run the migration's real Up statements verbatim against the pre-existing table.
	data, err := sqlFS.ReadFile("sql/" + searchetlMigrationFile)
	require.NoError(t, err, "read migration %s from embedded sql FS", searchetlMigrationFile)
	m, err := migrate.ParseMigration(searchetlMigrationFile, bytes.NewReader(data))
	require.NoError(t, err, "parse migration %s", searchetlMigrationFile)
	require.NotEmpty(t, m.Up, "migration %s has no Up statements", searchetlMigrationFile)

	// Assert: applying Up over an existing table must NOT error (the #528 boot panic).
	for _, stmt := range m.Up {
		_, err := ctx.DB().UpdateBySql(stmt).Exec()
		require.NoError(t, err,
			"migration %s Up must be idempotent against a pre-existing table (octo-server#528): %s",
			searchetlMigrationFile, stmt)
	}
}
