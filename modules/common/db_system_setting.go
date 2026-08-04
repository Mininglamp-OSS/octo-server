package common

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/config"
	ldb "github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/gocraft/dbr/v2"
)

// systemSettingDB is the persistence layer for the system_setting KV table.
//
// The table stores admin-tunable global config (paired with octo's static yaml
// defaults). Each row is uniquely identified by (category, key_name); upsert
// uses that unique key so admins can overwrite values without producing
// duplicate rows.
type systemSettingDB struct {
	session *dbr.Session
	ctx     *config.Context
}

func newSystemSettingDB(ctx *config.Context) *systemSettingDB {
	return &systemSettingDB{
		session: ctx.DB(),
		ctx:     ctx,
	}
}

// listAll returns every row in system_setting. Callers should treat the result
// as the full snapshot; empty value means "not configured, fall back to yaml".
func (s *systemSettingDB) listAll() ([]*systemSettingModel, error) {
	var rows []*systemSettingModel
	_, err := s.session.Select("*").From("system_setting").Load(&rows)
	return rows, err
}

// upsert writes the row identified by (category, key_name). If the row exists,
// value / value_type / description are overwritten; otherwise a new row is
// inserted. Implemented via INSERT ... ON DUPLICATE KEY UPDATE so the unique
// index `uk_category_key` enforces idempotency at the database layer.
func (s *systemSettingDB) upsert(category, key, value, valueType, description string) error {
	return upsertSystemSettingOn(s.session, category, key, value, valueType, description)
}

// upsertWithTx is the transactional flavour of upsert. The manager API uses
// it to apply a batch of admin edits atomically: either every row in the
// payload is persisted, or none are.
func (s *systemSettingDB) upsertWithTx(tx *dbr.Tx, category, key, value, valueType, description string) error {
	return upsertSystemSettingOn(tx, category, key, value, valueType, description)
}

// beginTx opens a new transaction on the underlying session; callers must
// commit or rollback. Exposed so the manager layer can batch the entire
// admin payload without producing partial writes on mid-batch error.
func (s *systemSettingDB) beginTx() (*dbr.Tx, error) {
	return s.session.Begin()
}

// errGuardAnchorMissing means the serialisation anchor row is gone even after
// ensureGuardAnchor tried to restore it, so a cross-key guard cannot be
// serialised. Fail closed: an admin config write is not a hot path, and
// validating without the lock is exactly the race the anchor exists to close.
var errGuardAnchorMissing = errors.New("system_setting_guard_lock anchor row is missing")

// ensureGuardAnchor restores the anchor row if it is absent. Call it BEFORE
// opening the guarded transaction — never inside it, so healing cannot interact
// with the lock the transaction is about to take.
//
// The migration seeds the row, so this is a no-op in a healthy deployment. It
// exists because the row is trivially destroyable — a truncate-based restore, a
// test harness that clears every table (testutil.CleanAllTables does) — and
// without it a lost row turns every guarded settings write into a hard failure
// that reads like a database outage. Healing is safe precisely because the row
// carries no state: its identity is the whole of its content.
func (s *systemSettingDB) ensureGuardAnchor() error {
	_, err := s.session.InsertBySql(
		"INSERT IGNORE INTO system_setting_guard_lock (guard_key) VALUES (1)",
	).Exec()
	return err
}

// lockGuardAnchorWithTx takes an X lock on the single system_setting_guard_lock
// row, serialising every cross-key guard evaluation that runs in a transaction.
//
// It must be the FIRST statement in the guarded transaction: everything after it
// — the re-read of the current rows, the merge, the violation check, the upserts
// — then happens with no other guarded writer in flight, on any replica, which is
// what makes the check-then-write atomic rather than merely adjacent.
//
// Locking a necessarily-existing single row rather than the (possibly absent)
// setting rows themselves is deliberate; see the migration comment for why gap
// locks degrade into a deadlock here.
func (s *systemSettingDB) lockGuardAnchorWithTx(tx *dbr.Tx) error {
	var key int
	found, err := tx.SelectBySql(
		"SELECT guard_key FROM system_setting_guard_lock WHERE guard_key=1 FOR UPDATE",
	).Load(&key)
	if err != nil {
		return err
	}
	if found == 0 {
		return errGuardAnchorMissing
	}
	return nil
}

// loadValuesWithTx reads the given (category, key_name) pairs inside the
// transaction and returns them keyed by schemaKey(category, key).
//
// This is the "re-read under the lock" half of the guard: validating against
// SystemSettings' in-memory snapshot is what allowed the skew, because that
// snapshot has a 60s TTL and only the writing replica refreshes it eagerly.
//
// Rows that do not exist are simply absent from the result, and an existing row
// with an empty value is returned as-is — the caller resolves both through the
// same env → code-default chain the snapshot getters use, where lookup() already
// treats "" as not-configured.
func (s *systemSettingDB) loadValuesWithTx(tx *dbr.Tx, pairs [][2]string) (map[string]string, error) {
	out := make(map[string]string, len(pairs))
	if len(pairs) == 0 {
		return out, nil
	}
	for _, p := range pairs {
		var value string
		found, err := tx.SelectBySql(
			"SELECT value FROM system_setting WHERE category=? AND key_name=?", p[0], p[1],
		).Load(&value)
		if err != nil {
			return nil, err
		}
		if found > 0 {
			out[schemaKey(p[0], p[1])] = value
		}
	}
	return out, nil
}

// runner is the minimal surface common to *dbr.Session and *dbr.Tx that we
// need for INSERT ... ON DUPLICATE KEY UPDATE. Keeping it unexported here
// avoids leaking a dbr type into the public API.
type sqlRunner interface {
	InsertBySql(query string, value ...interface{}) *dbr.InsertBuilder
}

func upsertSystemSettingOn(runner sqlRunner, category, key, value, valueType, description string) error {
	_, err := runner.InsertBySql(
		"INSERT INTO system_setting (category, key_name, value, value_type, description) "+
			"VALUES (?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE value = VALUES(value), value_type = VALUES(value_type), description = VALUES(description)",
		category, key, value, valueType, description,
	).Exec()
	return err
}

// systemSettingModel mirrors the system_setting row layout.
type systemSettingModel struct {
	Category    string
	KeyName     string
	Value       string
	ValueType   string
	Description string
	ldb.BaseModel
}
