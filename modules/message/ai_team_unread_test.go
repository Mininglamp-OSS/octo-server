package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAITeamUnreadSidebandSkipsDBWithoutAICandidates(t *testing.T) {
	// A nil context panics if this ordinary-chat path attempts to acquire a DB.
	co := &Conversation{}
	conversations := []*config.SyncUserConversationResp{
		nil, {ChannelID: "person", ChannelType: 1}, {ChannelID: "normal____session", ChannelType: 5},
		{ChannelID: "team____session", ChannelType: 5}, {ChannelID: "pair", ChannelType: 2},
		{ChannelID: "invalid", ChannelType: 5},
	}
	groups := map[string]*group.GroupResp{
		"normal": {}, "team": {Purpose: "ai_team_group"}, "pair": {Purpose: aiteam.GroupPurpose},
	}
	for _, full := range []bool{true, false} {
		result, err := co.aiTeamConversations("space", "owner", full, conversations, groups)
		require.NoError(t, err)
		assert.Empty(t, result.Items)
		assert.Equal(t, full, result.Mode == "full")
	}
}
