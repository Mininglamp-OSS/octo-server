package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilterAITeamSidebarItemsDropsParentAndSessionsFromNormalTabs(t *testing.T) {
	items := []*SidebarItem{
		{TargetID: "normal", TargetType: int(common.ChannelTypeGroup)},
		{TargetID: "pair", TargetType: int(common.ChannelTypeGroup)},
		{TargetID: "pair____session-1", TargetType: int(common.ChannelTypeCommunityTopic)},
		{TargetID: "team", TargetType: int(common.ChannelTypeGroup)},
		{TargetID: "team____session-2", TargetType: int(common.ChannelTypeCommunityTopic)},
		{TargetID: "custom", TargetType: int(common.ChannelTypeGroup)},
		{TargetID: "custom____thread-1", TargetType: int(common.ChannelTypeCommunityTopic)},
		{TargetID: "normal____thread-1", TargetType: int(common.ChannelTypeCommunityTopic)},
	}

	got := filterAITeamSidebarItems(items, map[string]struct{}{"pair": {}, "team": {}})
	assert.Equal(t, []string{"normal", "custom", "custom____thread-1", "normal____thread-1"}, sidebarTargetIDs(got))
}

func TestIsAITeamConversationMatchesParentAndThread(t *testing.T) {
	groups := map[string]*group.GroupResp{
		"normal": {GroupNo: "normal"},
		"pair":   {GroupNo: "pair", Purpose: aiteam.GroupPurpose},
		"team":   {GroupNo: "team", Purpose: aiteam.TeamGroupPurpose},
		"custom": {GroupNo: "custom", Purpose: aiteam.CustomTeamPurpose},
	}

	assert.False(t, isAITeamConversation("normal", common.ChannelTypeGroup.Uint8(), groups))
	assert.True(t, isAITeamConversation("pair", common.ChannelTypeGroup.Uint8(), groups))
	assert.True(t, isAITeamConversation("pair____session-1", common.ChannelTypeCommunityTopic.Uint8(), groups))
	assert.True(t, isAITeamConversation("team", common.ChannelTypeGroup.Uint8(), groups))
	assert.True(t, isAITeamConversation("team____session-2", common.ChannelTypeCommunityTopic.Uint8(), groups))
	assert.False(t, isAITeamConversation("custom", common.ChannelTypeGroup.Uint8(), groups))
	assert.False(t, isAITeamConversation("custom____thread-1", common.ChannelTypeCommunityTopic.Uint8(), groups))
	assert.False(t, isAITeamConversation("normal____thread-1", common.ChannelTypeCommunityTopic.Uint8(), groups))
}

func TestFilterAITeamLegacyConversationsDropsParentAndSessions(t *testing.T) {
	items := []conversationResp{
		{ChannelID: "normal", ChannelType: common.ChannelTypeGroup.Uint8()},
		{ChannelID: "pair", ChannelType: common.ChannelTypeGroup.Uint8()},
		{ChannelID: "pair____session-1", ChannelType: common.ChannelTypeCommunityTopic.Uint8()},
		{ChannelID: "team", ChannelType: common.ChannelTypeGroup.Uint8()},
		{ChannelID: "team____session-2", ChannelType: common.ChannelTypeCommunityTopic.Uint8()},
		{ChannelID: "custom", ChannelType: common.ChannelTypeGroup.Uint8()},
		{ChannelID: "custom____thread-1", ChannelType: common.ChannelTypeCommunityTopic.Uint8()},
		{ChannelID: "normal____thread-1", ChannelType: common.ChannelTypeCommunityTopic.Uint8()},
	}
	groups := map[string]*group.GroupResp{
		"normal": {GroupNo: "normal"},
		"pair":   {GroupNo: "pair", Purpose: aiteam.GroupPurpose},
		"team":   {GroupNo: "team", Purpose: aiteam.TeamGroupPurpose},
		"custom": {GroupNo: "custom", Purpose: aiteam.CustomTeamPurpose},
	}

	got := filterAITeamLegacyConversations(items, groups)
	require.Len(t, got, 4)
	assert.Equal(t, "normal", got[0].ChannelID)
	assert.Equal(t, "custom", got[1].ChannelID)
	assert.Equal(t, "custom____thread-1", got[2].ChannelID)
	assert.Equal(t, "normal____thread-1", got[3].ChannelID)
}

func sidebarTargetIDs(items []*SidebarItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.TargetID)
	}
	return ids
}
