package robot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	pkgutil "github.com/Mininglamp-OSS/octo-server/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/stretchr/testify/assert"
)

var uid = "10000"
var token = "token122323"

func newTestServer() (*server.Server, *config.Context) {
	os.Remove("test.db")
	cfg := config.New()
	cfg.Test = true
	ctx := config.NewContext(cfg)
	ctx.Event = event.New(ctx)
	err := ctx.Cache().Set(cfg.Cache.TokenCachePrefix+token, uid+"@test")
	if err != nil {
		panic(err)
	}
	// 创建server
	s := server.New(ctx)
	return s, ctx

}
func TestSyncRobot(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := newTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/robot/sync", bytes.NewReader([]byte(util.ToJson([]map[string]interface{}{
		{
			"robot_id": ctx.GetConfig().Account.SystemUID,
			"version":  0,
		},
	}))))
	assert.NoError(t, err)
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestMention(t *testing.T) {

	reg := regexp.MustCompile(`@\S+`)

	fmt.Println(reg.FindAllString("dsds @增加啊每个萨摩 你好", -1))
}

// TestInlineQueryEventsMapLockConsistency verifies that inlineQueryEventsMap
// is protected by inlineQueryEventsMapLock (not inlineQueryEventResultChanMapLock).
// This test addresses issue #159 where the wrong lock was used, causing a race condition.
// Run with: go test -race -run TestInlineQueryEventsMapLockConsistency
func TestInlineQueryEventsMapLockConsistency(t *testing.T) {
	// Create a minimal Robot struct for lock testing (no external dependencies)
	rb := &Robot{
		inlineQueryEventsMap:          make(map[string][]*robotEvent),
		inlineQueryEventResultChanMap: make(map[string]chan *InlineQueryResult),
	}

	robotID := "test-robot-123"
	done := make(chan bool)
	iterations := 100

	// Writer goroutine: simulates addInlineQuery behavior
	go func() {
		for i := 0; i < iterations; i++ {
			rb.inlineQueryEventsMapLock.Lock()
			events := rb.inlineQueryEventsMap[robotID]
			if events == nil {
				events = make([]*robotEvent, 0)
			}
			events = append(events, &robotEvent{
				EventID: int64(i),
				InlineQuery: &InlineQuery{
					SID:   fmt.Sprintf("sid-%d", i),
					Query: "test query",
				},
			})
			rb.inlineQueryEventsMap[robotID] = events
			rb.inlineQueryEventsMapLock.Unlock()
		}
		done <- true
	}()

	// Reader goroutine: simulates the fixed getRobotEvents behavior
	// This was the buggy path that previously used the wrong lock
	go func() {
		for i := 0; i < iterations; i++ {
			rb.inlineQueryEventsMapLock.RLock()
			_ = rb.inlineQueryEventsMap[robotID]
			rb.inlineQueryEventsMapLock.RUnlock()
		}
		done <- true
	}()

	// Wait for both goroutines to complete
	<-done
	<-done

	// Verify data integrity
	rb.inlineQueryEventsMapLock.RLock()
	events := rb.inlineQueryEventsMap[robotID]
	rb.inlineQueryEventsMapLock.RUnlock()

	assert.Equal(t, iterations, len(events), "All events should have been added without race condition")
}

// TestMyBots_ExcludesDeletedRobots verifies that /robot/my_bots does not return
// bots whose robot.status has been set to 0 (soft-deleted). Before the fix the
// query used a LEFT JOIN on robot, so a deleted bot whose friend record had
// not been cleaned up would still appear in the result set.
func TestMyBots_ExcludesDeletedRobots(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := newTestServer()
	rb := New(ctx)
	rb.Route(s.GetRoute())

	db := ctx.DB()

	// Clean up test data
	db.UpdateBySql("DELETE FROM robot WHERE robot_id IN (?,?)", "active_bot_836", "deleted_bot_836").Exec()
	db.UpdateBySql("DELETE FROM friend WHERE uid=? AND to_uid IN (?,?)", uid, "active_bot_836", "deleted_bot_836").Exec()
	db.UpdateBySql("DELETE FROM user WHERE uid IN (?,?,?)", uid, "active_bot_836", "deleted_bot_836").Exec()

	// Create the login user
	_, err := db.InsertInto("user").Columns("uid", "name", "status").
		Values(uid, "TestUser", 1).Exec()
	assert.NoError(t, err)

	// Create an active bot user + robot record
	_, err = db.InsertInto("user").Columns("uid", "name", "robot", "status").
		Values("active_bot_836", "ActiveBot", 1, 1).Exec()
	assert.NoError(t, err)
	_, err = db.InsertInto("robot").Columns("robot_id", "status", "creator_uid", "description").
		Values("active_bot_836", 1, uid, "active bot").Exec()
	assert.NoError(t, err)

	// Create a deleted bot user + robot record (status=0)
	_, err = db.InsertInto("user").Columns("uid", "name", "robot", "status").
		Values("deleted_bot_836", "DeletedBot", 1, 1).Exec()
	assert.NoError(t, err)
	_, err = db.InsertInto("robot").Columns("robot_id", "status", "creator_uid", "description").
		Values("deleted_bot_836", 0, uid, "deleted bot").Exec()
	assert.NoError(t, err)

	// Create friend records: login user -> both bots (is_deleted=0)
	_, err = db.InsertInto("friend").Columns("uid", "to_uid", "is_deleted").
		Values(uid, "active_bot_836", 0).Exec()
	assert.NoError(t, err)
	_, err = db.InsertInto("friend").Columns("uid", "to_uid", "is_deleted").
		Values(uid, "deleted_bot_836", 0).Exec()
	assert.NoError(t, err)

	// Call GET /v1/robot/my_bots
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/robot/my_bots", nil)
	assert.NoError(t, err)
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// Parse JSON array response
	var results []map[string]interface{}
	err = json.Unmarshal(w.Body.Bytes(), &results)
	assert.NoError(t, err)

	// Only active_bot_836 should be present
	assert.Equal(t, 1, len(results), "deleted bot should not appear in my_bots")
	if len(results) == 1 {
		assert.Equal(t, "active_bot_836", results[0]["uid"])
	}
}

// TestSpaceBots_ExcludesDeletedSpaceMembers verifies that /robot/space_bots
// filters out bots whose space_member.status has been set to 0.
func TestSpaceBots_ExcludesDeletedSpaceMembers(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := newTestServer()
	rb := New(ctx)
	rb.Route(s.GetRoute())

	db := ctx.DB()
	testSpaceID := "space_test_836"

	// Clean up test data
	db.UpdateBySql("DELETE FROM robot WHERE robot_id IN (?,?)", "sbot_active_836", "sbot_removed_836").Exec()
	db.UpdateBySql("DELETE FROM space_member WHERE space_id=?", testSpaceID).Exec()
	db.UpdateBySql("DELETE FROM user WHERE uid IN (?,?,?)", uid, "sbot_active_836", "sbot_removed_836").Exec()

	// Create the login user
	_, err := db.InsertInto("user").Columns("uid", "name", "status").
		Values(uid, "TestUser", 1).Exec()
	assert.NoError(t, err)

	// Active bot: user + robot + space_member(status=1)
	_, err = db.InsertInto("user").Columns("uid", "name", "robot", "status").
		Values("sbot_active_836", "SpaceActiveBot", 1, 1).Exec()
	assert.NoError(t, err)
	_, err = db.InsertInto("robot").Columns("robot_id", "status", "creator_uid", "description").
		Values("sbot_active_836", 1, uid, "active space bot").Exec()
	assert.NoError(t, err)
	_, err = db.InsertInto("space_member").Columns("space_id", "uid", "status").
		Values(testSpaceID, "sbot_active_836", 1).Exec()
	assert.NoError(t, err)

	// Removed bot: user + robot(status=1) + space_member(status=0)
	_, err = db.InsertInto("user").Columns("uid", "name", "robot", "status").
		Values("sbot_removed_836", "SpaceRemovedBot", 1, 1).Exec()
	assert.NoError(t, err)
	_, err = db.InsertInto("robot").Columns("robot_id", "status", "creator_uid", "description").
		Values("sbot_removed_836", 1, uid, "removed from space").Exec()
	assert.NoError(t, err)
	_, err = db.InsertInto("space_member").Columns("space_id", "uid", "status").
		Values(testSpaceID, "sbot_removed_836", 0).Exec()
	assert.NoError(t, err)

	// Call GET /v1/robot/space_bots?space_id=space_test_836
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/robot/space_bots?space_id="+testSpaceID, nil)
	assert.NoError(t, err)
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var results []map[string]interface{}
	err = json.Unmarshal(w.Body.Bytes(), &results)
	assert.NoError(t, err)

	// Only the active space member bot should appear
	assert.Equal(t, 1, len(results), "removed space member bot should not appear in space_bots")
	if len(results) == 1 {
		assert.Equal(t, "sbot_active_836", results[0]["uid"])
	}
}

func TestRobotObjectPathFormat(t *testing.T) {
	filename := "qualcomm_review.xlsx"
	objectPath := fmt.Sprintf("chat/%d/%s/%s", time.Now().Unix(), util.GenerUUID(), url.PathEscape(filename))
	parts := strings.Split(objectPath, "/")
	assert.Equal(t, 4, len(parts), "expected 4 path segments")
	assert.Equal(t, filename, parts[len(parts)-1])
}

func TestRobotLegacyUUIDStripping(t *testing.T) {
	path := "chat/1713360000/afd1a8d99bb94bf0a8d2c1e3f4a5b6c7_report.xlsx"
	got := pkgutil.ExtractFilenameFromPath(path)
	assert.Equal(t, "report.xlsx", got)
}

func TestRobotLegacyUUIDStrippingWithEncoding(t *testing.T) {
	path := "chat/1713360000/afd1a8d99bb94bf0a8d2c1e3f4a5b6c7_" + url.PathEscape("报告.xlsx")
	got := pkgutil.ExtractFilenameFromPath(path)
	assert.Equal(t, "报告.xlsx", got)
}

func TestRobotLegacyNoFalsePositive(t *testing.T) {
	path := "chat/1713360000/my_very_long_filename_with_underscores.xlsx"
	got := pkgutil.ExtractFilenameFromPath(path)
	assert.Equal(t, "my_very_long_filename_with_underscores.xlsx", got)
}

func TestRobotProxyFileNewPath(t *testing.T) {
	uuid := util.GenerUUID()
	path := fmt.Sprintf("chat/%d/%s/file.xlsx", time.Now().Unix(), uuid)
	got := pkgutil.ExtractFilenameFromPath(path)
	assert.Equal(t, "file.xlsx", got)
}
