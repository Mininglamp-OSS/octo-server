package bot_provision_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/Mininglamp-OSS/octo-server/internal"
)

// TestMain installs a stable 32-byte OCTO_MASTER_KEY so the `common`
// module (registered via the internal blank import) doesn't panic during
// its Route() setup. user_test files don't need this because they import
// modules/user directly which doesn't pull common; bot_provision_test
// pulls the full registry so common.Route runs.
func TestMain(m *testing.M) {
	const stableTestMasterKey = "0123456789abcdef0123456789abcdef"
	if os.Getenv("OCTO_MASTER_KEY") == "" {
		os.Setenv("OCTO_MASTER_KEY", stableTestMasterKey)
	}
	os.Exit(m.Run())
}

// GET /v1/bot/:uid/token (v3 §2.2)
// Daemon-callable endpoint that returns a bot_token given a valid api_key.
// Authz invariants enforced inline (Bearer prefix + resolveAPIKey two-step +
// space_member join for cross-space + r.status=1 + creator==caller):
//
//   1. valid api_key + caller is bot's creator + bot in caller's space  → 200
//   2. valid api_key + bot creator is a different user (same space)     → 403
//   3. valid api_key for SpaceB + bot lives only in SpaceA              → 404 (space join fails)
//   4. no Authorization header                                          → 401
//   5. api_key.space_id='' (legacy)                                     → 401 (resolveAPIKey filter)
//   6. bot.status=0 (admin-disabled)                                    → 404 (status=1 filter)
//   7. bot does not exist                                               → 404
//
// 404 (not 403) for cross-space / disabled / nonexistent keeps the same
// signal an outsider would see for a wrong api_key, denying probe-based
// enumeration. 403 is reserved for "caller is valid but explicitly not
// authorized" inside the caller's own space (creator mismatch).

const (
	bpTestSpaceA = "bp_space_a"
	bpTestSpaceB = "bp_space_b"
	bpTestUIDA   = "bp_uid_alice"
	bpTestUIDB   = "bp_uid_bob"
)

// seedBPFixtures installs two spaces, two users, with active memberships
// where Alice is in SpaceA, Bob is in SpaceA and SpaceB. Used by all
// bot_provision tests.
func seedBPFixtures(t *testing.T, ctx *config.Context) {
	t.Helper()
	// spaces
	for _, sid := range []string{bpTestSpaceA, bpTestSpaceB} {
		_, err := ctx.DB().InsertInto("space").
			Columns("space_id", "name", "creator", "status").
			Values(sid, "Test "+sid, bpTestUIDA, 1).Exec()
		require.NoError(t, err)
	}
	// Alice ∈ SpaceA only
	_, err := ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(bpTestSpaceA, bpTestUIDA, 0, 1).Exec()
	require.NoError(t, err)
	// Bob ∈ SpaceA and SpaceB (so Bob's api_key can be bound to either)
	for _, sid := range []string{bpTestSpaceA, bpTestSpaceB} {
		_, err = ctx.DB().InsertInto("space_member").
			Columns("space_id", "uid", "role", "status").
			Values(sid, bpTestUIDB, 0, 1).Exec()
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

// insertBotInSpace creates a robot row (creator, bot_token, status) and a
// space_member row putting the bot in spaceID. Mirrors botfather/db.go's
// MintBotOBO write pattern at the SQL level so tests bypass the OBO logic.
func insertBotInSpace(t *testing.T, ctx *config.Context, botUID, creatorUID, botToken, spaceID string, status int) {
	t.Helper()
	_, err := ctx.DB().InsertInto("robot").
		Columns("robot_id", "creator_uid", "bot_token", "status").
		Values(botUID, creatorUID, botToken, status).Exec()
	require.NoError(t, err)
	if spaceID != "" {
		_, err = ctx.DB().InsertInto("space_member").
			Columns("space_id", "uid", "role", "status").
			Values(spaceID, botUID, 0, 1).Exec()
		require.NoError(t, err)
	}
}

func doBotToken(t *testing.T, s *server.Server, botUID, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/bot/"+botUID+"/token", nil)
	require.NoError(t, err)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	s.GetRoute().ServeHTTP(w, req)
	return w
}

// 1) valid api_key + caller is bot's creator + bot is in caller's space
func TestBotToken_Valid_ReturnsBotToken(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedBPFixtures(t, ctx)
	insertAPIKey(t, ctx, bpTestUIDA, "uk_bp_valid_aaaaaaaaaaaaaaaaaaaaaaaa", bpTestSpaceA)
	insertBotInSpace(t, ctx, "bot_alice_1", bpTestUIDA, "bf_tok_alice_1", bpTestSpaceA, 1)

	w := doBotToken(t, s, "bot_alice_1", "uk_bp_valid_aaaaaaaaaaaaaaaaaaaaaaaa")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	body := w.Body.String()
	assert.Contains(t, body, `"bot_uid":"bot_alice_1"`)
	assert.Contains(t, body, `"bot_token":"bf_tok_alice_1"`)
}

// 2) valid api_key but caller is not the bot's creator (same space)
func TestBotToken_WrongOwner_403(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedBPFixtures(t, ctx)
	insertAPIKey(t, ctx, bpTestUIDA, "uk_bp_wo_aaaaaaaaaaaaaaaaaaaaaaaaaa", bpTestSpaceA)
	// bot's creator is Bob (Bob also in SpaceA), Alice's api_key trying to fetch
	insertBotInSpace(t, ctx, "bot_bob_1", bpTestUIDB, "bf_tok_bob_1", bpTestSpaceA, 1)

	w := doBotToken(t, s, "bot_bob_1", "uk_bp_wo_aaaaaaaaaaaaaaaaaaaaaaaaaa")
	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
}

// 3) cross-space attack: api_key bound to SpaceB, bot lives only in SpaceA.
// The space_member join (v2 fix) drops the row → 404 (not 403) to avoid
// leaking existence of bots in spaces the caller isn't a member of.
func TestBotToken_CrossSpace_404(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedBPFixtures(t, ctx)
	// Bob has api_key in SpaceB; bot belongs to SpaceA only (creator also Bob
	// to isolate the cross-space behavior from the creator-mismatch case).
	insertAPIKey(t, ctx, bpTestUIDB, "uk_bp_cs_bbbbbbbbbbbbbbbbbbbbbbbbbb", bpTestSpaceB)
	insertBotInSpace(t, ctx, "bot_bob_a_only", bpTestUIDB, "bf_tok_bob_a", bpTestSpaceA, 1)

	w := doBotToken(t, s, "bot_bob_a_only", "uk_bp_cs_bbbbbbbbbbbbbbbbbbbbbbbbbb")
	assert.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
}

// 4) no Authorization header → 401 (Bearer prefix required)
func TestBotToken_NoBearer_401(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedBPFixtures(t, ctx)
	insertBotInSpace(t, ctx, "bot_nobearer", bpTestUIDA, "bf_tok_nb", bpTestSpaceA, 1)

	w := doBotToken(t, s, "bot_nobearer", "" /* no bearer */)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
}

// 5) api_key.space_id='' (legacy pre-v2 rows) → 401 (resolveAPIKey filters)
func TestBotToken_LegacyEmptySpace_401(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedBPFixtures(t, ctx)
	insertAPIKey(t, ctx, bpTestUIDA, "uk_bp_legacy_xxxxxxxxxxxxxxxxxxxxxxxx", "")
	insertBotInSpace(t, ctx, "bot_legacy", bpTestUIDA, "bf_tok_lg", bpTestSpaceA, 1)

	w := doBotToken(t, s, "bot_legacy", "uk_bp_legacy_xxxxxxxxxxxxxxxxxxxxxxxx")
	assert.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
}

// 6) bot.status=0 (admin-disabled) → 404 — disabled bot must not leak token
// even to its rightful creator. status=1 filter is the kill switch.
func TestBotToken_BotDisabled_404(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedBPFixtures(t, ctx)
	insertAPIKey(t, ctx, bpTestUIDA, "uk_bp_dis_aaaaaaaaaaaaaaaaaaaaaaaaaa", bpTestSpaceA)
	insertBotInSpace(t, ctx, "bot_disabled", bpTestUIDA, "bf_tok_dis", bpTestSpaceA, 0 /* disabled */)

	w := doBotToken(t, s, "bot_disabled", "uk_bp_dis_aaaaaaaaaaaaaaaaaaaaaaaaaa")
	assert.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
}

// 7) bot does not exist → 404 (same shape as cross-space / disabled to avoid
// distinguishing "exists elsewhere" from "doesn't exist" via the daemon path)
func TestBotToken_NonexistentBot_404(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedBPFixtures(t, ctx)
	insertAPIKey(t, ctx, bpTestUIDA, "uk_bp_nx_aaaaaaaaaaaaaaaaaaaaaaaaaaa", bpTestSpaceA)
	// no insertBotInSpace

	w := doBotToken(t, s, "bot_does_not_exist", "uk_bp_nx_aaaaaaaaaaaaaaaaaaaaaaaaaaa")
	assert.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
}

// ─────────────────────────────────────────────────────────────────────
// 410 Gone stubs (v3.3.2 reduced to status + body only — Sunset/Deprecation
// headers dropped, see bot_api.go Route() comment). The v3.3.1 hardcoded
// dates were both wrong (Sunset said Fri but the date is Saturday;
// Deprecation @1749427200 resolves to 2025-06-09 not 2026), and there is
// no deploy-pipeline substitution to keep them fresh. Body still carries
// the actionable migration pointer.
// ─────────────────────────────────────────────────────────────────────

func doRaw(t *testing.T, s *server.Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest(method, path, nil)
	require.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	return w
}

func TestGone410_JWKS_ReturnsStatusAndBody(t *testing.T) {
	s, _ := testutil.NewTestServer()
	w := doRaw(t, s, "GET", "/.well-known/jwks.json")

	require.Equal(t, http.StatusGone, w.Code, "JWKS endpoint must respond 410")

	// v3.3.2: Sunset/Deprecation headers dropped — assert they are NOT
	// emitted. Locks the no-header decision in place so a future
	// "let's re-add Sunset" change has to update this test, and at that
	// point reviewer can confirm the constants are actually correct.
	assert.Empty(t, w.Header().Get("Sunset"), "Sunset header must NOT be set (v3.3.2 drop)")
	assert.Empty(t, w.Header().Get("Deprecation"), "Deprecation header must NOT be set (v3.3.2 drop)")

	body := w.Body.String()
	assert.Contains(t, body, "JWKS endpoint removed", "body must explain why + where to migrate")
	assert.Contains(t, body, "verify-api-key", "body must point at the replacement endpoint")
}

func TestGone410_TokenExchange_ReturnsStatusAndBody(t *testing.T) {
	s, _ := testutil.NewTestServer()
	w := doRaw(t, s, "POST", "/v1/auth/token")

	require.Equal(t, http.StatusGone, w.Code, "token exchange endpoint must respond 410")

	assert.Empty(t, w.Header().Get("Sunset"), "Sunset header must NOT be set (v3.3.2 drop)")
	assert.Empty(t, w.Header().Get("Deprecation"), "Deprecation header must NOT be set (v3.3.2 drop)")

	body := w.Body.String()
	assert.Contains(t, body, "Token exchange endpoint removed")
	assert.Contains(t, body, "Authorization: Bearer")
}
