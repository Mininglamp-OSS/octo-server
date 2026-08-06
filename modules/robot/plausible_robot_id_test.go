package robot

import (
	"os"
	"strings"
	"testing"
)

// TestPlausibleRobotID pins the syntactic gate in front of the payload.robot_id lookup.
//
// #697 review, sixth round: the lookup fails open on a query error so a DB blip cannot drop
// a bot event, which leaves the fail-open path able to adopt an arbitrary client-supplied
// string. The monotonic allocator turns every distinct value into a permanent
// `botEventSeq:counter:{id}` key (no TTL, `noeviction`) plus a `seq` row that is never
// reclaimed, so the bound is what keeps that state finite.
func TestPlausibleRobotID(t *testing.T) {
	cases := []struct {
		name      string
		candidate string
		want      bool
	}{
		// Shapes that really occur: UUID-hex in production, underscored ids in fixtures
		// and for well-known bots. Rejecting any of these would silently stop a real
		// bot's events, which is why there is no charset allowlist.
		{"uuid hex", "0f8fad5bd9cb469fa16570867728950e", true},
		{"botfather", "botfather", true},
		{"underscored fixture id", "test_robot_001", true},
		{"dashed id", "lp-bot-1", true},
		{"exactly the column width", strings.Repeat("a", robotIDMaxLen), true},

		// The decisive bound: longer than `robot.robot_id` (VARCHAR(40)) cannot be a row
		// in that table, so adopting it could only ever create permanent state for a bot
		// that provably does not exist.
		{"one over the column width", strings.Repeat("a", robotIDMaxLen+1), false},
		{"absurdly long", strings.Repeat("x", 4096), false},

		// An id that cannot round-trip identically through a log line or a key listing is
		// not one anybody meant to send, and botevent.NextEventID rejects padding for the
		// same reason: the allocator, the queue key and the doorbell must key off the
		// identical string.
		{"empty", "", false},
		{"leading space", " bot", false},
		{"trailing space", "bot ", false},
		{"inner space", "bot id", false},
		{"newline", "bot\nid", false},
		{"tab", "bot\tid", false},
		{"nul", "bot\x00id", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := plausibleRobotID(tc.candidate); got != tc.want {
				t.Errorf("plausibleRobotID(%q) = %v, want %v", tc.candidate, got, tc.want)
			}
		})
	}
}

// TestRobotIDMaxLenMatchesTheColumn keeps the constant honest.
//
// The bound's whole justification is "longer than the column cannot name a bot", so if the
// column ever widens and this does not, the gate starts rejecting real ids.
func TestRobotIDMaxLenMatchesTheColumn(t *testing.T) {
	const declared = "robot_id   VARCHAR(40)"
	const schemaFile = "sql/20210926000001_robot_legacy01.sql"
	schema, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatalf("read %s: %v", schemaFile, err)
	}
	if !strings.Contains(string(schema), declared) {
		t.Fatalf("robot.robot_id is no longer declared as %q in %s, so robotIDMaxLen = %d may "+
			"be wrong; plausibleRobotID rejects anything longer, which would silently stop a "+
			"real bot's events", declared, schemaFile, robotIDMaxLen)
	}
}
