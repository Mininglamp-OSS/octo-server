package botfather

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newUserAPITestServer wraps testutil.NewTestServer and wires the i18n
// ErrorRenderer onto the route, mirroring main.go. Without it, httperr.
// ResponseErrorLWithStatus → c.RenderError falls back to the legacy {msg,status}
// envelope with no error.code, and the bind/occupied assertions can't see the
// stable code.
func newUserAPITestServer(t *testing.T) (http.Handler, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	return s.GetRoute(), ctx
}

// mintUserAPIKey creates a no-space `uk_` for uid so the User API routes
// authenticate as that user (space isolation branch skipped).
func mintUserAPIKey(t *testing.T, ctx *config.Context, uid string) string {
	key, err := NewUserAPIKeyService(ctx).GetOrCreate(uid, "", clientIDBotFather)
	require.NoError(t, err)
	return key
}

// mintUserAPIKeyInSpace creates a space-bound `uk_`; the middleware then sets
// api_key_space_id, so handlers exercise the Space isolation branch.
func mintUserAPIKeyInSpace(t *testing.T, ctx *config.Context, uid, spaceID string) string {
	key, err := NewUserAPIKeyService(ctx).GetOrCreate(uid, spaceID, clientIDBotFather)
	require.NoError(t, err)
	return key
}

// addBotToSpace registers the bot as an active member of an active Space. It
// also ensures the `space` row exists with status=1 — isBotInSpace joins
// space.status=1, so membership alone is not enough.
func addBotToSpace(t *testing.T, ctx *config.Context, spaceID, botID string) {
	_, err := ctx.DB().InsertBySql(
		"INSERT IGNORE INTO space (space_id, name, status) VALUES (?, ?, 1)",
		spaceID, spaceID,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT IGNORE INTO space_member (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, 0, 1, NOW(), NOW())",
		spaceID, botID,
	).Exec()
	require.NoError(t, err)
}

func userAPIRequest(t *testing.T, method, path, token string, body interface{}) *http.Request {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// errEnvelope mirrors the httperr i18n envelope shape we assert on.
type errEnvelope struct {
	Error struct {
		Code       string         `json:"code"`
		HTTPStatus int            `json:"http_status"`
		Details    map[string]any `json:"details"`
	} `json:"error"`
}

func TestListUserBots_NoTokenLeak_WithOccupancy(t *testing.T) {
	route, ctx := newUserAPITestServer(t)
	db := newBotfatherDB(ctx)

	uid := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "owner")
	token := mintUserAPIKey(t, ctx, uid)

	freeBot := "free_" + util.GenerUUID()[:8]
	busyBot := "busy_" + util.GenerUUID()[:8]
	insertTestBot(t, ctx, freeBot, uid)
	insertTestBot(t, ctx, busyBot, uid)

	// Occupy busyBot through the CAS path.
	affected, err := db.bindRobotCAS(busyBot, uid, "octopush:agent_1")
	require.NoError(t, err)
	require.Equal(t, int64(1), affected)

	w := httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodGet, "/v1/user/bots", token, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Decode into raw maps so we can prove bot_token is present (contract kept)
	// but empty (no bf_ leaked).
	var raw []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	require.Len(t, raw, 2)

	byID := map[string]map[string]any{}
	for _, item := range raw {
		tok, hasToken := item["bot_token"]
		assert.True(t, hasToken, "bot_token field must be retained for backward compat: %v", item)
		assert.Equal(t, "", tok, "listUserBots must not leak a real bf_ token: %v", item)
		byID[item["robot_id"].(string)] = item
	}

	assert.Equal(t, "", byID[freeBot]["bound_agent_ref"], "free bot must report empty bound_agent_ref")
	assert.Equal(t, "octopush:agent_1", byID[busyBot]["bound_agent_ref"])
}

func TestBindUserBot_Lifecycle(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	uid := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "owner")
	token := mintUserAPIKey(t, ctx, uid)

	botID := "bot_" + util.GenerUUID()[:8]
	insertTestBot(t, ctx, botID, uid)
	path := fmt.Sprintf("/v1/user/bots/%s/bind", botID)

	// 1) First bind succeeds.
	w := httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodPost, path, token, BindBotReq{AgentRef: "octopush:agent_A"}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp BindBotResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, botID, resp.RobotID)
	assert.Equal(t, "octopush:agent_A", resp.BoundAgentRef)
	assert.NotEmpty(t, resp.BoundAt)

	// 2) Re-bind with the SAME agent_ref is idempotent (200).
	w = httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodPost, path, token, BindBotReq{AgentRef: "octopush:agent_A"}))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// 3) Bind with a DIFFERENT agent_ref is rejected 409 + occupied_by.
	w = httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodPost, path, token, BindBotReq{AgentRef: "octopush:agent_B"}))
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	var env errEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "err.server.bot.occupied", env.Error.Code)
	assert.Equal(t, http.StatusConflict, env.Error.HTTPStatus)
	assert.Equal(t, "octopush:agent_A", env.Error.Details["occupied_by"])

	// 4) Unbind by the owning agent releases the occupation (idempotent).
	// Response carries the documented shape: bound_agent_ref="" and
	// bound_at=null (docs §6).
	w = httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodDelete, path, token, BindBotReq{AgentRef: "octopush:agent_A"}))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var unbindBody map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &unbindBody))
	assert.Equal(t, "", unbindBody["bound_agent_ref"])
	if v, ok := unbindBody["bound_at"]; !ok || v != nil {
		t.Fatalf("unbind response bound_at = %v (present=%v), want explicit null", v, ok)
	}

	// 5) After release a new agent can occupy it.
	w = httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodPost, path, token, BindBotReq{AgentRef: "octopush:agent_B"}))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// Unbind must prove agent ownership: a different agent_ref (sharing the same
// user's uk_) cannot release another agent's binding — otherwise the
// single-holder mutual-exclusion contract is bypassable via unbind-then-bind.
func TestUnbindUserBot_OwnershipEnforced(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	uid := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "owner")
	token := mintUserAPIKey(t, ctx, uid)
	botID := "bot_" + util.GenerUUID()[:8]
	insertTestBot(t, ctx, botID, uid)
	path := fmt.Sprintf("/v1/user/bots/%s/bind", botID)

	// Agent A occupies the bot.
	w := httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodPost, path, token, BindBotReq{AgentRef: "octopush:agent_A"}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Missing agent_ref on unbind → 400 (cannot release without identifying self).
	w = httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodDelete, path, token, BindBotReq{AgentRef: "  "}))
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	// Agent B (same user's uk_, different agent_ref) cannot release A's binding.
	w = httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodDelete, path, token, BindBotReq{AgentRef: "octopush:agent_B"}))
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	var env errEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "err.server.bot.occupied", env.Error.Code)
	assert.Equal(t, "octopush:agent_A", env.Error.Details["occupied_by"])

	// Bot is still occupied by A — B cannot bind it either.
	w = httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodPost, path, token, BindBotReq{AgentRef: "octopush:agent_B"}))
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())

	// Owner A releases successfully.
	w = httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodDelete, path, token, BindBotReq{AgentRef: "octopush:agent_A"}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Idempotent: A releasing an already-free bot still succeeds.
	w = httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodDelete, path, token, BindBotReq{AgentRef: "octopush:agent_A"}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Now that it's free, B can occupy it.
	w = httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodPost, path, token, BindBotReq{AgentRef: "octopush:agent_B"}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestBindUserBot_MissingAgentRef(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	uid := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "owner")
	token := mintUserAPIKey(t, ctx, uid)
	botID := "bot_" + util.GenerUUID()[:8]
	insertTestBot(t, ctx, botID, uid)

	w := httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodPost, fmt.Sprintf("/v1/user/bots/%s/bind", botID), token, BindBotReq{AgentRef: "   "}))
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	var env errEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "err.shared.param.invalid", env.Error.Code)
}

func TestBindUserBot_NonCreatorNotFound(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	owner := "u_" + util.GenerUUID()[:8]
	other := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, owner, "owner")
	insertTestUser(t, ctx, other, "other")
	otherToken := mintUserAPIKey(t, ctx, other)

	botID := "bot_" + util.GenerUUID()[:8]
	insertTestBot(t, ctx, botID, owner) // owned by `owner`

	w := httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodPost, fmt.Sprintf("/v1/user/bots/%s/bind", botID), otherToken, BindBotReq{AgentRef: "octopush:x"}))
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	var env errEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "err.shared.not_found", env.Error.Code)
}

// Space isolation: a space-bound uk_ may bind a bot that belongs to that
// space, but a same-creator bot OUTSIDE the space is rejected 403 — exercising
// the spaceID != "" branch that the no-space tests skip.
func TestBindUserBot_SpaceIsolation(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	uid := "u_" + util.GenerUUID()[:8]
	spaceID := "s_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "owner")
	token := mintUserAPIKeyInSpace(t, ctx, uid, spaceID)

	botIn := "in_" + util.GenerUUID()[:8]
	botOut := "out_" + util.GenerUUID()[:8]
	insertTestBot(t, ctx, botIn, uid)
	insertTestBot(t, ctx, botOut, uid)
	addBotToSpace(t, ctx, spaceID, botIn) // only botIn joins the space

	// In-space bot: bind succeeds.
	w := httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodPost, fmt.Sprintf("/v1/user/bots/%s/bind", botIn), token, BindBotReq{AgentRef: "octopush:a"}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Out-of-space bot (same creator): bind rejected 403.
	w = httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodPost, fmt.Sprintf("/v1/user/bots/%s/bind", botOut), token, BindBotReq{AgentRef: "octopush:a"}))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var env errEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "err.shared.auth.forbidden", env.Error.Code)

	// Out-of-space bot: unbind also rejected 403.
	w = httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodDelete, fmt.Sprintf("/v1/user/bots/%s/bind", botOut), token, BindBotReq{AgentRef: "octopush:a"}))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}

// Concurrent binds on the same free bot: exactly one wins, the rest get 409.
func TestBindUserBot_ConcurrentMutualExclusion(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	uid := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "owner")
	token := mintUserAPIKey(t, ctx, uid)
	botID := "bot_" + util.GenerUUID()[:8]
	insertTestBot(t, ctx, botID, uid)
	path := fmt.Sprintf("/v1/user/bots/%s/bind", botID)

	const n = 8
	var success, conflict int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			w := httptest.NewRecorder()
			route.ServeHTTP(w, userAPIRequest(t, http.MethodPost, path, token, BindBotReq{AgentRef: fmt.Sprintf("octopush:agent_%d", i)}))
			switch w.Code {
			case http.StatusOK:
				atomic.AddInt32(&success, 1)
			case http.StatusConflict:
				atomic.AddInt32(&conflict, 1)
			default:
				t.Errorf("unexpected status %d: %s", w.Code, w.Body.String())
			}
		}(i)
	}
	close(start)
	wg.Wait()

	assert.Equal(t, int32(1), success, "exactly one concurrent bind must win")
	assert.Equal(t, int32(n-1), conflict, "all other concurrent binds must get 409")
}
