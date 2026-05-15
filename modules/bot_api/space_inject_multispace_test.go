// Package bot_api · Mininglamp-OSS/octo-server#36 (PR#35 deep-review High-2):
// querySpaceIDByRobotID multi-Space ambiguity. Coverage:
//
//   - 2-Space User Bot dispatch with no header → deterministic primary
//   - 2-Space User Bot with valid X-Space-ID header → header wins (Option B)
//   - 2-Space User Bot with X-Space-ID header for non-member space → fall
//     through to deterministic primary (Option B safe-bypass guard)
//   - X-Space-ID header preferred over App Bot scope=platform DB result
//   - X-Space-ID header IGNORED when App Bot scope=space already present
//     (CtxKeyAppBotSpaceID is the strongest server-authoritative signal)
//
// YUJ-688 / PR#43 R1 — additional coverage for the platform App Bot validator
// gap (Critical from Jerry-Xin + lml2468). The previous validator only
// checked space_member; platform App Bots have no space_member row but are
// visible in every active Space. Coverage added below:
//
//   - Production-shaped platform App Bot: NO space_member row, only an
//     app_bot row with scope='platform' status=1 → header is honored
//   - Platform App Bot but target Space is inactive → header is rejected
//   - Scope=space App Bot dispatching into its own Space (defensive path)
//   - Scope=space App Bot rejected when header asks for a different Space
//   - X-Space-ID header value with leading/trailing whitespace is trimmed
package bot_api

import (
	"testing"

	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/assert"
)

// Mininglamp-OSS/octo-server#36 — User Bot is a member of Space A and Space B.
// Without any header, the deterministic ORDER BY (earliest joined wins) picks
// Space A. This test asserts the result is *stable* — repeated calls return
// the same value — and matches the chosen primary from the multi-row stub.
func TestResolveBotActiveSpaceID_MultiSpace_NoHeader_DeterministicPrimary(t *testing.T) {
	q := &fakeSpaceQuerier{
		// `multiRows` for "user_bot_2_spaces" is the engine-stable order:
		// Space A is the earliest-joined → first element.
		multiRows: map[string][]string{
			"user_bot_2_spaces": {"space_A", "space_B"},
		},
		defaultSpace: "space_A", // for querySpaceIDByRobotID single-row path
	}
	ba := newTestBotAPI(q)
	c := fakeWkContext()

	first := ba.resolveBotActiveSpaceID(c, "user_bot_2_spaces")
	second := ba.resolveBotActiveSpaceID(c, "user_bot_2_spaces")

	assert.Equal(t, "space_A", first,
		"deterministic primary = earliest joined Space A")
	assert.Equal(t, first, second,
		"repeated calls must return the same SpaceID (no engine flapping)")
}

// Option B happy-path: 2-Space User Bot with X-Space-ID=space_B → bot is a
// member → header wins.
func TestResolveBotActiveSpaceID_MultiSpace_HeaderHit_HeaderWins(t *testing.T) {
	q := &fakeSpaceQuerier{
		multiRows: map[string][]string{
			"user_bot_2_spaces": {"space_A", "space_B"},
		},
		defaultSpace: "space_A",
		memberships: map[string]map[string]bool{
			"user_bot_2_spaces": {"space_B": true},
		},
	}
	ba := newTestBotAPI(q)
	c := fakeWkContextWithHeader("X-Space-ID", "space_B")

	got := ba.resolveBotActiveSpaceID(c, "user_bot_2_spaces")
	assert.Equal(t, "space_B", got,
		"X-Space-ID header should be honored when Bot is a member of that Space")
	assert.Equal(t, []memberCall{{"user_bot_2_spaces", "space_B"}}, q.authCalls,
		"isBotSpaceAuthorized must be called with the header value to validate it")
	// DB fallback should NOT have been called when header wins.
	assert.Empty(t, q.calls,
		"querySpaceIDByRobotID must not be reached when the header is honored")
}

// Option B safe-bypass guard: if the client sends X-Space-ID for a space the
// Bot is NOT authorized for, fall through to the deterministic DB query —
// never trust the header value standalone.
func TestResolveBotActiveSpaceID_MultiSpace_HeaderMiss_FallsThrough(t *testing.T) {
	q := &fakeSpaceQuerier{
		multiRows: map[string][]string{
			"user_bot_2_spaces": {"space_A", "space_B"},
		},
		defaultSpace: "space_A",
		memberships: map[string]map[string]bool{
			"user_bot_2_spaces": {"space_C": false}, // explicitly non-member
		},
	}
	ba := newTestBotAPI(q)
	c := fakeWkContextWithHeader("X-Space-ID", "space_C")

	got := ba.resolveBotActiveSpaceID(c, "user_bot_2_spaces")
	assert.Equal(t, "space_A", got,
		"non-member header → fall through to deterministic primary (Space A)")
	assert.Equal(t, []memberCall{{"user_bot_2_spaces", "space_C"}}, q.authCalls,
		"isBotSpaceAuthorized must still be called once to make the non-member decision")
	assert.Equal(t, []string{"user_bot_2_spaces"}, q.calls,
		"querySpaceIDByRobotID must be invoked (via querySpaceIDsByRobotID) on miss")
}

// App Bot scope=space (CtxKeyAppBotSpaceID present) outranks the X-Space-ID
// header. The header should NOT cause a Bot scope=space dispatch to leak into
// a different Space — authAppBot already wrote the authoritative SpaceID.
func TestResolveBotActiveSpaceID_AppBotScopeSpace_OutranksHeader(t *testing.T) {
	q := &fakeSpaceQuerier{}
	ba := newTestBotAPI(q)
	c := fakeWkContextWithHeader("X-Space-ID", "space_attacker")
	c.Set(CtxKeyAppBotScope, "space")
	c.Set(CtxKeyAppBotSpaceID, "space_authoritative")

	got := ba.resolveBotActiveSpaceID(c, "app_bot_scope_space")
	assert.Equal(t, "space_authoritative", got,
		"App Bot scope=space ctx must outrank X-Space-ID header")
	assert.Empty(t, q.authCalls,
		"isBotSpaceAuthorized must not be called when ctx already has the SpaceID")
	assert.Empty(t, q.calls,
		"querySpaceIDByRobotID must not be called when ctx already has the SpaceID")
}

// YUJ-688 / PR#43 R1 — production-shaped: platform App Bot has NO space_member
// row, only an `app_bot` row with scope='platform' status=1. With the previous
// validator (`isBotSpaceMember` checking only space_member) this case
// returned `false` and the caller's enrich path stripped the payload. The
// fix authorizes platform App Bots in every active Space.
//
// This test fails on the original `isBotSpaceMember`-only validator and
// passes on the new `isBotSpaceAuthorized` validator: it must NOT rely on
// `memberships` being stubbed true.
func TestResolveBotActiveSpaceID_AppBotScopePlatform_HeaderHit(t *testing.T) {
	q := &fakeSpaceQuerier{
		defaultSpace: "space_A",
		// CRITICAL: no `memberships` entry. Platform App Bots are not in
		// space_member; the production fake mirrors that shape.
		appBots: map[string]appBotShape{
			"app_bot_platform": {publishedPlatform: true},
		},
		// space_B is active by default (activeSpaces nil → all active).
	}
	ba := newTestBotAPI(q)
	c := fakeWkContextWithHeader("X-Space-ID", "space_B")
	c.Set(CtxKeyAppBotScope, "platform") // not "space" → ctx fast-path skipped

	got := ba.resolveBotActiveSpaceID(c, "app_bot_platform")
	assert.Equal(t, "space_B", got,
		"published platform App Bot must be authorized in any active Space, "+
			"even when no space_member row exists (PR#43 R1 fix)")
	assert.Empty(t, q.calls,
		"DB fallback must not be reached when header is honored")
	assert.Equal(t, []memberCall{{"app_bot_platform", "space_B"}}, q.authCalls,
		"isBotSpaceAuthorized must be called once with the header value")
}

// YUJ-688 medium ask (lml2468): if the header points at an INACTIVE Space, we
// must reject — never let a platform App Bot stamp a payload with a deleted
// or disabled SpaceID.
func TestResolveBotActiveSpaceID_AppBotScopePlatform_HeaderForInactiveSpace_FallsThrough(t *testing.T) {
	q := &fakeSpaceQuerier{
		defaultSpace: "space_A",
		appBots: map[string]appBotShape{
			"app_bot_platform": {publishedPlatform: true},
		},
		activeSpaces: map[string]bool{
			"space_disabled": false,
		},
	}
	ba := newTestBotAPI(q)
	c := fakeWkContextWithHeader("X-Space-ID", "space_disabled")
	c.Set(CtxKeyAppBotScope, "platform")

	got := ba.resolveBotActiveSpaceID(c, "app_bot_platform")
	assert.Equal(t, "space_A", got,
		"inactive target Space must be rejected even for platform App Bots; "+
			"resolver falls through to deterministic DB primary")
	assert.Equal(t, []memberCall{{"app_bot_platform", "space_disabled"}}, q.authCalls,
		"isBotSpaceAuthorized must still be called and return false for inactive Space")
}

// Defensive: scope=space App Bot dispatching into its own Space via the
// header path (bypassing the ctx fast-path) is authorized.
func TestResolveBotActiveSpaceID_AppBotScopeSpace_OwnSpaceAuthorized(t *testing.T) {
	q := &fakeSpaceQuerier{
		defaultSpace: "space_A",
		appBots: map[string]appBotShape{
			"app_bot_scope_space": {scopeSpaceID: "space_own", published: true},
		},
	}
	ba := newTestBotAPI(q)
	c := fakeWkContextWithHeader("X-Space-ID", "space_own")
	// Note: CtxKeyAppBotScope intentionally NOT "space" so we exercise the
	// header validator branch rather than the ctx fast-path. In production
	// this combination is rare (authAppBot would normally set the ctx) but
	// the validator must still fail-closed-correctly.

	got := ba.resolveBotActiveSpaceID(c, "app_bot_scope_space")
	assert.Equal(t, "space_own", got,
		"scope=space App Bot dispatching into its own Space is authorized")
}

// Defensive: scope=space App Bot is NOT authorized for a different Space.
// Without this guard the validator would let a scope=space App Bot send into
// any Space its caller asks for via the header.
func TestResolveBotActiveSpaceID_AppBotScopeSpace_DifferentSpaceRejected(t *testing.T) {
	q := &fakeSpaceQuerier{
		defaultSpace: "space_A",
		appBots: map[string]appBotShape{
			"app_bot_scope_space": {scopeSpaceID: "space_own", published: true},
		},
	}
	ba := newTestBotAPI(q)
	c := fakeWkContextWithHeader("X-Space-ID", "space_other")

	got := ba.resolveBotActiveSpaceID(c, "app_bot_scope_space")
	assert.Equal(t, "space_A", got,
		"scope=space App Bot must NOT be authorized in a Space other than its own; "+
			"resolver falls through to deterministic DB primary")
}

// YUJ-688 medium ask (lml2468): trim whitespace from the X-Space-ID header
// before validation. Some clients send "  space_X  " or trailing CR; without
// TrimSpace these were rejected as non-member and emitted noisy reject warns.
func TestResolveBotActiveSpaceID_HeaderWhitespaceTrimmed(t *testing.T) {
	q := &fakeSpaceQuerier{
		defaultSpace: "space_A",
		memberships: map[string]map[string]bool{
			"user_bot_2_spaces": {"space_B": true},
		},
	}
	ba := newTestBotAPI(q)
	c := fakeWkContextWithHeader("X-Space-ID", "  space_B  \r")

	got := ba.resolveBotActiveSpaceID(c, "user_bot_2_spaces")
	assert.Equal(t, "space_B", got,
		"X-Space-ID header value must be TrimSpace'd before validation")
	assert.Equal(t, []memberCall{{"user_bot_2_spaces", "space_B"}}, q.authCalls,
		"isBotSpaceAuthorized must receive the trimmed value, not the raw header")
}

// All-whitespace header is treated as no header (TrimSpace empty → skip path).
func TestResolveBotActiveSpaceID_HeaderAllWhitespace_TreatedAsAbsent(t *testing.T) {
	q := &fakeSpaceQuerier{defaultSpace: "space_A"}
	ba := newTestBotAPI(q)
	c := fakeWkContextWithHeader("X-Space-ID", "   ")

	got := ba.resolveBotActiveSpaceID(c, "user_bot_x")
	assert.Equal(t, "space_A", got)
	assert.Empty(t, q.authCalls,
		"all-whitespace header must skip the validator entirely (treated as absent)")
}

// Mininglamp-OSS/octo-server#36 enrich-end-to-end: 2-Space User Bot dispatching
// PERSONAL DM. Without the header, payload.space_id should match the
// deterministic primary (not whatever the client put in payload.space_id).
func TestEnrichBotPayloadWithSpaceID_MultiSpace_NoHeader_DeterministicOverride(t *testing.T) {
	q := &fakeSpaceQuerier{
		multiRows: map[string][]string{
			"user_bot_2_spaces": {"space_A", "space_B"},
		},
		defaultSpace: "space_A",
	}
	ba := newTestBotAPI(q)
	c := fakeWkContext()
	payload := map[string]interface{}{"content": "hi", "space_id": "space_attacker"}

	got := ba.enrichBotPayloadWithSpaceID(c, "user_bot_2_spaces", payload)
	assert.Equal(t, "space_A", got["space_id"],
		"client-supplied forged space_id must be overridden by deterministic primary")
}

// Mininglamp-OSS/octo-server#36 enrich-end-to-end with header: 2-Space User
// Bot dispatching PERSONAL DM with X-Space-ID=space_B → payload.space_id ends
// up as space_B (not the deterministic primary, not the client-supplied
// forged value).
func TestEnrichBotPayloadWithSpaceID_MultiSpace_HeaderHit_HeaderOverridesPrimary(t *testing.T) {
	q := &fakeSpaceQuerier{
		multiRows: map[string][]string{
			"user_bot_2_spaces": {"space_A", "space_B"},
		},
		defaultSpace: "space_A",
		memberships: map[string]map[string]bool{
			"user_bot_2_spaces": {"space_B": true},
		},
	}
	ba := newTestBotAPI(q)
	c := fakeWkContextWithHeader("X-Space-ID", "space_B")
	payload := map[string]interface{}{"content": "hi", "space_id": "space_attacker"}

	got := ba.enrichBotPayloadWithSpaceID(c, "user_bot_2_spaces", payload)
	assert.Equal(t, "space_B", got["space_id"],
		"valid X-Space-ID header must drive payload.space_id, overriding both client forge and DB primary")
}

// YUJ-688 / PR#43 R1 — end-to-end production case: platform App Bot dispatch
// with valid X-Space-ID. Before the fix, the validator rejected the header
// (no space_member row), the DB primary returned "" (no space_member rows
// for platform App Bots either), and the enrich path STRIPPED the payload
// (fail-closed) — silently downgrading every legitimate platform App Bot
// dispatch to a Space-less PERSONAL DM. After the fix, the header is honored
// and the payload carries the correct SpaceID.
func TestEnrichBotPayloadWithSpaceID_AppBotScopePlatform_NoSpaceMember_HeaderWins(t *testing.T) {
	q := &fakeSpaceQuerier{
		// Platform App Bot: not in space_member; only in app_bot scope=platform.
		// Production DB lookup of querySpaceIDsByRobotID returns ErrNotFound
		// because platform App Bots have no space_member rows.
		defaultErr: dbr.ErrNotFound,
		appBots: map[string]appBotShape{
			"app_bot_platform_prod": {publishedPlatform: true},
		},
	}
	ba := newTestBotAPI(q)
	c := fakeWkContextWithHeader("X-Space-ID", "space_target")
	c.Set(CtxKeyAppBotScope, "platform")
	payload := map[string]interface{}{"content": "hi", "space_id": "space_forged"}

	got := ba.enrichBotPayloadWithSpaceID(c, "app_bot_platform_prod", payload)
	assert.Equal(t, "space_target", got["space_id"],
		"platform App Bot with valid X-Space-ID must end up with space_target in payload "+
			"(regression: previous validator stripped it because the bot wasn't in space_member)")
}
