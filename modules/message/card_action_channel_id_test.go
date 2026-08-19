package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
)

// The projection that turns a click's channel identity into the one the card's
// sender uses. The DM row is the whole point: the two ends of a person channel
// name each other, so echoing what the operator sent hands the bot its own UID.
func TestCardActionConsumerChannelID(t *testing.T) {
	const (
		botUID  = "ultraman_tiga_bot"
		userUID = "825a777b"
		other   = "another_user"
	)
	cases := []struct {
		name             string
		row              *messageModel
		requestChannelID string
		operatorUID      string
		want             string
		wantOK           bool
	}{
		{
			name:             "dm click reports the sender's peer, not the sender",
			row:              &messageModel{FromUID: botUID, ChannelType: common.ChannelTypePerson.Uint8()},
			requestChannelID: botUID,
			operatorUID:      userUID,
			want:             userUID,
			wantOK:           true,
		},
		{
			name:             "dm click by the sender itself reports the other end",
			row:              &messageModel{FromUID: botUID, ChannelType: common.ChannelTypePerson.Uint8()},
			requestChannelID: other,
			operatorUID:      botUID,
			want:             other,
			wantOK:           true,
		},
		{
			name:             "self dm resolves to that single uid",
			row:              &messageModel{FromUID: userUID, ChannelType: common.ChannelTypePerson.Uint8()},
			requestChannelID: userUID,
			operatorUID:      userUID,
			want:             userUID,
			wantOK:           true,
		},
		{
			name:             "group channel id names the channel and passes through",
			row:              &messageModel{FromUID: botUID, ChannelType: common.ChannelTypeGroup.Uint8()},
			requestChannelID: "g_9f2c",
			operatorUID:      userUID,
			want:             "g_9f2c",
			wantOK:           true,
		},
		{
			name:             "community topic channel id passes through",
			row:              &messageModel{FromUID: botUID, ChannelType: common.ChannelTypeCommunityTopic.Uint8()},
			requestChannelID: "g_9f2c____123456789012345",
			operatorUID:      userUID,
			want:             "g_9f2c____123456789012345",
			wantOK:           true,
		},
		{
			name:             "dm row whose sender is in neither end has no peer to report",
			row:              &messageModel{FromUID: "stranger", ChannelType: common.ChannelTypePerson.Uint8()},
			requestChannelID: botUID,
			operatorUID:      userUID,
			want:             "",
			wantOK:           false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := cardActionConsumerChannelID(tc.row, tc.requestChannelID, tc.operatorUID)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Fatalf("channel id = %q, want %q", got, tc.want)
			}
		})
	}
}
