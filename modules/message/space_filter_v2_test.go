package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
)

// PR #21 review (Jerry-Xin Critical) regression：v2 sidebar 过去未做 Space 过滤，
// 这组测试覆盖 decideConvKeepInSpace（v2 入口 FilterRawConversationsBySpace 内核）
// 在群/子区/DM/bot/系统 bot 各分支下的可见性。
//
// 注意：FilterRawConversationsBySpace 本身依赖 ctx/DB 查询群表与 Bot 成员，整体
// 端到端测试由集成层覆盖；这里只验证 *决策* 部分。

func TestDecideConvKeepInSpace_Group_DirectSpaceMatch(t *testing.T) {
	keep := decideConvKeepInSpace(
		"grp1", common.ChannelTypeGroup.Uint8(), "",
		"spaceA", "spaceA",
		map[string]string{"grp1": "spaceA"}, nil,
		nil, nil, false, false, false, nil,
	)
	assert.True(t, keep, "group in spaceA must be kept when filter=spaceA")
}

func TestDecideConvKeepInSpace_Group_SpaceMismatch_Excluded(t *testing.T) {
	keep := decideConvKeepInSpace(
		"grp1", common.ChannelTypeGroup.Uint8(), "",
		"spaceB", "spaceA",
		map[string]string{"grp1": "spaceA"}, nil,
		nil, nil, false, false, false, nil,
	)
	assert.False(t, keep, "group in spaceA must NOT leak into spaceB sidebar request")
}

func TestDecideConvKeepInSpace_Group_LegacyNoSpace_VisibleEverywhere(t *testing.T) {
	keep := decideConvKeepInSpace(
		"old-grp", common.ChannelTypeGroup.Uint8(), "",
		"spaceB", "spaceA",
		map[string]string{}, nil,
		nil, nil, false, false, false, nil,
	)
	assert.True(t, keep, "legacy group without space_id stays visible everywhere")
}

func TestDecideConvKeepInSpace_Group_ExternalSourceSpace(t *testing.T) {
	keep := decideConvKeepInSpace(
		"grp1", common.ChannelTypeGroup.Uint8(), "",
		"spaceX", "spaceA",
		map[string]string{"grp1": "spaceB"},
		map[string]string{"grp1": "spaceX"},
		nil, nil, false, false, false, nil,
	)
	assert.True(t, keep, "external group must surface in its source space")
}

func TestDecideConvKeepInSpace_Thread_FollowsParentSpace(t *testing.T) {
	// thread channel id: "<parent>____<short>"
	keep := decideConvKeepInSpace(
		"grp1____thr1", common.ChannelTypeCommunityTopic.Uint8(), "",
		"spaceA", "spaceA",
		map[string]string{"grp1": "spaceA"}, nil,
		nil, nil, false, false, false, nil,
	)
	assert.True(t, keep, "thread inherits parent group's space visibility")

	keep = decideConvKeepInSpace(
		"grp1____thr1", common.ChannelTypeCommunityTopic.Uint8(), "",
		"spaceB", "spaceA",
		map[string]string{"grp1": "spaceA"}, nil,
		nil, nil, false, false, false, nil,
	)
	assert.False(t, keep, "thread of spaceA parent must not appear in spaceB sidebar")
}

func TestDecideConvKeepInSpace_DM_DefaultSpace_KeepBareConv(t *testing.T) {
	keep := decideConvKeepInSpace(
		"peer1", common.ChannelTypePerson.Uint8(), "",
		"spaceA", "spaceA",
		nil, nil,
		map[string]bool{}, map[string]bool{}, false, false, false,
		func(string) bool { return false },
	)
	assert.True(t, keep, "bare DM stays in user's default space when no bot match")
}

func TestDecideConvKeepInSpace_DM_NonDefaultSpace_NoSpaceMsg_Excluded(t *testing.T) {
	keep := decideConvKeepInSpace(
		"peer1", common.ChannelTypePerson.Uint8(), "",
		"spaceB", "spaceA",
		nil, nil,
		map[string]bool{}, map[string]bool{}, false, false, false,
		func(string) bool { return false },
	)
	assert.False(t, keep, "DM with no payload.space_id match must NOT appear in non-default space")
}

func TestDecideConvKeepInSpace_DM_NonDefaultSpace_HasSpaceMsg_Visible(t *testing.T) {
	keep := decideConvKeepInSpace(
		"peer1", common.ChannelTypePerson.Uint8(), "",
		"spaceB", "spaceA",
		nil, nil,
		map[string]bool{}, map[string]bool{}, false, false, false,
		func(target string) bool { return target == "spaceB" },
	)
	assert.True(t, keep, "DM with payload.space_id == filter must appear in that space")
}

func TestDecideConvKeepInSpace_DM_BotInSpace(t *testing.T) {
	keep := decideConvKeepInSpace(
		"bot1", common.ChannelTypePerson.Uint8(), "",
		"spaceB", "spaceA",
		nil, nil,
		map[string]bool{"bot1": true}, map[string]bool{"bot1": true}, false, false, false,
		nil,
	)
	assert.True(t, keep, "bot DM is visible when bot is a member of the target space")
}

func TestDecideConvKeepInSpace_DM_BotNotInSpace_Excluded(t *testing.T) {
	keep := decideConvKeepInSpace(
		"bot1", common.ChannelTypePerson.Uint8(), "",
		"spaceB", "spaceA",
		nil, nil,
		map[string]bool{"bot1": true}, map[string]bool{"bot1": false}, false, false, false,
		nil,
	)
	assert.False(t, keep, "bot not in target space must be hidden in that space's sidebar")
}

// PR #21 Round-6 P0-1 regression：失败语义切换 —— v1 fail-open vs v2 fail-closed。
func TestDecideConvKeepInSpace_SkipGroupFilter_v1FailOpen(t *testing.T) {
	keep := decideConvKeepInSpace(
		"grp1", common.ChannelTypeGroup.Uint8(), "",
		"spaceB", "spaceA",
		nil, nil,
		nil, nil, true, false, false /*v1 兼容: fail-open*/, nil,
	)
	assert.True(t, keep, "v1: skipGroupFilter must keep groups (兼容历史)")
}

func TestDecideConvKeepInSpace_SkipGroupFilter_v2FailClosed_Group(t *testing.T) {
	keep := decideConvKeepInSpace(
		"grp1", common.ChannelTypeGroup.Uint8(), "",
		"spaceB", "spaceA",
		nil, nil,
		nil, nil, true, false, true /*v2: fail-closed*/, nil,
	)
	assert.False(t, keep,
		"v2: skipGroupFilter 时 group 必须 drop，否则一次 GetGroups 抖动会让 Space A 群泄露到 Space B 请求")
}

func TestDecideConvKeepInSpace_SkipGroupFilter_v2FailClosed_Thread(t *testing.T) {
	keep := decideConvKeepInSpace(
		"grp1____thr1", common.ChannelTypeCommunityTopic.Uint8(), "",
		"spaceB", "spaceA",
		nil, nil,
		nil, nil, true, false, true, nil,
	)
	assert.False(t, keep,
		"v2: skipGroupFilter 时 thread 必须 drop（与父群同样语义）")
}

func TestFilterThreadConvCore_v1FailOpen_vs_v2FailClosed(t *testing.T) {
	// v1: skipGroupFilter=true 时 keep
	v1Keep := filterThreadConvCore("grp1____thr1", "spaceB", "spaceA", nil, nil, true, false)
	assert.True(t, v1Keep, "v1 thread filter must remain fail-open")
	// v2: skipGroupFilter=true 时 drop
	v2Keep := filterThreadConvCore("grp1____thr1", "spaceB", "spaceA", nil, nil, true, true)
	assert.False(t, v2Keep, "v2 thread filter must drop when parent space unknown")
}

// rawConvHasSpaceMessages 解析 IM 原始 Payload []byte：覆盖正常匹配、空 Payload、
// 非法 JSON、非 string 字段几种边界，保证不会因为 payload 异常错放 Space。
func TestRawConvHasSpaceMessages(t *testing.T) {
	tests := []struct {
		name    string
		recents []*config.MessageResp
		want    bool
	}{
		{name: "nil conv", recents: nil, want: false},
		{name: "empty payload", recents: []*config.MessageResp{{Payload: nil}}, want: false},
		{name: "match", recents: []*config.MessageResp{{Payload: []byte(`{"space_id":"spaceB"}`)}}, want: true},
		{name: "mismatch", recents: []*config.MessageResp{{Payload: []byte(`{"space_id":"spaceA"}`)}}, want: false},
		{name: "invalid json", recents: []*config.MessageResp{{Payload: []byte(`{not json`)}}, want: false},
		{name: "non-string space_id", recents: []*config.MessageResp{{Payload: []byte(`{"space_id":123}`)}}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conv := &config.SyncUserConversationResp{Recents: tc.recents}
			got := rawConvHasSpaceMessages(conv, "spaceB")
			assert.Equal(t, tc.want, got)
		})
	}
}
