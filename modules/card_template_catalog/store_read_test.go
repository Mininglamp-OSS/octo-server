package card_template_catalog

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

func TestStoreGetTemplateDetailReturnsSourceAndActivationWithoutBundle(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT active_version, status, revision")).
		WithArgs("test.runtime-card").
		WillReturnRows(sqlmock.NewRows([]string{"active_version", "status", "revision"}).
			AddRow("1.1.0", "active", 4))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.version, c.source, a.owner, a.visibility, a.engine_contract")).
		WithArgs("test.runtime-card", 101).
		WillReturnRows(sqlmock.NewRows([]string{
			"version", "source", "owner", "visibility", "engine_contract", "protocol",
			"contract_version", "content_sha256", "blocked_at", "created_at",
		}).
			AddRow("1.0.0", "static", nil, nil, nil, nil, nil, nil, nil, now).
			AddRow("1.1.0", "dynamic", "ai", "private", cardtmpl.JSONTemplateEngineV1,
				cardtmpl.Protocol, "1.0.0", "abc", now, now))

	detail, err := store.GetTemplateDetail(context.Background(), "test.runtime-card", 100)
	if err != nil {
		t.Fatalf("GetTemplateDetail: %v", err)
	}
	if detail.Activation.Version != "1.1.0" || detail.Activation.Revision != 4 || len(detail.Versions) != 2 {
		t.Fatalf("detail = %+v", detail)
	}
	if detail.Versions[0].Source != cardtmpl.RuntimeSourceStatic || detail.Versions[1].Source != cardtmpl.RuntimeSourceDynamic ||
		!detail.Versions[1].Blocked {
		t.Fatalf("versions = %+v", detail.Versions)
	}
	assertMockExpectations(t, mock)
}

func TestStoreListAuditUsesDescendingCursorAndHardLimit(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, actor_uid, operation, version, previous_version, resulting_version")).
		WithArgs("test.runtime-card", uint64(50), uint64(50), 3).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "actor_uid", "operation", "version", "previous_version", "resulting_version",
			"previous_revision", "resulting_revision",
			"principal_type", "principal_id", "scope_space_id",
			"previous_permissions", "resulting_permissions",
			"result", "reason", "change_ticket", "created_at",
		}).
			AddRow(49, "admin-1", "rollback", "1.0.0", "1.1.0", "1.0.0", 3, 4, "", "", "", "", "", "ok", "incident", "INC-1", now).
			AddRow(48, "admin-1", "grant", "", "", "", 0, 1, "bot", "bot-1", "space-1", "", "discover,send", "ok", "pilot", "CHG-2", now).
			AddRow(47, "admin-1", "publish", "1.1.0", "", "", nil, nil, "", "", "", "", "", "created", "reviewed", "CHG-1", now))

	page, err := store.ListAudit(context.Background(), "test.runtime-card", 50, 2)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(page.Entries) != 2 || page.NextCursor != 48 {
		t.Fatalf("page = %+v", page)
	}
	assertMockExpectations(t, mock)
}
