package group

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

func setupGroupMdTest(t *testing.T) (*Group, *server.Server) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// Create a group with testutil.UID as creator
	err = f.db.Insert(&Model{
		GroupNo: "g_md_test",
		Name:    "MD Test Group",
		Creator: testutil.UID,
		Status:  GroupStatusNormal,
		Version: 1,
	})
	assert.NoError(t, err)

	// Add creator as member
	err = f.db.InsertMember(&MemberModel{
		GroupNo: "g_md_test",
		UID:     testutil.UID,
		Role:    MemberRoleCreator,
		Status:  1,
		Version: 1,
		Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	})
	assert.NoError(t, err)

	return f, s
}

func TestGroupMdGet_NoContent(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, s := setupGroupMdTest(t)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/g_md_test/md", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"content":""`)
	assert.Contains(t, w.Body.String(), `"version":0`)
}

func TestGroupMdUpdate(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, s := setupGroupMdTest(t)

	// Update GROUP.md
	w := httptest.NewRecorder()
	body := util.ToJson(map[string]interface{}{
		"content": "# Hello Group",
	})
	req, _ := http.NewRequest("PUT", "/v1/groups/g_md_test/md", bytes.NewReader([]byte(body)))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"version":1`)

	// Verify by reading
	time.Sleep(100 * time.Millisecond)
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", "/v1/groups/g_md_test/md", nil)
	req2.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w2, req2)

	assert.Equal(t, http.StatusOK, w2.Code)
	assert.Contains(t, w2.Body.String(), `"content":"# Hello Group"`)
	assert.Contains(t, w2.Body.String(), `"version":1`)
}

func TestGroupMdUpdate_TooLarge(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, s := setupGroupMdTest(t)

	// Try to update with content exceeding max size
	largeContent := strings.Repeat("x", GetGroupMdMaxSize()+1)
	w := httptest.NewRecorder()
	body := util.ToJson(map[string]interface{}{
		"content": largeContent,
	})
	req, _ := http.NewRequest("PUT", "/v1/groups/g_md_test/md", bytes.NewReader([]byte(body)))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code) // API returns 200 with error in body
	assert.Contains(t, w.Body.String(), "exceeds max size")
}

func TestGroupMdDelete(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	f, s := setupGroupMdTest(t)

	// First create content
	_, err := f.db.UpdateGroupMd("g_md_test", "# To Delete", testutil.UID)
	assert.NoError(t, err)

	// Delete
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/v1/groups/g_md_test/md", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// Verify deletion
	time.Sleep(100 * time.Millisecond)
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", "/v1/groups/g_md_test/md", nil)
	req2.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w2, req2)

	assert.Equal(t, http.StatusOK, w2.Code)
	assert.Contains(t, w2.Body.String(), `"content":""`)
}

func TestGroupMdUpdate_CreatorAllowed(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, s := setupGroupMdTest(t)

	w := httptest.NewRecorder()
	body := util.ToJson(map[string]interface{}{
		"content": "# Creator update",
	})
	// The default test token maps to testutil.UID which IS the creator,
	// so this test verifies the creator CAN edit (positive authorization case)
	req, _ := http.NewRequest("PUT", "/v1/groups/g_md_test/md", bytes.NewReader([]byte(body)))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"version"`)
}

func TestBotAdminSet(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	f, s := setupGroupMdTest(t)

	// Add a bot member
	err := f.db.InsertMember(&MemberModel{
		GroupNo: "g_md_test",
		UID:     "bot_123",
		Role:    MemberRoleCommon,
		Robot:   1,
		Status:  1,
		Version: 2,
		Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	})
	assert.NoError(t, err)

	// Set bot as admin
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/groups/g_md_test/bot_admin/bot_123", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// Verify bot_admin is set
	isBotAdmin, err := f.db.QueryIsBotAdmin("g_md_test", "bot_123")
	assert.NoError(t, err)
	assert.True(t, isBotAdmin)
}

func TestBotAdminSet_NonRobotDenied(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	f, s := setupGroupMdTest(t)

	// Add a non-robot member
	err := f.db.InsertMember(&MemberModel{
		GroupNo: "g_md_test",
		UID:     "human_456",
		Role:    MemberRoleCommon,
		Robot:   0,
		Status:  1,
		Version: 2,
		Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	})
	assert.NoError(t, err)

	// Try to set non-robot as bot admin
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/groups/g_md_test/bot_admin/human_456", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "not a bot")
}

func TestBotAdminRemove(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	f, s := setupGroupMdTest(t)

	// Add a bot member with bot_admin=1
	err := f.db.InsertMember(&MemberModel{
		GroupNo: "g_md_test",
		UID:     "bot_789",
		Role:    MemberRoleCommon,
		Robot:   1,
		Status:  1,
		Version: 2,
		Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	})
	assert.NoError(t, err)
	err = f.db.UpdateBotAdmin("g_md_test", "bot_789", 1, 3)
	assert.NoError(t, err)

	// Remove bot admin
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/v1/groups/g_md_test/bot_admin/bot_789", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// Verify bot_admin is removed
	isBotAdmin, err := f.db.QueryIsBotAdmin("g_md_test", "bot_789")
	assert.NoError(t, err)
	assert.False(t, isBotAdmin)
}

func TestQueryBotMemberUIDs(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	f, _ := setupGroupMdTest(t)

	// Add some bot members
	err := f.db.InsertMember(&MemberModel{
		GroupNo: "g_md_test",
		UID:     "bot_a",
		Role:    MemberRoleCommon,
		Robot:   1,
		Status:  1,
		Version: 2,
		Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{
		GroupNo: "g_md_test",
		UID:     "bot_b",
		Role:    MemberRoleCommon,
		Robot:   1,
		Status:  1,
		Version: 3,
		Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	})
	assert.NoError(t, err)

	// Add a human member
	err = f.db.InsertMember(&MemberModel{
		GroupNo: "g_md_test",
		UID:     "human_c",
		Role:    MemberRoleCommon,
		Robot:   0,
		Status:  1,
		Version: 4,
		Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	})
	assert.NoError(t, err)

	uids, err := f.db.QueryBotMemberUIDs("g_md_test")
	assert.NoError(t, err)
	assert.Equal(t, 2, len(uids))
}

func TestGroupMdVersionAutoIncrement(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	f, _ := setupGroupMdTest(t)

	// First update
	v1, err := f.db.UpdateGroupMd("g_md_test", "version 1", testutil.UID)
	assert.NoError(t, err)
	assert.Equal(t, int64(1), v1)

	// Second update
	v2, err := f.db.UpdateGroupMd("g_md_test", "version 2", testutil.UID)
	assert.NoError(t, err)
	assert.Equal(t, int64(2), v2)

	// Delete increments too
	v3, err := f.db.DeleteGroupMd("g_md_test")
	assert.NoError(t, err)
	assert.Equal(t, int64(3), v3)
}
