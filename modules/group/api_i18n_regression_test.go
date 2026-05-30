package group

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
)

// putGroupSetting issues a PUT /setting with the given JSON body using the test
// caller's token.
func putGroupSetting(t *testing.T, handler http.Handler, groupNo, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest("PUT", "/v1/groups/"+groupNo+"/setting", bytes.NewReader([]byte(body)))
	assert.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	handler.ServeHTTP(w, req)
	return w
}

// TestGroupExit_NotFoundGroup pins the fix for the review finding that
// groupExit returned 500 (query_failed) for a missing / disbanded group
// because it ignored getGroupInfo's not-found sentinel. The exit of a
// non-existent group is a user-facing 404, not an internal error.
func TestGroupExit_NotFoundGroup(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForGroupTest(s)
	_ = New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/groups/does-not-exist/exit", nil)
	assert.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, "wire status 固定 400, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.group.not_found",
		"退不存在的群应是 404 业务错误而非内部错误, body=%s", w.Body.String())
}

// TestGroupMemberInviteSure_ExpiredCode pins the fix for the review finding
// that an expired / missing auth_code (Redis returns "") fell through to a
// JSON-decode failure mapped to store_failed (500). An expired authorization
// code is a normal user-facing state and must surface as auth_code_invalid.
func TestGroupMemberInviteSure_ExpiredCode(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForGroupTest(s)
	_ = New(ctx)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/group/invite/sure?auth_code=expired-"+util.GenerUUID(), nil)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, "wire status 固定 400, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.group.auth_code_invalid",
		"过期/无效 auth_code 应是用户态错误而非内部错误, body=%s", w.Body.String())
}

// TestGroupMemberAdd_BlankMembersIsRequestInvalid pins the fix for the review
// finding that members consisting solely of blank strings pass Check() but
// AddGroupMembers returns "no valid members after deduplication" — a 400
// validation error, not the store_failed (500) it was being mapped to.
func TestGroupMemberAdd_BlankMembersIsRequestInvalid(t *testing.T) {
	f, h := setupBotOwnershipGroup(t)
	_ = f

	w := postAddMembers(t, h, "g_bot_own", []string{"   "})
	assert.Equal(t, http.StatusBadRequest, w.Code, "wire status 固定 400, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.group.request_invalid",
		"全空白成员应是 400 校验错误而非内部错误, body=%s", w.Body.String())
}

// TestManagerMemberRemove_NotInGroupIsNotFound pins the fix for the review
// finding that the management (CheckLoginRole==nil) delete path skips the
// per-member pre-check, so removing UIDs that are not in the group made
// RemoveGroupMembers return "none of the members are in this group" — a 404
// business error, not the store_failed (500) it was being mapped to.
func TestManagerMemberRemove_NotInGroupIsNotFound(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForGroupTest(s)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// Promote the test caller to SuperAdmin so memberRemove takes the
	// management path that skips the normal-member pre-check.
	cfg := ctx.GetConfig()
	assert.NoError(t, ctx.Cache().Set(
		cfg.Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	groupNo := "g-ghost-rm"
	err = f.userDB.Insert(&user.Model{UID: testutil.UID, Name: "admin", ShortNo: "ghost_admin"})
	assert.NoError(t, err)
	err = f.db.Insert(&Model{GroupNo: groupNo, Name: "ghost rm", Creator: testutil.UID, Status: GroupStatusNormal, Version: 1})
	assert.NoError(t, err)

	body := util.ToJson(map[string]any{"members": []string{"ghost-not-in-group"}})
	w := httptest.NewRecorder()
	req, err := http.NewRequest("DELETE", "/v1/groups/"+groupNo+"/members", bytes.NewReader([]byte(body)))
	assert.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, "wire status 固定 400, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.group.member_not_in_group",
		"删除非群成员应是 404 业务错误而非内部错误, body=%s", w.Body.String())
}

// TestGroupSettingUpdate_InvalidValueTypeIsRequestInvalid pins the fix for the
// review finding that a wrong-typed setting value (e.g. a string for the
// numeric "mute" toggle) returned by settingActionMap was collapsed into
// store_failed (500). A malformed value is a 400 client error.
func TestGroupSettingUpdate_InvalidValueTypeIsRequestInvalid(t *testing.T) {
	_, h := setupBotOwnershipGroup(t)

	// "mute" expects a number; sending a string trips safeIntFromFloat64.
	w := putGroupSetting(t, h, "g_bot_own", `{"mute":"not-a-number"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, "wire status 固定 400, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.group.request_invalid",
		"错类型的设置值应是 400 校验错误而非内部错误, body=%s", w.Body.String())
}

// TestGroupSettingUpdate_AllowExternalRangeIsRequestInvalid pins the fix for the
// review finding that an out-of-range allow_external value returned by the
// group-attr action was collapsed into store_failed (500). The test caller is
// the creator, so checkPermissions passes and the range check is what rejects.
func TestGroupSettingUpdate_AllowExternalRangeIsRequestInvalid(t *testing.T) {
	_, h := setupBotOwnershipGroup(t)

	w := putGroupSetting(t, h, "g_bot_own", `{"allow_external":2}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, "wire status 固定 400, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.group.request_invalid",
		"allow_external 越界应是 400 校验错误而非内部错误, body=%s", w.Body.String())
}

// TestGroupSettingUpdate_NonManagerForbidden pins the fix for the review finding
// that a non-manager/creator toggling a group-level attribute had
// checkPermissions's "没有权限！" collapsed into store_failed (500). Updating a
// group attribute without permission is a 403, not an internal error.
func TestGroupSettingUpdate_NonManagerForbidden(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForGroupTest(s)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// Group is owned by someone else; the test caller (testutil.UID) is neither
	// creator nor manager, so checkPermissions returns errGroupUpdateForbidden.
	groupNo := "g-perm-deny"
	err = f.db.Insert(&Model{GroupNo: groupNo, Name: "perm deny", Creator: "other-owner", Status: GroupStatusNormal, Version: 1})
	assert.NoError(t, err)

	w := putGroupSetting(t, s.GetRoute(), groupNo, `{"forbidden":1}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, "wire status 固定 400, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.group.creator_or_manager_only",
		"非管理员改群属性应是 403 而非内部错误, body=%s", w.Body.String())
}

// TestGroupAvatarUpload_MissingFileIsRequestInvalid pins the fix for the review
// finding that avatarUpload mapped a missing multipart "file" field
// (http.ErrMissingFile) to store_failed (500). Forgetting to attach the file is
// a 400 client mistake, mirroring the sibling ParseMultipartForm branch.
func TestGroupAvatarUpload_MissingFileIsRequestInvalid(t *testing.T) {
	_, handler := setupBotOwnershipGroup(t)

	// Build a valid multipart body that carries a field but no "file", so
	// ParseMultipartForm succeeds and FormFile("file") returns ErrMissingFile.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	assert.NoError(t, mw.WriteField("other", "x"))
	assert.NoError(t, mw.Close())

	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/groups/g_bot_own/avatar", &buf)
	assert.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, "wire status 固定 400, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.group.request_invalid",
		"缺少上传文件应是 400 校验错误而非内部错误, body=%s", w.Body.String())
}
