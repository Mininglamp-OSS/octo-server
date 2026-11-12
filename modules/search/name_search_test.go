package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/namesearch"
)

func TestNameCandidatesEligibility(t *testing.T) {
	rows := []nameContactRow{
		{UID: "self", Robot: confirmedType(0), Member: true},
		{UID: "member", Name: "成员", Robot: confirmedType(0), Member: true},
		{UID: "friend", Name: "好友", Robot: confirmedType(0), Friend: true},
		{UID: "ordinary", Name: "Bot", Robot: confirmedType(1), Member: true, OrdinaryActive: true},
		{UID: "other-bot-friend", Robot: confirmedType(1), Friend: true, OrdinaryActive: true},
		{UID: "platform", Robot: confirmedType(1), AppID: "p", AppScope: "platform", AppStatus: 1},
		{UID: "space-a", Robot: confirmedType(1), AppID: "a", AppScope: "space", AppSpace: "A", AppStatus: 1},
		{UID: "space-b", Robot: confirmedType(1), Member: true, Friend: true, AppID: "b", AppScope: "space", AppSpace: "B", AppStatus: 1},
		{UID: "unpublished", Robot: confirmedType(1), Member: true, Friend: true, AppID: "u", AppScope: "platform", AppStatus: 2},
		{UID: "unknown", Member: true},
	}
	set := eligibleNameContacts(rows, "self", "A")
	ids := []string{}
	for _, c := range set.Candidates {
		ids = append(ids, c.ID)
	}
	if !reflect.DeepEqual(ids, []string{"member", "friend", "ordinary", "platform", "space-a"}) || set.Complete {
		t.Fatalf("got %+v", set)
	}
	set = eligibleNameContacts(rows, "self", "")
	ids = nil
	for _, c := range set.Candidates {
		ids = append(ids, c.ID)
	}
	if !reflect.DeepEqual(ids, []string{"friend", "platform"}) {
		t.Fatalf("no-space: %v", ids)
	}
	rows = []nameContactRow{{UID: "one", Name: "", RealName: "可见实名", Remark: "备注", Robot: confirmedType(0), Member: true}, {UID: "one", Name: "", RealName: "可见实名", Robot: confirmedType(0), Member: true}}
	set = eligibleNameContacts(rows, "self", "A")
	if len(set.Candidates) != 1 || set.Candidates[0].OriginalName != "可见实名" || set.Candidates[0].RemarkName != "备注" {
		t.Fatalf("dedup/name: %+v", set)
	}
}

func TestNamePagination(t *testing.T) {
	set := nameCandidateSet{Complete: true}
	for i := 256; i >= 0; i-- {
		set.Candidates = append(set.Candidates, namesearch.Candidate{ID: fmt.Sprintf("%03d", i), Kind: "contact", ContactType: "human", OriginalName: "向"})
	}
	query := nameQuery{Keyword: "向", Entry: "global", Scope: "directory", ContactType: "all", PageSize: 31}
	query.SpaceID = stringPointer("A")
	all := []string{}
	for {
		page, err := buildNamePage("u", "contact", query, set)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			all = append(all, item.ID)
		}
		if !page.HasMore {
			break
		}
		query.Cursor = page.NextCursor
	}
	if len(all) != 257 || all[0] != "000" || all[256] != "256" {
		t.Fatalf("bad pages: %v", all)
	}
	for i, id := range all {
		if id != fmt.Sprintf("%03d", i) {
			t.Fatalf("item %d = %s", i, id)
		}
	}
	query.Cursor = ""
	page, err := buildNamePage("u", "contact", query, set)
	if err != nil {
		t.Fatal(err)
	}
	query.Cursor = page.NextCursor
	set.Candidates[0].RemarkName = "向"
	if _, err = buildNamePage("u", "contact", query, set); !errors.Is(err, errNameQueryChanged) {
		t.Fatalf("changed query accepted: %v", err)
	}
	set.Candidates[0].RemarkName = ""
	query.SpaceID = stringPointer("B")
	if _, err = buildNamePage("u", "contact", query, set); !errors.Is(err, errNameQueryChanged) {
		t.Fatalf("cross-space cursor: %v", err)
	}
}

func TestNameSearchValidationAndPartial(t *testing.T) {
	base := nameQuery{Keyword: "向", Entry: "global", Scope: "directory", ContactType: "all", PageSize: 50}
	if field := base.validate("contact"); field != "" {
		t.Fatal(field)
	}
	for _, tc := range []struct {
		query nameQuery
		field string
	}{
		{nameQuery{Keyword: "  "}, "keyword"},
		{nameQuery{Keyword: strings.Repeat("😀", 65), Entry: "global"}, "keyword"},
		{nameQuery{Keyword: "向", Entry: "unknown"}, "entry"},
		{nameQuery{Keyword: "向", Entry: "global", Scope: "unknown"}, "scope"},
	} {
		if field := tc.query.validate("contact"); field != tc.field {
			t.Fatalf("field %s, want %s", field, tc.field)
		}
	}
	set := nameCandidateSet{Candidates: []namesearch.Candidate{{ID: "1", Kind: "contact", ContactType: "ai", OriginalName: "向"}}, Complete: false, Reasons: []string{"type_pending"}}
	page, err := buildNamePage("u", "contact", base, set)
	if err != nil || page.Total != nil || page.Complete || len(page.Items) != 1 || page.Items[0].Matches.Original[0] != (namesearch.Range{Start: 0, End: 1}) {
		t.Fatalf("partial: %+v %v", page, err)
	}
	base.ContactType = "human"
	page, err = buildNamePage("u", "contact", base, set)
	if err != nil || len(page.Items) != 0 || page.Complete {
		t.Fatalf("filtered partial: %+v %v", page, err)
	}
}

func TestNameSearchDoesNotSwallowSourceFailure(t *testing.T) {
	fail := errors.New("source unavailable")
	svc := nameSearchService{load: func(context.Context, string, string, nameQuery) (nameCandidateSet, error) {
		return nameCandidateSet{}, fail
	}}
	_, err := svc.search(context.Background(), "u", "contact", nameQuery{Keyword: "向"})
	if !errors.Is(err, fail) {
		t.Fatalf("source failure turned into success: %v", err)
	}
}

func stringPointer(s string) *string      { return &s }
func confirmedType(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

func TestNameSelectableSubsetBeforePagination(t *testing.T) {
	set := nameCandidateSet{Complete: true}
	for i := 0; i < 257; i++ {
		set.Candidates = append(set.Candidates, namesearch.Candidate{ID: fmt.Sprint(i), Kind: "contact", ContactType: "human", OriginalName: "向"})
	}
	allowed := []string{"256", "255", "254"}
	q := nameQuery{Keyword: "向", Entry: "name_selector", Scope: "directory", ContactType: "all", PageSize: 2, AllowedIDs: &allowed}
	page, err := buildNamePage("u", "contact", q, set)
	if err != nil || len(page.Items) != 2 || page.Items[0].ID != "254" || page.Items[1].ID != "255" || page.Total == nil || *page.Total != 3 || !page.HasMore {
		t.Fatalf("subset was applied after paging: %+v %v", page, err)
	}
	q.Cursor = page.NextCursor
	page, err = buildNamePage("u", "contact", q, set)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "256" || page.HasMore {
		t.Fatalf("subset next page: %+v %v", page, err)
	}
	empty := []string{}
	q.Cursor = ""
	q.AllowedIDs = &empty
	page, err = buildNamePage("u", "contact", q, set)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("empty allowed set expanded to all: %+v %v", page, err)
	}
	incomplete:=false;q.AllowedIDs=&allowed;q.AllowedIDsComplete=&incomplete
	page,err=buildNamePage("u","contact",q,set)
	if err!=nil||page.Complete||page.Total!=nil||len(page.IncompleteReasons)!=1||page.IncompleteReasons[0]!="selection_pending"{t.Fatalf("incomplete selection counted as complete: %+v %v",page,err)}
}
