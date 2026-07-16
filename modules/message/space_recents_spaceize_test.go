package message

import (
	"strconv"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recMsg builds a minimal recents message carrying an optional space_id tag.
// spaceID=="" → an untagged message (no payload.space_id key at all).
func recMsg(seq uint32, spaceID string) *MsgSyncResp {
	payload := map[string]interface{}{
		"type":    float64(1),
		"content": "m" + strconv.FormatUint(uint64(seq), 10),
	}
	if spaceID != "" {
		payload["space_id"] = spaceID
	}
	return &MsgSyncResp{
		MessageSeq:  seq,
		ClientMsgNo: "cmn-" + strconv.FormatUint(uint64(seq), 10),
		Payload:     payload,
	}
}

func personConv(channelID string, recents ...*MsgSyncResp) *SyncUserConversationResp {
	return &SyncUserConversationResp{
		ChannelID:   channelID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Recents:     recents,
	}
}

func recentSeqs(conv *SyncUserConversationResp) []uint32 {
	out := make([]uint32, 0, len(conv.Recents))
	for _, m := range conv.Recents {
		out = append(out, m.MessageSeq)
	}
	return out
}

// TestSpaceizePersonRecents_KeepsOnlyCurrentSpace is the core of the fix: a DM
// active in two Spaces whose globally-latest message belongs to another Space
// must, when queried for Space A, expose only Space-A messages in recents — so
// recents[0] (the client's per-Space hide signal) belongs to Space A.
func TestSpaceizePersonRecents_KeepsOnlyCurrentSpace(t *testing.T) {
	conv := personConv("peer", recMsg(11, "spaceA"), recMsg(12, "spaceB"))
	spaceizePersonRecents([]*SyncUserConversationResp{conv}, "spaceA", "spaceDefault")

	require.Equal(t, []uint32{11}, recentSeqs(conv))
	assert.Equal(t, "spaceA", conv.Recents[0].Payload["space_id"],
		"recents[0] must belong to the queried Space")
}

// TestSpaceizePersonRecents_WindowMissEmptiesRecents: when the current-Space
// message is out of the WuKongIM window, recents collapses to empty (the
// cross-Space message is dropped). We intentionally do NOT backfill recents from
// SpaceLastMessage — the preview is carried by space_last_message, and empty
// recents does not trigger client hiding; backfilling would surface an
// offset-bypassing, under-enriched fallback message as recents[0]. SpaceLastMessage
// (the preview) is left untouched.
func TestSpaceizePersonRecents_WindowMissEmptiesRecents(t *testing.T) {
	conv := personConv("peer", recMsg(12, "spaceB")) // window holds only the cross-Space msg
	seed := recMsg(3, "spaceA")                      // fillPersonSpaceUnread's fallback result
	conv.SpaceLastMessage = seed

	spaceizePersonRecents([]*SyncUserConversationResp{conv}, "spaceA", "spaceDefault")

	assert.Empty(t, conv.Recents,
		"cross-Space recents dropped and NOT backfilled from SpaceLastMessage")
	assert.Same(t, seed, conv.SpaceLastMessage,
		"SpaceLastMessage (the preview) is left intact")
}

// TestSpaceizePersonRecents_DegenerateEmptiesRatherThanLeakCrossSpace reproduces
// the captured bug exactly: the DM is kept for Space A (via presence/hasSpaceMsg,
// decided upstream) but Space A's message is unreachable, so SpaceLastMessage is
// nil. The cross-Space message MUST be stripped from recents — leaving it would
// make the client hide the whole conversation. Empty recents renders as a visible,
// preview-less row (verified client-side), which is the intended outcome.
func TestSpaceizePersonRecents_DegenerateEmptiesRatherThanLeakCrossSpace(t *testing.T) {
	conv := personConv("peer", recMsg(12, "spaceB")) // only cross-Space; no SpaceLastMessage
	spaceizePersonRecents([]*SyncUserConversationResp{conv}, "spaceA", "spaceDefault")

	assert.Empty(t, conv.Recents,
		"cross-Space recents[0] must be removed so the client stops hiding the kept conversation")
}

// TestSpaceizePersonRecents_DefaultSpaceMatchesUntagged: in the default Space an
// untagged DM message belongs to that Space (the bare-DM convention, aligned with
// findSpaceLastMessage / filterPersonMessagesBySpace rule 2).
func TestSpaceizePersonRecents_DefaultSpaceMatchesUntagged(t *testing.T) {
	conv := personConv("peer", recMsg(11, ""), recMsg(12, "spaceB")) // untagged + spaceB
	spaceizePersonRecents([]*SyncUserConversationResp{conv}, "spaceDefault", "spaceDefault")

	require.Equal(t, []uint32{11}, recentSeqs(conv))
	_, tagged := conv.Recents[0].Payload["space_id"]
	assert.False(t, tagged, "the surviving message is the untagged default-Space one")
}

// TestSpaceizePersonRecents_NonDefaultDropsUntagged: an untagged message must NOT
// leak into a NON-default Space (rule 3 — no fail-open across Spaces).
func TestSpaceizePersonRecents_NonDefaultDropsUntagged(t *testing.T) {
	conv := personConv("peer", recMsg(11, "")) // untagged only
	spaceizePersonRecents([]*SyncUserConversationResp{conv}, "spaceA", "spaceDefault")

	assert.Empty(t, conv.Recents,
		"untagged messages belong to the default Space, never a non-default one")
}

// TestSpaceizePersonRecents_SystemBotSpaceFiltered: system-bot recents ARE
// space-filtered, on the same footing as their message history
// (filterPersonMessagesBySpace rule 4) and preview/unread (fillPersonSpaceUnread):
// untagged history belongs to no Space and is dropped; only exact-tagged
// current-Space messages survive. The conversation itself stays visible via
// EnsureSystemBotsPresent + the client's system-bot exemption (not exercised here).
func TestSpaceizePersonRecents_SystemBotSpaceFiltered(t *testing.T) {
	// botfather is a system bot (spacepkg.IsSystemBot("botfather") == true).
	// Untagged + cross-Space messages are both dropped for a system bot.
	conv := personConv("botfather", recMsg(11, ""), recMsg(12, "spaceB"))
	spaceizePersonRecents([]*SyncUserConversationResp{conv}, "spaceA", "spaceDefault")
	assert.Empty(t, conv.Recents,
		"system-bot untagged/cross-Space recents are dropped (rule-4 parity)")

	// A message tagged with exactly the queried Space survives.
	conv2 := personConv("botfather", recMsg(11, ""), recMsg(12, "spaceA"))
	spaceizePersonRecents([]*SyncUserConversationResp{conv2}, "spaceA", "spaceDefault")
	require.Equal(t, []uint32{12}, recentSeqs(conv2),
		"a message tagged with the queried Space survives for a system bot")

	// Even in the DEFAULT Space, untagged system-bot history does NOT count — this
	// pins that the isSysBot flag is threaded through (a plain DM would keep it).
	conv3 := personConv("botfather", recMsg(11, ""))
	spaceizePersonRecents([]*SyncUserConversationResp{conv3}, "spaceDefault", "spaceDefault")
	assert.Empty(t, conv3.Recents,
		"untagged system-bot history is not a default-Space message (rule-4 parity)")
}

// TestSpaceizePersonRecents_NonPersonUntouched: GROUP/COMMUNITY_TOPIC isolation is
// enforced at the channel_id layer; their recents must not be touched here.
func TestSpaceizePersonRecents_NonPersonUntouched(t *testing.T) {
	conv := &SyncUserConversationResp{
		ChannelID:   "g1",
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Recents:     []*MsgSyncResp{recMsg(11, "spaceB")},
	}
	spaceizePersonRecents([]*SyncUserConversationResp{conv}, "spaceA", "spaceDefault")

	require.Equal(t, []uint32{11}, recentSeqs(conv),
		"group recents are isolated at the channel_id layer, not here")
}

// TestSpaceizePersonRecents_EmptySpaceIDNoop: no Space filter active → untouched.
func TestSpaceizePersonRecents_EmptySpaceIDNoop(t *testing.T) {
	conv := personConv("peer", recMsg(11, "spaceA"), recMsg(12, "spaceB"))
	spaceizePersonRecents([]*SyncUserConversationResp{conv}, "", "")

	require.Equal(t, []uint32{11, 12}, recentSeqs(conv), "no Space filter → recents unchanged")
}

// TestSpaceizePersonRecents_PreservesOrderAcrossMultipleMatches: relative order is
// preserved and interleaved cross-Space messages are dropped, so whichever end the
// client treats as "newest" stays a current-Space message.
func TestSpaceizePersonRecents_PreservesOrderAcrossMultipleMatches(t *testing.T) {
	conv := personConv("peer",
		recMsg(10, "spaceA"),
		recMsg(11, "spaceB"),
		recMsg(12, "spaceA"),
	)
	spaceizePersonRecents([]*SyncUserConversationResp{conv}, "spaceA", "spaceDefault")

	require.Equal(t, []uint32{10, 12}, recentSeqs(conv))
}

// TestSpaceizePersonRecents_NilSafe: nil conversations and nil recents entries are
// skipped without panicking.
func TestSpaceizePersonRecents_NilSafe(t *testing.T) {
	conv := personConv("peer", nil, recMsg(12, "spaceA"), nil)
	spaceizePersonRecents([]*SyncUserConversationResp{nil, conv}, "spaceA", "spaceDefault")

	require.Equal(t, []uint32{12}, recentSeqs(conv))
}
