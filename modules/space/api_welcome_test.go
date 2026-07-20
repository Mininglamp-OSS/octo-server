package space

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Per-Space welcome config CRUD HTTP tests
// (task space-welcome-per-space-admin-crud). Auth is fixed to testutil.UID via
// testutil.Token; the authz matrix is exercised by varying that user's
// membership/role and the Space status.

func doWelcome(t *testing.T, method, spaceId, body string) *httptest.ResponseRecorder {
	t.Helper()
	resetSpaceUIDRateLimit(t, testCtx)
	var r *strings.Reader
	if body != "" {
		r = strings.NewReader(body)
	} else {
		r = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, "/v1/space/"+spaceId+"/welcome", r)
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	testSrv.GetRoute().ServeHTTP(w, req)
	return w
}

type welcomeRespForTest struct {
	Configured bool   `json:"configured"`
	Enabled    bool   `json:"enabled"`
	ActiveFrom string `json:"active_from"`
	Message    string `json:"message"`
	UpdatedBy  string `json:"updated_by"`
	Effective  struct {
		Enabled bool   `json:"enabled"`
		Source  string `json:"source"`
		Message string `json:"message"`
	} `json:"effective"`
}

func decodeWelcomeResp(t *testing.T, w *httptest.ResponseRecorder) welcomeRespForTest {
	t.Helper()
	var resp welcomeRespForTest
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return resp
}

// seedWelcomeSpace creates an active Space with testutil.UID at the given role.
func seedWelcomeSpace(t *testing.T, spaceId string, role int) {
	t.Helper()
	require.NoError(t, testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: spaceId, Creator: testutil.UID, Status: SpaceStatusNormal,
	}))
	require.NoError(t, testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: testutil.UID, Role: role, Status: 1,
	}))
}

func TestWelcomeCRUD_AuthzMatrix(t *testing.T) {
	t.Run("admin can read", func(t *testing.T) {
		setup(t)
		seedWelcomeSpace(t, "spc_admin", 1) // Role=admin
		w := doWelcome(t, http.MethodGet, "spc_admin", "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		resp := decodeWelcomeResp(t, w)
		assert.False(t, resp.Configured, "no per-space row yet")
		assert.Equal(t, "none", resp.Effective.Source)
	})

	t.Run("plain member is denied", func(t *testing.T) {
		setup(t)
		seedWelcomeSpace(t, "spc_member", 0) // Role=member
		w := doWelcome(t, http.MethodGet, "spc_member", "")
		assertSpaceErrorCode(t, w, "err.server.space.permission_denied")
	})

	t.Run("non-member is denied", func(t *testing.T) {
		setup(t)
		// Space exists but testutil.UID is NOT a member.
		require.NoError(t, testSpaceDB.insertSpaceNoTx(&SpaceModel{
			SpaceId: "spc_nomember", Name: "spc_nomember", Creator: "someone_else", Status: SpaceStatusNormal,
		}))
		w := doWelcome(t, http.MethodGet, "spc_nomember", "")
		assertSpaceErrorCode(t, w, "err.server.space.permission_denied")
	})

	t.Run("inactive space is rejected", func(t *testing.T) {
		setup(t)
		require.NoError(t, testSpaceDB.insertSpaceNoTx(&SpaceModel{
			SpaceId: "spc_dead", Name: "spc_dead", Creator: testutil.UID, Status: 2, // banned
		}))
		require.NoError(t, testSpaceDB.insertMemberNoTx(&MemberModel{
			SpaceId: "spc_dead", UID: testutil.UID, Role: 2, Status: 1,
		}))
		w := doWelcome(t, http.MethodGet, "spc_dead", "")
		assertSpaceErrorCode(t, w, "err.server.space.not_found")
	})
}

func TestWelcomePut_CreateThenGet(t *testing.T) {
	setup(t)
	seedWelcomeSpace(t, "spc_1", 2) // owner

	body := `{"enabled":true,"active_from":"2026-07-10T00:00:00Z","message":"欢迎\n多行"}`
	w := doWelcome(t, http.MethodPut, "spc_1", body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp := decodeWelcomeResp(t, w)
	assert.True(t, resp.Configured)
	assert.True(t, resp.Enabled)
	assert.Equal(t, "欢迎\n多行", resp.Message, "newline preserved verbatim")
	assert.Equal(t, testutil.UID, resp.UpdatedBy)
	assert.Equal(t, "space", resp.Effective.Source)

	// GET reflects the stored config.
	g := decodeWelcomeResp(t, doWelcome(t, http.MethodGet, "spc_1", ""))
	assert.True(t, g.Configured)
	assert.True(t, g.Enabled)
	assert.Equal(t, "欢迎\n多行", g.Message)
}

func TestWelcomePut_InvalidCombinationRejected(t *testing.T) {
	setup(t)
	seedWelcomeSpace(t, "spc_1", 1)

	// enabled=true with an empty message is an invalid combination.
	body := `{"enabled":true,"active_from":"2026-07-10T00:00:00Z","message":"   "}`
	w := doWelcome(t, http.MethodPut, "spc_1", body)
	assertSpaceErrorCode(t, w, "err.server.common.space_welcome_config_invalid")

	// enabled=true with an unparseable active_from is also invalid.
	body = `{"enabled":true,"active_from":"not-a-time","message":"hi"}`
	w = doWelcome(t, http.MethodPut, "spc_1", body)
	assertSpaceErrorCode(t, w, "err.server.common.space_welcome_config_invalid")
}

func TestWelcomePut_MessageTooLong(t *testing.T) {
	setup(t)
	seedWelcomeSpace(t, "spc_1", 1)

	long := strings.Repeat("我", 2001) // > 2000 code points
	body := `{"enabled":false,"message":"` + long + `"}`
	w := doWelcome(t, http.MethodPut, "spc_1", body)
	assertSpaceErrorCode(t, w, "err.server.space.field_too_long")
}

func TestWelcomePut_EmptyPatchRejected(t *testing.T) {
	setup(t)
	seedWelcomeSpace(t, "spc_1", 1)
	w := doWelcome(t, http.MethodPut, "spc_1", `{}`)
	assertSpaceErrorCode(t, w, "err.server.space.request_invalid")
}

func TestWelcomePut_PartialMergePreservesMessage(t *testing.T) {
	setup(t)
	seedWelcomeSpace(t, "spc_1", 1)

	// Create enabled config with a message.
	require.Equal(t, http.StatusOK,
		doWelcome(t, http.MethodPut, "spc_1", `{"enabled":true,"active_from":"2026-07-10T00:00:00Z","message":"keep me"}`).Code)

	// Patch only enabled=false; message must be preserved and stay valid.
	w := doWelcome(t, http.MethodPut, "spc_1", `{"enabled":false}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp := decodeWelcomeResp(t, w)
	assert.False(t, resp.Enabled)
	assert.Equal(t, "keep me", resp.Message, "partial patch keeps the existing message")
}

func TestWelcomeDelete_RevertsToUnconfigured(t *testing.T) {
	setup(t)
	seedWelcomeSpace(t, "spc_1", 1)

	require.Equal(t, http.StatusOK,
		doWelcome(t, http.MethodPut, "spc_1", `{"enabled":true,"active_from":"2026-07-10T00:00:00Z","message":"bye soon"}`).Code)

	// Delete returns deleted=true.
	w := doWelcome(t, http.MethodDelete, "spc_1", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var del struct {
		Deleted bool `json:"deleted"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &del), w.Body.String())
	assert.True(t, del.Deleted)

	// GET now shows unconfigured.
	g := decodeWelcomeResp(t, doWelcome(t, http.MethodGet, "spc_1", ""))
	assert.False(t, g.Configured)

	// Deleting again is an idempotent no-op.
	w = doWelcome(t, http.MethodDelete, "spc_1", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &del), w.Body.String())
	assert.False(t, del.Deleted)
}
