package user

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// seedVerifyTokenFixtures adds testutil.UID as an active user + active
// member of two spaces. Reuses the same helper pattern as seedAPIKeyFixtures.
//
// v3.3.6 §P1: INSERT user row (status=1) is required since the daemon
// api_key membership SQL now joins `user` ON u.status=1. Without it, the
// happy-path tests would 401 because the join finds no matching user row.
// Tolerate pre-existing user row (testutil.NewTestServer may seed) via
// INSERT IGNORE — keep the function idempotent.
func seedVerifyTokenFixtures(t *testing.T, ctx *config.Context) {
	t.Helper()
	_, err := ctx.DB().Exec(
		"INSERT IGNORE INTO `user` (uid, name, status, short_no) VALUES (?, ?, ?, ?)",
		testutil.UID, "testutil_user", 1, "testutil_sn",
	)
	require.NoError(t, err)
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
	assert.NotContains(t, body, "context_included",
		"v3.3.3 §F: BC default schema must not include context_included (omitempty drops false)")
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
		ContextIncluded  bool                `json:"context_included"`
		Spaces           []string            `json:"spaces"`
		OwnedBotsBySpace map[string][]string `json:"owned_bots_by_space"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, testutil.UID, resp.UID)
	assert.True(t, resp.ContextIncluded,
		"v3.3.3 §F: context_included MUST be true on ?include=context — fail-closed vs fallback discriminator")
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

// v3.3.1 §A.1 (Jerry-Xin Critical 三审): queryUserSpaceContext query (1)
// now joins `space ON s.status=1`. Without this, a user with a lingering
// active space_member row in a soft-deleted space (s.status=0) would
// surface that space in both `spaces` and `owned_bots_by_space` —
// inconsistent with the api_key path which v3 §2.3 had already fixed via
// assertSpaceMember. This test seeds the exact disabled-space case to
// lock the fix.
func TestAuthVerifyToken_WithInclude_FiltersDisabledSpace(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedVerifyTokenFixtures(t, ctx)

	// disabled space: user is an active member but space.status=0
	const disabledSpace = "verify_token_space_disabled"
	_, err := ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(disabledSpace, "Disabled", testutil.UID, 0). // status=0 disabled
		Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(disabledSpace, testutil.UID, 0, 1). // member row still active
		Exec()
	require.NoError(t, err)

	// bot exists in disabled space — must not leak through owned_bots_by_space
	insertBot(t, ctx, "bot_in_disabled", testutil.UID, disabledSpace)

	w := doVerifyToken(t, s, map[string]string{"token": testutil.Token}, true)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Spaces           []string            `json:"spaces"`
		OwnedBotsBySpace map[string][]string `json:"owned_bots_by_space"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	// invariant 1: disabled space not in `spaces`
	assert.NotContains(t, resp.Spaces, disabledSpace,
		"disabled space (s.status=0) must not appear in spaces — v3.3.1 §A.1")

	// invariant 2: disabled space not a key in `owned_bots_by_space`
	require.NotNil(t, resp.OwnedBotsBySpace)
	assert.NotContains(t, resp.OwnedBotsBySpace, disabledSpace,
		"bots in a disabled space must not surface via owned_bots_by_space")
}

// v3.3.1 §A.2 (yujiawei P1 三审): when a user is an active member of more
// than the policy limit (100) of spaces, the over-fetch (LIMIT 101)
// detects the truncation and slices back to 100. The warn log is fired
// but we don't assert on log here (would require zap testcore); the
// behavioral guarantee tested is the slice length cap.
func TestQueryUserSpaceContext_SpacesTruncatedAtPolicyLimit(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	// seed 105 active spaces with the test user as an active member
	for i := 0; i < 105; i++ {
		sid := fmt.Sprintf("st_trunc_space_%03d", i)
		_, err := ctx.DB().InsertInto("space").
			Columns("space_id", "name", "creator", "status").
			Values(sid, "T"+sid, testutil.UID, 1).Exec()
		require.NoError(t, err)
		_, err = ctx.DB().InsertInto("space_member").
			Columns("space_id", "uid", "role", "status").
			Values(sid, testutil.UID, 0, 1).Exec()
		require.NoError(t, err)
	}

	w := doVerifyToken(t, s, map[string]string{"token": testutil.Token}, true)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Spaces []string `json:"spaces"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.Spaces, 100,
		"spaces must be capped at policy limit (100), got %d", len(resp.Spaces))
}

// v3 §2.4 (yujiawei P2): a bot the caller owns that also lives in a space
// the caller is NOT a member of must not leak that foreign space_id into
// owned_bots_by_space. Otherwise the map disagrees with `spaces` and any
// consumer that uses the map as a derived authz cache (rather than
// re-checking `spaces`) gets a foreign key.
//
// Original TestAuthVerifyToken_WithInclude_FiltersInactive claimed to cover
// this but only seeded bots in caller-active spaces; this test adds the
// missing seed (bot in a space the caller doesn't belong to).
func TestAuthVerifyToken_WithInclude_DoesNotLeakForeignSpace(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedVerifyTokenFixtures(t, ctx)

	// foreign space: exists + active, but caller is NOT a member
	const foreignSpace = "verify_token_space_foreign"
	_, err := ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(foreignSpace, "Foreign", "someone_else", 1).Exec()
	require.NoError(t, err)
	// (no insert into space_member for testutil.UID into foreignSpace)

	// caller owns bot_shared; bot_shared is a member of SpaceA (caller's
	// space) AND foreignSpace (caller is NOT a member). After v3 §2.4
	// fix, foreignSpace must not appear in owned_bots_by_space.
	insertBot(t, ctx, "bot_shared", testutil.UID, testVerifyTokenSpaceA)
	_, err = ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(foreignSpace, "bot_shared", 0, 1).Exec()
	require.NoError(t, err)

	w := doVerifyToken(t, s, map[string]string{"token": testutil.Token}, true)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Spaces           []string            `json:"spaces"`
		OwnedBotsBySpace map[string][]string `json:"owned_bots_by_space"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	// invariant 1: spaces is the caller's *own* membership only
	assert.ElementsMatch(t, []string{testVerifyTokenSpaceA, testVerifyTokenSpaceB}, resp.Spaces)
	assert.NotContains(t, resp.Spaces, foreignSpace, "foreign space must not appear in spaces")

	// invariant 2: owned_bots_by_space stays in lockstep with spaces —
	// no key for foreignSpace, bot_shared appears only under SpaceA
	require.NotNil(t, resp.OwnedBotsBySpace)
	assert.NotContains(t, resp.OwnedBotsBySpace, foreignSpace,
		"owned_bots_by_space must not leak foreign space_id (v3 §2.4)")
	assert.Contains(t, resp.OwnedBotsBySpace[testVerifyTokenSpaceA], "bot_shared",
		"bot in caller's space should still appear")
}

// v3.3.5 regression — `?include=context` MUST still populate top-level
// `OwnedBots`. matter's applyUserResult builds `related_uids` from
// top-level `OwnedBots` regardless of `ContextIncluded`, and that feeds
// the matter-access gate (canAccessMatter → isCreator/HasAccess with
// CallerUIDs IN ?). v3.3.4 attempted to skip this load on the context
// path as a "pure latency win" — yujiawei caught it as a fail-closed
// regression that broke user access to matters created by their own
// bots.
//
// **Note: this test is independent on purpose**, NOT a patch of
// TestAuthVerifyToken_WithInclude_ReturnsSpacesAndOwnedBotsMap (line 85).
// That test's resp struct does not declare an `OwnedBots` field, which
// is exactly why v3.3.4 N1 silent-PASSed there. Patching the existing
// test to add OwnedBots would change its assertion semantics; we add
// an explicit regression test instead.
func TestAuthVerifyToken_WithInclude_StillReturnsTopLevelOwnedBots(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedVerifyTokenFixtures(t, ctx)
	insertBot(t, ctx, "bot_a_1", testutil.UID, testVerifyTokenSpaceA)
	insertBot(t, ctx, "bot_b_1", testutil.UID, testVerifyTokenSpaceB)
	// insertBot helper only writes robot + space_member; the OwnedBots
	// SQL is `SELECT ... FROM robot r INNER JOIN user u ON r.robot_id=u.uid`,
	// so we must also seed a user row per bot or the join returns empty
	// (the historical reason existing tests didn't catch v3.3.4 N1 — they
	// asserted only OwnedBotsBySpace, whose SQL doesn't INNER JOIN user).
	for _, botUID := range []string{"bot_a_1", "bot_b_1"} {
		_, err := ctx.DB().InsertInto("user").
			Columns("uid", "name", "robot", "short_no").
			Values(botUID, botUID+"_name", 1, botUID+"_sn").Exec()
		require.NoError(t, err)
	}

	w := doVerifyToken(t, s, map[string]string{"token": testutil.Token}, true)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		UID             string `json:"uid"`
		ContextIncluded bool   `json:"context_included"`
		OwnedBots       []struct {
			UID  string `json:"uid"`
			Name string `json:"name"`
		} `json:"owned_bots"`
		OwnedBotsBySpace map[string][]string `json:"owned_bots_by_space"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.ContextIncluded, "context_included MUST be true on ?include=context")

	// Top-level OwnedBots must be populated (the v3.3.4 N1 regression).
	require.NotEmpty(t, resp.OwnedBots,
		"v3.3.5 regression (yujiawei v3.3.4 P1): top-level owned_bots MUST be populated on ?include=context — matter applyUserResult reads it for related_uids")
	gotBots := make(map[string]bool, len(resp.OwnedBots))
	for _, b := range resp.OwnedBots {
		gotBots[b.UID] = true
	}
	assert.True(t, gotBots["bot_a_1"], "bot_a_1 must appear in top-level OwnedBots")
	assert.True(t, gotBots["bot_b_1"], "bot_b_1 must appear in top-level OwnedBots")

	// Sanity: OwnedBotsBySpace is also populated (both fields valid on
	// the context path).
	require.NotNil(t, resp.OwnedBotsBySpace)
	assert.Contains(t, resp.OwnedBotsBySpace, testVerifyTokenSpaceA)
	assert.Contains(t, resp.OwnedBotsBySpace, testVerifyTokenSpaceB)
}

// v3.3.6 §P2#3 regression — yujiawei R2 P2#3: ?include=context DB-error
// fail-secure contract. authVerifyToken on context-query err MUST return:
//   - HTTP 200 (not 500)
//   - context_included = true (downstream fleet/matter use this as
//     fail-closed-vs-fallback discriminator; flipping to false would
//     silently downgrade them to pre-v2 fallback and re-open X-Space-Id
//     trust)
//   - spaces = empty list, owned_bots_by_space = empty non-nil map
//
// Trigger: RENAME the space_member table away (atomic + defer-restore
// keeps schema intact). DROP TABLE would break subsequent tests because
// CleanAllTables only TRUNCATEs, not recreates schema.
func TestAuthVerifyToken_IncludeContext_DBError_FailSecure(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedVerifyTokenFixtures(t, ctx)

	// Hide robot so the bots query inside queryUserSpaceContext fails
	// (table not found). Cannot RENAME space_member: the token-path
	// authVerifyToken doesn't have a separate membership-check step but
	// queryUserSpaceContext's spaces query joins space_member — RENAME
	// space_member would still fail-secure, but so would the spaces
	// query. RENAME robot fails only the bots query, exercising the
	// same fail-secure branch via err return from queryUserSpaceContext.
	// RENAME is atomic; defer restore so no test pollution.
	_, err := ctx.DB().Exec("RENAME TABLE robot TO robot_tmp_v336_token")
	require.NoError(t, err)
	defer func() {
		_, _ = ctx.DB().Exec("RENAME TABLE robot_tmp_v336_token TO robot")
	}()

	w := doVerifyToken(t, s, map[string]string{"token": testutil.Token}, true)
	require.Equal(t, http.StatusOK, w.Code,
		"DB-err on context query MUST NOT 500 (v3.3.6 §P2#3 fail-secure); body: %s", w.Body.String())

	var resp struct {
		UID              string              `json:"uid"`
		ContextIncluded  bool                `json:"context_included"`
		Spaces           []string            `json:"spaces"`
		OwnedBotsBySpace map[string][]string `json:"owned_bots_by_space"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.True(t, resp.ContextIncluded,
		"context_included MUST stay true on DB err — flipping to false would silently downgrade fleet/matter to pre-v2 fallback (opens X-Space-Id trust)")
	// Note: `omitempty` on Spaces/OwnedBotsBySpace makes empty slices/maps
	// vanish on wire; client unmarshal sees nil. That's fine — both nil
	// and empty are semantically "no spaces / no bots", authz stays
	// fail-closed (range over nil = 0 iterations). assert.Empty accepts
	// both nil and zero-length.
	assert.Empty(t, resp.Spaces, "spaces MUST be empty (nil or len 0) on DB err")
	assert.Empty(t, resp.OwnedBotsBySpace, "owned_bots_by_space MUST be empty on DB err")
}
