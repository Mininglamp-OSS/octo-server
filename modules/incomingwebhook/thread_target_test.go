package incomingwebhook

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
)

// TestTargetChannel 钉住投递目标解析——这是频道映射的【唯一来源】，handlePush / testPush
// 都经它取 (channelID, channelType)。测试床没有 SendMessageWithResult 录制器（见
// richtext_push_test.go 说明），无法端到端断言线上频道，故用这条纯单测覆盖正确性：
//   - 群 webhook → 父群频道；
//   - 子区 webhook → 子区频道 group_no____short_id（线协议格式在此处显式钉住）；
//   - 任何「半成品」组合（社区类型但空 short_id、或存量零值 channel_type）防御性落到父群，
//     绝不发到残缺子区频道。
func TestTargetChannel(t *testing.T) {
	const groupNo = "04f51b141553442ca63d7d10b1274be5"
	const shortID = "2039626171074744320"

	cases := []struct {
		name        string
		channelType int
		threadShort string
		wantChannel string
		wantType    uint8
	}{
		{
			name:        "group webhook to parent group",
			channelType: int(common.ChannelTypeGroup.Uint8()),
			threadShort: "",
			wantChannel: groupNo,
			wantType:    common.ChannelTypeGroup.Uint8(),
		},
		{
			name:        "thread webhook to thread channel",
			channelType: int(common.ChannelTypeCommunityTopic.Uint8()),
			threadShort: shortID,
			// 显式钉住线协议频道格式（group_no____short_id），不只是回指 BuildChannelID。
			wantChannel: groupNo + "____" + shortID,
			wantType:    common.ChannelTypeCommunityTopic.Uint8(),
		},
		{
			name:        "defensive: community type but empty short_id falls back to group",
			channelType: int(common.ChannelTypeCommunityTopic.Uint8()),
			threadShort: "",
			wantChannel: groupNo,
			wantType:    common.ChannelTypeGroup.Uint8(),
		},
		{
			name:        "defensive: legacy zero channel_type falls back to group",
			channelType: 0,
			threadShort: "",
			wantChannel: groupNo,
			wantType:    common.ChannelTypeGroup.Uint8(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &incomingWebhookModel{GroupNo: groupNo, ChannelType: tc.channelType, ThreadShortID: tc.threadShort}
			gotChannel, gotType := m.targetChannel()
			if gotChannel != tc.wantChannel {
				t.Fatalf("channelID = %q, want %q", gotChannel, tc.wantChannel)
			}
			if gotType != tc.wantType {
				t.Fatalf("channelType = %d, want %d", gotType, tc.wantType)
			}
		})
	}

	// 与 thread.BuildChannelID 的口径一致性（防止子区频道格式两处漂移）。
	m := &incomingWebhookModel{GroupNo: groupNo, ChannelType: int(common.ChannelTypeCommunityTopic.Uint8()), ThreadShortID: shortID}
	if got, _ := m.targetChannel(); got != thread.BuildChannelID(groupNo, shortID) {
		t.Fatalf("targetChannel channelID = %q, want thread.BuildChannelID = %q", got, thread.BuildChannelID(groupNo, shortID))
	}
}
