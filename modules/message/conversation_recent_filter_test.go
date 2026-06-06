package message

// =============================================================================
// /v1/conversation/sync — recent-filter opt-in (issue #294)
//
// The web "Recent" tab loads its conversation list via POST /v1/conversation/sync
// (not /v1/sidebar/sync), so the admin-tunable sidebar.recent_filter_*_days
// windows never reached it. This adds an OPT-IN filter on the response list,
// reusing the exact same recentCutoffs/cutoffFor logic the sidebar recent tab
// uses, so existing clients (mobile offline sync) are byte-for-byte unaffected
// unless they set the flag.
//
// These are pure-logic tests for filterRecentConversations — no DB / IM needed.
// =============================================================================

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeSyncResp(channelID string, channelType uint8, ts int64) *SyncUserConversationResp {
	return &SyncUserConversationResp{
		ChannelID:   channelID,
		ChannelType: channelType,
		Timestamp:   ts,
	}
}

// defaultRecentCutoffs reproduces PR #291 defaults: group/thread = 3-day window,
// DM unfiltered (cutoff 0).
func defaultRecentCutoffs() recentCutoffs {
	now := time.Now()
	return recentCutoffs{
		group:  daysCutoff(now, 3),
		thread: daysCutoff(now, 3),
		person: daysCutoff(now, 0),
	}
}

func channelIDs(resps []*SyncUserConversationResp) []string {
	ids := make([]string, 0, len(resps))
	for _, r := range resps {
		ids = append(ids, r.ChannelID)
	}
	return ids
}

func TestFilterRecentConversations_StaleGroupDropped(t *testing.T) {
	resps := []*SyncUserConversationResp{
		makeSyncResp("g-fresh", common.ChannelTypeGroup.Uint8(), nowRecent()),
		makeSyncResp("g-stale", common.ChannelTypeGroup.Uint8(), now3DaysAgo()),
	}
	got := filterRecentConversations(resps, defaultRecentCutoffs())
	assert.Equal(t, []string{"g-fresh"}, channelIDs(got))
}

func TestFilterRecentConversations_StaleThreadDropped(t *testing.T) {
	resps := []*SyncUserConversationResp{
		makeSyncResp("t-fresh", common.ChannelTypeCommunityTopic.Uint8(), nowRecent()),
		makeSyncResp("t-stale", common.ChannelTypeCommunityTopic.Uint8(), now3DaysAgo()),
	}
	got := filterRecentConversations(resps, defaultRecentCutoffs())
	assert.Equal(t, []string{"t-fresh"}, channelIDs(got))
}

// person window defaults to 0 → DMs are never dropped, even when stale. This
// also protects system-bot (Person) entries from being filtered out.
func TestFilterRecentConversations_StaleDMKeptWhenPersonZero(t *testing.T) {
	resps := []*SyncUserConversationResp{
		makeSyncResp("dm-stale", common.ChannelTypePerson.Uint8(), now3DaysAgo()),
	}
	got := filterRecentConversations(resps, defaultRecentCutoffs())
	assert.Equal(t, []string{"dm-stale"}, channelIDs(got))
}

func TestFilterRecentConversations_GroupCutoffZero_AllKept(t *testing.T) {
	cutoffs := recentCutoffs{group: 0, thread: daysCutoff(time.Now(), 3), person: 0}
	resps := []*SyncUserConversationResp{
		makeSyncResp("g-stale", common.ChannelTypeGroup.Uint8(), now3DaysAgo()),
		makeSyncResp("g-fresh", common.ChannelTypeGroup.Uint8(), nowRecent()),
	}
	got := filterRecentConversations(resps, cutoffs)
	assert.Equal(t, []string{"g-stale", "g-fresh"}, channelIDs(got))
}

func TestFilterRecentConversations_PersonWindowFiltersDMs(t *testing.T) {
	cutoffs := recentCutoffs{
		group:  daysCutoff(time.Now(), 3),
		thread: daysCutoff(time.Now(), 3),
		person: daysCutoff(time.Now(), 3),
	}
	resps := []*SyncUserConversationResp{
		makeSyncResp("dm-fresh", common.ChannelTypePerson.Uint8(), nowRecent()),
		makeSyncResp("dm-stale", common.ChannelTypePerson.Uint8(), now3DaysAgo()),
	}
	got := filterRecentConversations(resps, cutoffs)
	assert.Equal(t, []string{"dm-fresh"}, channelIDs(got))
}

// Unknown channel types are never filtered (recent tab only carries group/
// thread/DM; defaulting unknown → kept avoids silently dropping a future type).
func TestFilterRecentConversations_UnknownTypeKept(t *testing.T) {
	cutoffs := recentCutoffs{
		group:  daysCutoff(time.Now(), 3),
		thread: daysCutoff(time.Now(), 3),
		person: daysCutoff(time.Now(), 3),
	}
	resps := []*SyncUserConversationResp{
		makeSyncResp("x", 99, now3DaysAgo()),
	}
	got := filterRecentConversations(resps, cutoffs)
	assert.Equal(t, []string{"x"}, channelIDs(got))
}

func TestFilterRecentConversations_Empty(t *testing.T) {
	got := filterRecentConversations(nil, defaultRecentCutoffs())
	assert.Empty(t, got)
}

// The helper must not mutate its input slice (immutability): a new slice is
// returned and the original ordering/content is preserved.
func TestFilterRecentConversations_DoesNotMutateInput(t *testing.T) {
	resps := []*SyncUserConversationResp{
		makeSyncResp("g-fresh", common.ChannelTypeGroup.Uint8(), nowRecent()),
		makeSyncResp("g-stale", common.ChannelTypeGroup.Uint8(), now3DaysAgo()),
	}
	_ = filterRecentConversations(resps, defaultRecentCutoffs())
	require.Len(t, resps, 2)
	assert.Equal(t, "g-fresh", resps[0].ChannelID)
	assert.Equal(t, "g-stale", resps[1].ChannelID)
}
