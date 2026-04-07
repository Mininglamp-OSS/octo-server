package message

import (
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
)

func makeMessageResp(seq uint32, spaceID string) *config.MessageResp {
	payload := map[string]interface{}{
		"type":    1,
		"content": "hello",
	}
	if spaceID != "" {
		payload["space_id"] = spaceID
	}
	data, _ := json.Marshal(payload)
	return &config.MessageResp{
		MessageSeq: seq,
		Payload:    data,
	}
}

func TestCountSpaceUnreadFromMessages_Basic(t *testing.T) {
	messages := []*config.MessageResp{
		makeMessageResp(1, "spaceA"),
		makeMessageResp(2, "spaceB"),
		makeMessageResp(3, "spaceA"),
		makeMessageResp(4, ""),       // 无 space_id
		makeMessageResp(5, "spaceA"),
	}

	// readSeq=0 → 所有消息都是未读
	count := countSpaceUnreadFromMessages(messages, "spaceA", 0)
	assert.Equal(t, 3, count)

	count = countSpaceUnreadFromMessages(messages, "spaceB", 0)
	assert.Equal(t, 1, count)
}

func TestCountSpaceUnreadFromMessages_ReadSeqFilters(t *testing.T) {
	messages := []*config.MessageResp{
		makeMessageResp(1, "spaceA"),
		makeMessageResp(2, "spaceA"),
		makeMessageResp(3, "spaceA"),
		makeMessageResp(4, "spaceA"),
		makeMessageResp(5, "spaceA"),
	}

	// readSeq=3 → 只有 seq 4,5 是未读
	count := countSpaceUnreadFromMessages(messages, "spaceA", 3)
	assert.Equal(t, 2, count)
}

func TestCountSpaceUnreadFromMessages_NoSpaceID(t *testing.T) {
	// 老消息没有 space_id 字段
	messages := []*config.MessageResp{
		makeMessageResp(1, ""),
		makeMessageResp(2, ""),
		makeMessageResp(3, ""),
	}

	count := countSpaceUnreadFromMessages(messages, "spaceA", 0)
	assert.Equal(t, 0, count)
}

func TestCountSpaceUnreadFromMessages_EmptyMessages(t *testing.T) {
	count := countSpaceUnreadFromMessages(nil, "spaceA", 0)
	assert.Equal(t, 0, count)

	count = countSpaceUnreadFromMessages([]*config.MessageResp{}, "spaceA", 0)
	assert.Equal(t, 0, count)
}

func TestCountSpaceUnreadFromMessages_InvalidPayload(t *testing.T) {
	messages := []*config.MessageResp{
		{MessageSeq: 1, Payload: []byte("invalid json")},
		{MessageSeq: 2, Payload: nil},
		makeMessageResp(3, "spaceA"), // 有效消息
	}

	count := countSpaceUnreadFromMessages(messages, "spaceA", 0)
	assert.Equal(t, 1, count)
}

func TestCountSpaceUnreadFromMessages_MixedSpaces(t *testing.T) {
	// 模拟真实场景：同一 Person 频道中混合不同 Space 的消息
	messages := []*config.MessageResp{
		makeMessageResp(10, "spaceA"),
		makeMessageResp(11, "spaceB"),
		makeMessageResp(12, "spaceA"),
		makeMessageResp(13, "spaceB"),
		makeMessageResp(14, ""),       // 无 space_id 的老消息
		makeMessageResp(15, "spaceA"),
	}

	// readSeq=12 → 未读: seq 13(spaceB), 14(none), 15(spaceA)
	count := countSpaceUnreadFromMessages(messages, "spaceA", 12)
	assert.Equal(t, 1, count)

	count = countSpaceUnreadFromMessages(messages, "spaceB", 12)
	assert.Equal(t, 1, count)
}

func TestFillPersonSpaceUnread_OnlyPersonChannels(t *testing.T) {
	// Group 频道不应计算 space_unread
	convs := []*SyncUserConversationResp{
		{ChannelID: "g1", ChannelType: common.ChannelTypeGroup.Uint8(), Unread: 5},
		{ChannelID: "user1", ChannelType: common.ChannelTypePerson.Uint8(), Unread: 3},
	}
	rawConvs := []*config.SyncUserConversationResp{
		{
			ChannelID: "user1", ChannelType: common.ChannelTypePerson.Uint8(),
			Unread: 3, LastMsgSeq: 10,
			Recents: []*config.MessageResp{
				makeMessageResp(8, "spaceA"),
				makeMessageResp(9, "spaceB"),
				makeMessageResp(10, "spaceA"),
			},
		},
	}

	fillPersonSpaceUnread(convs, rawConvs, "spaceA", "me", nil)

	// Group 不应有 space_unread
	assert.Nil(t, convs[0].SpaceUnread)
	// Person 应计算 space_unread: readSeq=10-3=7, seq 8(A) 9(B) 10(A) → 2
	assert.NotNil(t, convs[1].SpaceUnread)
	assert.Equal(t, 2, *convs[1].SpaceUnread)
}

func TestFillPersonSpaceUnread_ZeroUnread(t *testing.T) {
	convs := []*SyncUserConversationResp{
		{ChannelID: "user1", ChannelType: common.ChannelTypePerson.Uint8(), Unread: 0},
	}
	rawConvs := []*config.SyncUserConversationResp{
		{ChannelID: "user1", ChannelType: common.ChannelTypePerson.Uint8(), Unread: 0, LastMsgSeq: 10},
	}

	fillPersonSpaceUnread(convs, rawConvs, "spaceA", "me", nil)
	assert.Nil(t, convs[0].SpaceUnread)
}

func TestFillPersonSpaceUnread_EmptySpaceID(t *testing.T) {
	convs := []*SyncUserConversationResp{
		{ChannelID: "user1", ChannelType: common.ChannelTypePerson.Uint8(), Unread: 3},
	}

	fillPersonSpaceUnread(convs, nil, "", "me", nil)
	assert.Nil(t, convs[0].SpaceUnread)
}

func TestFillPersonSpaceUnread_RecentsCoversAllUnread(t *testing.T) {
	// Recents 包含 5 条消息，unread=3 → 不需要额外 API 调用
	convs := []*SyncUserConversationResp{
		{ChannelID: "user1", ChannelType: common.ChannelTypePerson.Uint8(), Unread: 3},
	}
	rawConvs := []*config.SyncUserConversationResp{
		{
			ChannelID: "user1", ChannelType: common.ChannelTypePerson.Uint8(),
			Unread: 3, LastMsgSeq: 20,
			Recents: []*config.MessageResp{
				makeMessageResp(16, "spaceA"),
				makeMessageResp(17, ""),
				makeMessageResp(18, "spaceA"),
				makeMessageResp(19, "spaceB"),
				makeMessageResp(20, "spaceA"),
			},
		},
	}

	// readSeq = 20 - 3 = 17, 未读: seq 18(A), 19(B), 20(A) → spaceA=2
	fillPersonSpaceUnread(convs, rawConvs, "spaceA", "me", nil)
	assert.NotNil(t, convs[0].SpaceUnread)
	assert.Equal(t, 2, *convs[0].SpaceUnread)
}

func TestFillPersonSpaceUnread_AllUnreadInDifferentSpace(t *testing.T) {
	convs := []*SyncUserConversationResp{
		{ChannelID: "user1", ChannelType: common.ChannelTypePerson.Uint8(), Unread: 2},
	}
	rawConvs := []*config.SyncUserConversationResp{
		{
			ChannelID: "user1", ChannelType: common.ChannelTypePerson.Uint8(),
			Unread: 2, LastMsgSeq: 5,
			Recents: []*config.MessageResp{
				makeMessageResp(4, "spaceB"),
				makeMessageResp(5, "spaceB"),
			},
		},
	}

	// readSeq = 5 - 2 = 3, 未读 seq 4(B) 5(B) → spaceA=0
	fillPersonSpaceUnread(convs, rawConvs, "spaceA", "me", nil)
	assert.NotNil(t, convs[0].SpaceUnread)
	assert.Equal(t, 0, *convs[0].SpaceUnread)
}

func TestFillPersonSpaceUnread_NoRawConversation(t *testing.T) {
	// raw 中没有对应的会话（不应 panic）
	convs := []*SyncUserConversationResp{
		{ChannelID: "user1", ChannelType: common.ChannelTypePerson.Uint8(), Unread: 3},
	}
	rawConvs := []*config.SyncUserConversationResp{}

	fillPersonSpaceUnread(convs, rawConvs, "spaceA", "me", nil)
	assert.Nil(t, convs[0].SpaceUnread)
}

func TestFindSpaceLastMessage_Basic(t *testing.T) {
	recents := []*MsgSyncResp{
		{MessageSeq: 1, Payload: map[string]interface{}{"content": "msg1", "space_id": "spaceA"}},
		{MessageSeq: 2, Payload: map[string]interface{}{"content": "msg2", "space_id": "spaceB"}},
		{MessageSeq: 3, Payload: map[string]interface{}{"content": "msg3", "space_id": "spaceA"}},
	}

	// spaceA 的最后一条是 seq=3
	result := findSpaceLastMessage(recents, "spaceA")
	assert.NotNil(t, result)
	assert.Equal(t, uint32(3), result.MessageSeq)

	// spaceB 的最后一条是 seq=2
	result = findSpaceLastMessage(recents, "spaceB")
	assert.NotNil(t, result)
	assert.Equal(t, uint32(2), result.MessageSeq)

	// spaceC 没有消息
	result = findSpaceLastMessage(recents, "spaceC")
	assert.Nil(t, result)
}

func TestFindSpaceLastMessage_EmptyRecents(t *testing.T) {
	assert.Nil(t, findSpaceLastMessage(nil, "spaceA"))
	assert.Nil(t, findSpaceLastMessage([]*MsgSyncResp{}, "spaceA"))
}

func TestFindSpaceLastMessage_NilPayload(t *testing.T) {
	recents := []*MsgSyncResp{
		{MessageSeq: 1, Payload: nil},
		{MessageSeq: 2, Payload: map[string]interface{}{"content": "no space"}},
	}
	assert.Nil(t, findSpaceLastMessage(recents, "spaceA"))
}

func TestFillPersonSpaceUnread_SetsSpaceLastMessage(t *testing.T) {
	convs := []*SyncUserConversationResp{
		{
			ChannelID:   "user1",
			ChannelType: common.ChannelTypePerson.Uint8(),
			Unread:      0,
			Recents: []*MsgSyncResp{
				{MessageSeq: 1, Payload: map[string]interface{}{"content": "111", "space_id": "spaceA"}},
				{MessageSeq: 2, Payload: map[string]interface{}{"content": "222", "space_id": "spaceB"}},
			},
		},
	}

	fillPersonSpaceUnread(convs, nil, "spaceA", "login", nil)

	// SpaceLastMessage 应为 spaceA 的消息 "111"
	assert.NotNil(t, convs[0].SpaceLastMessage)
	assert.Equal(t, uint32(1), convs[0].SpaceLastMessage.MessageSeq)
	assert.Equal(t, "111", convs[0].SpaceLastMessage.Payload["content"])

	// SpaceUnread 未设置（unread=0）
	assert.Nil(t, convs[0].SpaceUnread)
}
