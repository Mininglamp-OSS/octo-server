package group

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	common2 "github.com/Mininglamp-OSS/octo-server/modules/common"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func postGroupCreateWire(t *testing.T, srv *server.Server, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, "/v1/group/create", bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.GetRoute().ServeHTTP(w, req)
	return w
}

func seedGroupCreateUser(t *testing.T, ctx *config.Context, uid string, status int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, short_no, status, is_destroy, robot) VALUES (?, ?, ?, ?, 0, 0)",
		uid, "user-"+uid, uid, status,
	).Exec()
	require.NoError(t, err)
}

func enableProjectGroupsForWire(t *testing.T, ctx *config.Context) {
	t.Helper()
	t.Setenv("OCTO_PROJECT_CREATE_ENABLED", "true")
	// CleanAllTables clears system_setting but the SystemSettings snapshot is
	// process-wide; reload it so an earlier case cannot leave a stale override.
	require.NoError(t, common2.EnsureSystemSettings(ctx).Reload())
}

func TestProjectGroupCreateSentinelsUseRelationEnvelope(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantCode   string
		wantStatus int
	}{
		{name: "invalid", err: projectmod.ErrGroupProjectInvalid, wantCode: "err.server.group.request_invalid", wantStatus: http.StatusBadRequest},
		{name: "not found", err: projectmod.ErrGroupProjectNotFound, wantCode: "err.server.group.not_found", wantStatus: http.StatusNotFound},
		{name: "forbidden", err: projectmod.ErrGroupProjectForbidden, wantCode: "err.server.group.view_forbidden", wantStatus: http.StatusForbidden},
		{name: "space conflict", err: projectmod.ErrGroupProjectSpaceConflict, wantCode: "err.server.group.project_conflict", wantStatus: http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			relationErr, ok := mapCreateProjectGroupError(tc.err)
			require.True(t, ok)
			route := helperHarness(func(c *wkhttp.Context) {
				respondGroupProjectError(c, relationErr)
			})
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/probe", nil)
			route.ServeHTTP(w, req)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			env := decodeEnvelope(t, w.Body.Bytes())
			assert.Equal(t, tc.wantCode, env.Error.Code)
			assert.Equal(t, tc.wantStatus, env.Error.HTTPStatus)
		})
	}

	_, ok := mapCreateProjectGroupError(errors.New("database unavailable"))
	assert.False(t, ok, "unknown service failures must remain on groupCreate's internal store_failed path")
}

func TestGroupCreateMissingProjectReturnsLocalizedNotFound(t *testing.T) {
	srv, ctx := newTestServer(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	wireI18nRendererForGroupTest(srv)
	resetGroupUIDRateLimit(t, ctx)
	enableProjectGroupsForWire(t, ctx)

	creator := testutil.UID
	spaceID := "space-create-notfound-" + util.GenerUUID()[:8]
	seedSpaceSeat(t, ctx, spaceID, creator)

	w := postGroupCreateWire(t, srv, map[string]any{
		"name":       "missing project group",
		"space_id":   spaceID,
		"project_id": "project-does-not-exist-" + util.GenerUUID()[:8],
	})
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	env := decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "err.server.group.not_found", env.Error.Code)
	assert.Equal(t, http.StatusNotFound, env.Error.HTTPStatus)
}

func TestGroupCreateProjectNonMemberReturnsLocalizedForbidden(t *testing.T) {
	srv, ctx := newTestServer(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	wireI18nRendererForGroupTest(srv)
	resetGroupUIDRateLimit(t, ctx)
	enableProjectGroupsForWire(t, ctx)

	creator := testutil.UID
	spaceID := "space-create-forbidden-" + util.GenerUUID()[:8]
	projectID := "project-create-forbidden-" + util.GenerUUID()[:8]
	seedSpaceSeat(t, ctx, spaceID, creator)
	seedProject(t, ctx, projectID, spaceID)

	w := postGroupCreateWire(t, srv, map[string]any{
		"name":       "forbidden project group",
		"space_id":   spaceID,
		"project_id": projectID,
	})
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	env := decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "err.server.group.view_forbidden", env.Error.Code)
	assert.Equal(t, http.StatusForbidden, env.Error.HTTPStatus)
}

func TestGroupCreateProjectCrossSpaceReturnsConflict(t *testing.T) {
	srv, ctx := newTestServer(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	wireI18nRendererForGroupTest(srv)
	resetGroupUIDRateLimit(t, ctx)
	enableProjectGroupsForWire(t, ctx)

	creator := testutil.UID
	spaceID := "space-create-request-" + util.GenerUUID()[:8]
	projectSpaceID := "space-create-project-" + util.GenerUUID()[:8]
	projectID := "project-create-conflict-" + util.GenerUUID()[:8]
	seedSpaceSeat(t, ctx, spaceID, creator)
	seedSpaceSeat(t, ctx, projectSpaceID, "project-owner-"+util.GenerUUID()[:8])
	seedProject(t, ctx, projectID, projectSpaceID)

	w := postGroupCreateWire(t, srv, map[string]any{
		"name":       "cross space project group",
		"space_id":   spaceID,
		"project_id": projectID,
	})
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	env := decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "err.server.group.project_conflict", env.Error.Code)
	assert.Equal(t, http.StatusConflict, env.Error.HTTPStatus)
}

func TestGroupCreateProjectDisabledExplicitTargetReturnsForbidden(t *testing.T) {
	srv, ctx := newTestServer(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	wireI18nRendererForGroupTest(srv)
	resetGroupUIDRateLimit(t, ctx)
	enableProjectGroupsForWire(t, ctx)

	creator := testutil.UID
	spaceID := "space-create-disabled-" + util.GenerUUID()[:8]
	projectID := "project-create-disabled-" + util.GenerUUID()[:8]
	disabled := "disabled-target-" + util.GenerUUID()[:8]
	seedGroupCreateUser(t, ctx, disabled, 0)
	seedSpaceSeat(t, ctx, spaceID, creator)
	seedProject(t, ctx, projectID, spaceID)
	seedProjectMember(t, ctx, projectID, spaceID, creator, 0)

	w := postGroupCreateWire(t, srv, map[string]any{
		"name":       "disabled explicit target",
		"members":    []string{disabled},
		"space_id":   spaceID,
		"project_id": projectID,
	})
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	env := decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "err.server.group.view_forbidden", env.Error.Code)
	assert.Equal(t, http.StatusForbidden, env.Error.HTTPStatus)
}

func TestGroupCreateUsesSharedUIDRateLimitAndRejectsOverBurst(t *testing.T) {
	srv, ctx := newTestServer(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	wireI18nRendererForGroupTest(srv)
	resetGroupUIDRateLimit(t, ctx)

	first := postGroupCreateWire(t, srv, map[string]any{})
	require.Equal(t, http.StatusBadRequest, first.Code, first.Body.String())
	require.Equal(t, "uid", first.Header().Get("X-RateLimit-Scope"))
	burst, err := strconv.Atoi(first.Header().Get("X-RateLimit-Limit"))
	require.NoError(t, err, first.Header().Get("X-RateLimit-Limit"))
	require.Greater(t, burst, 0)

	for i := 1; i < burst+1; i++ {
		w := postGroupCreateWire(t, srv, map[string]any{})
		if w.Code == http.StatusTooManyRequests {
			assert.Equal(t, "uid", w.Header().Get("X-RateLimit-Scope"))
			return
		}
		assert.Equal(t, http.StatusBadRequest, w.Code, "request %d body: %s", i+1, w.Body.String())
	}
	t.Fatalf("sending burst+1 requests did not produce a 429 (burst=%d)", burst)
}
