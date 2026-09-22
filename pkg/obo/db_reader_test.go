package obo

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
)

func snapshotMock(t *testing.T) (DBSnapshotReader, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conn := &dbr.Connection{DB: db, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	return DBSnapshotReader{Session: conn.NewSession(nil)}, mock
}

func expectBotAndSpace(mock sqlmock.Sqlmock) {
	mock.ExpectBegin().WillReturnError(nil)
	mock.ExpectQuery("SELECT robot_id, COALESCE\\(creator_uid.*BINARY bot_token=BINARY").
		WillReturnRows(sqlmock.NewRows([]string{"robot_id", "creator_uid"}).AddRow("bot-1", "human-1"))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM user WHERE uid=").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM space WHERE space_id=").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM space_member WHERE space_id=").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(1))
}

func TestDBSnapshotAsBotDoesNotReadGrant(t *testing.T) {
	reader, mock := snapshotMock(t)
	expectBotAndSpace(mock)
	mock.ExpectCommit()
	state, err := reader.Read(context.Background(), "bf_token", "S", ModeAsBot)
	if err != nil || state.BotUID != "bot-1" || state.GrantID != 0 || state.OwnerUID != "" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDBSnapshotOBORequiresOwnerGrantAndALL(t *testing.T) {
	reader, mock := snapshotMock(t)
	expectBotAndSpace(mock)
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM user WHERE uid=").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM space_member WHERE space_id=").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(1))
	mock.ExpectQuery("SELECT id, policy_version FROM obo_grants.*UTC_TIMESTAMP\\(6\\)").
		WillReturnRows(sqlmock.NewRows([]string{"id", "policy_version"}).AddRow(23, 4))
	mock.ExpectQuery("SELECT scope_code FROM obo_grant_scope_bindings").
		WillReturnRows(sqlmock.NewRows([]string{"scope_code"}).AddRow("ALL"))
	mock.ExpectCommit()
	state, err := reader.Read(context.Background(), "bf_token", "S", ModeOBO)
	if err != nil || state.BotUID != "bot-1" || state.OwnerUID != "human-1" || state.GrantID != 23 || state.PolicyVersion != 4 || len(state.BoundScopes) != 1 || state.BoundScopes[0] != "ALL" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDBSnapshotSpaceDeniedBeforeGrant(t *testing.T) {
	reader, mock := snapshotMock(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT robot_id, COALESCE\\(creator_uid").
		WillReturnRows(sqlmock.NewRows([]string{"robot_id", "creator_uid"}).AddRow("bot-1", "human-1"))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM user WHERE uid=").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM space WHERE space_id=").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM space_member WHERE space_id=").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(0))
	mock.ExpectRollback()
	_, err := reader.Read(context.Background(), "bf_token", "S", ModeOBO)
	if DecisionCode(err) != "space_not_allowed" {
		t.Fatalf("expected Space denial, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDBSnapshotOBOGrantRevocationDenies(t *testing.T) {
	reader, mock := snapshotMock(t)
	expectBotAndSpace(mock)
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM user WHERE uid=").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM space_member WHERE space_id=").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(1))
	// The SQL filters out inactive, revoked and expired Grants; a missing row
	// must be a denial, not a fallback to the Bot identity.
	mock.ExpectQuery("SELECT id, policy_version FROM obo_grants.*UTC_TIMESTAMP\\(6\\)").
		WillReturnRows(sqlmock.NewRows([]string{"id", "policy_version"}))
	mock.ExpectRollback()
	_, err := reader.Read(context.Background(), "bf_token", "S", ModeOBO)
	if DecisionCode(err) != "delegation_denied" {
		t.Fatalf("expected delegation denial, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDBSnapshotOBOWithoutALLDoesNotAuthorize(t *testing.T) {
	reader, mock := snapshotMock(t)
	expectBotAndSpace(mock)
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM user WHERE uid=").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM space_member WHERE space_id=").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(1))
	mock.ExpectQuery("SELECT id, policy_version FROM obo_grants.*UTC_TIMESTAMP\\(6\\)").
		WillReturnRows(sqlmock.NewRows([]string{"id", "policy_version"}).AddRow(23, 4))
	mock.ExpectQuery("SELECT scope_code FROM obo_grant_scope_bindings").
		WillReturnRows(sqlmock.NewRows([]string{"scope_code"}))
	mock.ExpectCommit()
	state, err := reader.Read(context.Background(), "bf_token", "S", ModeOBO)
	if err != nil || len(state.BoundScopes) != 0 {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDBSnapshotOBORejectsOwnerWithoutSpaceSeat(t *testing.T) {
	reader, mock := snapshotMock(t)
	expectBotAndSpace(mock)
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM user WHERE uid=").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM space_member WHERE space_id=").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(0))
	mock.ExpectRollback()
	_, err := reader.Read(context.Background(), "bf_token", "S", ModeOBO)
	if DecisionCode(err) != "space_not_allowed" {
		t.Fatalf("expected owner Space denial, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
