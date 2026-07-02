package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/assert"
)

func catchallConv(peer string, recentSpaceIDs ...string) *SyncUserConversationResp {
	recents := make([]*MsgSyncResp, 0, len(recentSpaceIDs))
	for i, sid := range recentSpaceIDs {
		payload := map[string]interface{}{"type": 1, "content": "m"}
		if sid != "" {
			payload["space_id"] = sid
		}
		recents = append(recents, &MsgSyncResp{MessageSeq: uint32(i + 1), Payload: payload})
	}
	return &SyncUserConversationResp{
		ChannelID:   peer,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Recents:     recents,
	}
}

func catchallHide(convs []*SyncUserConversationResp, elsewhereOnly, botSet map[string]bool, skipBotFilter bool) []string {
	out := hideElsewhereOnlyDMsInDefaultSpace(
		convs,
		func(c *SyncUserConversationResp) string { return c.ChannelID },
		func(c *SyncUserConversationResp) uint8 { return c.ChannelType },
		func(c *SyncUserConversationResp) bool { return personConvAllRecentsTaggedElsewhere(c, "spaceDefault") },
		elsewhereOnly, botSet, skipBotFilter,
	)
	ids := make([]string, 0, len(out))
	for _, c := range out {
		ids = append(ids, c.ChannelID)
	}
	return ids
}

func TestHideElsewhereOnlyDMs_HidesOnlyWithPositiveEvidence(t *testing.T) {
	convs := []*SyncUserConversationResp{
		catchallConv("peer_elsewhere", "spaceB"),     // presence 只在他空间 + Recents 全他空间 → 隐藏
		catchallConv("peer_untracked", "spaceB"),     // 无 presence 行 → 保留（catch-all 现状）
		catchallConv("peer_untagged", ""),            // Recents 有未打标消息 → 反证保留
		catchallConv("peer_default", "spaceDefault"), // Recents 有默认 Space 消息 → 反证保留
		catchallConv("peer_mixed", "spaceB", ""),     // 混合（含未打标）→ 反证保留
		catchallConv("botfather", "spaceB"),          // 系统 bot → 永不隐藏
		catchallConv("robot_x", "spaceB"),            // 普通 bot → 本 pass 不裁决
	}
	elsewhereOnly := map[string]bool{
		"peer_elsewhere": true,
		"peer_untagged":  true,
		"peer_default":   true,
		"peer_mixed":     true,
		"botfather":      true,
		"robot_x":        true,
	}
	botSet := map[string]bool{"robot_x": true}

	kept := catchallHide(convs, elsewhereOnly, botSet, false)
	assert.NotContains(t, kept, "peer_elsewhere", "presence 只在他空间且无反证 → 从默认 Space 隐藏")
	assert.Contains(t, kept, "peer_untracked", "无 presence 证据 → 保持 catch-all 现状")
	assert.Contains(t, kept, "peer_untagged", "未打标 Recents 反证 → 保留")
	assert.Contains(t, kept, "peer_default", "默认 Space Recents 反证 → 保留")
	assert.Contains(t, kept, "peer_mixed", "混合 Recents 含未打标 → 保留")
	assert.Contains(t, kept, "botfather", "系统 bot 永不隐藏")
	assert.Contains(t, kept, "robot_x", "普通 bot 的可见性由 catch-all bot 子判定决定，本 pass 不隐藏")
}

func TestHideElsewhereOnlyDMs_FailOpenGates(t *testing.T) {
	convs := []*SyncUserConversationResp{catchallConv("peer_elsewhere", "spaceB")}
	elsewhereOnly := map[string]bool{"peer_elsewhere": true}

	// skipBotFilter=true（bot 解析失败）→ 整体跳过。
	kept := catchallHide(convs, elsewhereOnly, nil, true)
	assert.Contains(t, kept, "peer_elsewhere", "bot 身份不可知时不隐藏")

	// elsewhereOnly 为 nil/空（presence 查询失败或无数据）→ 整体跳过。
	kept = catchallHide(convs, nil, nil, false)
	assert.Contains(t, kept, "peer_elsewhere", "无 presence 证据时不隐藏")
}

func TestHideElsewhereOnlyDMs_GroupUntouched(t *testing.T) {
	group := &SyncUserConversationResp{ChannelID: "g1", ChannelType: common.ChannelTypeGroup.Uint8()}
	kept := catchallHide([]*SyncUserConversationResp{group}, map[string]bool{"g1": true}, nil, false)
	assert.Contains(t, kept, "g1", "非 Person 会话不受本 pass 影响")
}

func TestPersonConvAllRecentsTaggedElsewhere(t *testing.T) {
	assert.True(t, personConvAllRecentsTaggedElsewhere(catchallConv("p"), "spaceDefault"),
		"空 Recents 无本地反证 → true（由 presence 决定）")
	assert.True(t, personConvAllRecentsTaggedElsewhere(catchallConv("p", "spaceB", "spaceC"), "spaceDefault"))
	assert.False(t, personConvAllRecentsTaggedElsewhere(catchallConv("p", "spaceB", ""), "spaceDefault"),
		"任一未打标消息 → false")
	assert.False(t, personConvAllRecentsTaggedElsewhere(catchallConv("p", "spaceDefault"), "spaceDefault"),
		"默认 Space 消息 → false")
	assert.True(t, personConvAllRecentsTaggedElsewhere(nil, "spaceDefault"))
}

func TestRawConvAllRecentsTaggedElsewhere(t *testing.T) {
	rawConv := func(payloads ...[]byte) *config.SyncUserConversationResp {
		recents := make([]*config.MessageResp, 0, len(payloads))
		for i, p := range payloads {
			recents = append(recents, &config.MessageResp{MessageSeq: uint32(i + 1), Payload: p})
		}
		return &config.SyncUserConversationResp{ChannelID: "p", Recents: recents}
	}
	tagged := func(sid string) []byte {
		return []byte(util.ToJson(map[string]interface{}{"type": 1, "space_id": sid}))
	}
	untagged := []byte(util.ToJson(map[string]interface{}{"type": 1}))

	assert.True(t, rawConvAllRecentsTaggedElsewhere(rawConv(tagged("spaceB")), "spaceDefault"))
	assert.False(t, rawConvAllRecentsTaggedElsewhere(rawConv(tagged("spaceDefault")), "spaceDefault"))
	assert.False(t, rawConvAllRecentsTaggedElsewhere(rawConv(untagged), "spaceDefault"))
	assert.False(t, rawConvAllRecentsTaggedElsewhere(rawConv([]byte("not-json")), "spaceDefault"),
		"payload 解析失败视同未打标 → 反证保留")
	assert.True(t, rawConvAllRecentsTaggedElsewhere(rawConv(), "spaceDefault"), "空 Recents → true")
}
