package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilterAITeamSidebarItemsDropsParentAndSessionsFromNormalTabs(t *testing.T) {
	items := []*SidebarItem{
		{TargetID: "normal", TargetType: int(common.ChannelTypeGroup)},
		{TargetID: "protected", TargetType: int(common.ChannelTypeGroup)},
		{TargetID: "protected____session-1", TargetType: int(common.ChannelTypeCommunityTopic)},
		{TargetID: "normal____thread-1", TargetType: int(common.ChannelTypeCommunityTopic)},
	}

	got := filterAITeamSidebarItems(items, map[string]struct{}{"protected": {}})
	assert.Equal(t, []string{"normal", "normal____thread-1"}, sidebarTargetIDs(got))
}

func TestIsAITeamConversationMatchesParentAndThread(t *testing.T) {
	groups := map[string]*group.GroupResp{
		"normal":    {GroupNo: "normal"},
		"protected": {GroupNo: "protected", Purpose: "ai_session_container"},
	}

	assert.False(t, isAITeamConversation("normal", common.ChannelTypeGroup.Uint8(), groups))
	assert.True(t, isAITeamConversation("protected", common.ChannelTypeGroup.Uint8(), groups))
	assert.True(t, isAITeamConversation("protected____session-1", common.ChannelTypeCommunityTopic.Uint8(), groups))
	assert.False(t, isAITeamConversation("normal____thread-1", common.ChannelTypeCommunityTopic.Uint8(), groups))
}

func TestFilterAITeamLegacyConversationsDropsParentAndSessions(t *testing.T) {
	items := []conversationResp{
		{ChannelID: "normal", ChannelType: common.ChannelTypeGroup.Uint8()},
		{ChannelID: "protected", ChannelType: common.ChannelTypeGroup.Uint8()},
		{ChannelID: "protected____session-1", ChannelType: common.ChannelTypeCommunityTopic.Uint8()},
		{ChannelID: "normal____thread-1", ChannelType: common.ChannelTypeCommunityTopic.Uint8()},
	}
	groups := map[string]*group.GroupResp{
		"normal":    {GroupNo: "normal"},
		"protected": {GroupNo: "protected", Purpose: "ai_session_container"},
	}

	got := filterAITeamLegacyConversations(items, groups)
	require.Len(t, got, 2)
	assert.Equal(t, "normal", got[0].ChannelID)
	assert.Equal(t, "normal____thread-1", got[1].ChannelID)
}

func sidebarTargetIDs(items []*SidebarItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.TargetID)
	}
	return ids
}
