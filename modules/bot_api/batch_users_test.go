package bot_api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// batch_users_test.go — POST /v1/bot/users/batch (bot, under the authenticated
// /v1/bot group; reuses bot auth / identity / business rate limiter).
//
// Wire-level only (raw JSON in, generic map out) so the file compiles and runs
// red on HEAD (route 404) before the handler exists and green after. Pins the
// batch contract on the bot surface: route + auth, request validation, input
// order, missing-uid reporting, non-enabled users (disabled / blacklisted) NOT
// reported present, and minimal-DTO PII stripping.

const (
	buRobotID  = "bot_bu_1"
	buBotToken = "bf_bu_token_1"
)

type rawBotBatchResp struct {
	Users       []map[string]json.RawMessage `json:"users"`
	MissingUIDs []string                     `json:"missing_uids"`
}

func setupBotBatchUsers(t *testing.T) (http.Handler, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	// POST /v1/bot/users/batch mounts an always-on per-IP strict bucket
	// (ratelimit:strict:bot_users_batch:*). httptest requests carry no client IP,
	// so they share the middleware's unknown-IP fallback bucket, which persists in
	// Redis across tests in this binary and is NOT cleared by CleanAllTables. Reset
	// it so each test starts from a full bucket (see rate-limit.md testing note).
	_ = ctx.GetRedisConn().Del("ratelimit:strict:bot_users_batch:__unknown_ip__")
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid, bot_token) VALUES (?, 1, ?, ?)",
		buRobotID, "owner_bu", buBotToken,
	).Exec()
	require.NoError(t, err)
	route := s.GetRoute()
	// Install the localized error renderer (mirrors production and the human batch
	// harness) so error responses carry the i18n `{"error":{...}}` envelope rather
	// than the bare default-renderer shape.
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	return route, ctx
}

// seedBotBatchUser inserts a user row carrying contact PII so the minimal-DTO
// stripping assertions can catch a leak. status: 0 disabled, 1 enabled, 2
// blacklist. robot: 0 human, 1 bot.
func seedBotBatchUser(t *testing.T, ctx *config.Context, uid, name string, status, robot int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, username, short_no, vercode, zone, phone, email, status, robot) "+
			"VALUES (?, ?, ?, ?, ?, '0086', '13800008888', ?, ?, ?)",
		uid, name, "un_"+uid, "sn_"+uid, "vc_"+uid, uid+"@example.com", status, robot,
	).Exec()
	require.NoError(t, err)
}

func postBotBatchUsers(handler http.Handler, token, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/bot/users/batch", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	handler.ServeHTTP(w, req)
	return w
}

func decodeBotBatchResp(t *testing.T, w *httptest.ResponseRecorder) rawBotBatchResp {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var b rawBotBatchResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &b))
	return b
}

func botBatchUserUID(t *testing.T, m map[string]json.RawMessage) string {
	t.Helper()
	var s string
	require.NoError(t, json.Unmarshal(m["uid"], &s))
	return s
}

func TestBotBatchUsers_RouteExistsAndRequiresAuth(t *testing.T) {
	handler, _ := setupBotBatchUsers(t)

	// No bot token: authBot must reject before the handler (401, not 404/200).
	w := postBotBatchUsers(handler, "", `{"uids":["u1"]}`)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "body=%s", w.Body.String())

	// Valid token, malformed body: route exists and validation runs (400).
	w = postBotBatchUsers(handler, buBotToken, "not-json")
	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
}

func TestBotBatchUsers_RequestValidation(t *testing.T) {
	handler, _ := setupBotBatchUsers(t)

	cases := map[string]string{
		"empty body":       "",
		"empty uids":       `{"uids":[]}`,
		"missing uids":     `{}`,
		"non-string uid":   `{"uids":[123]}`,
		"empty string uid": `{"uids":["a",""]}`,
		"duplicate uid":    `{"uids":["dup","dup"]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			w := postBotBatchUsers(handler, buBotToken, body)
			assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
		})
	}

	t.Run("cap overflow", func(t *testing.T) {
		uids := make([]string, user.MaxBatchUserUIDs+1)
		for i := range uids {
			uids[i] = fmt.Sprintf("u%d", i)
		}
		payload, _ := json.Marshal(map[string][]string{"uids": uids})
		w := postBotBatchUsers(handler, buBotToken, string(payload))
		assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	})

	t.Run("at cap boundary accepted", func(t *testing.T) {
		// Exactly MaxBatchUserUIDs unseeded uids: within the cap, accepted (200) with
		// every uid missing. Pins the boundary a hardcoded 1001 could not.
		uids := make([]string, user.MaxBatchUserUIDs)
		for i := range uids {
			uids[i] = fmt.Sprintf("cap%d", i)
		}
		payload, _ := json.Marshal(map[string][]string{"uids": uids})
		b := decodeBotBatchResp(t, postBotBatchUsers(handler, buBotToken, string(payload)))
		assert.Len(t, b.MissingUIDs, user.MaxBatchUserUIDs)
		assert.Empty(t, b.Users)
	})
}

// TestBotBatchUsers_OversizedBodyRejected pins that a request body over the 32 KiB
// cap is rejected through the i18n envelope (respondBotAPIRequestInvalid), not as a
// bare 400 from MaxBytesReader.
func TestBotBatchUsers_OversizedBodyRejected(t *testing.T) {
	handler, _ := setupBotBatchUsers(t)

	// One oversized uid string pushes the JSON body past maxBatchRequestBodyBytes.
	huge := strings.Repeat("a", maxBatchRequestBodyBytes+1024)
	body := fmt.Sprintf(`{"uids":["%s"]}`, huge)
	w := postBotBatchUsers(handler, buBotToken, body)

	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	// i18n envelope carries a structured error object, not an empty/plain 400 body.
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.NotEmpty(t, env.Error, "expected i18n error envelope, got=%s", w.Body.String())
}

func TestBotBatchUsers_OrderMissingDisabledAndPIIStripped(t *testing.T) {
	handler, ctx := setupBotBatchUsers(t)
	seedBotBatchUser(t, ctx, "bu_alpha", "Alpha", 1, 0)
	seedBotBatchUser(t, ctx, "bu_bot", "BotAcct", 1, 1)
	seedBotBatchUser(t, ctx, "bu_disabled", "Disabled", 0, 0)
	seedBotBatchUser(t, ctx, "bu_blacklist", "Blacklisted", 2, 0)

	body := `{"uids":["bu_bot","bu_disabled","bu_ghost","bu_alpha","bu_blacklist"]}`
	b := decodeBotBatchResp(t, postBotBatchUsers(handler, buBotToken, body))

	// Only enabled users, in request order.
	require.Len(t, b.Users, 2)
	assert.Equal(t, "bu_bot", botBatchUserUID(t, b.Users[0]))
	assert.Equal(t, "bu_alpha", botBatchUserUID(t, b.Users[1]))

	// Disabled + blacklisted + unknown all reported missing, never present, in
	// request order (multi-element assert.Equal pins ordering; ElementsMatch would
	// not catch a reorder — aligns with the human leg's ordered assertion).
	assert.Equal(t, []string{"bu_disabled", "bu_ghost", "bu_blacklist"}, b.MissingUIDs)

	// Minimal DTO: identity fields present, robot flag surfaced, no contact PII.
	for _, u := range b.Users {
		_, hasUID := u["uid"]
		_, hasStatus := u["status"]
		assert.True(t, hasUID, "uid must be present")
		assert.True(t, hasStatus, "status must be present")
		for _, pii := range []string{"phone", "email", "zone", "username", "short_no", "created_at"} {
			_, leaked := u[pii]
			assert.Falsef(t, leaked, "PII field %q must not be exposed, got=%v", pii, u)
		}
	}

	// The bot account must carry robot=1 so callers can distinguish bots.
	var robot int
	require.NoError(t, json.Unmarshal(b.Users[0]["robot"], &robot))
	assert.Equal(t, 1, robot)
}

// TestBotBatchUsers_SpacePrefixStripped pins the space-prefix normalization: a
// space-scoped uid (s<digits>_<baseUID>) resolves against its base user row, and
// BOTH the resolved user and a missing space-scoped uid echo the caller's ORIGINAL
// string. A caller indexing the response by the exact key it sent must find every
// requested uid in exactly one slice — a resolved uid coming back under its bare
// base form would leave the caller unable to find it, misjudging a live member as
// a ghost.
func TestBotBatchUsers_SpacePrefixStripped(t *testing.T) {
	handler, ctx := setupBotBatchUsers(t)
	seedBotBatchUser(t, ctx, "bu_scoped", "Scoped", 1, 0)

	body := `{"uids":["s1_bu_scoped","s42_bu_ghost"]}`
	b := decodeBotBatchResp(t, postBotBatchUsers(handler, buBotToken, body))

	// Space-prefixed uid resolved via its base row; response echoes the caller's
	// original space-prefixed key, not the bare base uid.
	require.Len(t, b.Users, 1)
	assert.Equal(t, "s1_bu_scoped", botBatchUserUID(t, b.Users[0]))
	// Missing uid echoes exactly what the caller sent (the space-prefixed form).
	assert.Equal(t, []string{"s42_bu_ghost"}, b.MissingUIDs)
}

// TestBotBatchUsers_StripCreatesDuplicateRejected pins that stripping which
// collapses two distinct originals onto the same base surfaces as a 400 on the
// re-validated stripped list rather than silently coalescing.
func TestBotBatchUsers_StripCreatesDuplicateRejected(t *testing.T) {
	handler, ctx := setupBotBatchUsers(t)
	seedBotBatchUser(t, ctx, "bu_dup", "Dup", 1, 0)

	// "s1_bu_dup" and "bu_dup" both strip to "bu_dup".
	body := `{"uids":["s1_bu_dup","bu_dup"]}`
	w := postBotBatchUsers(handler, buBotToken, body)
	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
}
