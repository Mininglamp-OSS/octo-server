package common

import (
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
