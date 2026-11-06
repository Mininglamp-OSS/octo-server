package search

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
)

// This opt-in test uses a dedicated disposable schema, never an application DB.
func TestNameMySQLIntegration(t *testing.T) {
	dsn := os.Getenv("S14_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set S14_MYSQL_DSN to a dedicated s14_* database")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil || !strings.HasPrefix(cfg.DBName, "s14_") {
		t.Fatal("refusing to change a database without the s14_ prefix")
	}
	conn, err := dbr.Open("mysql", dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(2)
	defer conn.Close()
	for _, table := range []string{"thread", "conversation_extra", "user", "space", "space_member", "friend", "user_setting", "user_verification", "app_bot", "robot", "group", "group_member", "group_setting"} {
		if _, err = conn.Exec("DROP TABLE IF EXISTS `" + table + "`"); err != nil {
			t.Fatal(err)
		}
	}
	// Column names/types mirror the owning modules' migrations; queries execute
	// on real MySQL, independently of sqlmock interpolation expectations.
	for _, ddl := range []string{
		"CREATE TABLE `user` (uid VARCHAR(40) PRIMARY KEY,name VARCHAR(100),robot SMALLINT,status SMALLINT DEFAULT 1,is_destroy SMALLINT DEFAULT 0)",
		"CREATE TABLE space (space_id VARCHAR(40) PRIMARY KEY,status SMALLINT)",
		"CREATE TABLE space_member (space_id VARCHAR(40),uid VARCHAR(40),status SMALLINT)",
		"CREATE TABLE friend (uid VARCHAR(40),to_uid VARCHAR(40),remark VARCHAR(100),is_deleted SMALLINT)",
		"CREATE TABLE user_setting (uid VARCHAR(40),to_uid VARCHAR(40),remark VARCHAR(100))",
		"CREATE TABLE user_verification (user_id VARCHAR(40) PRIMARY KEY,real_name VARCHAR(128))",
		"CREATE TABLE app_bot (id VARCHAR(40) PRIMARY KEY,uid VARCHAR(40),display_name VARCHAR(100),scope VARCHAR(20),space_id VARCHAR(40),status SMALLINT)",
		"CREATE TABLE robot (robot_id VARCHAR(40) PRIMARY KEY,status SMALLINT)",
		"CREATE TABLE `group` (group_no VARCHAR(40) PRIMARY KEY,name VARCHAR(100),status SMALLINT,purpose VARCHAR(32) DEFAULT '',space_id VARCHAR(40))",
		"CREATE TABLE group_member (group_no VARCHAR(40),uid VARCHAR(40),is_deleted SMALLINT,status SMALLINT)",
		"CREATE TABLE group_setting (group_no VARCHAR(40),uid VARCHAR(40),remark VARCHAR(100))",
	} {
		if _, err = conn.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(query string, args ...interface{}) {
		t.Helper()
		if _, err := conn.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO space VALUES ('A',1),('B',1)")
	exec("INSERT INTO `user` (uid,name,robot) VALUES ('self','向自己',0),('friend','刘向东',0),('ordinary','向普通Bot',1),('foreign','向外部Bot',1),('space-app','向其他空间应用',1),('unpublished','向未发布应用',1),('removed','向失效成员',0)")
	exec("INSERT INTO space_member VALUES ('A','self',1),('B','self',1),('A','ordinary',1),('B','foreign',1),('A','space-app',1),('A','unpublished',1),('A','removed',0)")
	exec("INSERT INTO robot VALUES ('ordinary',1),('foreign',1)")
	exec("INSERT INTO friend VALUES ('self','friend','自己的备注',0),('self','foreign','外部好友Bot',0),('self','space-app','跨空间App好友',0),('self','unpublished','未发布好友',0),('other','friend','其他人的私密备注',0)")
	exec("INSERT INTO user_setting VALUES ('self','friend','自己的备注'),('other','friend','其他人的私密备注')")
	exec("INSERT INTO app_bot VALUES ('s','space-app','向其他空间应用','space','B',1),('u','unpublished','向未发布应用','platform','',2)")
	for i := 0; i < 257; i++ {
		id := fmt.Sprintf("person%03d", i)
		exec("INSERT INTO `user`(uid,name,robot) VALUES (?, '向成员',0)", id)
		exec("INSERT INTO space_member VALUES ('A',?,1)", id)
	}
	for i := 0; i < 105; i++ {
		id := fmt.Sprintf("app%03d", i)
		exec("INSERT INTO `user`(uid,name,robot) VALUES (?,'向平台应用',1)", id)
		exec("INSERT INTO app_bot VALUES (?,?,'向平台应用','platform','',1)", id, id)
	}
	exec("INSERT INTO `group` VALUES ('joined','向已加入群',1,'','B'),('saved','向只收藏群',1,'','A'),('gone','向解散群',2,'','A'),('left','向退出群',1,'','A'),('hidden','向内部容器',1,'ai_team','A')")
	exec("INSERT INTO group_member VALUES ('joined','self',0,1),('gone','self',0,1),('left','self',1,1),('hidden','self',0,1)")
	exec("INSERT INTO group_setting VALUES ('joined','self','我的向群备注'),('joined','other','别人的群备注'),('saved','self','只收藏')")
	// Legacy tables inherit MySQL8 default 0900; these migrations explicitly pin general_ci.
	for _, table := range []string{"app_bot", "user_verification", "user_setting"} {
		exec("ALTER TABLE `" + table + "` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	}
	provider := nameSQLProvider{session: conn.NewSession(nil)}
	svc := nameSearchService{load: provider.Load}
	q := nameQuery{Keyword: "向", SpaceID: stringPointer("A"), Entry: "global", Scope: "directory", ContactType: "all", PageSize: 37}
	var ids []string
	var first namePage
	for {
		page, err := svc.search(context.Background(), "self", "contact", q)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) == 0 {
			first = page
		}
		for _, item := range page.Items {
			ids = append(ids, item.ID)
		}
		if !page.HasMore {
			break
		}
		q.Cursor = page.NextCursor
	}
	if len(ids) != 364 || first.Total == nil || *first.Total != 364 {
		t.Fatalf("wanted friend+257 members+ordinary+105 apps: %d, %+v", len(ids), first)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatal("duplicate", id)
		}
		seen[id] = true
	}
	for _, excluded := range []string{"self", "foreign", "space-app", "unpublished", "removed"} {
		if seen[excluded] {
			t.Fatal("ineligible", excluded)
		}
	}
	q.Cursor = first.NextCursor
	exec("UPDATE user_setting SET remark='向' WHERE uid='self' AND to_uid='friend'")
	if _, err = svc.search(context.Background(), "self", "contact", q); !errors.Is(err, errNameQueryChanged) {
		t.Fatalf("changed remark did not invalidate: %v", err)
	}
	q.Cursor = ""
	q.SpaceID = nil
	page, err := svc.search(context.Background(), "self", "contact", q)
	if err != nil || page.Total == nil || *page.Total != 106 {
		t.Fatalf("no Space %+v %v", page, err)
	}
	q.SpaceID = stringPointer("A")
	groups, err := svc.search(context.Background(), "self", "group", q)
	if err != nil || len(groups.Items) != 1 || groups.Items[0].ID != "joined" || groups.Items[0].RemarkName != "我的向群备注" {
		t.Fatalf("groups %+v %v", groups, err)
	}

	// Space removal precedes asynchronous group cleanup. Old group rows must not grant access.
	for _, scope := range []string{"member", "mention"} {
		scoped := q
		scoped.Scope, scoped.ChannelID, scoped.ChannelType = scope, "joined", 2
		if _, err = svc.search(context.Background(), "self", "contact", scoped); err != nil {
			t.Fatalf("active external group: %v", err)
		}
		exec("UPDATE space SET status=0 WHERE space_id='B'")
		for _, space := range []*string{stringPointer("A"), nil} {
			scoped.SpaceID = space
			if _, err = svc.search(context.Background(), "self", "contact", scoped); !errors.Is(err, errNameForbidden) {
				t.Fatalf("%s disabled group Space: %v", scope, err)
			}
		}
		exec("UPDATE space SET status=1 WHERE space_id='B'")
		exec("UPDATE `group` SET space_id='' WHERE group_no='joined'")
		if _, err = svc.search(context.Background(), "self", "contact", scoped); err != nil {
			t.Fatalf("group without Space: %v", err)
		}
		exec("UPDATE `group` SET space_id='B' WHERE group_no='joined'")
	}
	runNameScopeMySQL(t, conn)
	exec("UPDATE space_member SET status=0 WHERE uid='self' AND space_id='A'")
	if _, err = svc.search(context.Background(), "self", "contact", q); !errors.Is(err, errNameForbidden) {
		t.Fatalf("removed caller: %v", err)
	}
}
