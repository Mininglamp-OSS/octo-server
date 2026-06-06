package user

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authVerifyToken: POST /v1/auth/verify
// v2 鉴权关系数据补全 (合并 plan §3.2): adds ?include=context to return
// server-validated `spaces` + `owned_bots_by_space` map for fleet/matter.
//
// Pre-v2 schema (uid + name + role + owned_bots list) must stay unchanged
// when ?include is absent — IM / admin clients depend on the original shape.

const (
	testVerifyTokenSpaceA = "verify_token_space_a"
	testVerifyTokenSpaceB = "verify_token_space_b"
)

// seedVerifyTokenFixtures adds testutil.UID as an active member of two
// spaces. Reuses the same helper pattern as seedAPIKeyFixtures.
func seedVerifyTokenFixtures(t *testing.T, ctx *config.Context) {
	t.Helper()
	for _, sid := range []string{testVerifyTokenSpaceA, testVerifyTokenSpaceB} {
		_, err := ctx.DB().InsertInto("space").
			Columns("space_id", "name", "creator", "status").
			Values(sid, "Test "+sid, testutil.UID, 1).Exec()
		require.NoError(t, err)
		_, err = ctx.DB().InsertInto("space_member").
			Columns("space_id", "uid", "role", "status").
			Values(sid, testutil.UID, 0, 1).Exec()
		require.NoError(t, err)
	}
}

func doVerifyToken(t *testing.T, s *server.Server, body interface{}, withInclude bool) *httptest.ResponseRecorder {
	t.Helper()
	reqBody := bytes.NewReader([]byte(util.ToJson(body)))
	w := httptest.NewRecorder()
	path := "/v1/auth/verify"
	if withInclude {
		path += "?include=context"
	}
	req, err := http.NewRequest("POST", path, reqBody)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	return w
}

// BC test — the critical one. Without ?include the response must NOT
// contain the new fields. Any change to default behavior breaks IM and
// other historic callers that lock their schema.
func TestAuthVerifyToken_NoInclude_NoNewFields(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedVerifyTokenFixtures(t, ctx)
	insertBot(t, ctx, "bot_in_a_bc", testutil.UID, testVerifyTokenSpaceA)

	w := doVerifyToken(t, s, map[string]string{"token": testutil.Token}, false)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	body := w.Body.String()
	assert.NotContains(t, body, "spaces", "BC: default schema must not include spaces")
	assert.NotContains(t, body, "owned_bots_by_space", "BC: default schema must not include owned_bots_by_space")
	assert.Contains(t, body, `"uid"`)
	assert.Contains(t, body, `"owned_bots"`, "legacy owned_bots list field must remain in default response")
}

// With ?include=context the response carries both new fields. Legacy
// owned_bots list is also still present (we don't remove it for BC).
func TestAuthVerifyToken_WithInclude_ReturnsSpacesAndOwnedBotsMap(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedVerifyTokenFixtures(t, ctx)
	insertBot(t, ctx, "bot_a_1", testutil.UID, testVerifyTokenSpaceA)
	insertBot(t, ctx, "bot_a_2", testutil.UID, testVerifyTokenSpaceA)
	insertBot(t, ctx, "bot_b_1", testutil.UID, testVerifyTokenSpaceB)

	w := doVerifyToken(t, s, map[string]string{"token": testutil.Token}, true)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		UID              string              `json:"uid"`
		Spaces           []string            `json:"spaces"`
		OwnedBotsBySpace map[string][]string `json:"owned_bots_by_space"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, testutil.UID, resp.UID)
	assert.ElementsMatch(t, []string{testVerifyTokenSpaceA, testVerifyTokenSpaceB}, resp.Spaces)

	require.NotNil(t, resp.OwnedBotsBySpace)
	require.Contains(t, resp.OwnedBotsBySpace, testVerifyTokenSpaceA)
	require.Contains(t, resp.OwnedBotsBySpace, testVerifyTokenSpaceB)
	assert.ElementsMatch(t, []string{"bot_a_1", "bot_a_2"}, resp.OwnedBotsBySpace[testVerifyTokenSpaceA])
	assert.ElementsMatch(t, []string{"bot_b_1"}, resp.OwnedBotsBySpace[testVerifyTokenSpaceB])
}

// User with no bots → spaces present but every space's owned_bots is [].
// Stable map shape guarantees fleet/matter handlers don't NPE on lookup.
func TestAuthVerifyToken_WithInclude_NoBots(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedVerifyTokenFixtures(t, ctx)
	// no insertBot

	w := doVerifyToken(t, s, map[string]string{"token": testutil.Token}, true)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Spaces           []string            `json:"spaces"`
		OwnedBotsBySpace map[string][]string `json:"owned_bots_by_space"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.ElementsMatch(t, []string{testVerifyTokenSpaceA, testVerifyTokenSpaceB}, resp.Spaces)
	require.NotNil(t, resp.OwnedBotsBySpace)
	assert.Empty(t, resp.OwnedBotsBySpace[testVerifyTokenSpaceA])
	assert.Empty(t, resp.OwnedBotsBySpace[testVerifyTokenSpaceB])
}

// Disabled bots (status=0) and bots in spaces where the user is no longer
// an active member must not leak through.
func TestAuthVerifyToken_WithInclude_FiltersInactive(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedVerifyTokenFixtures(t, ctx)
	insertBot(t, ctx, "bot_active", testutil.UID, testVerifyTokenSpaceA)
	insertBot(t, ctx, "bot_disabled", testutil.UID, testVerifyTokenSpaceA)

	// Flip bot_disabled status to 0.
	_, err := ctx.DB().Update("robot").Set("status", 0).
		Where("robot_id=?", "bot_disabled").Exec()
	require.NoError(t, err)

	w := doVerifyToken(t, s, map[string]string{"token": testutil.Token}, true)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		OwnedBotsBySpace map[string][]string `json:"owned_bots_by_space"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.ElementsMatch(t, []string{"bot_active"}, resp.OwnedBotsBySpace[testVerifyTokenSpaceA],
		"disabled bot must not leak through owned_bots_by_space")
}

// Unknown include value (e.g. ?include=foo) must be treated as not set —
// keep the default schema, no 4xx. Forward-compat: future include flags
// don't break old callers that hardcoded include=context.
func TestAuthVerifyToken_UnknownIncludeValue_TreatedAsAbsent(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedVerifyTokenFixtures(t, ctx)

	reqBody := bytes.NewReader([]byte(util.ToJson(map[string]string{"token": testutil.Token})))
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/auth/verify?include=foo", reqBody)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	body := w.Body.String()
	assert.NotContains(t, body, "spaces", "unknown include must fall back to default schema")
}
