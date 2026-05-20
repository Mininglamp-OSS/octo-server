// Package bot_api · YUJ-1166 / Mininglamp-OSS/octo-server#81 — Persona Clone
// OBO REST endpoints.
//
// These endpoints are mounted under /v1/obo behind the standard user-auth
// middleware (ba.ctx.AuthMiddleware) — they take a USER token, not a bot
// token. The acting user must be the grantor of the row they CRUD; we do
// NOT support cross-user persona management in v0 (RFC §2 / out-of-scope).
//
// Status code map (kept narrow on purpose):
//
//	200 — success (single object or list)
//	400 — bad request body / missing required fields
//	401 — no user token (handled by upstream middleware)
//	403 — (reserved — production currently uses 404 for cross-user attempts
//	       as a user-enumeration defense; see requireOwnedGrant comment.)
//	404 — grant_id / scope_id not found; cross-user grant/scope access
//	       (existence-leak defense)
//	409 — duplicate (grantor+grantee already exists / scope already exists,
//	       with no soft-deleted row to reactivate in place)
//	500 — DB error
package bot_api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
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
//
// PR#82 hardening:
//   - grantee_bot_uid MUST resolve to a robot row with CreatorUID == uid
//     AND user.robot=1. Without this check a user could install an OBO
//     grant against someone ELSE's bot, force-feeding it copies of any
//     channel traffic the grantor can see and muddying audit trails that
//     key on grantee_bot_uid. (Review #1 task spec P1-2 + review #2 P2-3.)
//   - When the UNIQUE KEY uk_grantor_grantee fires on insert, look up the
//     existing row. If it's a soft-deleted row the caller already owns,
//     reactivate it in place (active=1, global_enabled=0, revoked_at=NULL)
//     rather than returning 409. Without this path a single
//     DELETE /v1/obo/grants/:id would permanently brick the (grantor, bot)
//     pair (review #2 P1-1). global_enabled is intentionally reset to 0
//     on reactivation so the caller must re-issue a PUT to enable fan-out
//     — matches the fail-closed default for a brand-new grant.
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

	// PR#82 review #1 P1-2 / review #2 P2-3 — grantee_bot_uid must be a bot
	// the caller owns. Lookup hits the robot table (creator_uid) joined to
	// the user table (robot=1). 404 (not 403) on miss to keep with the
	// existence-leak posture used elsewhere in this module.
	creatorUID, isBot, found, err := ba.oboStoreOrDefault().queryRobotOwner(req.GranteeBotUID)
	if err != nil {
		ba.Error("queryRobotOwner failed", zap.Error(err), zap.String("bot", req.GranteeBotUID))
		c.ResponseError(errors.New("内部错误"))
		return
	}
	if !found || !isBot {
		c.JSON(http.StatusNotFound, gin404("grantee_bot_uid not a registered bot"))
		return
	}
	if creatorUID != uid {
		// Owned by someone else; treat as not-found to avoid telling the
		// caller "this bot exists but isn't yours". Same posture as
		// requireOwnedGrant.
		c.JSON(http.StatusNotFound, gin404("grantee_bot_uid not a registered bot"))
		return
	}

	id, err := ba.oboStoreOrDefault().insertGrant(uid, req.GranteeBotUID, mode)
	if err != nil {
		if isDuplicateKeyErr(err) {
			// Reactivation path: a row already exists. If it's a soft-deleted
			// row owned by the same caller, flip it back to active and return
			// the refreshed row. Anything else → 409.
			existing, lookupErr := ba.oboStoreOrDefault().findGrantByGrantorBot(uid, req.GranteeBotUID)
			if lookupErr != nil {
				ba.Error("post-duplicate findGrant lookup failed", zap.Error(lookupErr))
				c.JSON(http.StatusConflict, gin404("grant already exists"))
				return
			}
			if existing == nil || existing.GrantorUID != uid {
				// Shouldn't happen — UNIQUE on (grantor, grantee) means the
				// duplicate must be ours. Defensive: 409 rather than 500.
				c.JSON(http.StatusConflict, gin404("grant already exists"))
				return
			}
			if existing.Active == 1 {
				// Live row → genuine duplicate, not a reactivation case.
				c.JSON(http.StatusConflict, gin404("grant already exists"))
				return
			}
			if reactErr := ba.oboStoreOrDefault().reactivateGrant(existing.ID); reactErr != nil {
				ba.Error("reactivateGrant failed", zap.Error(reactErr), zap.Int64("id", existing.ID))
				c.ResponseError(errors.New("内部错误"))
				return
			}
			refreshed, _ := ba.oboStoreOrDefault().findGrantByID(existing.ID)
			if refreshed != nil {
				c.Response(refreshed)
				return
			}
			c.Response(map[string]interface{}{"id": existing.ID})
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
//
// YUJ-1424 / PR#82 Jerry-Xin review (2026-05-20, "Also fix" non-blocking):
// requireOwnedGrant verifies ownership but NOT `active` status, so a
// caller can flip mode / global_enabled on a grant they previously
// revoked (active=0). That silently un-tombstones the row's logical
// state from the caller's perspective (the row still has active=0 and
// won't be picked up by findActiveGrantByGrantorBot / -ForChannel, but
// PUTting mode="auto" + global_enabled=1 reads back as "live" via
// findGrantByID and gives misleading client UX). The fix is to reject
// the PUT when the row is revoked — callers that want to revive a
// revoked grant must POST /v1/obo/grants (Create takes the
// reactivation path; see oboCreateGrant's "soft-revoked → reactivate
// in place" branch). 404 (not 409) mirrors the existence-leak posture
// of requireOwnedGrant: a revoked grant is treated as "no longer
// addressable" by per-grant write endpoints.
//
// Scope: oboUpdateGrant only. oboDeleteGrant is intentionally left
// idempotent on already-revoked rows (re-revoke is a no-op), and
// oboListScopes / per-grant reads on a revoked grant still surface
// history so the UI can render audit trails.
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
	// YUJ-1424 — active gate. See function doc for rationale. Defensive
	// check on the row we already loaded; no extra DB roundtrip.
	if grant.Active != 1 {
		ba.Warn("OBO update rejected: grant is revoked",
			zap.Int64("grant_id", id),
			zap.String("grantor", uid),
			zap.Int("active", grant.Active))
		c.JSON(http.StatusNotFound, gin404("grant not found"))
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
//
// PR#82 P0 (channel-wiretap, three reviewers concur): after grant ownership
// passes, verify the GRANTOR (= calling user uid) has read access to the
// target (channel_id, channel_type). Without this check, a logged-in user
// could create a scope for a channel they are not a member of — every
// inbound message in that channel would then be fan-out-copied to their
// bot, exfiltrating channel traffic the grantor was never authorized to
// see. The check fails closed: any DB error, missing membership row, or
// unknown channel type → 404 (existence-leak posture). 404 (not 403)
// matches requireOwnedGrant; combined, the two checks never reveal
// "grant exists but channel access denied" vs "channel doesn't exist".
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
	// P0 — channel-wiretap defense. Grantor MUST be able to read the
	// target channel themselves before they can authorize a bot to read it
	// on their behalf. See grantorCanReadChannel for the per-channel-type
	// predicate. Failed check → 404 (existence-leak defense; never tell
	// the caller whether the channel exists).
	ok, err := ba.grantorCanReadChannel(uid, req.ChannelID, req.ChannelType)
	if err != nil {
		ba.Error("grantor channel-access check failed",
			zap.Error(err), zap.String("grantor", uid),
			zap.String("channel_id", req.ChannelID),
			zap.Uint8("channel_type", req.ChannelType))
		c.ResponseError(errors.New("内部错误"))
		return
	}
	if !ok {
		ba.Warn("OBO scope denied: grantor has no read access to channel",
			zap.String("grantor", uid),
			zap.String("channel_id", req.ChannelID),
			zap.Uint8("channel_type", req.ChannelType))
		c.JSON(http.StatusNotFound, gin404("channel not found"))
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
// grant.
//
// PR#82 review #2 P1-3: ownership resolves in a single JOIN query
// (oboStore.findScopeOwner) instead of the previous
// O(grants × scopes_per_grant) scan. A power user with 50 grants × 200
// scopes/grant required ~10k DB queries to delete one scope under the
// old path; now it is two queries (find owner + delete row).
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
	owner, found, err := ba.oboStoreOrDefault().findScopeOwner(id)
	if err != nil {
		ba.Error("scope ownership lookup failed", zap.Error(err), zap.Int64("id", id))
		c.ResponseError(errors.New("内部错误"))
		return
	}
	if !found || owner != uid {
		// Existence-leak defense: cross-user delete attempts return 404,
		// indistinguishable from "scope id never existed". Matches the
		// posture in requireOwnedGrant.
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

// grantorCanReadChannel verifies the grantor (the calling user) has read
// access to (channelID, channelType). Used by oboCreateScope to plug the
// channel-wiretap vulnerability described in PR#82 reviews — a scope row
// must NOT be creatable for a channel the grantor cannot themselves read,
// because once it lands, the fan-out listener will replay every inbound
// message from that channel to the grantee bot regardless of whether the
// grantor is still (or ever was) a member.
//
// Per-type predicates mirror checkSendPermission (modules/bot_api/send.go)
// so the rules can't diverge over time:
//   - ChannelTypeGroup → grantor must have an undeleted group_member row
//     for group_no = channel_id.
//   - ChannelTypeCommunityTopic → channel_id is "<parent_group_no>____<short_id>";
//     grantor must be a member of the parent group. Threads inherit
//     parent-group read-ACL in the rest of the codebase (see send.go:200,
//     sync.go:106); we reuse that invariant here.
//   - ChannelTypePerson → channel_id is the peer uid for a DM. The
//     grantor is "in" a DM iff they ARE the peer (DM channel ids in
//     octo-server are bare uids — see resolveSpaceChannelID for the no-op
//     return that documents this) or they're friends with the peer. We
//     allow either: a user has read access to "their own DM with X" if
//     they are X (the peer), and a real user is allowed to authorize
//     fan-out from a DM-with-friend regardless of which side initiated.
//
// Test hook: ba.oboChannelAccessOverride lets unit tests stub the answer
// without standing up MySQL or a user service. nil override → DB path.
func (ba *BotAPI) grantorCanReadChannel(uid, channelID string, channelType uint8) (bool, error) {
	if ba.oboChannelAccessOverride != nil {
		return ba.oboChannelAccessOverride(uid, channelID, channelType)
	}
	if uid == "" || channelID == "" {
		return false, nil
	}
	switch channelType {
	case common.ChannelTypeGroup.Uint8():
		return ba.userIsGroupMember(uid, channelID)
	case common.ChannelTypeCommunityTopic.Uint8():
		// Thread channel id format: "<parent_group_no>____<short_id>".
		// Reuse threadChannelIDSeparator + the convention already
		// established by send.go / sync.go for parent-group membership.
		parts := strings.SplitN(channelID, threadChannelIDSeparator, 2)
		if len(parts) != 2 || parts[0] == "" {
			// Malformed thread id — treat as no-access (fail-closed).
			return false, nil
		}
		return ba.userIsGroupMember(uid, parts[0])
	case common.ChannelTypePerson.Uint8():
		// DM peer self-access: a user is trivially "in" a DM that is
		// themselves (the channel id is the peer; if uid == peer they
		// would be DMing themselves, which is degenerate but allowed).
		if uid == channelID {
			return true, nil
		}
		// Otherwise: must be friends with the peer. Mirrors the
		// BotKindUser ChannelTypePerson branch of checkSendPermission.
		if ba.userService == nil {
			// No user service wired (tests should set override) — fail-closed.
			return false, nil
		}
		return ba.userService.IsFriend(uid, channelID)
	default:
		// Unknown channel type → cannot authorize fan-out for it.
		return false, nil
	}
}

// userIsGroupMember returns true iff `uid` has an undeleted row in
// group_member for group_no=groupNo. Mirrors the SQL used by
// checkSendPermission's BotKindUser/ChannelTypeGroup branch.
func (ba *BotAPI) userIsGroupMember(uid, groupNo string) (bool, error) {
	if ba.db == nil || ba.db.session == nil {
		// No DB session (some test contexts) — fail-closed.
		return false, nil
	}
	var count int
	err := ba.db.session.SelectBySql(
		"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0",
		groupNo, uid,
	).LoadOne(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
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
