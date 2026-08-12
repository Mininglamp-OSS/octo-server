package notification

import (
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
)

type dbStore struct {
	session *dbr.Session
}

func newDBStore(ctx *config.Context) *dbStore {
	return &dbStore{session: ctx.DB()}
}

func (s *dbStore) get(uid string) (*pauseRecord, error) {
	var record *pauseRecord
	_, err := s.session.Select("uid", "paused_until", "revision", "updated_at").
		From("user_notification_pause").
		Where("uid=?", uid).
		Load(&record)
	return record, err
}

func (s *dbStore) upsert(uid string, pausedUntil time.Time, now time.Time) (*pauseRecord, error) {
	tx, err := s.session.Begin()
	if err != nil {
		return nil, err
	}
	_, err = tx.InsertBySql(
		"INSERT INTO user_notification_pause (uid, paused_until, revision, updated_at) VALUES (?, ?, 1, ?) "+
			"ON DUPLICATE KEY UPDATE paused_until=VALUES(paused_until), revision=revision+1, updated_at=VALUES(updated_at)",
		uid, pausedUntil.UTC(), now.UTC(),
	).Exec()
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.get(uid)
}

func (s *dbStore) clear(uid string, now time.Time) (*pauseRecord, error) {
	tx, err := s.session.Begin()
	if err != nil {
		return nil, err
	}
	_, err = tx.InsertBySql(
		"INSERT INTO user_notification_pause (uid, paused_until, revision, updated_at) VALUES (?, NULL, 1, ?) "+
			"ON DUPLICATE KEY UPDATE paused_until=NULL, revision=revision+1, updated_at=VALUES(updated_at)",
		uid, now.UTC(),
	).Exec()
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.get(uid)
}

func (s *dbStore) getActiveByUIDs(uids []string, now time.Time) (map[string]struct{}, error) {
	active := make(map[string]struct{})
	if len(uids) == 0 {
		return active, nil
	}
	var records []*pauseRecord
	_, err := s.session.Select("uid", "paused_until", "revision", "updated_at").
		From("user_notification_pause").
		Where("uid IN ? AND paused_until IS NOT NULL AND paused_until > ?", uids, now.UTC()).
		Load(&records)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record != nil {
			active[record.UID] = struct{}{}
		}
	}
	return active, nil
}
