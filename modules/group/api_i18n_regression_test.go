package group

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

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
