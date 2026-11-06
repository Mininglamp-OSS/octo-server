package search

import (
	"context"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"testing"
)

func TestNameScopeValidation(t *testing.T) {
	for _, tc := range []struct {
		kind, scope string
		channel     int
		valid       bool
	}{
		{"thread", "directory", 0, false}, {"thread", "forward", 0, true}, {"thread", "thread_filter", 2, true},
		{"thread", "summary_source", 0, true}, {"group", "summary_source", 0, true},
		{"contact", "summary_source", 0, true}, {"contact", "summary_member", 0, true},
		{"group", "summary_member", 0, false}, {"contact", "forward", 0, false},
	} {
		q := nameQuery{Keyword: "向", Entry: "name_selector", Scope: tc.scope}
		if tc.channel != 0 {
			q.ChannelID = "g"
			q.ChannelType = tc.channel
		}
		if (q.validate(tc.kind) == "") != tc.valid {
			t.Errorf("%+v: rejected %s", tc, q.validate(tc.kind))
		}
	}
}

func TestNameScopeThreadProvider(t *testing.T) {
	p, m := nameProviderDB(t)
	m.ExpectBegin()
	m.ExpectQuery("(?s)SELECT.*thread.*group_member.*gm.status=1.*g.status=1.*g.purpose='' .*g.space_id.*t.status=1.*g.forbidden.*forbidden_expir_time").WillReturnRows(sqlmock.NewRows([]string{"id", "original_name", "parent_id", "archived"}).AddRow("g____s", "向晨瑜", "g", false))
	m.ExpectCommit()
	set, err := p.Load(context.Background(), "u", "thread", nameQuery{Scope: "forward"})
	if err != nil || len(set.Candidates) != 1 || set.Candidates[0].Kind != "thread" || set.Candidates[0].ParentID != "g" {
		t.Fatalf("%+v %v", set, err)
	}
	if err = m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestNameScopeSummaryRequiresSpace(t *testing.T) {
	p, _ := nameProviderDB(t)
	_, err := p.Load(context.Background(), "u", "contact", nameQuery{Scope: "summary_member"})
	if !errors.Is(err, errNameForbidden) {
		t.Fatalf("expected no-Space denied: %v", err)
	}
}

func TestNameScopeRejectsMissingIncompleteRoster(t *testing.T) {
	incomplete := false
	q := nameQuery{Keyword: "向", Entry: "name_selector", AllowedIDsComplete: &incomplete}
	if q.validate("contact") != "allowed_ids" {
		t.Fatal("missing incomplete roster accepted")
	}
}
