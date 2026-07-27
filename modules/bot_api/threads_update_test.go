package bot_api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	utRobotID  = "bot_ut_update"
	utBotToken = "bf_ut_update_token"
	// 32-char hex (valid group_no)
	utGroupNo = "abcdef0123456789abcdef0123456789"
	// 18-digit numeric short_id
	utShortID = "148910429168271399"
)

var utRequestInvalidMsg = errcode.ErrBotAPIRequestInvalid.DefaultMessage
var utNotGroupMemberMsg = errcode.ErrBotAPINotGroupMember.DefaultMessage
var utStoreFailedMsg = errcode.ErrBotAPIStoreFailed.DefaultMessage

// setupBotUpdateThread seeds robot + group row + active group_member + one thread row.
// nonMember=true skips the group_member insert (bot not in group).
// noThread=true skips the thread insert (short_id points to nothing).
func setupBotUpdateThread(t *testing.T, nonMember, noThread bool) (http.Handler, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid, bot_token) VALUES (?, 1, ?, ?)",
		utRobotID, "owner_ut", utBotToken,
	).Exec()
	require.NoError(t, err)

	_, err = ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, status, version) VALUES (?, ?, 0, 1)",
		utGroupNo, "ut-group",
	).Exec()
	require.NoError(t, err)

	if !nonMember {
		_, err = ctx.DB().InsertBySql(
			"INSERT INTO group_member (group_no, uid, vercode, is_deleted, status, version) VALUES (?, ?, ?, 0, ?, 1)",
			utGroupNo, utRobotID, util.GenerUUID(), int(common.GroupMemberStatusNormal),
		).Exec()
		require.NoError(t, err)
	}

	if !noThread {
		tdb := thread.NewDB(ctx)
		require.NoError(t, tdb.Insert(&thread.Model{
			ShortID:    utShortID,
			GroupNo:    utGroupNo,
			Name:       "original-name",
			CreatorUID: utRobotID,
			Status:     thread.ThreadStatusActive,
			Version:    1,
		}))
	}

	return s.GetRoute(), ctx
}

func doBotPut(t *testing.T, handler http.Handler, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	w := httptest.NewRecorder()
	req, err := http.NewRequest("PUT", path, &buf)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+utBotToken)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, req)
	return w
}

func doBotGet2(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", path, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+utBotToken)
	handler.ServeHTTP(w, req)
	return w
}

func TestBotUpdateThread_Success(t *testing.T) {
	handler, _ := setupBotUpdateThread(t, false, false)
	path := "/v1/bot/groups/" + utGroupNo + "/threads/" + utShortID

	w := doBotPut(t, handler, path, map[string]string{"name": "new-name"})
	assert.Equal(t, http.StatusOK, w.Code, "rename must succeed, body=%s", w.Body.String())

	gw := doBotGet2(t, handler, path)
	require.Equal(t, http.StatusOK, gw.Code, "get after rename must succeed, body=%s", gw.Body.String())
	var got struct {
		Name string `json:"name"`
	}
	require.NoError(t, json.Unmarshal(gw.Body.Bytes(), &got))
	assert.Equal(t, "new-name", got.Name, "thread name must reflect rename")
}

func TestBotUpdateThread_NotGroupMember(t *testing.T) {
	handler, _ := setupBotUpdateThread(t, true, false)
	path := "/v1/bot/groups/" + utGroupNo + "/threads/" + utShortID

	w := doBotPut(t, handler, path, map[string]string{"name": "new-name"})
	// ResponseErrorL legacy wire maps 403 to 400 on the HTTP layer (see threads_blacklist_test.go)
	assert.Equal(t, http.StatusBadRequest, w.Code, "non-member bot must be denied, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), utNotGroupMemberMsg, "deny reason must be not_group_member")
}

func TestBotUpdateThread_EmptyName(t *testing.T) {
	handler, _ := setupBotUpdateThread(t, false, false)
	path := "/v1/bot/groups/" + utGroupNo + "/threads/" + utShortID

	w := doBotPut(t, handler, path, map[string]string{"name": ""})
	assert.Equal(t, http.StatusBadRequest, w.Code, "empty name must be rejected, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), utRequestInvalidMsg)
}

func TestBotUpdateThread_ThreadNotFound(t *testing.T) {
	handler, _ := setupBotUpdateThread(t, false, true)
	path := "/v1/bot/groups/" + utGroupNo + "/threads/" + utShortID

	w := doBotPut(t, handler, path, map[string]string{"name": "new-name"})
	assert.Equal(t, http.StatusInternalServerError, w.Code, "non-existent thread must return 500, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), utStoreFailedMsg)
}

func TestBotUpdateThread_NameTooLong(t *testing.T) {
	handler, _ := setupBotUpdateThread(t, false, false)
	path := "/v1/bot/groups/" + utGroupNo + "/threads/" + utShortID

	longName := strings.Repeat("a", 101)
	w := doBotPut(t, handler, path, map[string]string{"name": longName})
	assert.Equal(t, http.StatusBadRequest, w.Code, "name > 100 chars must be rejected, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), utRequestInvalidMsg)
}
