// Package robot · Mininglamp-OSS/octo-server#36 (PR#35 deep-review High-2):
// querySpaceIDByRobotID multi-Space ambiguity. legacy /v1/robot/.../sendMessage
// has no SpaceMiddleware / authAppBot context, so it falls back exclusively to
// the deterministic ORDER BY (Option C). Coverage:
//
//   - 2-Space User Bot dispatch returns deterministic primary (earliest joined)
//   - Repeated calls return the same SpaceID (no engine flapping)
//   - PERSONAL DM enrich path overrides client-forged payload.space_id with
//     the deterministic primary, not engine-dependent first row.
package robot

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// 2-Space User Bot — deterministic primary on the legacy robot path.
func TestResolveBotActiveSpaceID_MultiSpace_DeterministicPrimary(t *testing.T) {
	q := &fakeRobotSpaceQuerier{
		// rows[0] is the deterministic primary (earliest joined wins).
		rows: []string{"space_A", "space_B"},
	}
	rb := newTestRobot(q)

	first := rb.resolveBotActiveSpaceID("user_bot_2_spaces")
	second := rb.resolveBotActiveSpaceID("user_bot_2_spaces")

	assert.Equal(t, "space_A", first,
		"legacy /v1/robot path: primary = earliest joined Space A")
	assert.Equal(t, first, second,
		"repeated calls must be stable (deterministic ORDER BY)")
}

// PERSONAL DM enrich on the legacy /v1/robot/.../sendMessage path with a
// 2-Space User Bot and a forged client-supplied payload.space_id. The forged
// value must be overridden by the deterministic primary.
func TestEnrichBotPayloadWithSpaceID_MultiSpace_OverridesForgedClient(t *testing.T) {
	q := &fakeRobotSpaceQuerier{rows: []string{"space_A", "space_B"}}
	rb := newTestRobot(q)
	payload := map[string]interface{}{"content": "hi", "space_id": "space_attacker"}

	got := rb.enrichBotPayloadWithSpaceID("user_bot_2_spaces", payload)
	assert.Equal(t, "space_A", got["space_id"],
		"deterministic primary must override forged client space_id on legacy path")
}
