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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// FlexBoolInt — YUJ-1738 / PR#131 R2 B1.
//
// Wire-shape adapter for fields whose on-disk representation is an
// integer (0/1) but whose historical clients send either a JSON
// boolean (true/false) OR a JSON integer (0/1). encoding/json's
// default *int decoder rejects boolean tokens with
// `json: cannot unmarshal bool into Go struct field`, which silently
// 400s the entire PUT — the symptom Jerry-Xin flagged on R2 where
// the octo-web persona toggle (sends raw {"active": false}) could
// never reach the handler.
//
// Round-trip semantics:
//   - JSON `true`  → 1
//   - JSON `false` → 0
//   - JSON integer N → N (passed through; the handler still treats
//     "non-zero ⇒ activate, zero ⇒ pause" so any non-zero integer
//     is equivalent to `true`).
//   - JSON `null` → leaves the value zero (the OUTER pointer is
//     what carries the "field absent" semantic, see usage in
//     oboUpdateGrantReq.Active).
//   - any other shape (string, array, object, float) → typed error
//     so the caller still gets a clean 400 instead of a silent skip.
//
// Marshalling emits an integer so downstream consumers that already
// parse the response as a number keep working. The underlying type
// is `int` (not a struct) so callers in package-internal tests can
// keep using the pointer-pattern `v := FlexBoolInt(0); &v` they
// already use for `*int` fields like GlobalEnabled.
type FlexBoolInt int

// UnmarshalJSON implements the dual boolean-or-integer decode.
func (f *FlexBoolInt) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	switch string(trimmed) {
	case "true":
		*f = 1
		return nil
	case "false":
		*f = 0
		return nil
	case "null":
		// Defensive: encoding/json on a *FlexBoolInt receiver normally
		// leaves the pointer nil before reaching this method when the
		// token is `null`, so this branch should not fire — but if it
		// does we treat null as "no change" (zero value) rather than
		// erroring, matching std library convention for *int.
		return nil
	}
	var n int
	if err := json.Unmarshal(trimmed, &n); err != nil {
		return fmt.Errorf("expected boolean or integer, got %s", string(trimmed))
	}
	*f = FlexBoolInt(n)
	return nil
}

// MarshalJSON emits the value as a JSON integer (NOT a boolean),
// preserving wire compatibility with the original `*int` field.
func (f FlexBoolInt) MarshalJSON() ([]byte, error) {
	return strconv.AppendInt(nil, int64(f), 10), nil
}

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

// oboPersonaPromptMaxBytes caps the length of `persona_prompt` accepted on
// the wire (PR#109 / YUJ-1471). The fan-out path appends the prompt to
// every dispatched copy's `obo_system_hint`; an unbounded value would
// balloon storage, the fan-out payload, and downstream LLM token budgets.
// 4096 bytes is generous for natural-language guidance and matches the
// cap surfaced by the persona editor in octo-web.
const oboPersonaPromptMaxBytes = 4096

type oboCreateGrantReq struct {
	GranteeBotUID string `json:"grantee_bot_uid"`
	// Mode defaults to "auto" on the server. v0 rejects anything else so a
	// client can't quietly set "draft" and expect functionality. The field
	// is kept on the wire for forward-compat with v1.
	Mode string `json:"mode,omitempty"`
	// PersonaPrompt — YUJ-1465 / Mininglamp-OSS/octo-server#108 (OBO v2).
	// Optional free-form prompt the fan-out path appends to the synthetic
	// `obo_system_hint` string. Empty / absent preserves legacy behavior
	// (no prompt). Stored verbatim, including newlines and Unicode.
	PersonaPrompt string `json:"persona_prompt,omitempty"`
}

type oboUpdateGrantReq struct {
	Mode string `json:"mode,omitempty"`
	// GlobalEnabled uses *int (not int / bool) so "field omitted" and
	// "field set to 0" are distinguishable on the wire. Per RFC §5.1
	// PUT semantics: only provided fields are updated.
	GlobalEnabled *int    `json:"global_enabled,omitempty"`
	PersonaPrompt *string `json:"persona_prompt,omitempty"`
	// Active — YUJ-1728 / octo-server#129 — persona selector switch.
	// `*FlexBoolInt` (not int / bool) so "field omitted" stays
	// distinguishable from "field set to 0" — same wire convention as
	// `GlobalEnabled` above. The wrapper type accepts EITHER a JSON
	// boolean (true/false) OR a JSON integer (0/1) on the wire —
	// YUJ-1738 / PR#131 R2 B1: octo-web's PersonaSettings ships raw
	// {"active": false}; the historical `*int` decoder rejected the
	// boolean token and silently 400'd the entire PUT, leaving the
	// persona toggle inert. The handler still treats `*Active != 0`
	// as activate and `*Active == 0` as pause. Activate mutex-demotes
	// every OTHER active grant under the same grantor (single-active-
	// persona invariant, matching createOrReactivateGrantAtomic).
	// Pause only flips this row's bit — siblings are untouched.
	// Active=nil leaves the row's active column untouched. The
	// handler's pre-existing active-status gate continues to reject
	// PUTs on REVOKED rows (revoked_at != NULL); resurrecting a
	// revoked grant requires POST /v1/obo/grants. PAUSED rows
	// (active=0, revoked_at=NULL) remain addressable when the PUT
	// explicitly carries an `active` field — that's the supported
	// re-activate path (YUJ-1735 / PR#131 follow-up). PUTs that omit
	// `active` on a paused row still 404, matching the original
	// misleading-UX defense.
	Active *FlexBoolInt `json:"active,omitempty"`
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
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPISharedAuthRequired, nil, nil)
		return
	}
	var req oboCreateGrantReq
	if err := c.BindJSON(&req); err != nil {
		respondBotAPIRequestInvalid(c, "")
		return
	}
	if strings.TrimSpace(req.GranteeBotUID) == "" {
		respondBotAPIRequestInvalid(c, "grantee_bot_uid")
		return
	}
	if req.GranteeBotUID == uid {
		respondBotAPIRequestInvalid(c, "grantee_bot_uid")
		return
	}
	mode := req.Mode
	if mode == "" {
		mode = "auto"
	}
	if mode != "auto" {
		// v0 — see RFC §2 / out-of-scope. Draft mode lands in v1.
		httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOModeUnsupported, nil, nil)
		return
	}
	// PR#109 / YUJ-1471 — persona_prompt length cap. Fan-out appends
	// the prompt to every dispatched copy; a runaway-size prompt would
	// balloon storage, the obo_system_hint payload, and the LLM token
	// budget. 4096 bytes is generous for natural-language guidance and
	// matches the cap UI surfaces (see web persona editor).
	if len(req.PersonaPrompt) > oboPersonaPromptMaxBytes {
		respondBotAPIContentTooLarge(c, "persona_prompt", oboPersonaPromptMaxBytes)
		return
	}

	// PR#82 review #1 P1-2 / review #2 P2-3 — grantee_bot_uid must be a bot
	// the caller owns. Lookup hits the robot table (creator_uid) joined to
	// the user table (robot=1). 404 (not 403) on miss to keep with the
	// existence-leak posture used elsewhere in this module.
	creatorUID, isBot, found, err := ba.oboStoreOrDefault().queryRobotOwner(req.GranteeBotUID)
	if err != nil {
		ba.Error("queryRobotOwner failed", zap.Error(err), zap.String("bot", req.GranteeBotUID))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
		return
	}
	if !found || !isBot {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIBotNotRegistered, nil, nil)
		return
	}
	if creatorUID != uid {
		// Owned by someone else; treat as not-found to avoid telling the
		// caller "this bot exists but isn't yours". Same posture as
		// requireOwnedGrant.
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIBotNotRegistered, nil, nil)
		return
	}

	// PR#109 / YUJ-1471 / PR#121 R5 B2 — atomic create-or-reactivate +
	// demote. The store call wraps INSERT-or-reactivate + the v2 mutex
	// demote in a single MySQL transaction (`SELECT ... FOR UPDATE` on
	// the grantor's user row serializes concurrent create flows for
	// the same grantor). The handler MUST NOT split these into separate
	// autocommit ops: two concurrent POSTs for different bots under the
	// same grantor could both succeed and then mutually demote each
	// other to active=0, leaving the grantor with zero active personas
	// and a 200 OK on the wire. Reactivation also re-writes
	// persona_prompt verbatim (including "") so a tombstoned grant
	// never inherits the prior persona's instructions.
	grant, _, err := ba.oboStoreOrDefault().createOrReactivateGrantAtomic(uid, req.GranteeBotUID, mode, req.PersonaPrompt)
	if err != nil {
		if errors.Is(err, errOBOGrantAlreadyActive) {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIOBOGrantExists, nil, nil)
			return
		}
		ba.Error("createOrReactivateGrantAtomic failed",
			zap.Error(err),
			zap.String("grantor", uid),
			zap.String("bot", req.GranteeBotUID))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
		return
	}
	c.Response(grant)
}

// oboListGrants — GET /v1/obo/grants. Lists ALL grants (active + revoked)
// owned by the caller. UI usually filters to active on its side.
func (ba *BotAPI) oboListGrants(c *wkhttp.Context) {
	uid := c.GetLoginUID()
	if uid == "" {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPISharedAuthRequired, nil, nil)
		return
	}
	grants, err := ba.oboStoreOrDefault().listGrantsByGrantor(uid)
	if err != nil {
		ba.Error("listGrants failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
		return
	}
	c.ResponseOK()
}

// oboUpdateGrant — PUT /v1/obo/grants/:id. Toggle global_enabled / change
// mode. mode validation matches Create (v0 only accepts "auto").
//
// YUJ-1424 / PR#82 Jerry-Xin review (restored after PR#121 R5 / W1
// rebase regression): requireOwnedGrant verifies ownership but NOT
// `active` status, so a caller can flip mode / global_enabled on a
// grant they previously revoked (active=0). That silently un-tombstones
// the row's logical state from the caller's perspective (the row still
// has active=0 and won't be picked up by findActiveGrantByGrantorBot /
// -ForChannel, but PUTting mode="auto" + global_enabled=1 reads back
// as "live" via findGrantByID and gives misleading client UX). The fix
// is to reject the PUT when the row is revoked — callers that want to
// revive a revoked grant must POST /v1/obo/grants (Create takes the
// reactivation path; see oboCreateGrant's atomic create-or-reactivate
// flow). 404 (not 409) mirrors the existence-leak posture of
// requireOwnedGrant: a revoked grant is treated as "no longer
// addressable" by per-grant write endpoints.
//
// Scope: oboUpdateGrant only. oboDeleteGrant is intentionally left
// idempotent on already-revoked rows (re-revoke is a no-op), and
// oboListScopes / per-grant reads on a revoked grant still surface
// history so the UI can render audit trails.
//
// YUJ-1735 / PR#131 follow-up — distinguish "paused" (active=0,
// revoked_at=NULL) from "revoked" (active=0, revoked_at!=NULL). The
// original gate above rejected BOTH with 404, which made PUT
// {active:1} on a paused grant unreachable: the gate fired before
// BindJSON, so the reactivation intent encoded in the request body
// was never observed. Jerry-Xin and lml2468 flagged this as a P0
// blocker on the PR#131 review. The fix is to:
//
//  1. Parse the request body FIRST (no DB writes are issued until
//     after validation, so reading the body up front is cheap).
//  2. Reject revoked rows (`grant.RevokedAt != nil`) unconditionally —
//     the DELETE path is one-way; revival must go through the POST
//     create-or-reactivate flow regardless of what the body says.
//  3. For paused rows (`grant.Active != 1 && grant.RevokedAt == nil`),
//     allow the PUT iff `req.Active != nil` (i.e. the caller is
//     explicitly toggling the active bit — either re-activating, or
//     idempotently re-pausing an already-paused row). PUTs that touch
//     only mode / global_enabled / persona_prompt on a paused row
//     still 404 — those columns are settings the caller would expect
//     to take effect immediately, and silently writing them to a row
//     no findActiveGrant* lookup will surface would reproduce the
//     misleading-UX class of bug the original gate was added for.
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
		respondBotAPIRequestInvalid(c, "")
		return
	}
	// YUJ-1424 / W1 / YUJ-1735 — paused-vs-revoked gate. See function
	// doc for rationale. Defensive check on the row we already loaded;
	// no extra DB roundtrip.
	if grant.RevokedAt != nil {
		ba.Warn("OBO update rejected: grant is revoked",
			zap.Int64("grant_id", id),
			zap.String("grantor", uid),
			zap.Int("active", grant.Active))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIOBOGrantNotFound, nil, nil)
		return
	}
	if grant.Active != 1 && req.Active == nil {
		ba.Warn("OBO update rejected: grant is paused and PUT has no active field",
			zap.Int64("grant_id", id),
			zap.String("grantor", uid),
			zap.Int("active", grant.Active))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIOBOGrantNotFound, nil, nil)
		return
	}
	if req.Mode != "" && req.Mode != "auto" {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOModeUnsupported, nil, nil)
		return
	}
	// PR#109 / YUJ-1471 — persona_prompt length cap. Same rationale as
	// the create handler; rejected before any DB work hits the row.
	if req.PersonaPrompt != nil && len(*req.PersonaPrompt) > oboPersonaPromptMaxBytes {
		respondBotAPIContentTooLarge(c, "persona_prompt", oboPersonaPromptMaxBytes)
		return
	}
	if req.Mode == "" && req.GlobalEnabled == nil && req.PersonaPrompt == nil && req.Active == nil {
		// Idempotent no-op — return the existing row.
		c.Response(grant)
		return
	}
	// YUJ-1728 / octo-server#129 — apply the `active` selector first.
	// It has mutex semantics (activate ⇒ demote every other active
	// grant for the grantor in one tx) that don't compose with the
	// per-column update path, so it lives behind its own store method.
	// Order vs. updateGrant: active-first means a single PUT that
	// flips `active=true` AND sets `mode`/`persona_prompt` will land
	// on the row in its post-activation state — siblings are demoted
	// before the persona-prompt write, eliminating the race where a
	// concurrent fan-out could observe the new prompt against the
	// pre-demotion sibling set.
	if req.Active != nil {
		v := 0
		if int(*req.Active) != 0 {
			v = 1
		}
		if err := ba.oboStoreOrDefault().setGrantActive(id, v); err != nil {
			ba.Error("setGrantActive failed", zap.Error(err), zap.Int64("id", id))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
			return
		}
	}
	if req.Mode != "" || req.GlobalEnabled != nil || req.PersonaPrompt != nil {
		if err := ba.oboStoreOrDefault().updateGrant(id, req.Mode, req.GlobalEnabled, req.PersonaPrompt); err != nil {
			ba.Error("updateGrant failed", zap.Error(err), zap.Int64("id", id))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
			return
		}
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
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPISharedAuthRequired, nil, nil)
		return
	}
	var req oboCreateScopeReq
	if err := c.BindJSON(&req); err != nil {
		respondBotAPIRequestInvalid(c, "")
		return
	}
	if req.GrantID == 0 || strings.TrimSpace(req.ChannelID) == "" || req.ChannelType == 0 {
		respondBotAPIRequestInvalid(c, "")
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
		httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
		return
	}
	if !ok {
		ba.Warn("OBO scope denied: grantor has no read access to channel",
			zap.String("grantor", uid),
			zap.String("channel_id", req.ChannelID),
			zap.Uint8("channel_type", req.ChannelType))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIOBOChannelNotFound, nil, nil)
		return
	}

	enabled := 1
	if req.Enabled != nil && *req.Enabled == 0 {
		enabled = 0
	}
	id, err := ba.oboStoreOrDefault().insertScope(req.GrantID, req.ChannelID, req.ChannelType, enabled)
	if err != nil {
		if isDuplicateKeyErr(err) {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIOBOScopeExists, nil, nil)
			return
		}
		ba.Error("insertScope failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
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
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPISharedAuthRequired, nil, nil)
		return
	}
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	owner, found, err := ba.oboStoreOrDefault().findScopeOwner(id)
	if err != nil {
		ba.Error("scope ownership lookup failed", zap.Error(err), zap.Int64("id", id))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
		return
	}
	if !found || owner != uid {
		// Existence-leak defense: cross-user delete attempts return 404,
		// indistinguishable from "scope id never existed". Matches the
		// posture in requireOwnedGrant.
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIOBOScopeNotFound, nil, nil)
		return
	}
	if err := ba.oboStoreOrDefault().deleteScope(id); err != nil {
		ba.Error("deleteScope failed", zap.Error(err), zap.Int64("id", id))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
		return
	}
	c.Response(map[string]interface{}{"items": scopes})
}

// ==================== Bot-token endpoint (issue #135) ====================

// oboBotGetGrantResp is the wire shape for GET /v1/bot/obo-grant.
// Field set is intentionally narrow — the adapter only needs the four
// values listed in the issue (grantor uid + the persona prompt + the
// two booleans that gate "should I apply the persona right now?").
// We do not surface the grant id, timestamps, or any scope rows: a bot
// authenticated by its own token has no use for grant-management
// metadata, and leaking it would expand the blast radius if the token
// is exfiltrated.
type oboBotGetGrantResp struct {
	GrantorUID    string `json:"grantor_uid"`
	PersonaPrompt string `json:"persona_prompt"`
	Active        bool   `json:"active"`
	GlobalEnabled bool   `json:"global_enabled"`
}

// oboBotGetGrant — GET /v1/bot/obo-grant.
// Mininglamp-OSS/octo-server#135 (YUJ-1762). Bot-token authenticated;
// returns the active OBO grant where the calling bot is the grantee so
// the adapter can pull `persona_prompt` at runtime without keeping a
// local copy.
//
// Auth posture: this handler intentionally reads the bot UID from the
// authenticated context (CtxKeyRobotID set by ba.authBot()), NOT from
// the URL or body. The endpoint never accepts a bot uid as input — a
// compromised bot can only ever read its OWN grant, never probe for
// someone else's persona by spoofing a path parameter.
//
// Status code map:
//   - 200 — grant found; body is oboBotGetGrantResp.
//   - 401 — handled by ba.authBot() upstream (this handler only runs
//     on authenticated requests; the defensive empty-uid branch
//     below 500s because reaching it means the middleware broke
//     its invariant and a 401 would mask that).
//   - 404 — no active grant for this bot. Note: "no active grant"
//     covers both "grant never existed" and "grant was paused
//     or revoked"; the adapter should treat 404 as "do not
//     apply a persona" without distinguishing the cause.
//   - 500 — store error.
//
// `persona_prompt` round-trips as an empty string when the column is
// NULL, courtesy of the COALESCE wrapper in oboGrantColumns (issue
// requirement: "persona_prompt 为 NULL 时返回空串").
func (ba *BotAPI) oboBotGetGrant(c *wkhttp.Context) {
	botUID := getRobotIDFromContext(c)
	if botUID == "" {
		// authBot middleware should have populated this. Treat as 500
		// rather than 401 so the misconfiguration is noisy upstream
		// instead of being silently mis-attributed to an auth failure.
		ba.Error("oboBotGetGrant: empty robot id from auth context")
		httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
		return
	}
	grant, err := ba.oboStoreOrDefault().findActiveGrantByBot(botUID)
	if err != nil {
		ba.Error("findActiveGrantByBot failed",
			zap.Error(err),
			zap.String("bot", botUID))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
		return
	}
	if grant == nil {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIOBOGrantNotFound, nil, nil)
		return
	}
	c.Response(oboBotGetGrantResp{
		GrantorUID:    grant.GrantorUID,
		PersonaPrompt: grant.PersonaPrompt,
		Active:        grant.Active == 1,
		GlobalEnabled: grant.GlobalEnabled == 1,
	})
}

// ==================== Helpers ====================

// requireOwnedGrant resolves the grant and verifies the caller owns it.
// Writes the appropriate HTTP error response and returns (nil, err) on
// any failure path so callers can simply `return`.
func (ba *BotAPI) requireOwnedGrant(c *wkhttp.Context, uid string, id int64) (*oboGrantModel, error) {
	if uid == "" {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPISharedAuthRequired, nil, nil)
		return nil, nil
	}
	grant, err := ba.oboStoreOrDefault().findGrantByID(id)
	if err != nil {
		ba.Error("findGrantByID failed", zap.Error(err), zap.Int64("id", id))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
		return nil, err
	}
	if grant == nil {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIOBOGrantNotFound, nil, nil)
		return nil, nil
	}
	if grant.GrantorUID != uid {
		// Treat as 404, not 403, so we don't leak grant existence to
		// non-owners. (Same logic as classic "user enumeration" defense.)
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIOBOGrantNotFound, nil, nil)
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
//
// Test hook: ba.oboGroupMemberOverride lets unit tests stub the answer
// without standing up MySQL (PR#121 R8 / YUJ-1673 — needed to exercise
// the explicit-scope Gate 4 paths in fanoutForMessage). nil override →
// DB path.
func (ba *BotAPI) userIsGroupMember(uid, groupNo string) (bool, error) {
	if ba.oboGroupMemberOverride != nil {
		return ba.oboGroupMemberOverride(uid, groupNo)
	}
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
		respondBotAPIRequestInvalid(c, name)
		return 0, false
	}
	return id, true
}

// gin404 is a tiny helper to avoid importing gin.H here (keeps the package's
// import surface for tests slim).
func gin404(msg string) map[string]interface{} {
	return map[string]interface{}{"msg": msg}
}
