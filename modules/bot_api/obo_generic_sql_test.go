package bot_api

import (
	"context"
	"database/sql"
	"encoding/json"
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
	mock.ExpectQuery("SELECT policy_version,expires_at FROM obo_grants").
		WillReturnRows(sqlmock.NewRows([]string{"policy_version", "expires_at"}).AddRow(2, nil))
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
	mock.ExpectQuery("SELECT policy_version,expires_at FROM obo_grants").
		WillReturnRows(sqlmock.NewRows([]string{"policy_version", "expires_at"}).AddRow(2, nil))
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

func TestGrantorCannotReadGrantWhenBotOwnerRecordDoesNotMatch(t *testing.T) {
	d, mock, closeDB := newSqlmockBotAPIDB(t)
	defer closeDB()
	mock.ExpectQuery("SELECT g.id,g.grantee_bot_uid").
		WillReturnRows(sqlmock.NewRows([]string{"id", "grantee_bot_uid", "mode", "active", "global_enabled", "revoked_at", "expires_at", "policy_version"}))
	ba := &BotAPI{db: d}
	_, err := ba.genericGrantForOwner("human-1", 7)
	require.ErrorIs(t, err, errGenericBotNotOwned)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestEmptyOwnerCannotReadGrant(t *testing.T) {
	d, mock, closeDB := newSqlmockBotAPIDB(t)
	defer closeDB()
	ba := &BotAPI{db: d}
	_, err := ba.genericGrantForOwner("", 7)
	require.ErrorIs(t, err, errGenericBotNotOwned)
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

func TestGrantAuditStateCarriesCommittedPolicyVersion(t *testing.T) {
	state, err := encodeGrantAuditState(map[string]any{"active": 1}, 7, 4, sql.NullTime{})
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(state, &decoded))
	require.Equal(t, float64(4), decoded["policy_version"])
	require.Equal(t, float64(7), decoded["id"])
}

func TestPolicyAuditReadsVersionInsideMutationTransaction(t *testing.T) {
	d, mock, closeDB := newSqlmockBotAPIDB(t)
	defer closeDB()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT policy_version,expires_at FROM obo_grants").
		WillReturnRows(sqlmock.NewRows([]string{"policy_version", "expires_at"}).AddRow(4, nil))
	mock.ExpectExec("INSERT INTO obo_policy_audits").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	tx, err := d.session.Begin()
	require.NoError(t, err)
	require.NoError(t, appendGrantPolicyAudit(tx, 7, "human-1", "pause_grant",
		map[string]any{"active": 1}, map[string]any{"active": 0}))
	require.NoError(t, tx.Commit())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDisableOnlyPutPreservesRevokedGrant(t *testing.T) {
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
			AddRow(7, "auto", 0, 0, fakeTime, nil, 3))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM obo_grant_scope_bindings").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(0))
	mock.ExpectCommit()
	view, err := d.putDelegationAtomic(context.Background(), "human-1", "bot-1", false, false, false, optionalExpiry{})
	require.NoError(t, err)
	require.False(t, view.Active)
	require.Equal(t, int64(3), view.PolicyVersion)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRevokedGrantReauthorizationDoesNotChangeLegacyPersona(t *testing.T) {
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
			AddRow(7, "auto", 0, 0, fakeTime, nil, 3))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM obo_grant_scope_bindings").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(0))
	mock.ExpectExec("revoked_at=CASE WHEN \\?=1 THEN NULL ELSE revoked_at END, expires_at=CASE WHEN \\?=1 THEN \\? ELSE expires_at END").
		WithArgs(1, 1, 1, 0, nil, int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT id,active,global_enabled,policy_version FROM obo_grants").
		WillReturnRows(sqlmock.NewRows([]string{"id", "active", "global_enabled", "policy_version"}))
	mock.ExpectExec("INSERT INTO obo_policy_audits").WillReturnResult(sqlmock.NewResult(3, 1))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT .* FROM obo_scopes").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	view, err := d.putDelegationAtomic(context.Background(), "human-1", "bot-1", true, true, false, optionalExpiry{})
	require.NoError(t, err)
	require.Equal(t, int64(4), view.PolicyVersion)
	require.NoError(t, mock.ExpectationsWereMet())
}
