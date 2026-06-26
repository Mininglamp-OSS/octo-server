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

// authVerifyAPIKey: POST /v1/auth/verify-api-key
// 15 test cases covering: valid / unknown / owner-left-space /
// non-active-membership / legacy-empty-space / disabled-space /
// missing-field / multi-space / no-include (default shape) /
// with-include (owned_bots map) / owned-bots only-in-bound-space /
// owned-bots empty / owned-bots filters-disabled-bot /
// account-banned (v3.3.6 §P1) / include-context DB-error fail-secure.

const (
	testAPIKeySpaceA = "verify_apikey_space_a"
	testAPIKeySpaceB = "verify_apikey_space_b"
	testAPIKeyUID    = "verify_apikey_uid_001"
)

func seedAPIKeyFixtures(t *testing.T, ctx *config.Context) {
	t.Helper()
	// v3.3.6 §P1: INSERT user row (status=1) is required since the daemon
	// api_key membership SQL now joins `user` ON u.status=1. Without it,
	// happy-path tests would 401. INSERT IGNORE keeps the helper idempotent
	// if testutil.NewTestServer or another helper already seeded the row.
	_, err := ctx.DB().Exec(
		"INSERT IGNORE INTO `user` (uid, name, status, short_no) VALUES (?, ?, ?, ?)",
		testAPIKeyUID, "apikey_test_user", 1, "apikey_test_sn",
	)
	require.NoError(t, err)
	// Two spaces, the test uid is an active member of both.
	for _, sid := range []string{testAPIKeySpaceA, testAPIKeySpaceB} {
		_, err := ctx.DB().InsertInto("space").
			Columns("space_id", "name", "creator", "status").
			Values(sid, "Test "+sid, testAPIKeyUID, 1).Exec()
		require.NoError(t, err)

		_, err = ctx.DB().InsertInto("space_member").
			Columns("space_id", "uid", "role", "status").
			Values(sid, testAPIKeyUID, 0, 1).Exec()
		require.NoError(t, err)
	}
}

func insertAPIKey(t *testing.T, ctx *config.Context, uid, apiKey, spaceID string) {
	t.Helper()
	_, err := ctx.DB().InsertInto("user_api_key").
		Columns("uid", "api_key", "space_id").
		Values(uid, apiKey, spaceID).Exec()
	require.NoError(t, err)
}

// insertAPIKeyFull inserts a user_api_key row with explicit status/client_id
// (the 3-col insertAPIKey relies on the column DEFAULTs status=1 /
// client_id='botfather'). Used to seed revoked keys and legacy plaintext
// non-botfather rows the daemon path must reject.
func insertAPIKeyFull(t *testing.T, ctx *config.Context, uid, apiKey, spaceID string, status int, clientID string) {
	t.Helper()
	_, err := ctx.DB().InsertInto("user_api_key").
		Columns("uid", "api_key", "space_id", "status", "client_id").
		Values(uid, apiKey, spaceID, status, clientID).Exec()
	require.NoError(t, err)
}

func doVerifyAPIKey(t *testing.T, s *server.Server, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var reqBody *bytes.Reader
	if body != nil {
		reqBody = bytes.NewReader([]byte(util.ToJson(body)))
	} else {
		reqBody = bytes.NewReader(nil)
	}
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/auth/verify-api-key", reqBody)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	return w
}

// Case 1: 有效 api_key → 200 返 uid + space_id
func TestAuthVerifyAPIKey_Valid(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	insertAPIKey(t, ctx, testAPIKeyUID, "uk_valid_test_key_abc12345678901234567", testAPIKeySpaceA)

	w := doVerifyAPIKey(t, s, map[string]string{"api_key": "uk_valid_test_key_abc12345678901234567"})

	require.Equal(t, http.StatusOK, w.Code)
	// c.Response 直接序列化 data, 不 wrap (octo-lib wkhttp.Context).
	var resp struct {
		UID     string `json:"uid"`
		SpaceID string `json:"space_id"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, testAPIKeyUID, resp.UID)
	assert.Equal(t, testAPIKeySpaceA, resp.SpaceID)
}

// Case 2: 不存在的 api_key → 401
func TestAuthVerifyAPIKey_Unknown(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	// no INSERT — api_key 不存在

	w := doVerifyAPIKey(t, s, map[string]string{"api_key": "uk_never_existed_xxxxxxxxxxxxxxxxxxxx"})

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// Case 3: api_key 存在但 owner 已退出 space (status=0) → 401
func TestAuthVerifyAPIKey_OwnerLeftSpace(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	insertAPIKey(t, ctx, testAPIKeyUID, "uk_owner_left_xxxxxxxxxxxxxxxxxxxxxxx", testAPIKeySpaceA)

	// flip status: owner 退出 space
	_, err := ctx.DB().Update("space_member").
		Set("status", 0).
		Where("space_id=? AND uid=?", testAPIKeySpaceA, testAPIKeyUID).
		Exec()
	require.NoError(t, err)

	w := doVerifyAPIKey(t, s, map[string]string{"api_key": "uk_owner_left_xxxxxxxxxxxxxxxxxxxxxxx"})

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// Case 3b: space_member.status 不是 1 也不是 0 (其他值, 如 2 "pending") →
// 同样 401. SQL filter 是 `status=1`, 任何非 1 值都被拒, 这个 case 提供
// 显式覆盖避免 future 改成 `status != 0` 引入 regression.
func TestAuthVerifyAPIKey_NonActiveStatus(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	insertAPIKey(t, ctx, testAPIKeyUID, "uk_nonactive_xxxxxxxxxxxxxxxxxxxxxxxx", testAPIKeySpaceA)

	// flip status to 2 (e.g. "pending invitation" — 任何非 1 值)
	_, err := ctx.DB().Update("space_member").
		Set("status", 2).
		Where("space_id=? AND uid=?", testAPIKeySpaceA, testAPIKeyUID).
		Exec()
	require.NoError(t, err)

	w := doVerifyAPIKey(t, s, map[string]string{"api_key": "uk_nonactive_xxxxxxxxxxxxxxxxxxxxxxxx"})

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// Case 3c: user_api_key.status=0 (revoked key) → 401. Distinct from the
// space_member.status cases above — this gates the KEY's own revocation
// flag (added with the client-dimension migration). Without the SQL
// `status=1` filter a revoked daemon key keeps verifying.
func TestAuthVerifyAPIKey_RevokedKey_401(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	insertAPIKeyFull(t, ctx, testAPIKeyUID, "uk_revoked_xxxxxxxxxxxxxxxxxxxxxxxxxx", testAPIKeySpaceA, 0, "botfather")

	w := doVerifyAPIKey(t, s, map[string]string{"api_key": "uk_revoked_xxxxxxxxxxxxxxxxxxxxxxxxxx"})

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// Case 3d: a legacy plaintext row owned by a non-botfather client → 401.
// verify-api-key is the native daemon path and must only accept botfather
// keys; integration-client keys belong to external apps. Seeded as a
// PLAINTEXT row (not via the service, which stores integration keys as a
// "hash:" digest) precisely so the api_key=? match would still hit — only
// the client_id='botfather' filter rejects it, proving that filter works.
func TestAuthVerifyAPIKey_NonBotfatherClient_401(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	insertAPIKeyFull(t, ctx, testAPIKeyUID, "uk_integration_xxxxxxxxxxxxxxxxxxxxxx", testAPIKeySpaceA, 1, "some-integration")

	w := doVerifyAPIKey(t, s, map[string]string{"api_key": "uk_integration_xxxxxxxxxxxxxxxxxxxxxx"})

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// Case 4: legacy space_id="" api_key -> 401 (merge plan section 3 rejects it)
func TestAuthVerifyAPIKey_LegacyEmptySpace(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	insertAPIKey(t, ctx, testAPIKeyUID, "uk_legacy_no_space_xxxxxxxxxxxxxxxxx", "")

	w := doVerifyAPIKey(t, s, map[string]string{"api_key": "uk_legacy_no_space_xxxxxxxxxxxxxxxxx"})

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// v3.3.3 §D (yujiawei v3.3.1 stale review #2 三审): api_key path 的 disabled-
// space 修 (v3 §2.3 加 `INNER JOIN space ON s.status=1`) 没回归 test.
// 攻击形态: space 已 soft-delete (s.status=0) 但 space_member.status=1
// 残留 — api_key 仍然能 verify 通过, 给 fleet/matter 一个 disabled space
// 的 valid uid+space context. 跟 token-path TestAuthVerifyToken_
// WithInclude_FiltersDisabledSpace 同模板, 这里 api_key path 同款锁住.
func TestAuthVerifyAPIKey_DisabledSpace_401(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	const disabledSpace = "verify_apikey_disabled_space"
	const disabledKey = "uk_disabled_space_aaaaaaaaaaaaaaaaaa"

	// seed: space.status=0 (disabled) + space_member.status=1 (active member)
	_, err := ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(disabledSpace, "Disabled", testAPIKeyUID, 0). // ← status=0
		Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(disabledSpace, testAPIKeyUID, 0, 1). // ← member still active
		Exec()
	require.NoError(t, err)
	insertAPIKey(t, ctx, testAPIKeyUID, disabledKey, disabledSpace)

	w := doVerifyAPIKey(t, s, map[string]string{"api_key": disabledKey})

	assert.Equal(t, http.StatusUnauthorized, w.Code,
		"api_key bound to a disabled space (s.status=0) must 401 — v3.3.3 §D")
}

// Case 5: 缺 api_key 字段 → 400 (respondUserTokenRequired 走 400 系列)
func TestAuthVerifyAPIKey_MissingField(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)

	w := doVerifyAPIKey(t, s, map[string]string{})

	assert.NotEqual(t, http.StatusOK, w.Code)
	assert.Less(t, w.Code, http.StatusInternalServerError, "缺字段应 4xx, 不应是 5xx")
}

// Case 6: 同一 user 在两个 space 各有 api_key → 各自返回对应 space_id
func TestAuthVerifyAPIKey_MultiSpace(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	keyA := "uk_multi_space_a_xxxxxxxxxxxxxxxxxxx"
	keyB := "uk_multi_space_b_xxxxxxxxxxxxxxxxxxx"
	insertAPIKey(t, ctx, testAPIKeyUID, keyA, testAPIKeySpaceA)
	insertAPIKey(t, ctx, testAPIKeyUID, keyB, testAPIKeySpaceB)

	for _, tc := range []struct {
		apiKey  string
		wantSID string
	}{
		{keyA, testAPIKeySpaceA},
		{keyB, testAPIKeySpaceB},
	} {
		w := doVerifyAPIKey(t, s, map[string]string{"api_key": tc.apiKey})
		require.Equal(t, http.StatusOK, w.Code, "api_key=%s", tc.apiKey)
		var resp struct {
			UID     string `json:"uid"`
			SpaceID string `json:"space_id"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, testAPIKeyUID, resp.UID)
		assert.Equal(t, tc.wantSID, resp.SpaceID, "api_key=%s 应返回 %s", tc.apiKey, tc.wantSID)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// ?include=context opt-in tests (v2 鉴权关系数据补全).
//
// fleet/matter middleware passes ?include=context to fetch owned_bots map;
// other callers (none today, but defensively) keep the original response
// schema by omitting the query param.
// ─────────────────────────────────────────────────────────────────────────

// doVerifyAPIKeyCtx is the helper variant that adds ?include=context.
func doVerifyAPIKeyCtx(t *testing.T, s *server.Server, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	reqBody := bytes.NewReader([]byte(util.ToJson(body)))
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/auth/verify-api-key?include=context", reqBody)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	return w
}

// insertBot creates a robot row + space_member row so it appears in
// owned_bots queries. bot is itself a user — its space membership lives in
// space_member, robot table has no space_id (see botfather/db.go:71).
func insertBot(t *testing.T, ctx *config.Context, botUID, creatorUID, spaceID string) {
	t.Helper()
	_, err := ctx.DB().InsertInto("robot").
		Columns("robot_id", "creator_uid", "status").
		Values(botUID, creatorUID, 1).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(spaceID, botUID, 0, 1).Exec()
	require.NoError(t, err)
}

// BC test: omit ?include and the response schema must stay identical to the
// pre-v2 contract (uid + space_id only, no owned_bots key). This is the red
// line — any change to default behavior breaks fleet/matter rollback paths.
func TestAuthVerifyAPIKey_NoInclude_NoOwnedBotsField(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	insertAPIKey(t, ctx, testAPIKeyUID, "uk_bc_default_xxxxxxxxxxxxxxxxxxxxxxx", testAPIKeySpaceA)
	insertBot(t, ctx, "bot_in_a_1", testAPIKeyUID, testAPIKeySpaceA)

	w := doVerifyAPIKey(t, s, map[string]string{"api_key": "uk_bc_default_xxxxxxxxxxxxxxxxxxxxxxx"})
	require.Equal(t, http.StatusOK, w.Code)

	// Raw JSON check — must NOT contain "owned_bots" when include not requested.
	body := w.Body.String()
	assert.NotContains(t, body, "owned_bots", "BC: default schema must not include owned_bots")
	assert.NotContains(t, body, "context_included",
		"v3.3.3 §F: BC default schema must not include context_included (omitempty drops false)")
	assert.Contains(t, body, `"uid"`)
	assert.Contains(t, body, `"space_id"`)
}

// With ?include=context the response contains owned_bots map keyed by the
// api_key's bound space, listing bot_uids the user owns in that space.
func TestAuthVerifyAPIKey_WithInclude_ReturnsOwnedBotsMap(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	insertAPIKey(t, ctx, testAPIKeyUID, "uk_ctx_owned_xxxxxxxxxxxxxxxxxxxxxxxx", testAPIKeySpaceA)
	insertBot(t, ctx, "bot_a_1", testAPIKeyUID, testAPIKeySpaceA)
	insertBot(t, ctx, "bot_a_2", testAPIKeyUID, testAPIKeySpaceA)

	w := doVerifyAPIKeyCtx(t, s, map[string]string{"api_key": "uk_ctx_owned_xxxxxxxxxxxxxxxxxxxxxxxx"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		UID             string              `json:"uid"`
		SpaceID         string              `json:"space_id"`
		ContextIncluded bool                `json:"context_included"`
		OwnedBots       map[string][]string `json:"owned_bots"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, testAPIKeyUID, resp.UID)
	assert.Equal(t, testAPIKeySpaceA, resp.SpaceID)
	assert.True(t, resp.ContextIncluded,
		"v3.3.3 §F: context_included MUST be true on ?include=context — discriminator that downstream uses for fail-closed vs fallback")
	require.NotNil(t, resp.OwnedBots)
	require.Len(t, resp.OwnedBots, 1, "api_key bound to one space → exactly one key in owned_bots map")
	assert.ElementsMatch(t, []string{"bot_a_1", "bot_a_2"}, resp.OwnedBots[testAPIKeySpaceA])
}

// Cross-space leak guard: bot the user created in SpaceB must NOT appear
// when api_key is bound to SpaceA. This is the core safety property — if it
// fails, an api_key for SpaceA could enumerate / claim bots in SpaceB.
func TestAuthVerifyAPIKey_OwnedBots_OnlyInBoundSpace(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	insertAPIKey(t, ctx, testAPIKeyUID, "uk_crossspace_xxxxxxxxxxxxxxxxxxxxxxx", testAPIKeySpaceA)
	insertBot(t, ctx, "bot_in_a", testAPIKeyUID, testAPIKeySpaceA)
	insertBot(t, ctx, "bot_in_b", testAPIKeyUID, testAPIKeySpaceB) // 跨 space, 不该出现

	w := doVerifyAPIKeyCtx(t, s, map[string]string{"api_key": "uk_crossspace_xxxxxxxxxxxxxxxxxxxxxxx"})
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		OwnedBots map[string][]string `json:"owned_bots"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.ElementsMatch(t, []string{"bot_in_a"}, resp.OwnedBots[testAPIKeySpaceA])
	_, hasB := resp.OwnedBots[testAPIKeySpaceB]
	assert.False(t, hasB, "bot_in_b in SpaceB must not leak through SpaceA-bound api_key")
}

// User has no bots → owned_bots map still present, with the bound space
// key pointing to an empty list (not nil/missing — caller can assume map
// shape).
func TestAuthVerifyAPIKey_OwnedBots_Empty(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	insertAPIKey(t, ctx, testAPIKeyUID, "uk_nobots_xxxxxxxxxxxxxxxxxxxxxxxxxxx", testAPIKeySpaceA)
	// no insertBot calls

	w := doVerifyAPIKeyCtx(t, s, map[string]string{"api_key": "uk_nobots_xxxxxxxxxxxxxxxxxxxxxxxxxxx"})
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		OwnedBots map[string][]string `json:"owned_bots"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotNil(t, resp.OwnedBots)
	require.Contains(t, resp.OwnedBots, testAPIKeySpaceA)
	assert.Empty(t, resp.OwnedBots[testAPIKeySpaceA])
}

// Disabled bot (status=0) must not leak through owned_bots — admin disable
// is the kill switch and shouldn't be bypassable via api_key listing.
func TestAuthVerifyAPIKey_OwnedBots_FiltersDisabled(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	insertAPIKey(t, ctx, testAPIKeyUID, "uk_disabled_xxxxxxxxxxxxxxxxxxxxxxxxx", testAPIKeySpaceA)
	insertBot(t, ctx, "bot_active", testAPIKeyUID, testAPIKeySpaceA)
	insertBot(t, ctx, "bot_disabled", testAPIKeyUID, testAPIKeySpaceA)
	// flip bot_disabled status to 0
	_, err := ctx.DB().Update("robot").Set("status", 0).
		Where("robot_id=?", "bot_disabled").Exec()
	require.NoError(t, err)

	w := doVerifyAPIKeyCtx(t, s, map[string]string{"api_key": "uk_disabled_xxxxxxxxxxxxxxxxxxxxxxxxx"})
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		OwnedBots map[string][]string `json:"owned_bots"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.ElementsMatch(t, []string{"bot_active"}, resp.OwnedBots[testAPIKeySpaceA])
}

// v3.3.6 §P1 regression — yujiawei R2 P1: account ban (user.status=0)
// MUST revoke daemon api_key. Pre-v3.3.6 the verify-api-key SQL only
// joined space_member + space, not user, so a globally banned user
// kept fully valid daemon credentials. liftBanUser
// (modules/user/api_manager.go:909) sets user.status=0 + QuitUserDevice;
// the redis token cache clear handles the session-token path, but
// daemon api_key sits behind no such cache → must be SQL-gated.
func TestAuthVerifyAPIKey_AccountBanned_401(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	insertAPIKey(t, ctx, testAPIKeyUID, "uk_banned_aaaaaaaaaaaaaaaaaaaaaaaa", testAPIKeySpaceA)

	// Ban the user (mirrors liftBanUser path: user.status=0, leave
	// space_member.status=1 to exercise the exact gap the P1 closes).
	_, err := ctx.DB().Update("user").
		Set("status", 0).
		Where("uid=?", testAPIKeyUID).
		Exec()
	require.NoError(t, err)

	w := doVerifyAPIKey(t, s, map[string]string{"api_key": "uk_banned_aaaaaaaaaaaaaaaaaaaaaaaa"})
	assert.Equal(t, http.StatusUnauthorized, w.Code,
		"v3.3.6 §P1 (yujiawei R2): banned user's daemon api_key MUST be revoked (user.status=0 → 401)")
}

// v3.3.6 §P2#3 regression — yujiawei R2 P2#3: ?include=context DB-error
// fail-secure contract. authVerifyAPIKey on context-query err MUST return:
//   - HTTP 200 (not 500)
//   - context_included = true (downstream fleet/matter use this as
//     fail-closed-vs-fallback discriminator; flipping to false would
//     silently downgrade them to pre-v2 fallback and re-open X-Space-Id
//     trust)
//   - owned_bots = empty non-nil map
//
// Trigger: RENAME the space_member table away (atomic + defer-restore
// keeps schema intact for subsequent tests in the same package run).
// DROP TABLE would also work but CleanAllTables only TRUNCATEs and does
// not recreate schema → DROP would break every subsequent test in the
// package.
func TestAuthVerifyAPIKey_IncludeContext_DBError_FailSecure(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	insertAPIKey(t, ctx, testAPIKeyUID, "uk_dberr_aaaaaaaaaaaaaaaaaaaaaaaaa", testAPIKeySpaceA)

	// Hide robot so queryOwnedBotsBySpace fails (table not found), while
	// step 2 membership check (space_member + space + user) still PASSes —
	// otherwise step 2 would 401 early and we'd never reach the step 3
	// fail-secure path. RENAME is atomic; defer restore so no test
	// pollution.
	_, err := ctx.DB().Exec("RENAME TABLE robot TO robot_tmp_v336_apikey")
	require.NoError(t, err)
	defer func() {
		_, _ = ctx.DB().Exec("RENAME TABLE robot_tmp_v336_apikey TO robot")
	}()

	w := doVerifyAPIKeyCtx(t, s, map[string]string{"api_key": "uk_dberr_aaaaaaaaaaaaaaaaaaaaaaaaa"})
	require.Equal(t, http.StatusOK, w.Code,
		"DB-err on context query MUST NOT 500 (v3.3.6 §P2#3 fail-secure); body: %s", w.Body.String())

	var resp struct {
		UID             string              `json:"uid"`
		SpaceID         string              `json:"space_id"`
		ContextIncluded bool                `json:"context_included"`
		OwnedBots       map[string][]string `json:"owned_bots"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.True(t, resp.ContextIncluded,
		"context_included MUST stay true on DB err — flipping to false would silently downgrade fleet/matter to pre-v2 fallback (opens X-Space-Id trust)")
	// fail-secure contract: OwnedBots returns map with the bound space
	// key but empty value list (not entirely empty map — the api_key is
	// always bound to exactly one space, so the key is the API contract).
	require.NotNil(t, resp.OwnedBots, "owned_bots map MUST be present on context path even on err")
	require.Contains(t, resp.OwnedBots, testAPIKeySpaceA, "bound space key MUST be present")
	assert.Empty(t, resp.OwnedBots[testAPIKeySpaceA], "bot list under bound space MUST be empty on DB err")
}
