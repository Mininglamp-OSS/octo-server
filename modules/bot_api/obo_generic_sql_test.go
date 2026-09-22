package bot_api

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// A repeated one-shot management request must not bump policy_version or
// append an audit row. The mock intentionally expects no write statement.
func TestPutDelegationAtomicIdempotent(t *testing.T) {
	d, mock, closeDB := newSqlmockBotAPIDB(t)
	defer closeDB()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT uid FROM user WHERE uid=").
		WillReturnRows(sqlmock.NewRows([]string{"uid"}).AddRow("human-1"))
	mock.ExpectQuery("SELECT COALESCE\\(creator_uid").
		WillReturnRows(sqlmock.NewRows([]string{"COALESCE(creator_uid,'')"}).AddRow("human-1"))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM user WHERE uid=").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(1))
	mock.ExpectQuery("SELECT id, mode, active, global_enabled, revoked_at, expires_at, policy_version FROM obo_grants").
		WillReturnRows(sqlmock.NewRows([]string{"id", "mode", "active", "global_enabled", "revoked_at", "expires_at", "policy_version"}).
			AddRow(7, "auto", 0, 0, nil, nil, 3))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM obo_grant_scope_bindings").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(0))
	mock.ExpectCommit()
	view, err := d.putDelegationAtomic(context.Background(), "human-1", "bot-1", false, false, false, optionalExpiry{})
	require.NoError(t, err)
	require.Equal(t, int64(3), view.PolicyVersion)
	require.Empty(t, view.ScopeCodes)
	require.NoError(t, mock.ExpectationsWereMet())
}

func legacyGrantRows(mode string) *sqlmock.Rows {
	return sqlmock.NewRows(grantRowCols()).AddRow(
		int64(7), "human-1", "bot-1", mode, 0, 1,
		fakeTime, fakeTime, sql.NullTime{}, "",
	)
}

func TestLegacyGrantUpdateVersionsAndAuditsAtomically(t *testing.T) {
	d, mock, closeDB := newSqlmockBotAPIDB(t)
	defer closeDB()
	mock.ExpectQuery("COALESCE\\(persona_prompt").WillReturnRows(legacyGrantRows("draft"))
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT 1 FROM `user` WHERE uid=").
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectQuery("COALESCE\\(persona_prompt").WillReturnRows(legacyGrantRows("draft"))
	mock.ExpectExec("UPDATE .*obo_grants").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE obo_grants SET policy_version").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO obo_policy_audits").WillReturnResult(sqlmock.NewResult(3, 1))
	mock.ExpectCommit()
	require.NoError(t, d.updateGrant(7, "auto", nil, nil))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLegacyGrantRevocationVersionsAndAuditsAtomically(t *testing.T) {
	d, mock, closeDB := newSqlmockBotAPIDB(t)
	defer closeDB()
	mock.ExpectQuery("COALESCE\\(persona_prompt").WillReturnRows(legacyGrantRows("auto"))
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT 1 FROM `user` WHERE uid=").
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectQuery("COALESCE\\(persona_prompt").WillReturnRows(legacyGrantRows("auto"))
	mock.ExpectExec("UPDATE .*obo_grants").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE obo_grants SET policy_version").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO obo_policy_audits").WillReturnResult(sqlmock.NewResult(3, 1))
	mock.ExpectCommit()
	require.NoError(t, d.revokeGrant(7))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSetGenericBindingIdempotent(t *testing.T) {
	d, mock, closeDB := newSqlmockBotAPIDB(t)
	defer closeDB()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT uid FROM user WHERE uid=").
		WillReturnRows(sqlmock.NewRows([]string{"uid"}).AddRow("human-1"))
	mock.ExpectQuery("SELECT id,grantee_bot_uid,mode,active,global_enabled,revoked_at,expires_at,policy_version FROM obo_grants").
		WillReturnRows(sqlmock.NewRows([]string{"id", "grantee_bot_uid", "mode", "active", "global_enabled", "revoked_at", "expires_at", "policy_version"}).
			AddRow(7, "bot-1", "auto", 1, 1, nil, nil, 3))
	mock.ExpectQuery("SELECT COALESCE\\(creator_uid").
		WillReturnRows(sqlmock.NewRows([]string{"COALESCE(creator_uid,'')"}).AddRow("human-1"))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM obo_grant_scope_bindings").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(1))
	mock.ExpectCommit()
	view, err := d.setGenericBinding(context.Background(), "human-1", 7, true)
	require.NoError(t, err)
	require.Equal(t, int64(3), view.PolicyVersion)
	require.Equal(t, []string{"ALL"}, view.ScopeCodes)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGrantorCanReadHistoricalGrantAfterBotOwnershipChanges(t *testing.T) {
	d, mock, closeDB := newSqlmockBotAPIDB(t)
	defer closeDB()
	mock.ExpectQuery("SELECT id,grantee_bot_uid,mode,active,global_enabled,revoked_at,expires_at,policy_version FROM obo_grants").
		WillReturnRows(sqlmock.NewRows([]string{"id", "grantee_bot_uid", "mode", "active", "global_enabled", "revoked_at", "expires_at", "policy_version"}).
			AddRow(7, "bot-1", "auto", 0, 0, fakeTime, nil, 4))
	ba := &BotAPI{db: d}
	grant, err := ba.genericGrantForOwner("human-1", 7)
	require.NoError(t, err)
	require.Equal(t, int64(7), grant.ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPutDelegationAtomicCreatesGrantAndBindingTogether(t *testing.T) {
	d, mock, closeDB := newSqlmockBotAPIDB(t)
	defer closeDB()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT uid FROM user WHERE uid=").
		WillReturnRows(sqlmock.NewRows([]string{"uid"}).AddRow("human-1"))
	mock.ExpectQuery("SELECT COALESCE\\(creator_uid").
		WillReturnRows(sqlmock.NewRows([]string{"COALESCE(creator_uid,'')"}).AddRow("human-1"))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM user WHERE uid=").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(1))
	mock.ExpectQuery("SELECT id, mode, active, global_enabled, revoked_at, expires_at, policy_version FROM obo_grants").
		WillReturnRows(sqlmock.NewRows([]string{"id", "mode", "active", "global_enabled", "revoked_at", "expires_at", "policy_version"}))
	deadline := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	mock.ExpectExec("INSERT INTO obo_grants").WithArgs("human-1", "bot-1", 0, 0, "2026-10-01 04:00:00").
		WillReturnResult(sqlmock.NewResult(7, 1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM obo_grant_scope_bindings").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(0))
	mock.ExpectExec("INSERT INTO obo_grant_scope_bindings").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO obo_policy_audits").
		WillReturnResult(sqlmock.NewResult(11, 1))
	mock.ExpectCommit()
	view, err := d.putDelegationAtomic(context.Background(), "human-1", "bot-1", false, false, true,
		optionalExpiry{Set: true, Value: &deadline})
	require.NoError(t, err)
	require.Equal(t, int64(7), view.GrantID)
	require.Equal(t, int64(1), view.PolicyVersion)
	require.Equal(t, []string{"ALL"}, view.ScopeCodes)
	require.Equal(t, deadline, *view.ExpiresAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPutDelegationAtomicRollsBackWhenAuditFails(t *testing.T) {
	d, mock, closeDB := newSqlmockBotAPIDB(t)
	defer closeDB()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT uid FROM user WHERE uid=").
		WillReturnRows(sqlmock.NewRows([]string{"uid"}).AddRow("human-1"))
	mock.ExpectQuery("SELECT COALESCE\\(creator_uid").
		WillReturnRows(sqlmock.NewRows([]string{"COALESCE(creator_uid,'')"}).AddRow("human-1"))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM user WHERE uid=").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(1))
	mock.ExpectQuery("SELECT id, mode, active, global_enabled, revoked_at, expires_at, policy_version FROM obo_grants").
		WillReturnRows(sqlmock.NewRows([]string{"id", "mode", "active", "global_enabled", "revoked_at", "expires_at", "policy_version"}))
	mock.ExpectExec("INSERT INTO obo_grants").WillReturnResult(sqlmock.NewResult(7, 1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM obo_grant_scope_bindings").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(0))
	mock.ExpectExec("INSERT INTO obo_policy_audits").WillReturnError(errors.New("audit unavailable"))
	mock.ExpectRollback()
	_, err := d.putDelegationAtomic(context.Background(), "human-1", "bot-1", false, false, false, optionalExpiry{})
	require.ErrorContains(t, err, "audit Grant change")
	require.NoError(t, mock.ExpectationsWereMet())
}
