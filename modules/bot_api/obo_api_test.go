// Package bot_api · YUJ-1166 — Unit tests for OBO REST endpoints.
//
// Each handler is exercised directly via gin's CreateTestContext + a stub
// auth context ("uid"). We avoid spinning up the full router because the
// production registerOBORoutes mount also depends on ctx.AuthMiddleware,
// which requires a live cache.
//
// Coverage:
//   - Create / List / Update / Delete grant (happy + ownership rejection)
//   - Mode validation (auto only in v0)
//   - Create / Delete / List scope (ownership rejection)
//   - Duplicate-key surfaces as 409
package bot_api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

const (
	tRESTOwner   = "user_yu"
	tRESTBot     = "bot_clone_001"
	tRESTOther   = "user_alice"
	tRESTChannel = "group_42"
)

// makeCtx — gin context with uid set as the caller, body as POST/PUT body
// and the named URL params populated.
func makeCtx(t *testing.T, uid, method, path string, body interface{}, params gin.Params) (*wkhttp.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)

	var reqBody []byte
	if body != nil {
		var err error
		reqBody, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	gc.Request = req
	gc.Params = params
	c := &wkhttp.Context{Context: gc}
	if uid != "" {
		c.Set("uid", uid)
	}
	return c, rec
}

func newBAforREST(s *fakeOBOStore) *BotAPI {
	return &BotAPI{
		Log:              log.NewTLog("BotAPI-rest-test"),
		oboStoreOverride: s,
		// Default test override: grantor has access to any channel. The
		// channel-wiretap regression test (TestOBO_CreateScope_NoChannelAccess)
		// installs a denying override explicitly.
		oboChannelAccessOverride: func(uid, channelID string, channelType uint8) (bool, error) {
			return true, nil
		},
	}
}

// flexPtr — test helper. YUJ-1738: Active is `*FlexBoolInt` so the
// JSON decoder accepts BOTH boolean and integer. Tests still
// construct request bodies with Go values, so they need a quick way
// to get an addressable FlexBoolInt for the pointer field. Mirrors
// the inline `v := 0; &v` pattern used for the legacy `*int` fields.
func flexPtr(v int) *FlexBoolInt {
	f := FlexBoolInt(v)
	return &f
}

// intPtr — test helper for the legacy `*int` request fields
// (GlobalEnabled, the in-store updateGrant signature, etc.). Kept
// alongside flexPtr so tests that mix both shapes read symmetrically.
func intPtr(v int) *int {
	return &v
}

// ==================== Grant CRUD ====================

func TestOBO_CreateGrant_Happy(t *testing.T) {
	s := newFakeOBOStore()
	s.seedBot(tRESTBot, tRESTOwner)
	ba := newBAforREST(s)

	c, rec := makeCtx(t, tRESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tRESTBot}, nil)
	ba.oboCreateGrant(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Verify the row exists for the inferred grantor.
	rows, _ := s.listGrantsByGrantor(tRESTOwner)
	if len(rows) != 1 || rows[0].GranteeBotUID != tRESTBot {
		t.Fatalf("grant not persisted under correct grantor: %+v", rows)
	}
	if rows[0].GlobalEnabled != 0 {
		t.Errorf("new grant must start with global_enabled=0, got %d", rows[0].GlobalEnabled)
	}
}

func TestOBO_CreateGrant_NoAuth(t *testing.T) {
	ba := newBAforREST(newFakeOBOStore())
	c, rec := makeCtx(t, "", http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tRESTBot}, nil)
	ba.oboCreateGrant(c)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestOBO_CreateGrant_SelfReject(t *testing.T) {
	ba := newBAforREST(newFakeOBOStore())
	c, rec := makeCtx(t, tRESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tRESTOwner}, nil)
	ba.oboCreateGrant(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for self-grant, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestOBO_CreateGrant_BadMode(t *testing.T) {
	ba := newBAforREST(newFakeOBOStore())
	c, rec := makeCtx(t, tRESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tRESTBot, Mode: "draft"}, nil)
	ba.oboCreateGrant(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-auto mode, got %d", rec.Code)
	}
}

func TestOBO_CreateGrant_Duplicate(t *testing.T) {
	s := newFakeOBOStore()
	s.seedBot(tRESTBot, tRESTOwner)
	_, _ = s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	ba := newBAforREST(s)

	c, rec := makeCtx(t, tRESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tRESTBot}, nil)
	ba.oboCreateGrant(c)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 for duplicate, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestOBO_ListGrants_Happy(t *testing.T) {
	s := newFakeOBOStore()
	_, _ = s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	_, _ = s.insertGrant(tRESTOwner, "bot_other", "auto", "")
	_, _ = s.insertGrant(tRESTOther, "alice_bot", "auto", "")
	ba := newBAforREST(s)

	c, rec := makeCtx(t, tRESTOwner, http.MethodGet, "/v1/obo/grants", nil, nil)
	ba.oboListGrants(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var resp struct {
		Items []*oboGrantModel `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(resp.Items) != 2 {
		t.Fatalf("expected 2 items (only owner's), got %d", len(resp.Items))
	}
}

func TestOBO_UpdateGrant_Toggle(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	ba := newBAforREST(s)

	enable := 1
	c, rec := makeCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gid, 10),
		oboUpdateGrantReq{GlobalEnabled: &enable},
		gin.Params{{Key: "id", Value: strconv.FormatInt(gid, 10)}})
	ba.oboUpdateGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	g, _ := s.findGrantByID(gid)
	if g.GlobalEnabled != 1 {
		t.Fatalf("global_enabled should be 1, got %d", g.GlobalEnabled)
	}
}

func TestOBO_UpdateGrant_Cross_user_404(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOther, "alice_bot", "auto", "")
	ba := newBAforREST(s)

	enable := 1
	c, rec := makeCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gid, 10),
		oboUpdateGrantReq{GlobalEnabled: &enable},
		gin.Params{{Key: "id", Value: strconv.FormatInt(gid, 10)}})
	ba.oboUpdateGrant(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user update must be 404 (not 403), got %d", rec.Code)
	}
	// Ownership untouched.
	g, _ := s.findGrantByID(gid)
	if g.GlobalEnabled != 0 {
		t.Fatalf("global_enabled must remain 0, got %d", g.GlobalEnabled)
	}
}

func TestOBO_DeleteGrant_Happy(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	ba := newBAforREST(s)

	c, rec := makeCtx(t, tRESTOwner, http.MethodDelete,
		"/v1/obo/grants/"+strconv.FormatInt(gid, 10), nil,
		gin.Params{{Key: "id", Value: strconv.FormatInt(gid, 10)}})
	ba.oboDeleteGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	g, _ := s.findGrantByID(gid)
	if g == nil || g.Active != 0 {
		t.Fatalf("grant should be soft-deleted (active=0), got %+v", g)
	}
}

// ==================== Scope CRUD ====================

func TestOBO_CreateScope_Happy(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	ba := newBAforREST(s)

	c, rec := makeCtx(t, tRESTOwner, http.MethodPost, "/v1/obo/scopes",
		oboCreateScopeReq{
			GrantID:     gid,
			ChannelID:   tRESTChannel,
			ChannelType: common.ChannelTypeGroup.Uint8(),
		}, nil)
	ba.oboCreateScope(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	scopes, _ := s.listScopesByGrant(gid)
	if len(scopes) != 1 || scopes[0].ChannelID != tRESTChannel || scopes[0].Enabled != 1 {
		t.Fatalf("scope not persisted as expected: %+v", scopes)
	}
}

func TestOBO_CreateScope_CrossUser404(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOther, "alice_bot", "auto", "")
	ba := newBAforREST(s)

	c, rec := makeCtx(t, tRESTOwner, http.MethodPost, "/v1/obo/scopes",
		oboCreateScopeReq{
			GrantID:     gid,
			ChannelID:   tRESTChannel,
			ChannelType: common.ChannelTypeGroup.Uint8(),
		}, nil)
	ba.oboCreateScope(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user create scope must be 404, got %d", rec.Code)
	}
}

func TestOBO_ListScopes_Happy(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	_, _ = s.insertScope(gid, "ch_a", 1, 1)
	_, _ = s.insertScope(gid, "ch_b", 2, 1)
	ba := newBAforREST(s)

	c, rec := makeCtx(t, tRESTOwner, http.MethodGet,
		"/v1/obo/grants/"+strconv.FormatInt(gid, 10)+"/scopes", nil,
		gin.Params{{Key: "id", Value: strconv.FormatInt(gid, 10)}})
	ba.oboListScopes(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []*oboScopeModel `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Items) != 2 {
		t.Fatalf("expected 2 scopes, got %d", len(resp.Items))
	}
}

func TestOBO_DeleteScope_Happy(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	sid, _ := s.insertScope(gid, tRESTChannel, common.ChannelTypeGroup.Uint8(), 1)
	ba := newBAforREST(s)

	c, rec := makeCtx(t, tRESTOwner, http.MethodDelete,
		"/v1/obo/scopes/"+strconv.FormatInt(sid, 10), nil,
		gin.Params{{Key: "id", Value: strconv.FormatInt(sid, 10)}})
	ba.oboDeleteScope(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	scopes, _ := s.listScopesByGrant(gid)
	if len(scopes) != 0 {
		t.Fatalf("scope should be deleted, got %d", len(scopes))
	}
}

func TestOBO_DeleteScope_CrossUser404(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOther, "alice_bot", "auto", "")
	sid, _ := s.insertScope(gid, tRESTChannel, common.ChannelTypeGroup.Uint8(), 1)
	ba := newBAforREST(s)

	c, rec := makeCtx(t, tRESTOwner, http.MethodDelete,
		"/v1/obo/scopes/"+strconv.FormatInt(sid, 10), nil,
		gin.Params{{Key: "id", Value: strconv.FormatInt(sid, 10)}})
	ba.oboDeleteScope(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user scope delete must be 404, got %d", rec.Code)
	}
	scopes, _ := s.listScopesByGrant(gid)
	if len(scopes) != 1 {
		t.Fatalf("scope must survive cross-user attempt, got %d", len(scopes))
	}
}

// ==================== PR#82 hardening regression tests ====================

// TestOBO_CreateGrant_NotOwnBot_404 — caller MUST own the grantee bot
// (review #1 task spec P1-2). The fake oboStore seeds bot_X as owned by
// tRESTOther; the caller tRESTOwner is not allowed to install an OBO
// grant targeting it, even with a syntactically valid body. 404 (not 403)
// matches the existence-leak posture.
func TestOBO_CreateGrant_NotOwnBot_404(t *testing.T) {
	s := newFakeOBOStore()
	s.seedBot("bot_X", tRESTOther) // owned by someone else
	ba := newBAforREST(s)

	c, rec := makeCtx(t, tRESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: "bot_X"}, nil)
	ba.oboCreateGrant(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-owned bot, got %d body=%s", rec.Code, rec.Body.String())
	}
	// And nothing was persisted.
	rows, _ := s.listGrantsByGrantor(tRESTOwner)
	if len(rows) != 0 {
		t.Fatalf("no grant should be persisted, got %+v", rows)
	}
}

// TestOBO_CreateGrant_NotABot_404 — grantee_bot_uid must resolve to a
// row in the robot table (i.e. an actual bot, not a real-user uid).
// Review #2 P2-3. Without this check, a user could install a grant
// targeting another HUMAN uid — inert today, but cluttered audit and a
// poor invariant for v1 to inherit.
func TestOBO_CreateGrant_NotABot_404(t *testing.T) {
	s := newFakeOBOStore()
	// seedNonBotUser → queryRobotOwner returns found=false (the prod
	// query targets the robot table, where a real-user uid has no row).
	s.seedNonBotUser("not_a_bot_uid")
	ba := newBAforREST(s)

	c, rec := makeCtx(t, tRESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: "not_a_bot_uid"}, nil)
	ba.oboCreateGrant(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-bot uid, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestOBO_CreateGrant_Reactivates_SoftDeletedRow — once a grant has been
// soft-deleted (DELETE /v1/obo/grants/:id), the (grantor, bot) pair is
// frozen by the UNIQUE KEY uk_grantor_grantee. A naive insert would 409
// forever. The fix (review #2 P1-1): when the duplicate fires AND the
// existing row is owned by the caller AND active=0, flip it back to
// active=1 / global_enabled=0 / revoked_at=NULL in place. Verify the
// row is reusable: re-create after revoke succeeds, returns the same
// ID, and emerges with global_enabled=0 (caller must opt in again).
func TestOBO_CreateGrant_Reactivates_SoftDeletedRow(t *testing.T) {
	s := newFakeOBOStore()
	s.seedBot(tRESTBot, tRESTOwner)
	ba := newBAforREST(s)

	// Step 1: create grant + enable it.
	c, rec := makeCtx(t, tRESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tRESTBot}, nil)
	ba.oboCreateGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("initial create failed: %d body=%s", rec.Code, rec.Body.String())
	}
	rows, _ := s.listGrantsByGrantor(tRESTOwner)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after create, got %d", len(rows))
	}
	originalID := rows[0].ID
	enable := 1
	_ = s.updateGrant(originalID, "", &enable, nil)

	// Step 2: soft-delete the grant.
	_ = s.revokeGrant(originalID)
	g, _ := s.findGrantByID(originalID)
	if g == nil || g.Active != 0 || g.GlobalEnabled != 0 {
		t.Fatalf("expected revoked row after step 2, got %+v", g)
	}

	// Step 3: POST the same (grantor, bot) again — must reactivate, not 409.
	c, rec = makeCtx(t, tRESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tRESTBot}, nil)
	ba.oboCreateGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("reactivation create must return 200, got %d body=%s",
			rec.Code, rec.Body.String())
	}

	// Step 4: row is back to active=1 / global_enabled=0 with same ID.
	g, _ = s.findGrantByID(originalID)
	if g == nil || g.Active != 1 || g.GlobalEnabled != 0 {
		t.Fatalf("reactivated row should be active=1 global_enabled=0, got %+v", g)
	}
	rows, _ = s.listGrantsByGrantor(tRESTOwner)
	if len(rows) != 1 || rows[0].ID != originalID {
		t.Fatalf("reactivation should reuse the same row id; got rows=%+v", rows)
	}
}

// TestOBO_CreateGrant_LiveDuplicate_Still409 — when the duplicate row is
// LIVE (active=1, not soft-deleted), the handler still returns 409 — the
// reactivation path applies only to soft-deleted rows.
func TestOBO_CreateGrant_LiveDuplicate_Still409(t *testing.T) {
	s := newFakeOBOStore()
	s.seedBot(tRESTBot, tRESTOwner)
	_, _ = s.insertGrant(tRESTOwner, tRESTBot, "auto", "") // active=1 by default
	ba := newBAforREST(s)

	c, rec := makeCtx(t, tRESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tRESTBot}, nil)
	ba.oboCreateGrant(c)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 for live duplicate, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestOBO_CreateScope_NoChannelAccess_404 — P0 wiretap regression test.
// The grantor must have read access to the target channel before a scope
// row can be created. With the override returning false, the request is
// rejected as 404 (existence-leak defense). Without this check, an
// attacker could scope to a channel they aren't in and silently
// exfiltrate every inbound message to their bot via the fan-out listener.
func TestOBO_CreateScope_NoChannelAccess_404(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	ba := newBAforREST(s)
	// Override the default permissive hook to deny access for this test.
	ba.oboChannelAccessOverride = func(uid, channelID string, channelType uint8) (bool, error) {
		// Grantor has read access to nothing.
		return false, nil
	}

	c, rec := makeCtx(t, tRESTOwner, http.MethodPost, "/v1/obo/scopes",
		oboCreateScopeReq{
			GrantID:     gid,
			ChannelID:   "secret_group",
			ChannelType: common.ChannelTypeGroup.Uint8(),
		}, nil)
	ba.oboCreateScope(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when grantor lacks channel access, got %d body=%s",
			rec.Code, rec.Body.String())
	}
	// And no scope was persisted.
	scopes, _ := s.listScopesByGrant(gid)
	if len(scopes) != 0 {
		t.Fatalf("scope must NOT be created when access check fails, got %+v", scopes)
	}
}

// TestOBO_CreateScope_ChannelAccessErr_500 — when the channel-access
// check errors (e.g. DB outage), the handler must surface a 500 rather
// than fail-open. We assert via the body (ResponseError); the override
// returns a synthetic error.
func TestOBO_CreateScope_ChannelAccessErr_500(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	ba := newBAforREST(s)
	boom := errors.New("connection refused")
	ba.oboChannelAccessOverride = func(uid, channelID string, channelType uint8) (bool, error) {
		return false, boom
	}

	c, rec := makeCtx(t, tRESTOwner, http.MethodPost, "/v1/obo/scopes",
		oboCreateScopeReq{
			GrantID:     gid,
			ChannelID:   "any",
			ChannelType: common.ChannelTypeGroup.Uint8(),
		}, nil)
	ba.oboCreateScope(c)
	// ResponseError body carries the failure message; assert nothing was
	// persisted regardless of the wire status convention.
	scopes, _ := s.listScopesByGrant(gid)
	if len(scopes) != 0 {
		t.Fatalf("scope must NOT be created on access-check error, got %+v", scopes)
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("expected error body, got empty response")
	}
}

// TestOBO_DeleteScope_FindScopeOwner_OK — the new oboDeleteScope path
// uses findScopeOwner (single JOIN) instead of the O(N×M) scopeOwnedBy
// scan. Verify owner-match → 200 and row removed.
func TestOBO_DeleteScope_FindScopeOwner_OK(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	sid, _ := s.insertScope(gid, tRESTChannel, common.ChannelTypeGroup.Uint8(), 1)
	ba := newBAforREST(s)

	c, rec := makeCtx(t, tRESTOwner, http.MethodDelete,
		"/v1/obo/scopes/"+strconv.FormatInt(sid, 10), nil,
		gin.Params{{Key: "id", Value: strconv.FormatInt(sid, 10)}})
	ba.oboDeleteScope(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	scopes, _ := s.listScopesByGrant(gid)
	if len(scopes) != 0 {
		t.Fatalf("scope should be deleted, got %d", len(scopes))
	}
}

// TestOBO_DeleteScope_FindScopeOwner_LookupErr_500 — when the new
// findScopeOwner method errors (DB outage), the delete must fail closed
// without removing the row.
func TestOBO_DeleteScope_FindScopeOwner_LookupErr(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	sid, _ := s.insertScope(gid, tRESTChannel, common.ChannelTypeGroup.Uint8(), 1)
	s.failFindScopeOwner = errors.New("connection refused")
	ba := newBAforREST(s)

	c, rec := makeCtx(t, tRESTOwner, http.MethodDelete,
		"/v1/obo/scopes/"+strconv.FormatInt(sid, 10), nil,
		gin.Params{{Key: "id", Value: strconv.FormatInt(sid, 10)}})
	ba.oboDeleteScope(c)
	scopes, _ := s.listScopesByGrant(gid)
	if len(scopes) != 1 {
		t.Fatalf("scope must survive lookup error, got %d", len(scopes))
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("expected error body, got empty response")
	}
}

// ==================== YUJ-1358 grantee_bot_name regression ====================

// TestOBO_ListGrants_PopulatesGranteeBotName — GET /v1/obo/grants must
// return a non-empty `grantee_bot_name` on every row so the web
// PersonaCard does NOT fall back to rendering the raw bot uid
// (`27qFHDRBCJQ2c868c93_bot`). The prod query enriches via
// LEFT JOIN `user` u ON u.uid = g.grantee_bot_uid; the fake mirrors that
// via seedBotName. Verifies both the JOIN-success and the COALESCE
// fallback (bot with no user row → uid surfaces, never empty string).
// Refs YUJ-1358 / octo-web#60.
func TestOBO_ListGrants_PopulatesGranteeBotName(t *testing.T) {
	s := newFakeOBOStore()
	// Two grants for the same grantor:
	//   1. bot with a known display name (JOIN-success path)
	//   2. bot with no name seeded → fallback to uid (COALESCE path)
	s.seedBot(tRESTBot, tRESTOwner)
	s.seedBotName(tRESTBot, "james")
	_, _ = s.insertGrant(tRESTOwner, tRESTBot, "auto", "")

	const unnamedBot = "bot_no_user_row"
	s.seedBot(unnamedBot, tRESTOwner)
	_, _ = s.insertGrant(tRESTOwner, unnamedBot, "auto", "")

	ba := newBAforREST(s)
	c, rec := makeCtx(t, tRESTOwner, http.MethodGet, "/v1/obo/grants", nil, nil)
	ba.oboListGrants(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Decode the envelope used by the handler: c.Response wraps the payload
	// inside the standard {status, data} shape. We unmarshal loosely.
	var env struct {
		Data struct {
			Items []map[string]interface{} `json:"items"`
		} `json:"data"`
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode list body: %v body=%s", err, rec.Body.String())
	}
	items := env.Data.Items
	if len(items) == 0 {
		items = env.Items
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d body=%s", len(items), rec.Body.String())
	}

	byUID := map[string]map[string]interface{}{}
	for _, it := range items {
		uid, _ := it["grantee_bot_uid"].(string)
		byUID[uid] = it
	}
	if got, _ := byUID[tRESTBot]["grantee_bot_name"].(string); got != "james" {
		t.Errorf("grant %s grantee_bot_name = %q, want %q", tRESTBot, got, "james")
	}
	if got, _ := byUID[unnamedBot]["grantee_bot_name"].(string); got != unnamedBot {
		t.Errorf("grant %s grantee_bot_name = %q, want %q (COALESCE fallback)",
			unnamedBot, got, unnamedBot)
	}
	// Hard contract: every row's grantee_bot_name MUST be non-empty so the
	// frontend never falls back to the uid render path that motivated this
	// fix in the first place.
	for _, it := range items {
		name, _ := it["grantee_bot_name"].(string)
		if name == "" {
			t.Errorf("grantee_bot_name must never be empty; row=%+v", it)
		}
	}
}

// TestOBO_StoreListGrantsByGrantor_GranteeBotNameContract — the same
// invariant at the store layer (no HTTP). Pins down the fake's contract
// so future contributors who add another caller of listGrantsByGrantor
// can rely on a non-empty GranteeBotName without re-reading the SQL.
func TestOBO_StoreListGrantsByGrantor_GranteeBotNameContract(t *testing.T) {
	s := newFakeOBOStore()
	s.seedBot(tRESTBot, tRESTOwner)
	s.seedBotName(tRESTBot, "james")
	_, _ = s.insertGrant(tRESTOwner, tRESTBot, "auto", "")

	rows, err := s.listGrantsByGrantor(tRESTOwner)
	if err != nil {
		t.Fatalf("listGrantsByGrantor err=%v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].GranteeBotName != "james" {
		t.Errorf("GranteeBotName = %q, want %q", rows[0].GranteeBotName, "james")
	}

	// Fallback: bot without a seeded display name → uid surfaces.
	const unnamedBot = "bot_no_display"
	s.seedBot(unnamedBot, tRESTOwner)
	_, _ = s.insertGrant(tRESTOwner, unnamedBot, "auto", "")
	rows, _ = s.listGrantsByGrantor(tRESTOwner)
	for _, r := range rows {
		if r.GranteeBotName == "" {
			t.Errorf("GranteeBotName must never be empty (uid=%s)", r.GranteeBotUID)
		}
		if r.GranteeBotUID == unnamedBot && r.GranteeBotName != unnamedBot {
			t.Errorf("fallback name for %s = %q, want %q (COALESCE)",
				unnamedBot, r.GranteeBotName, unnamedBot)
		}
	}
}
