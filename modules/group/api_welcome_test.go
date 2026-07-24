package group

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Per-group 入群欢迎语 CRUD HTTP tests (task group-welcome-message). Auth is fixed
// to testutil.UID via testutil.Token; the authz matrix is exercised by varying
// that user's group role and the group status.

func setupGroupWelcome(t *testing.T) (*Group, *server.Server) {
	t.Helper()
	s, ctx := newTestServer(t)
	// module.Setup inside NewTestServer already mounts the group routes (incl.
	// /welcome); construct a local Group only for the DB seed helpers. Calling
	// f.Route again would double-register and panic.
	f := New(ctx)
	require.NoError(t, testutil.CleanAllTables(ctx))
	wireI18nRendererForGroupTest(s)
	return f, s
}

// resetGroupUIDRateLimit clears the per-UID SharedUIDRateLimiter bucket, which
// persists in Redis and is NOT cleared by CleanAllTables.
func resetGroupUIDRateLimit(t *testing.T, ctx *config.Context) {
	t.Helper()
	rds := redis.NewClient(&redis.Options{Addr: ctx.GetConfig().DB.RedisAddr, Password: ctx.GetConfig().DB.RedisPass})
	defer rds.Close()
	if keys, err := rds.Keys("ratelimit:uid:*").Result(); err == nil && len(keys) > 0 {
		_ = rds.Del(keys...).Err()
	}
}

// gwSeedGroup inserts a group (status) with testutil.UID as a member at callerRole
// (MemberRoleCommon/Creator/Manager). Creator field is a different uid so the role
// gate (not the creator field) is what's exercised.
func gwSeedGroup(t *testing.T, f *Group, groupNo string, callerRole, status int) {
	t.Helper()
	require.NoError(t, f.db.Insert(&Model{GroupNo: groupNo, Name: groupNo, Creator: "creator_other", Status: status, Version: 1}))
	require.NoError(t, f.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: testutil.UID, Role: callerRole, Status: 1, Version: 1, Vercode: util.GenerUUID(),
	}))
}

func doGroupWelcome(t *testing.T, f *Group, s *server.Server, method, groupNo, body string) *httptest.ResponseRecorder {
	t.Helper()
	resetGroupUIDRateLimit(t, f.ctx)
	var r *strings.Reader
	if body != "" {
		r = strings.NewReader(body)
	} else {
		r = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, "/v1/groups/"+groupNo+"/welcome", r)
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)
	return w
}

type gwWelcomeResp struct {
	Configured bool   `json:"configured"`
	Enabled    bool   `json:"enabled"`
	ActiveFrom string `json:"active_from"`
	Message    string `json:"message"`
	Effective  struct {
		Enabled    bool   `json:"enabled"`
		Source     string `json:"source"`
		ActiveFrom string `json:"active_from"`
		Message    string `json:"message"`
	} `json:"effective"`
}

func gwDecode(t *testing.T, w *httptest.ResponseRecorder) gwWelcomeResp {
	t.Helper()
	var resp gwWelcomeResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return resp
}

func gwAssertErrCode(t *testing.T, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), w.Body.String())
	assert.Equal(t, want, env.Error.Code, w.Body.String())
}

func TestGroupWelcomeCRUD_AuthzMatrix(t *testing.T) {
	t.Run("creator can read", func(t *testing.T) {
		f, s := setupGroupWelcome(t)
		gwSeedGroup(t, f, "g_creator", MemberRoleCreator, GroupStatusNormal)
		w := doGroupWelcome(t, f, s, http.MethodGet, "g_creator", "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		resp := gwDecode(t, w)
		assert.False(t, resp.Configured)
		assert.Equal(t, "none", resp.Effective.Source)
	})

	t.Run("manager can read", func(t *testing.T) {
		f, s := setupGroupWelcome(t)
		gwSeedGroup(t, f, "g_manager", MemberRoleManager, GroupStatusNormal)
		w := doGroupWelcome(t, f, s, http.MethodGet, "g_manager", "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})

	t.Run("plain member is denied", func(t *testing.T) {
		f, s := setupGroupWelcome(t)
		gwSeedGroup(t, f, "g_member", MemberRoleCommon, GroupStatusNormal)
		w := doGroupWelcome(t, f, s, http.MethodGet, "g_member", "")
		gwAssertErrCode(t, w, "err.server.group.creator_or_manager_only")
	})

	t.Run("non-member is denied", func(t *testing.T) {
		f, s := setupGroupWelcome(t)
		require.NoError(t, f.db.Insert(&Model{GroupNo: "g_nomember", Name: "g_nomember", Creator: "someone", Status: GroupStatusNormal, Version: 1}))
		w := doGroupWelcome(t, f, s, http.MethodGet, "g_nomember", "")
		gwAssertErrCode(t, w, "err.server.group.creator_or_manager_only")
	})

	t.Run("disbanded group is not found", func(t *testing.T) {
		f, s := setupGroupWelcome(t)
		gwSeedGroup(t, f, "g_dead", MemberRoleCreator, GroupStatusDisband)
		w := doGroupWelcome(t, f, s, http.MethodGet, "g_dead", "")
		gwAssertErrCode(t, w, "err.server.group.not_found")
	})

	t.Run("disabled group is not found", func(t *testing.T) {
		// Parity with the space welcome's checkSpaceActive: an admin-disabled
		// (non-Normal) group must reject welcome CRUD, not just a disbanded one.
		f, s := setupGroupWelcome(t)
		gwSeedGroup(t, f, "g_disabled", MemberRoleCreator, GroupStatusDisabled)
		w := doGroupWelcome(t, f, s, http.MethodGet, "g_disabled", "")
		gwAssertErrCode(t, w, "err.server.group.not_found")
	})
}

func TestGroupWelcomePut_CreateThenGet(t *testing.T) {
	f, s := setupGroupWelcome(t)
	gwSeedGroup(t, f, "g_1", MemberRoleCreator, GroupStatusNormal)

	body := `{"enabled":true,"active_from":"2026-07-10T00:00:00Z","message":"欢迎 {member} 入群"}`
	w := doGroupWelcome(t, f, s, http.MethodPut, "g_1", body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp := gwDecode(t, w)
	assert.True(t, resp.Configured)
	assert.True(t, resp.Enabled)
	assert.Equal(t, "欢迎 {member} 入群", resp.Message, "placeholder stored raw (rendered at send time)")
	assert.Equal(t, "group", resp.Effective.Source)
	assert.Equal(t, "欢迎 {member} 入群", resp.Effective.Message)

	g := gwDecode(t, doGroupWelcome(t, f, s, http.MethodGet, "g_1", ""))
	assert.True(t, g.Configured)
	assert.True(t, g.Enabled)
	assert.Equal(t, "欢迎 {member} 入群", g.Message)
}

func TestGroupWelcomePut_InvalidCombinationRejected(t *testing.T) {
	f, s := setupGroupWelcome(t)
	gwSeedGroup(t, f, "g_1", MemberRoleManager, GroupStatusNormal)

	w := doGroupWelcome(t, f, s, http.MethodPut, "g_1", `{"enabled":true,"active_from":"2026-07-10T00:00:00Z","message":"   "}`)
	gwAssertErrCode(t, w, "err.server.group.request_invalid")

	w = doGroupWelcome(t, f, s, http.MethodPut, "g_1", `{"enabled":true,"active_from":"not-a-time","message":"hi"}`)
	gwAssertErrCode(t, w, "err.server.group.request_invalid")
}

func TestGroupWelcomePut_MessageTooLong(t *testing.T) {
	f, s := setupGroupWelcome(t)
	gwSeedGroup(t, f, "g_1", MemberRoleCreator, GroupStatusNormal)
	long := strings.Repeat("我", 2001)
	w := doGroupWelcome(t, f, s, http.MethodPut, "g_1", `{"enabled":false,"message":"`+long+`"}`)
	gwAssertErrCode(t, w, "err.server.group.request_invalid")
}

func TestGroupWelcomePut_EmptyPatchRejected(t *testing.T) {
	f, s := setupGroupWelcome(t)
	gwSeedGroup(t, f, "g_1", MemberRoleCreator, GroupStatusNormal)
	w := doGroupWelcome(t, f, s, http.MethodPut, "g_1", `{}`)
	gwAssertErrCode(t, w, "err.server.group.request_invalid")
}

func TestGroupWelcomePut_PartialMergePreservesMessage(t *testing.T) {
	f, s := setupGroupWelcome(t)
	gwSeedGroup(t, f, "g_1", MemberRoleCreator, GroupStatusNormal)
	require.Equal(t, http.StatusOK,
		doGroupWelcome(t, f, s, http.MethodPut, "g_1", `{"enabled":true,"active_from":"2026-07-10T00:00:00Z","message":"keep {member}"}`).Code)

	w := doGroupWelcome(t, f, s, http.MethodPut, "g_1", `{"enabled":false}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp := gwDecode(t, w)
	assert.False(t, resp.Enabled)
	assert.Equal(t, "keep {member}", resp.Message, "partial patch keeps the existing message")
	assert.Equal(t, "none", resp.Effective.Source, "disabled -> nothing in effect")
}

func TestGroupWelcomeDelete_RevertsToOff(t *testing.T) {
	f, s := setupGroupWelcome(t)
	gwSeedGroup(t, f, "g_1", MemberRoleCreator, GroupStatusNormal)
	require.Equal(t, http.StatusOK,
		doGroupWelcome(t, f, s, http.MethodPut, "g_1", `{"enabled":true,"active_from":"2026-07-10T00:00:00Z","message":"bye {member}"}`).Code)

	w := doGroupWelcome(t, f, s, http.MethodDelete, "g_1", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var del struct {
		Deleted bool `json:"deleted"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &del), w.Body.String())
	assert.True(t, del.Deleted)

	g := gwDecode(t, doGroupWelcome(t, f, s, http.MethodGet, "g_1", ""))
	assert.False(t, g.Configured)
	assert.Equal(t, "none", g.Effective.Source)

	w = doGroupWelcome(t, f, s, http.MethodDelete, "g_1", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &del), w.Body.String())
	assert.False(t, del.Deleted)
}
