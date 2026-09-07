package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/stretchr/testify/assert"
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

func sidebarTargetIDs(items []*SidebarItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.TargetID)
	}
	return ids
}
