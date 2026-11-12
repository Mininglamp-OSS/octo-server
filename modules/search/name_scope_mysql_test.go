package search

import (
	"context"
	"errors"
	"fmt"
	"github.com/gocraft/dbr/v2"
	"testing"
)

// Extends the guarded, opt-in disposable s14_* schema fixture.
func runNameScopeMySQL(t *testing.T, conn *dbr.Connection) {
	t.Helper()
	exec := func(s string, args ...interface{}) {
		t.Helper()
		if _, e := conn.Exec(s, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec("ALTER TABLE `group` ADD forbidden SMALLINT DEFAULT 0")
	exec("ALTER TABLE group_member ADD role SMALLINT DEFAULT 0, ADD forbidden_expir_time BIGINT DEFAULT 0")
	exec("ALTER TABLE robot ADD creator_uid VARCHAR(40) DEFAULT ''")
	exec("CREATE TABLE thread (short_id VARCHAR(32),group_no VARCHAR(40),name VARCHAR(100),status SMALLINT,message_count BIGINT) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci")
	exec("CREATE TABLE conversation_extra (uid VARCHAR(40),channel_id VARCHAR(40),channel_type SMALLINT)")
	exec("INSERT INTO `group`(group_no,name,status,purpose,space_id) VALUES ('parent','向父群',1,'','A'),('black','向黑名单群',1,'','A'),('muted','向禁言群',1,'','A')")
	exec("INSERT INTO group_member(group_no,uid,is_deleted,status) VALUES ('parent','self',0,1),('black','self',0,2),('muted','self',0,1)")
	exec("UPDATE `group` SET forbidden=1 WHERE group_no='muted'")
	for i := 0; i < 257; i++ {
		exec("INSERT INTO thread VALUES (?,'parent','向子区',1,1)", fmt.Sprintf("t%03d", i))
	}
	exec("INSERT INTO thread VALUES ('pinyin','parent','向晨瑜',1,1),('archived','parent','向归档',2,1),('deleted','parent','向删除',3,1),('empty','parent','向空内容',1,0),('external','joined','向外部子区',1,1),('black','black','向被拉黑',1,1),('gone','gone','向群已解散',1,1),('left','left','向已退出',1,1),('hidden','hidden','向内部容器',1,1),('muted','muted','向禁言',1,1)")
	svc := nameSearchService{load: nameSQLProvider{session: conn.NewSession(nil)}.Load}
	search := func(kind string, q nameQuery) namePage {
		t.Helper()
		if f := q.validate(kind); f != "" {
			t.Fatalf("invalid query %s: %+v", f, q)
		}
		p, e := svc.search(context.Background(), "self", kind, q)
		if e != nil {
			t.Fatal(e)
		}
		return p
	}
	q := nameQuery{Keyword: "向", SpaceID: stringPointer("A"), Entry: "name_selector", Scope: "forward", PageSize: 31}
	first := search("thread", q)
	ids := map[string]bool{}
	for p := first; ; p = search("thread", q) {
		for _, i := range p.Items {
			if ids[i.ID] {
				t.Fatal("duplicate", i.ID)
			}
			ids[i.ID] = true
			if i.Kind != "thread" || i.ParentID == "" {
				t.Fatal("missing identity", i)
			}
		}
		if !p.HasMore {
			break
		}
		q.Cursor = p.NextCursor
	}
	if len(ids) != 260 {
		t.Fatalf("all forward candidates wanted 260, got %d", len(ids))
	}
	for _, id := range []string{"parent____archived", "parent____deleted", "black____black", "muted____muted", "gone____gone", "left____left", "hidden____hidden"} {
		if ids[id] {
			t.Fatal("ineligible", id)
		}
	}
	q.Cursor = first.NextCursor
	exec("UPDATE thread SET status=2 WHERE short_id='t000'")
	if _, e := svc.search(context.Background(), "self", "thread", q); !errors.Is(e, errNameQueryChanged) {
		t.Fatalf("archiving did not invalidate cursor: %v", e)
	}
	q.Cursor = ""
	q.Keyword = "父群"
	if p := search("thread", q); *p.Total != 0 {
		t.Fatal("parent name falsely matched child", p)
	}
	q.Keyword = "xcy"
	if p := search("thread", q); len(p.Items) != 1 || len(p.Items[0].Matches.Original) != 1 || p.Items[0].Matches.Original[0].End != 3 {
		t.Fatal("thread pinyin", p)
	}
	q.Keyword = "向"
	q.Scope = "summary_source"
	q.IncludeArchived = true
	p := search("thread", q)
	if *p.Total != 260 {
		t.Fatalf("summary threads incl archive excluding external/empty expected260: %+v", p)
	}
	q.IncludeArchived = false
	if p := search("thread", q); *p.Total != 258 {
		t.Fatalf("summary active threads: %+v", p)
	}
	q.Scope = "thread_filter"
	q.ChannelID = "parent"
	q.ChannelType = 2
	q.IncludeArchived = true
	if p := search("thread", q); *p.Total != 260 {
		t.Fatalf("thread filter %+v", p)
	}
	exec("UPDATE space SET status=0 WHERE space_id='A'")
	q.SpaceID = nil
	if _, e := svc.search(context.Background(), "self", "thread", q); !errors.Is(e, errNameForbidden) {
		t.Fatalf("disabled parent Space: %v", e)
	}
	exec("UPDATE space SET status=1 WHERE space_id='A'")
	exec("UPDATE group_member SET role=2 WHERE group_no='muted' AND uid='self'")
	q = nameQuery{Keyword: "向禁言", SpaceID: stringPointer("A"), Entry: "name_selector", Scope: "forward"}
	if p := search("thread", q); *p.Total != 1 {
		t.Fatal("manager cannot forward", p)
	}
	exec("UPDATE group_member SET forbidden_expir_time=UNIX_TIMESTAMP()+3600 WHERE group_no='muted' AND uid='self'")
	if p := search("thread", q); *p.Total != 0 {
		t.Fatal("individual mute ignored", p)
	}

	// Summary human roster and existing DM sources retain original/remark fields.
	human := "11111111111111111111111111111111"
	external := "22222222222222222222222222222222"
	exec("INSERT INTO `user`(uid,name,robot) VALUES (?,'向晨瑜',0),(?,'向外部成员',0)", human, external)
	exec("INSERT INTO space_member VALUES ('A',?,1),('B',?,1)", human, external)
	exec("INSERT INTO group_member(group_no,uid,is_deleted,status) VALUES ('parent',?,0,1),('parent',?,0,1)", human, external)
	exec("INSERT INTO friend VALUES ('self',?,'小王',0),('other',?,'不许泄露',0)", human, human)
	exec("INSERT INTO user_setting VALUES ('self',?,'小王'),('other',?,'不许泄露')", human, human)
	exec("INSERT INTO conversation_extra VALUES ('self',?,1),('other',?,1)", human, external)
	q = nameQuery{Keyword: "小王", SpaceID: stringPointer("A"), Entry: "name_selector", Scope: "summary_member"}
	if p := search("contact", q); len(p.Items) != 1 || p.Items[0].OriginalName != "向晨瑜" || len(p.Items[0].Matches.Remark) != 1 {
		t.Fatal("summary remarks", p)
	}
	q.Keyword = "向"
	if p := search("contact", q); *p.Total != 1 {
		t.Fatal("space roster expanded", p)
	}
	q.ChannelID = "parent____pinyin"
	q.ChannelType = 5
	if p := search("contact", q); *p.Total != 2 {
		t.Fatal("external parent member missing", p)
	}

	for _, scope := range []string{"member", "mention"} {
		cq := nameQuery{Keyword: "向", SpaceID: stringPointer("A"), Entry: "name_selector", Scope: scope, ChannelID: "parent", ChannelType: 2}
		if p := search("contact", cq); *p.Total != 2 {
			t.Fatalf("%s lost external selectable member: %+v", scope, p)
		}
	}
	exec("UPDATE group_member SET status=2 WHERE group_no='parent' AND uid=?", external)
	if p := search("contact", q); *p.Total != 1 {
		t.Fatal("blacklisted participant", p)
	}
	q.Scope = "summary_source"
	q.ChannelID = ""
	q.ChannelType = 0
	if p := search("contact", q); *p.Total != 1 || p.Items[0].ID != human {
		t.Fatal("not own existing DM", p)
	}
	q.Keyword = "小王"
	if p := search("contact", q); *p.Total != 1 {
		t.Fatal("DM remark not matched", p)
	}

	exec("INSERT INTO conversation_extra VALUES ('self',?,1)", external)
	q.Keyword = "向"
	q.SpaceID = nil
	if p := search("contact", q); *p.Total != 2 {
		t.Fatal("existing readable no-Space DM source lost", p)
	}
	q.SpaceID = stringPointer("A")
	if p := search("contact", q); *p.Total != 1 {
		t.Fatal("foreign-Space DM source escaped", p)
	}
	q.Keyword = "向"
	q.Scope = "summary_member"
	q.SpaceID = nil
	if _, e := svc.search(context.Background(), "self", "contact", q); !errors.Is(e, errNameForbidden) {
		t.Fatal("unscoped summary roster", e)
	}
	// Physical general_ci can equate s/ß; identity binding must not do so.
	exec("INSERT INTO `user`(uid,name,robot) VALUES ('s','隔离真人',0),('ß','隔离应用',1),('MiXeDÄA','大小写成员',0)")
	exec("INSERT INTO space_member VALUES ('A','s',1),('A','ß',1),('A','mIxEdÄa',1)")
	exec("INSERT INTO app_bot VALUES ('sharp-app','ß','隔离应用','platform','',1)")
	exec("INSERT INTO user_setting VALUES ('self','ß','应用私密备注'),('ß',?,'仅他人的备注')", human)
	iq := nameQuery{Keyword: "隔离", SpaceID: stringPointer("A"), Entry: "name_selector", Scope: "directory"}
	p = search("contact", iq)
	if len(p.Items) != 2 || p.Items[0].ID != "s" || p.Items[0].ContactType != "human" || p.Items[0].RemarkName != "" || p.Items[1].ID != "ß" || p.Items[1].BotKind != "platform_app" {
		t.Fatal("mixed collation bound different identity", p)
	}
	iq.Keyword = "仅他人的备注"
	if p, e := svc.search(context.Background(), "s", "contact", iq); e != nil || len(p.Items) != 0 {
		t.Fatal("another principal's remark leaked", p, e)
	}
	iq.Keyword = "大小写"
	iq.SpaceID = stringPointer("a")
	if p := search("contact", iq); len(p.Items) != 1 || p.Items[0].ID != "MiXeDÄA" {
		t.Fatal("embedded ASCII identity case lost", p)
	}
	// Deleting a current remark must not resurrect friend.remark's stale value.
	exec("UPDATE user_setting SET remark='' WHERE uid='self' AND to_uid=?", human)
	iq.Keyword = "小王"
	iq.SpaceID = stringPointer("A")
	if p := search("contact", iq); len(p.Items) != 0 {
		t.Fatal("stale friend remark resurrected", p)
	}

}
