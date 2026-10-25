package internal_membership_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	_ "github.com/Mininglamp-OSS/octo-server/internal"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/require"
)

const (
	iupSpaceA = "space_a"
	iupSpaceB = "space_b"
)

func newMembershipHTTPTest(t *testing.T) (*server.Server, *config.Context) {
	t.Helper()
	// These HTTP tests own their fixture setup rather than depending on Project's TestMain.
	if os.Getenv("OCTO_MASTER_KEY") == "" {
		t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	}
	t.Setenv("OCTO_PROJECT_CREATE_ENABLED", "true")
	t.Setenv("OCTO_PROJECT_COLLABORATION_ROLE_ENABLED", "true")
	srv, ctx := testutil.NewTestServer()
	srv.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	ctx.DB().SetMaxOpenConns(5)
	ctx.DB().SetMaxIdleConns(5)
	// Registered modules can retain this session across fixtures; the test process owns its lifetime.
	cfg := ctx.GetConfig()
	cache := redis.NewClient(&redis.Options{Addr: cfg.DB.RedisAddr, Password: cfg.DB.RedisPass})
	defer cache.Close()
	for _, pattern := range []string{"ratelimit:uid:*", "project:member:*", "space:member:*"} {
		keys, err := cache.Keys(pattern).Result()
		require.NoError(t, err)
		if len(keys) > 0 {
			require.NoError(t, cache.Del(keys...).Err())
		}
	}
	return srv, ctx
}

// writeProjectE2E exercises the ordinary Project routes used by the parity fixtures.
func writeProjectE2E(t *testing.T, srv *server.Server, method, path, token string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("token", token)
	w := httptest.NewRecorder()
	srv.GetRoute().ServeHTTP(w, req)
	return w
}

func seedUserHTTP(t *testing.T, ctx *config.Context, uid string) string {
	t.Helper()
	seedUserBareHTTP(t, ctx, uid)
	token := "extok-" + uid + "-" + util.GenerUUID()[:8]
	require.NoError(t, ctx.Cache().Set(ctx.GetConfig().Cache.TokenCachePrefix+token, uid+"@"+uid))
	return token
}

func seedAgentHTTP(t *testing.T, ctx *config.Context, spaceID, uid, owner, hosting string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql("INSERT INTO `user` (uid, name, short_no, robot) VALUES (?, ?, ?, 1)", uid, "agent-"+uid, uid).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql("INSERT INTO robot (robot_id, token, status, creator_uid, agent_hosting) VALUES (?, ?, 1, ?, ?)", uid, "tok-"+uid, owner, hosting).Exec()
	require.NoError(t, err)
	seedSpaceMemberHTTP(t, ctx, spaceID, uid, 0, 1)
}

func iupProjectWithMembers(t *testing.T, srv *server.Server, ctx *config.Context, uids ...string) (string, *projectmod.Resp) {
	t.Helper()
	seedSpaceHTTP(t, ctx, iupSpaceA, 1)
	token := seedUserHTTP(t, ctx, "owner1")
	seedSpaceMemberHTTP(t, ctx, iupSpaceA, "owner1", 0, 1)
	members := make([]map[string]any, 0, len(uids))
	for _, uid := range uids {
		seedUserBareHTTP(t, ctx, uid)
		seedSpaceMemberHTTP(t, ctx, iupSpaceA, uid, 0, 1)
		members = append(members, map[string]any{"uid": uid, "role": projectmod.RoleCommon})
	}
	w := writeProjectE2E(t, srv, http.MethodPost, "/v1/space/"+iupSpaceA+"/projects", token, map[string]any{"name": "p"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var created projectmod.Resp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	if len(members) > 0 {
		w = writeProjectE2E(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add", token, map[string]any{"members": members})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var outcomes []struct {
			OK bool `json:"ok"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes))
		require.Len(t, outcomes, len(members))
		for _, outcome := range outcomes {
			require.True(t, outcome.OK)
		}
	}
	return token, &created
}

func iupRoleCatalog(t *testing.T, srv *server.Server, projectID, token string) []projectmod.CollaborationRoleResp {
	t.Helper()
	w := writeProjectE2E(t, srv, http.MethodGet, "/v1/projects/"+projectID+"/collaboration-roles", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var catalog struct {
		Roles []projectmod.CollaborationRoleResp `json:"roles"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &catalog))
	return catalog.Roles
}

func iupRoleNamed(t *testing.T, roles []projectmod.CollaborationRoleResp, name string) projectmod.CollaborationRoleResp {
	t.Helper()
	for _, role := range roles {
		if role.Name == name {
			return role
		}
	}
	t.Fatalf("role %q not found", name)
	return projectmod.CollaborationRoleResp{}
}

func iupCreateRole(t *testing.T, srv *server.Server, projectID, token, name string) projectmod.CollaborationRoleResp {
	t.Helper()
	w := writeProjectE2E(t, srv, http.MethodPost, "/v1/projects/"+projectID+"/collaboration-roles", token, map[string]any{"name": name})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var role projectmod.CollaborationRoleResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &role))
	return role
}

func seedSpaceHTTP(t *testing.T, ctx *config.Context, spaceID string, status int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, ?)",
		spaceID, spaceID, "creator", status,
	).Exec()
	require.NoError(t, err)
}

func seedSpaceMemberHTTP(t *testing.T, ctx *config.Context, spaceID, uid string, role, status int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, ?, ?)",
		spaceID, uid, role, status,
	).Exec()
	require.NoError(t, err)
}

func seedUserBareHTTP(t *testing.T, ctx *config.Context, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)", uid, "user-"+uid, uid,
	).Exec()
	require.NoError(t, err)
}

func setSpaceStatusHTTP(t *testing.T, ctx *config.Context, spaceID string, status int) {
	t.Helper()
	_, err := ctx.DB().UpdateBySql("UPDATE `space` SET status = ? WHERE space_id = ?",
		status, spaceID).Exec()
	require.NoError(t, err)
}

func iupProjectHTTP(t *testing.T, ctx *config.Context, projectID, spaceID, creator, name, description, logo string, status int, activated bool) {
	t.Helper()
	activatedAt := "NULL"
	if activated {
		activatedAt = "UTC_TIMESTAMP(3)"
	}
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO octo_project "+
			"(project_id, space_id, name, description, logo, creator, status, activated_at, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, "+activatedAt+", UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))",
		projectID, spaceID, name, description, logo, creator, status,
	).Exec()
	require.NoError(t, err)
}

func iupSeatHTTP(t *testing.T, ctx *config.Context, projectID, uid, spaceID string, role, status, removing int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO octo_project_member "+
			"(project_id, uid, space_id, role, status, removing, invite_uid, created_at, joined_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))",
		projectID, uid, spaceID, role, status, removing, uid,
	).Exec()
	require.NoError(t, err)
}
