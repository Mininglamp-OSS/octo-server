package user

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// api_batch_test.go — POST /v1/users/batch (human, authenticated /v1 group).
//
// Assertions are wire-level only (raw JSON in, generic map out) so the file
// compiles and runs red on HEAD (route 404, no DTO) before the handler exists
// and green after. It pins the batch contract: route existence + auth, request
// validation (empty / non-string / duplicate / cap overflow), request-order
// preservation, missing-uid reporting, non-enabled users treated as missing,
// and the minimal DTO's PII stripping (phone / email / zone never emitted).

// rawBatchResp decodes the response without depending on the production DTO type,
// so key-absence (PII stripping) can be asserted directly on the emitted object.
type rawBatchResp struct {
	Users       []map[string]json.RawMessage `json:"users"`
	MissingUIDs []string                     `json:"missing_uids"`
}

func newBatchUserServer(t *testing.T) (*server.Server, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	// POST /v1/users/batch now mounts the process-level SharedUIDRateLimiter, whose
	// per-uid bucket (ratelimit:uid:{uid}) persists in Redis and is NOT cleared by
	// CleanAllTables. Reset testutil.UID's bucket so earlier tests in this binary
	// (and TestBatchUsersHuman_RequestValidation's 7 back-to-back requests against
	// the singleton) start from a full bucket. See rate-limit.md testing note.
	resetUserUIDRateLimit(t, ctx)
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	return s, ctx
}

// seedBatchUserFull inserts a user row with an explicit is_destroy value so the
// liveness gate (Status == enable AND IsDestroy == IsDestroyNo) can be exercised.
// status: 0 disabled, 1 enabled, 2 blacklist. robot: 0 human, 1 bot. isDestroy:
// 0 normal, 1 applying (cooling-off), 2 destroyed.
func seedBatchUserFull(t *testing.T, ctx *config.Context, uid, name string, status, robot, isDestroy int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, username, short_no, vercode, zone, phone, email, status, robot, is_destroy) "+
			"VALUES (?, ?, ?, ?, ?, '0086', '13800008888', ?, ?, ?, ?)",
		uid, name, "un_"+uid, "sn_"+uid, "vc_"+uid, uid+"@example.com", status, robot, isDestroy,
	).Exec()
	require.NoError(t, err)
}

// seedBatchUser inserts a user row carrying contact PII (zone/phone/email) so the
// minimal-DTO stripping assertions have something to leak if the projection is
// wrong. status: 0 disabled, 1 enabled, 2 blacklist. robot: 0 human, 1 bot.
func seedBatchUser(t *testing.T, ctx *config.Context, uid, name string, status, robot int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, username, short_no, vercode, zone, phone, email, status, robot) "+
			"VALUES (?, ?, ?, ?, ?, '0086', '13800008888', ?, ?, ?)",
		uid, name, "un_"+uid, "sn_"+uid, "vc_"+uid, uid+"@example.com", status, robot,
	).Exec()
	require.NoError(t, err)
}

func postBatchUsers(s *server.Server, token, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/users/batch", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("token", token)
	}
	s.GetRoute().ServeHTTP(w, req)
	return w
}

func decodeBatchResp(t *testing.T, w *httptest.ResponseRecorder) rawBatchResp {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var b rawBatchResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &b))
	return b
}

func batchUserUID(t *testing.T, m map[string]json.RawMessage) string {
	t.Helper()
	var s string
	require.NoError(t, json.Unmarshal(m["uid"], &s))
	return s
}

func TestBatchUsersHuman_RouteExistsAndRequiresAuth(t *testing.T) {
	s, _ := newBatchUserServer(t)

	// No token: auth middleware must reject before the handler — proves the
	// route is mounted under the authenticated group (401, not 404/200).
	w := postBatchUsers(s, "", `{"uids":["u1"]}`)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "body=%s", w.Body.String())

	// Valid token, malformed body: route exists and validation runs (400).
	w = postBatchUsers(s, testutil.Token, "not-json")
	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
}

func TestBatchUsersHuman_RequestValidation(t *testing.T) {
	s, _ := newBatchUserServer(t)

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
			w := postBatchUsers(s, testutil.Token, body)
			assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
		})
	}

	t.Run("cap overflow", func(t *testing.T) {
		uids := make([]string, 1001)
		for i := range uids {
			uids[i] = fmt.Sprintf("u%d", i)
		}
		payload, _ := json.Marshal(map[string][]string{"uids": uids})
		w := postBatchUsers(s, testutil.Token, string(payload))
		assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	})
}

func TestBatchUsersHuman_OrderMissingAndPIIStripped(t *testing.T) {
	s, ctx := newBatchUserServer(t)
	seedBatchUser(t, ctx, "u_alpha", "Alpha", 1, 0)
	seedBatchUser(t, ctx, "u_bravo", "Bravo", 1, 1)

	body := `{"uids":["u_bravo","u_ghost","u_alpha"]}`
	b := decodeBatchResp(t, postBatchUsers(s, testutil.Token, body))

	// Request order preserved, missing filtered out of users but reported.
	require.Len(t, b.Users, 2)
	assert.Equal(t, "u_bravo", batchUserUID(t, b.Users[0]))
	assert.Equal(t, "u_alpha", batchUserUID(t, b.Users[1]))
	assert.Equal(t, []string{"u_ghost"}, b.MissingUIDs)
	// Minimal DTO: identity fields present, contact PII never emitted.
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
}

func TestBatchUsersHuman_DisabledAndBlacklistedTreatedAsMissing(t *testing.T) {
	s, ctx := newBatchUserServer(t)
	seedBatchUser(t, ctx, "u_ok", "Ok", 1, 0)
	seedBatchUser(t, ctx, "u_disabled", "Disabled", 0, 0)
	seedBatchUser(t, ctx, "u_blacklist", "Blacklisted", 2, 0)

	body := `{"uids":["u_disabled","u_ok","u_blacklist"]}`
	b := decodeBatchResp(t, postBatchUsers(s, testutil.Token, body))

	require.Len(t, b.Users, 1)
	assert.Equal(t, "u_ok", batchUserUID(t, b.Users[0]))
	// Non-enabled accounts must never be reported present.
	assert.ElementsMatch(t, []string{"u_disabled", "u_blacklist"}, b.MissingUIDs)

	joined := strings.Join(b.MissingUIDs, ",")
	assert.NotContains(t, joined, "u_ok")
}

// TestBatchUsersHuman_DestroyedTreatedAsMissing pins the second half of the
// liveness predicate: an account with status=1 (enabled) but is_destroy != 0 is
// mid/post-teardown and must be reported missing, never present.
func TestBatchUsersHuman_DestroyedTreatedAsMissing(t *testing.T) {
	s, ctx := newBatchUserServer(t)
	seedBatchUserFull(t, ctx, "u_live", "Live", 1, 0, IsDestroyNo)
	seedBatchUserFull(t, ctx, "u_destroyed", "Destroyed", 1, 0, IsDestroyDone)
	seedBatchUserFull(t, ctx, "u_destroying", "Destroying", 1, 0, IsDestroyApplying)

	body := `{"uids":["u_destroyed","u_live","u_destroying"]}`
	b := decodeBatchResp(t, postBatchUsers(s, testutil.Token, body))

	require.Len(t, b.Users, 1)
	assert.Equal(t, "u_live", batchUserUID(t, b.Users[0]))
	// status=1 but is_destroy!=0 must be reported missing even though status alone
	// would pass.
	assert.ElementsMatch(t, []string{"u_destroyed", "u_destroying"}, b.MissingUIDs)
}

// TestBatchUsersHuman_MissingOrderPreserved pins the request-order contract on
// missing_uids with a multi-element assert.Equal (ElementsMatch elsewhere does not
// catch reordering).
func TestBatchUsersHuman_MissingOrderPreserved(t *testing.T) {
	s, ctx := newBatchUserServer(t)
	seedBatchUser(t, ctx, "u_present", "Present", 1, 0)

	body := `{"uids":["u_m3","u_present","u_m1","u_m2"]}`
	b := decodeBatchResp(t, postBatchUsers(s, testutil.Token, body))

	require.Len(t, b.Users, 1)
	assert.Equal(t, "u_present", batchUserUID(t, b.Users[0]))
	// Missing uids echo the request order exactly, present uid filtered out.
	assert.Equal(t, []string{"u_m3", "u_m1", "u_m2"}, b.MissingUIDs)
}

// TestBatchUsersHuman_UIDTooLong rejects an over-length uid (defense-in-depth
// bound in ValidateBatchUIDs).
func TestBatchUsersHuman_UIDTooLong(t *testing.T) {
	s, _ := newBatchUserServer(t)
	long := strings.Repeat("a", MaxBatchUIDLength+1)
	payload, _ := json.Marshal(map[string][]string{"uids": {long}})
	w := postBatchUsers(s, testutil.Token, string(payload))
	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
}
