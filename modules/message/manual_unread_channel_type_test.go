package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/stretchr/testify/assert"
)

func TestSupportsManualUnreadChannelType(t *testing.T) {
	tests := []struct {
		name        string
		channelType uint8
		want        bool
	}{
		{name: "person", channelType: common.ChannelTypePerson.Uint8(), want: false},
		{name: "group", channelType: common.ChannelTypeGroup.Uint8(), want: true},
		{name: "community topic", channelType: common.ChannelTypeCommunityTopic.Uint8(), want: true},
		{name: "unknown", channelType: 255, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, supportsManualUnreadChannelType(tc.channelType))
		})
	}
}

func TestNewConversationExtraRespSuppressesUnsupportedPersonManualUnread(t *testing.T) {
	tests := []struct {
		name        string
		channelType uint8
		want        bool
	}{
		{name: "person", channelType: common.ChannelTypePerson.Uint8(), want: false},
		{name: "group", channelType: common.ChannelTypeGroup.Uint8(), want: true},
		{name: "community topic", channelType: common.ChannelTypeCommunityTopic.Uint8(), want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := newConversationExtraResp(&conversationExtraModel{
				ChannelID:    "channel",
				ChannelType:  tc.channelType,
				ManualUnread: true,
			})
			assert.Equal(t, tc.want, resp.ManualUnread)
		})
	}
}
