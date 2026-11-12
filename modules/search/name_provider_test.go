package search

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
)

func nameProviderDB(t *testing.T) (nameSQLProvider, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	conn := &dbr.Connection{DB: db, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	return nameSQLProvider{session: conn.NewSession(nil)}, mock
}

func TestNameSQLProviderMembershipAndFailure(t *testing.T) {
	p, mock := nameProviderDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery("(?s)SELECT COUNT.*space_member.*space.*status=1").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectRollback()
	_, err := p.Load(context.Background(), "u", "contact", nameQuery{SpaceID: stringPointer("A"), Scope: "directory"})
	if !errors.Is(err, errNameForbidden) {
		t.Fatalf("nonmember: %v", err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	p, mock = nameProviderDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery("(?s)SELECT.*FROM.*user.*u.*status=1.*is_destroy<>2").WillReturnError(errors.New("database down"))
	mock.ExpectRollback()
	if _, err = p.Load(context.Background(), "u", "contact", nameQuery{Scope: "directory"}); err == nil {
		t.Fatal("database failure became empty result")
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestNameSQLProviderContacts(t *testing.T) {
	p, mock := nameProviderDB(t)
	mock.ExpectBegin()
	// Exercise the actual dbr query and row decoder, not a mock search method.
	mock.ExpectQuery("(?s)SELECT.*user_verification.*friend.*app_bot.*robot.*u.status=1.*u.is_destroy<>2").WillReturnRows(
		sqlmock.NewRows([]string{"uid", "name", "real_name", "remark", "robot", "is_member", "friend", "ordinary_active", "app_id", "app_scope", "app_space", "app_status", "app_name"}).
			AddRow("human", "原名", "隐藏实名", "我的备注", 0, false, true, false, "", "", "", 0, "").
			AddRow("app", "", "", "", 1, false, false, false, "p", "platform", "", 1, "平台应用"))
	mock.ExpectCommit()
	set, err := p.Load(context.Background(), "u", "contact", nameQuery{Scope: "directory"})
	if err != nil || !set.Complete || len(set.Candidates) != 2 || set.Candidates[0].OriginalName != "原名" || set.Candidates[0].RemarkName != "我的备注" || set.Candidates[1].BotKind != "platform_app" {
		t.Fatalf("got %+v %v", set, err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestNameSQLProviderGroups(t *testing.T) {
	p, mock := nameProviderDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery("(?s)SELECT.*group_setting.*gm.uid.*gm.is_deleted=0.*gm.status=1.*g.status=1.*g.purpose=''").WillReturnRows(sqlmock.NewRows([]string{"id", "original_name", "remark_name"}).AddRow("g", "原群名", "我的群备注"))
	mock.ExpectCommit()
	set, err := p.Load(context.Background(), "u", "group", nameQuery{Scope: "directory"})
	if err != nil || len(set.Candidates) != 1 || set.Candidates[0].ID != "g" || set.Candidates[0].RemarkName != "我的群备注" {
		t.Fatalf("got %+v %v", set, err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
