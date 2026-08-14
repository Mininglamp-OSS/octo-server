package notification

import (
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
)

type dbStore struct {
	session *dbr.Session
}

// dbr interpolates MySQL time arguments as UTC text. Rebuild values read from
// DATETIME using their wall-clock components so a connection configured with
// loc=Local does not shift the persisted UTC value on read-back.
func normalizePauseRecord(record *pauseRecord) *pauseRecord {
	if record == nil || record.PausedUntil == nil {
		return record
	}
	value := *record.PausedUntil
	utc := time.Date(value.Year(), value.Month(), value.Day(), value.Hour(), value.Minute(), value.Second(), value.Nanosecond(), time.UTC)
	record.PausedUntil = &utc
	return record
}

func newDBStore(ctx *config.Context) *dbStore {
	return &dbStore{session: ctx.DB()}
}

func (s *dbStore) get(uid string) (*pauseRecord, error) {
	var record *pauseRecord
	_, err := s.session.Select("uid", "mode", "paused_until", "revision", "updated_at").
		From("user_notification_pause").
		Where("uid=?", uid).
		Load(&record)
	return normalizePauseRecord(record), err
}

func (s *dbStore) upsert(uid, mode string, pausedUntil *time.Time, now time.Time) (*pauseRecord, bool, error) {
	tx, err := s.session.Begin()
	if err != nil {
		return nil, false, err
	}
	var until interface{}
	if pausedUntil != nil {
		until = pausedUntil.UTC().Truncate(time.Millisecond)
	}
	result, err := tx.InsertBySql(
		"INSERT INTO user_notification_pause (uid, mode, paused_until, revision, updated_at) VALUES (?, ?, ?, 1, ?) "+
			"ON DUPLICATE KEY UPDATE "+
			"revision=revision+IF(mode <=> VALUES(mode) AND paused_until <=> VALUES(paused_until), 0, 1), "+
			"updated_at=IF(mode <=> VALUES(mode) AND paused_until <=> VALUES(paused_until), updated_at, VALUES(updated_at)), "+
			"mode=VALUES(mode), paused_until=VALUES(paused_until)",
		uid, mode, until, now.UTC(),
	).Exec()
	if err != nil {
		_ = tx.Rollback()
		return nil, false, err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return nil, false, err
	}
	var record *pauseRecord
	_, err = tx.Select("uid", "mode", "paused_until", "revision", "updated_at").
		From("user_notification_pause").
		Where("uid=?", uid).
		Suffix("FOR UPDATE").
		Load(&record)
	if err != nil {
		_ = tx.Rollback()
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return normalizePauseRecord(record), rowsAffected > 0, nil
}

func (s *dbStore) clear(uid string, now time.Time) (*pauseRecord, bool, error) {
	tx, err := s.session.Begin()
	if err != nil {
		return nil, false, err
	}
	// Expired timed rows are inactive on the wire but still persisted state;
	// clearing them is a real cleanup transition and advances the revision.
	result, err := tx.Update("user_notification_pause").
		Set("mode", nil).
		Set("paused_until", nil).
		Set("updated_at", now.UTC()).
		Set("revision", dbr.Expr("revision+1")).
		Where("uid=? AND (mode IS NOT NULL OR paused_until IS NOT NULL)", uid).
		Exec()
	if err != nil {
		_ = tx.Rollback()
		return nil, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return nil, false, err
	}
	var record *pauseRecord
	_, err = tx.Select("uid", "mode", "paused_until", "revision", "updated_at").
		From("user_notification_pause").
		Where("uid=?", uid).
		Suffix("FOR UPDATE").
		Load(&record)
	if err != nil {
		_ = tx.Rollback()
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return normalizePauseRecord(record), rows > 0, nil
}

func (s *dbStore) getActiveByUIDs(uids []string, now time.Time) (map[string]struct{}, error) {
	active := make(map[string]struct{})
	if len(uids) == 0 {
		return active, nil
	}
	const batchSize = 1000
	for start := 0; start < len(uids); start += batchSize {
		end := start + batchSize
		if end > len(uids) {
			end = len(uids)
		}
		var records []*pauseRecord
		_, err := s.session.Select("uid", "mode", "paused_until", "revision", "updated_at").
			From("user_notification_pause").
			Where("uid IN ? AND (mode=? OR ((mode=? OR mode IS NULL) AND paused_until IS NOT NULL AND paused_until > ?))", uids[start:end], pauseModeManual, pauseModeTimed, now.UTC()).
			Load(&records)
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			if record != nil {
				active[record.UID] = struct{}{}
			}
		}
	}
	return active, nil
}
