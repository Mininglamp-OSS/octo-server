package message

// PR-2a writer-cutover tests. The message-module write endpoints (edit / pin /
// delete / revoke / readed) have no live HTTP integration harness in this repo —
// those tests are all t.Skip'd behind the issue-#17 migration TODO — so these
// focused tests exercise the genuinely new PR-2a logic directly against a live
// MySQL: the chunking reserveVersions helper and the transactional
// insertOrUpdateDeletedTx used by the mutual-delete cutover. The allocator
// itself is covered exhaustively in internal/msgextraseq.
//
// Infra mirrors internal/msgextraseq's tests: NewTestContext (no module.Setup /
// migration) + a re-seeded allocator-state row. Override the DSN with
// OCTO_TEST_MYSQL_ADDR when MySQL is elsewhere.

import (
	"os"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/internal/msgextraseq"
	"github.com/gocraft/dbr/v2"
)

func seqTestMySQLAddr() string {
	if v := os.Getenv("OCTO_TEST_MYSQL_ADDR"); v != "" {
		return v
	}
	return "root:demo@tcp(127.0.0.1)/test?charset=utf8mb4&parseTime=true"
}

func newSeqTestMessage(t *testing.T, mode int, floor int64) (*Message, *config.Context) {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	cfg.DB.MySQLAddr = seqTestMySQLAddr()
	cfg.DB.Migration = false
	ctx := testutil.NewTestContext(cfg)
	db := ctx.DB()
	stmts := []string{
		"CREATE TABLE IF NOT EXISTS `seq` (`key` VARCHAR(100) NOT NULL, `min_seq` BIGINT NOT NULL DEFAULT 0, `step` INT NOT NULL DEFAULT 0, PRIMARY KEY (`key`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
		"CREATE TABLE IF NOT EXISTS `message_extra` (`id` BIGINT NOT NULL AUTO_INCREMENT, `message_id` VARCHAR(40) NOT NULL DEFAULT '', `message_seq` BIGINT NOT NULL DEFAULT 0, `channel_id` VARCHAR(100) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci NOT NULL DEFAULT '', `channel_type` SMALLINT NOT NULL DEFAULT 0, `revoke` INT NOT NULL DEFAULT 0, `revoker` VARCHAR(40) NOT NULL DEFAULT '', `is_deleted` SMALLINT NOT NULL DEFAULT 0, `is_mutual_deleted` INT NOT NULL DEFAULT 0, `readed_count` INT NOT NULL DEFAULT 0, `content_edit` TEXT, `content_edit_hash` VARCHAR(40) NOT NULL DEFAULT '', `edited_at` INT NOT NULL DEFAULT 0, `is_pinned` INT NOT NULL DEFAULT 0, `card_seq` BIGINT, `version` BIGINT NOT NULL DEFAULT 0, PRIMARY KEY (`id`), UNIQUE KEY `message_id` (`message_id`), KEY `channel_version_idx` (`channel_id`,`channel_type`,`version`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
		"CREATE TABLE IF NOT EXISTS `octo_message_extra_channel_seq` (`channel_id` VARCHAR(100) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci NOT NULL DEFAULT '', `channel_type` SMALLINT NOT NULL DEFAULT 0, `last_version` BIGINT NOT NULL DEFAULT 0, PRIMARY KEY (`channel_id`,`channel_type`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
		"CREATE TABLE IF NOT EXISTS `octo_message_extra_version_state` (`singleton_id` TINYINT UNSIGNED NOT NULL, `mode` TINYINT NOT NULL DEFAULT 0, `epoch` BIGINT UNSIGNED NOT NULL DEFAULT 0, `cutover_floor` BIGINT NOT NULL DEFAULT 0, PRIMARY KEY (`singleton_id`), CONSTRAINT `chk_message_extra_version_singleton` CHECK (`singleton_id` = 1)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
	}
	for _, s := range stmts {
		if _, err := db.UpdateBySql(s).Exec(); err != nil {
			t.Fatalf("ensure schema: %v", err)
		}
	}
	// message_extra is shared across this package's tests (no migration runs;
	// Migration=false), which create it with divergent ad-hoc schemas. Additively
	// ensure the columns AND the message_id unique key the mutual-delete upsert
	// needs — never drop it, so sibling tests keep their columns/rows. In CI the
	// real migration owns the full table and these are no-ops.
	ensureColumn(t, db, "message_extra", "message_seq", "BIGINT NOT NULL DEFAULT 0")
	ensureColumn(t, db, "message_extra", "is_deleted", "SMALLINT NOT NULL DEFAULT 0")
	ensureUniqueIndex(t, db, "message_extra", "message_id", "`message_id`")
	// Only the octo_ tables are exclusive to this package's seq tests; clean just
	// those (leave shared message_extra / seq rows untouched).
	for _, tbl := range []string{"octo_message_extra_channel_seq", "octo_message_extra_version_state"} {
		if _, err := db.UpdateBySql("DELETE FROM `" + tbl + "`").Exec(); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}
	if _, err := db.InsertBySql(
		"INSERT INTO `octo_message_extra_version_state` (`singleton_id`,`mode`,`epoch`,`cutover_floor`) VALUES (1,?,0,?)",
		mode, floor,
	).Exec(); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	// Minimal Message: only the fields the PR-2a helpers touch.
	m := &Message{ctx: ctx, seqStore: msgextraseq.New(ctx), messageExtraDB: newMessageExtraDB(ctx)}
	return m, ctx
}

// ensureColumn adds a column if absent (MySQL lacks ADD COLUMN IF NOT EXISTS).
func ensureColumn(t *testing.T, db *dbr.Session, table, column, def string) {
	t.Helper()
	var count int
	if err := db.SelectBySql(
		"SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? AND column_name=?",
		table, column,
	).LoadOne(&count); err != nil {
		t.Fatalf("check column %s.%s: %v", table, column, err)
	}
	if count == 0 {
		if _, err := db.UpdateBySql("ALTER TABLE `" + table + "` ADD COLUMN `" + column + "` " + def).Exec(); err != nil {
			t.Fatalf("add column %s.%s: %v", table, column, err)
		}
	}
}

// ensureUniqueIndex adds a unique index if absent (idempotent across test order).
func ensureUniqueIndex(t *testing.T, db *dbr.Session, table, indexName, cols string) {
	t.Helper()
	var count int
	if err := db.SelectBySql(
		"SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? AND index_name=?",
		table, indexName,
	).LoadOne(&count); err != nil {
		t.Fatalf("check index %s.%s: %v", table, indexName, err)
	}
	if count == 0 {
		if _, err := db.UpdateBySql("ALTER TABLE `" + table + "` ADD UNIQUE KEY `" + indexName + "` (" + cols + ")").Exec(); err != nil {
			t.Fatalf("add unique index %s.%s: %v", table, indexName, err)
		}
	}
}

func seqLastVersion(t *testing.T, ctx *config.Context, channelID string, channelType uint8) int64 {
	t.Helper()
	var v int64
	err := ctx.DB().SelectBySql(
		"SELECT COALESCE(`last_version`,0) FROM `octo_message_extra_channel_seq` WHERE `channel_id`=? AND `channel_type`=?",
		channelID, channelType,
	).LoadOne(&v)
	if err != nil && err != dbr.ErrNotFound {
		t.Fatalf("read last_version: %v", err)
	}
	return v
}

// TestReserveVersions_ChunksAcrossCap proves the read-receipt/batch helper
// chunks a request larger than MaxReserveCount into monotonic contiguous
// reservations without gaps or duplicates (brief D3, Acceptance batch case).
func TestReserveVersions_ChunksAcrossCap(t *testing.T) {
	floor := int64(1000)
	m, ctx := newSeqTestMessage(t, msgextraseq.ModeTransactional, floor)
	ch, ct := "c-chunk", uint8(2)
	const n = msgextraseq.MaxReserveCount + 500 // 1500: forces two chunks

	tx, err := ctx.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.RollbackUnlessCommitted()
	versions, err := m.reserveVersions(tx, ch, ct, n)
	if err != nil {
		t.Fatalf("reserveVersions: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if len(versions) != n {
		t.Fatalf("len=%d want %d", len(versions), n)
	}
	for i, v := range versions {
		want := floor + int64(i) + 1
		if v != want {
			t.Fatalf("versions[%d]=%d want %d (not contiguous/monotonic)", i, v, want)
		}
	}
	if got := seqLastVersion(t, ctx, ch, ct); got != floor+int64(n) {
		t.Fatalf("last_version=%d want %d", got, floor+int64(n))
	}
}

// TestReserveVersions_Legacy proves legacy mode returns n distinct values and
// does not touch the transactional channel-sequence table.
func TestReserveVersions_Legacy(t *testing.T) {
	m, ctx := newSeqTestMessage(t, msgextraseq.ModeLegacy, 0)
	ch, ct := "c-legacy", uint8(2)

	tx, err := ctx.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.RollbackUnlessCommitted()
	versions, err := m.reserveVersions(tx, ch, ct, 3)
	if err != nil {
		t.Fatalf("reserveVersions: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("len=%d want 3", len(versions))
	}
	seen := map[int64]bool{}
	for _, v := range versions {
		if seen[v] {
			t.Fatalf("legacy duplicate %d", v)
		}
		seen[v] = true
	}
	if got := seqLastVersion(t, ctx, ch, ct); got != 0 {
		t.Fatalf("legacy mode wrote the transactional table: last_version=%d", got)
	}
}

// TestInsertOrUpdateDeletedTx proves the mutual-delete cutover's new
// transactional DB writer persists is_deleted + the reserved version, and that a
// second call (upsert) advances the version on the same row.
func TestInsertOrUpdateDeletedTx(t *testing.T) {
	m, ctx := newSeqTestMessage(t, msgextraseq.ModeTransactional, 1000)
	ch, ct := "c-del", uint8(2)
	msgID := "990001"

	writeDeleted := func() int64 {
		tx, err := ctx.DB().Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.RollbackUnlessCommitted()
		versions, err := m.reserveVersions(tx, ch, ct, 1)
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if err := m.messageExtraDB.insertOrUpdateDeletedTx(&messageExtraModel{
			MessageID:   msgID,
			ChannelID:   ch,
			ChannelType: ct,
			IsDeleted:   1,
			Version:     versions[0],
		}, tx); err != nil {
			t.Fatalf("insertOrUpdateDeletedTx: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return versions[0]
	}

	v1 := writeDeleted()
	v2 := writeDeleted() // upsert on the same message_id
	if v2 <= v1 {
		t.Fatalf("second version %d not greater than first %d", v2, v1)
	}

	var row struct {
		IsDeleted int   `db:"is_deleted"`
		Version   int64 `db:"version"`
	}
	if err := ctx.DB().SelectBySql(
		"SELECT `is_deleted`, `version` FROM `message_extra` WHERE `message_id`=?", msgID,
	).LoadOne(&row); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if row.IsDeleted != 1 {
		t.Fatalf("is_deleted=%d want 1", row.IsDeleted)
	}
	if row.Version != v2 {
		t.Fatalf("row version=%d want %d (latest reserved)", row.Version, v2)
	}
	// Exactly one row (upsert, not insert-twice).
	var count int
	if err := ctx.DB().SelectBySql("SELECT COUNT(*) FROM `message_extra` WHERE `message_id`=?", msgID).LoadOne(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("row count=%d want 1 (upsert)", count)
	}
}
