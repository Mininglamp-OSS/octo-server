//go:build integration

package message

// End-to-end regression guard for "DM conversation missing from the per-Space
// recent list", observed via POST /v1/conversation/sync?space_id=:space_id.
//
// Scenario (mirrors the real capture): one physical DM channel with messages in
// two non-default Spaces, whose globally-latest message belongs to Space B. The
// client keys its per-Space show/hide decision on the LAST message in recents
// (recents[0].payload.space_id). Before the fix the server passed WuKongIM's raw
// recents through untouched, so Space A's response carried Space B's message at
// recents[0] and the client hid the whole conversation — even though the server
// kept it (hasSpaceMsg/presence).
//
// After the fix (spaceizePersonRecents), each kept DM's recents is narrowed to the
// queried Space, so recents[0] always belongs to that Space.
//
// Reuses the shared httptest WuKongIM fake + helpers from
// dm_space_last_message_repro_test.go / conversation_recent_filter_e2e_test.go.

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFix_ConversationSync_SpaceizesDMRecents drives the real handler through the
// registered /v1/conversation route and asserts the per-Space recents narrowing.
func TestFix_ConversationSync_SpaceizesDMRecents(t *testing.T) {
	const (
		dmChannel   = "dm-peer-recents"
		spaceA      = "space-a-recents"      // non-default
		spaceB      = "space-b-recents"      // non-default, holds the globally-latest msg
		spaceDefaul = "space-default-recents" // earliest membership → default Space
	)

	now := time.Now()
	// History: seq 11 tagged Space A, seq 12 tagged Space B (globally latest, like
	// the captured "1111"/668cc9 message).
	msgA := dmMsgRepro(dmChannel, 11, now.Add(-2*time.Hour).Unix(),
		`{"type":1,"content":"hi-from-A","space_id":"`+spaceA+`"}`)
	msgB := dmMsgRepro(dmChannel, 12, now.Add(-1*time.Hour).Unix(),
		`{"type":1,"content":"1111","space_id":"`+spaceB+`"}`)

	convs := []*config.SyncUserConversationResp{
		dmIMConvMulti(dmChannel, now.Add(-1*time.Hour).Unix(), 100,
			[]*config.MessageResp{msgA, msgB}),
	}

	s, ctx := setupConvSyncE2E(t, convs)
	// Default Space MUST have the earliest created_at (GetUserDefaultSpaceIDE =
	// earliest membership); Space A / B are non-default.
	seedSpaceMemberRepro(t, ctx, spaceDefaul, testutil.UID, "2020-01-01 00:00:00")
	seedSpaceMemberRepro(t, ctx, spaceA, testutil.UID, "2020-06-01 00:00:00")
	seedSpaceMemberRepro(t, ctx, spaceB, testutil.UID, "2020-07-01 00:00:00")

	// ---- Space A: the DM is present and its recents carry ONLY the Space-A msg ----
	convA := findConv(callConvSyncSpace(t, s, spaceA), dmChannel)
	require.NotNil(t, convA, "DM active in Space A must appear in Space A's recent list")
	require.NotEmpty(t, convA.Recents, "Space A recents must carry the Space-A message")
	for _, m := range convA.Recents {
		assert.Equal(t, spaceA, spaceIDOf(m),
			"Space A recents must not contain any other Space's message")
	}
	assert.Equal(t, spaceA, spaceIDOf(convA.Recents[0]),
		"recents[0] — the client's per-Space hide signal — belongs to Space A")

	// ---- Space B: symmetric, only its own message ----
	convB := findConv(callConvSyncSpace(t, s, spaceB), dmChannel)
	require.NotNil(t, convB, "DM active in Space B must appear in Space B's recent list")
	require.NotEmpty(t, convB.Recents)
	for _, m := range convB.Recents {
		assert.Equal(t, spaceB, spaceIDOf(m),
			"Space B recents must not contain any other Space's message")
	}
}

// spaceIDOf reads payload.space_id from a decoded recents message ("" if absent).
func spaceIDOf(m *MsgSyncResp) string {
	if m == nil || m.Payload == nil {
		return ""
	}
	if sid, ok := m.Payload["space_id"].(string); ok {
		return sid
	}
	return ""
}
