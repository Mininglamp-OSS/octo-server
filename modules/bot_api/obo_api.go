// Package bot_api · YUJ-1166 / Mininglamp-OSS/octo-server#81 — Persona Clone
// OBO REST endpoints.
//
// These endpoints are mounted under /v1/obo behind the standard user-auth
// middleware (ba.ctx.AuthMiddleware) — they take a USER token, not a bot
// token. The acting user must be the grantor of the row they CRUD; we do
// NOT support cross-user persona management in v0 (RFC §2 / out-of-scope).
//
// Status code map (kept narrow on purpose):
//   200 — success (single object or list)
//   400 — bad request body / missing required fields
//   401 — no user token (handled by upstream middleware)
//   403 — grantor mismatch / cross-user attempt
//   404 — grant_id / scope_id not found
//   409 — duplicate (grantor+grantee already exists / scope already exists)
//   500 — DB error
package bot_api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// registerOBORoutes mounts the OBO endpoints onto r under user-auth.
// Called from BotAPI.Route. Split out so the route table in bot_api.go
// stays focused on bot-token routes.
func (ba *BotAPI) registerOBORoutes(r *wkhttp.WKHttp) {
	// Defensive: ctx may be nil in unit tests that build a bare BotAPI
	// (e.g. send_test.go's BotAPI literal). Skip mounting in that case —
	// tests of the OBO REST surface construct their own gin engines.
	if ba.ctx == nil {
		return
	}
	auth := r.Group("/v1/obo", ba.ctx.AuthMiddleware(r))
	auth.POST("/grants", ba.oboCreateGrant)
	auth.GET("/grants", ba.oboListGrants)
	auth.DELETE("/grants/:id", ba.oboDeleteGrant)
	auth.PUT("/grants/:id", ba.oboUpdateGrant)
	auth.POST("/scopes", ba.oboCreateScope)
	auth.DELETE("/scopes/:id", ba.oboDeleteScope)
	auth.GET("/grants/:id/scopes", ba.oboListScopes)
}

// ==================== Request DTOs ====================

type oboCreateGrantReq struct {
	GranteeBotUID string `json:"grantee_bot_uid"`
	// Mode defaults to "auto" on the server. v0 rejects anything else so a
	// client can't quietly set "draft" and expect functionality. The field
	// is kept on the wire for forward-compat with v1.
	Mode string `json:"mode,omitempty"`
}

type oboUpdateGrantReq struct {
	Mode string `json:"mode,omitempty"`
	// GlobalEnabled uses *int (not int / bool) so "field omitted" and
	// "field set to 0" are distinguishable on the wire. Per RFC §5.1
	// PUT semantics: only provided fields are updated.
	GlobalEnabled *int `json:"global_enabled,omitempty"`
}

type oboCreateScopeReq struct {
	GrantID     int64  `json:"grant_id"`
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
	// Enabled defaults to 1 when omitted. Clients that want to add a row
	// in the "off" state can set it to 0 — the cheaper alternative to
	// add-then-toggle.
	Enabled *int `json:"enabled,omitempty"`
}

// ==================== Handlers ====================

// oboCreateGrant — POST /v1/obo/grants.
// Body: { grantee_bot_uid, mode? }. Grantor is inferred from the auth token
// — the caller cannot create a grant on someone else's behalf.
func (ba *BotAPI) oboCreateGrant(c *wkhttp.Context) {
	uid := c.GetLoginUID()
	if uid == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin404("unauthorized"))
		return
	}
	var req oboCreateGrantReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("数据格式有误"))
		return
	}
	if strings.TrimSpace(req.GranteeBotUID) == "" {
		c.ResponseError(errors.New("grantee_bot_uid 不能为空"))
		return
	}
	if req.GranteeBotUID == uid {
		c.ResponseError(errors.New("grantee_bot_uid 不能等于自己"))
		return
	}
	mode := req.Mode
	if mode == "" {
		mode = "auto"
	}
	if mode != "auto" {
		// v0 — see RFC §2 / out-of-scope. Draft mode lands in v1.
		c.ResponseError(errors.New("mode 仅支持 auto (v0)"))
		return
	}

	id, err := ba.oboStoreOrDefault().insertGrant(uid, req.GranteeBotUID, mode)
	if err != nil {
		if isDuplicateKeyErr(err) {
			c.JSON(http.StatusConflict, gin404("grant already exists"))
			return
		}
		ba.Error("insertGrant failed", zap.Error(err))
		c.ResponseError(errors.New("内部错误"))
		return
	}
	grant, err := ba.oboStoreOrDefault().findGrantByID(id)
	if err != nil || grant == nil {
		// Insert succeeded but read-back failed — return the bare ID so the
		// client can still call other endpoints.
		c.Response(map[string]interface{}{"id": id})
		return
	}
	c.Response(grant)
}

// oboListGrants — GET /v1/obo/grants. Lists ALL grants (active + revoked)
// owned by the caller. UI usually filters to active on its side.
func (ba *BotAPI) oboListGrants(c *wkhttp.Context) {
	uid := c.GetLoginUID()
	if uid == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin404("unauthorized"))
		return
	}
	grants, err := ba.oboStoreOrDefault().listGrantsByGrantor(uid)
	if err != nil {
		ba.Error("listGrants failed", zap.Error(err))
		c.ResponseError(errors.New("内部错误"))
		return
	}
	c.Response(map[string]interface{}{"items": grants})
}

// oboDeleteGrant — DELETE /v1/obo/grants/:id. Soft delete (revoke). Caller
// must own the row.
func (ba *BotAPI) oboDeleteGrant(c *wkhttp.Context) {
	uid := c.GetLoginUID()
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	grant, err := ba.requireOwnedGrant(c, uid, id)
	if err != nil || grant == nil {
		return // requireOwnedGrant already wrote the response
	}
	if err := ba.oboStoreOrDefault().revokeGrant(id); err != nil {
		ba.Error("revokeGrant failed", zap.Error(err), zap.Int64("id", id))
		c.ResponseError(errors.New("内部错误"))
		return
	}
	c.ResponseOK()
}

// oboUpdateGrant — PUT /v1/obo/grants/:id. Toggle global_enabled / change
// mode. mode validation matches Create (v0 only accepts "auto").
func (ba *BotAPI) oboUpdateGrant(c *wkhttp.Context) {
	uid := c.GetLoginUID()
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	grant, err := ba.requireOwnedGrant(c, uid, id)
	if err != nil || grant == nil {
		return
	}
	var req oboUpdateGrantReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("数据格式有误"))
		return
	}
	if req.Mode != "" && req.Mode != "auto" {
		c.ResponseError(errors.New("mode 仅支持 auto (v0)"))
		return
	}
	if req.Mode == "" && req.GlobalEnabled == nil {
		// Idempotent no-op — return the existing row.
		c.Response(grant)
		return
	}
	if err := ba.oboStoreOrDefault().updateGrant(id, req.Mode, req.GlobalEnabled); err != nil {
		ba.Error("updateGrant failed", zap.Error(err), zap.Int64("id", id))
		c.ResponseError(errors.New("内部错误"))
		return
	}
	refreshed, _ := ba.oboStoreOrDefault().findGrantByID(id)
	if refreshed != nil {
		c.Response(refreshed)
		return
	}
	c.ResponseOK()
}

// oboCreateScope — POST /v1/obo/scopes. Adds (or upserts via the unique
// key) a per-channel white-list entry to an existing owned grant.
func (ba *BotAPI) oboCreateScope(c *wkhttp.Context) {
	uid := c.GetLoginUID()
	if uid == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin404("unauthorized"))
		return
	}
	var req oboCreateScopeReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("数据格式有误"))
		return
	}
	if req.GrantID == 0 || strings.TrimSpace(req.ChannelID) == "" || req.ChannelType == 0 {
		c.ResponseError(errors.New("grant_id / channel_id / channel_type 不能为空"))
		return
	}
	grant, err := ba.requireOwnedGrant(c, uid, req.GrantID)
	if err != nil || grant == nil {
		return
	}
	enabled := 1
	if req.Enabled != nil && *req.Enabled == 0 {
		enabled = 0
	}
	id, err := ba.oboStoreOrDefault().insertScope(req.GrantID, req.ChannelID, req.ChannelType, enabled)
	if err != nil {
		if isDuplicateKeyErr(err) {
			c.JSON(http.StatusConflict, gin404("scope already exists"))
			return
		}
		ba.Error("insertScope failed", zap.Error(err))
		c.ResponseError(errors.New("内部错误"))
		return
	}
	c.Response(map[string]interface{}{
		"id":           id,
		"grant_id":     req.GrantID,
		"channel_id":   req.ChannelID,
		"channel_type": req.ChannelType,
		"enabled":      enabled,
	})
}

// oboDeleteScope — DELETE /v1/obo/scopes/:id. Caller must own the parent
// grant. (No ownership shortcut — we have to look up the scope first.)
func (ba *BotAPI) oboDeleteScope(c *wkhttp.Context) {
	uid := c.GetLoginUID()
	if uid == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin404("unauthorized"))
		return
	}
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	// Look up scope → grant → ownership. We use listScopesByGrant with a
	// known grant_id once we have it, but to *get* the grant_id from a
	// scope id we need a peek; simplest is to issue findGrantByID after a
	// dedicated lookup. To avoid adding another store method, REST tests
	// drive this through the in-memory fake which short-circuits ownership.
	scopeOwnerOK, err := ba.scopeOwnedBy(uid, id)
	if err != nil {
		ba.Error("scope ownership lookup failed", zap.Error(err))
		c.ResponseError(errors.New("内部错误"))
		return
	}
	if !scopeOwnerOK {
		c.JSON(http.StatusNotFound, gin404("scope not found"))
		return
	}
	if err := ba.oboStoreOrDefault().deleteScope(id); err != nil {
		ba.Error("deleteScope failed", zap.Error(err), zap.Int64("id", id))
		c.ResponseError(errors.New("内部错误"))
		return
	}
	c.ResponseOK()
}

// oboListScopes — GET /v1/obo/grants/:id/scopes.
func (ba *BotAPI) oboListScopes(c *wkhttp.Context) {
	uid := c.GetLoginUID()
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	grant, err := ba.requireOwnedGrant(c, uid, id)
	if err != nil || grant == nil {
		return
	}
	scopes, err := ba.oboStoreOrDefault().listScopesByGrant(id)
	if err != nil {
		ba.Error("listScopes failed", zap.Error(err))
		c.ResponseError(errors.New("内部错误"))
		return
	}
	c.Response(map[string]interface{}{"items": scopes})
}

// ==================== Helpers ====================

// requireOwnedGrant resolves the grant and verifies the caller owns it.
// Writes the appropriate HTTP error response and returns (nil, err) on
// any failure path so callers can simply `return`.
func (ba *BotAPI) requireOwnedGrant(c *wkhttp.Context, uid string, id int64) (*oboGrantModel, error) {
	if uid == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin404("unauthorized"))
		return nil, nil
	}
	grant, err := ba.oboStoreOrDefault().findGrantByID(id)
	if err != nil {
		ba.Error("findGrantByID failed", zap.Error(err), zap.Int64("id", id))
		c.ResponseError(errors.New("内部错误"))
		return nil, err
	}
	if grant == nil {
		c.JSON(http.StatusNotFound, gin404("grant not found"))
		return nil, nil
	}
	if grant.GrantorUID != uid {
		// Treat as 404, not 403, so we don't leak grant existence to
		// non-owners. (Same logic as classic "user enumeration" defense.)
		c.JSON(http.StatusNotFound, gin404("grant not found"))
		return nil, nil
	}
	return grant, nil
}

// scopeOwnedBy returns true if scope `id` exists AND its parent grant is
// owned by `uid`. Implemented by iterating the caller's grants because the
// store interface deliberately does not expose findScopeByID (the v0
// surface stays minimal and the per-user scope volume is tiny).
func (ba *BotAPI) scopeOwnedBy(uid string, id int64) (bool, error) {
	if uid == "" {
		return false, nil
	}
	grants, err := ba.oboStoreOrDefault().listGrantsByGrantor(uid)
	if err != nil {
		return false, err
	}
	for _, g := range grants {
		scopes, err := ba.oboStoreOrDefault().listScopesByGrant(g.ID)
		if err != nil {
			return false, err
		}
		for _, s := range scopes {
			if s.ID == id {
				return true, nil
			}
		}
	}
	return false, nil
}

// parseIDParam reads ":id" as int64. On failure writes 400 and returns
// (0, false) so the caller can `return`.
func parseIDParam(c *wkhttp.Context, name string) (int64, bool) {
	raw := c.Param(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		c.ResponseError(errors.New(name + " 无效"))
		return 0, false
	}
	return id, true
}

// gin404 is a tiny helper to avoid importing gin.H here (keeps the package's
// import surface for tests slim).
func gin404(msg string) map[string]interface{} {
	return map[string]interface{}{"msg": msg}
}
