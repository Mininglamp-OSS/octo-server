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
package bot_api

import (
	"testing"

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
	assert.Equal(t, []memberCall{{"user_bot_2_spaces", "space_B"}}, q.memberCalls,
		"isBotSpaceMember must be called with the header value to validate it")
	// DB fallback should NOT have been called when header wins.
	assert.Empty(t, q.calls,
		"querySpaceIDByRobotID must not be reached when the header is honored")
}

// Option B safe-bypass guard: if the client sends X-Space-ID for a space the
// Bot is NOT a member of, fall through to the deterministic DB query — never
// trust the header value standalone.
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
	assert.Equal(t, []memberCall{{"user_bot_2_spaces", "space_C"}}, q.memberCalls,
		"isBotSpaceMember must still be called once to make the non-member decision")
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
	assert.Empty(t, q.memberCalls,
		"isBotSpaceMember must not be called when ctx already has the SpaceID")
	assert.Empty(t, q.calls,
		"querySpaceIDByRobotID must not be called when ctx already has the SpaceID")
}

// App Bot scope=platform (no CtxKeyAppBotSpaceID) — header is honored when the
// platform Bot is a member of the requested Space. Platform Bots can dispatch
// to multiple Spaces; the header makes the dispatch context explicit.
func TestResolveBotActiveSpaceID_AppBotScopePlatform_HeaderHit(t *testing.T) {
	q := &fakeSpaceQuerier{
		defaultSpace: "space_A",
		memberships: map[string]map[string]bool{
			"app_bot_platform": {"space_B": true},
		},
	}
	ba := newTestBotAPI(q)
	c := fakeWkContextWithHeader("X-Space-ID", "space_B")
	c.Set(CtxKeyAppBotScope, "platform") // not "space" → ctx fast-path skipped

	got := ba.resolveBotActiveSpaceID(c, "app_bot_platform")
	assert.Equal(t, "space_B", got)
	assert.Empty(t, q.calls,
		"DB fallback must not be reached when header is honored")
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
