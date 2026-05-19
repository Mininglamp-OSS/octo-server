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
	}
}

// ==================== Grant CRUD ====================

func TestOBO_CreateGrant_Happy(t *testing.T) {
	s := newFakeOBOStore()
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
	_, _ = s.insertGrant(tRESTOwner, tRESTBot, "auto")
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
	_, _ = s.insertGrant(tRESTOwner, tRESTBot, "auto")
	_, _ = s.insertGrant(tRESTOwner, "bot_other", "auto")
	_, _ = s.insertGrant(tRESTOther, "alice_bot", "auto")
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
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto")
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
	gid, _ := s.insertGrant(tRESTOther, "alice_bot", "auto")
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
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto")
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
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto")
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
	gid, _ := s.insertGrant(tRESTOther, "alice_bot", "auto")
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
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto")
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
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto")
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
	gid, _ := s.insertGrant(tRESTOther, "alice_bot", "auto")
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
