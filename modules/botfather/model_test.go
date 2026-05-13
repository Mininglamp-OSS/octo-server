package botfather

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBotRegisterResp_JSON(t *testing.T) {
	resp := BotRegisterResp{
		RobotID:        "test_bot",
		IMToken:        "im_token_123",
		WSURL:          "ws://localhost:5200",
		APIURL:         "http://localhost:8090",
		OwnerUID:       "owner_001",
		OwnerChannelID: "owner_001",
	}

	data, err := json.Marshal(resp)
	assert.NoError(t, err)

	var decoded BotRegisterResp
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, resp, decoded)
}

func TestBotSendMessageReq_JSON(t *testing.T) {
	req := BotSendMessageReq{
		ChannelID:   "ch_001",
		ChannelType: 1,
		StreamNo:    "stream_001",
		Payload: map[string]interface{}{
			"type":    float64(1),
			"content": "Hello!",
		},
	}

	data, err := json.Marshal(req)
	assert.NoError(t, err)

	var decoded BotSendMessageReq
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, req.ChannelID, decoded.ChannelID)
	assert.Equal(t, req.ChannelType, decoded.ChannelType)
	assert.Equal(t, req.StreamNo, decoded.StreamNo)
	assert.Equal(t, "Hello!", decoded.Payload["content"])
}

func TestBotTypingReq_JSON(t *testing.T) {
	req := BotTypingReq{
		ChannelID:   "ch_001",
		ChannelType: 2,
	}

	data, err := json.Marshal(req)
	assert.NoError(t, err)
	assert.Contains(t, string(data), `"channel_id":"ch_001"`)
	assert.Contains(t, string(data), `"channel_type":2`)

	var decoded BotTypingReq
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, req, decoded)
}

func TestBotEventsReq_JSON(t *testing.T) {
	req := BotEventsReq{
		EventID: 100,
		Limit:   20,
	}

	data, err := json.Marshal(req)
	assert.NoError(t, err)

	var decoded BotEventsReq
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, req, decoded)
}

func TestBotEventAckReq_JSON(t *testing.T) {
	req := BotEventAckReq{EventID: 42}

	data, err := json.Marshal(req)
	assert.NoError(t, err)

	var decoded BotEventAckReq
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, int64(42), decoded.EventID)
}

func TestBotReadReceiptReq_JSON(t *testing.T) {
	req := BotReadReceiptReq{
		ChannelID:   "ch_001",
		ChannelType: 1,
		MessageIDs:  []string{"msg_001", "msg_002"},
	}

	data, err := json.Marshal(req)
	assert.NoError(t, err)

	var decoded BotReadReceiptReq
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, req.ChannelID, decoded.ChannelID)
	assert.Len(t, decoded.MessageIDs, 2)
}

func TestBotHeartbeatReq_JSON(t *testing.T) {
	req := BotHeartbeatReq{}

	data, err := json.Marshal(req)
	assert.NoError(t, err)
	assert.Equal(t, "{}", string(data))
}

func TestBotInfo_JSON(t *testing.T) {
	info := BotInfo{
		RobotID:     "test_bot",
		Name:        "Test Bot",
		Description: "A test bot",
		BotToken:    "bf_abc123",
		CreatorUID:  "uid_001",
		Status:      1,
	}

	data, err := json.Marshal(info)
	assert.NoError(t, err)

	var decoded BotInfo
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, info, decoded)
}

func TestBotSendMessageReq_EmptyPayload(t *testing.T) {
	req := BotSendMessageReq{
		ChannelID:   "ch_001",
		ChannelType: 1,
	}

	data, err := json.Marshal(req)
	assert.NoError(t, err)

	var decoded BotSendMessageReq
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, "ch_001", decoded.ChannelID)
	assert.Nil(t, decoded.Payload)
}

func TestBotEventsReq_Defaults(t *testing.T) {
	// Pure JSON marshal/unmarshal of a struct — no external dependencies.
	req := BotEventsReq{}

	data, err := json.Marshal(req)
	assert.NoError(t, err)

	var decoded BotEventsReq
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, int64(0), decoded.EventID)
	assert.Equal(t, int64(0), decoded.Limit)
}

func TestBotReadReceiptReq_EmptyMessageIDs(t *testing.T) {
	req := BotReadReceiptReq{
		ChannelID:   "ch_001",
		ChannelType: 1,
		MessageIDs:  []string{},
	}

	data, err := json.Marshal(req)
	assert.NoError(t, err)

	var decoded BotReadReceiptReq
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Empty(t, decoded.MessageIDs)
}

func TestBotRegisterResp_AllFields(t *testing.T) {
	resp := BotRegisterResp{
		RobotID:        "my_bot",
		IMToken:        "im_token_xyz",
		WSURL:          "ws://example.com:5200",
		APIURL:         "http://example.com:5001",
		OwnerUID:       "owner_uid",
		OwnerChannelID: "owner_channel",
	}

	data, err := json.Marshal(resp)
	assert.NoError(t, err)
	assert.Contains(t, string(data), `"robot_id":"my_bot"`)
	assert.Contains(t, string(data), `"im_token":"im_token_xyz"`)
	assert.Contains(t, string(data), `"ws_url":"ws://example.com:5200"`)
}

func TestBotInfo_EmptyFields(t *testing.T) {
	info := BotInfo{}

	data, err := json.Marshal(info)
	assert.NoError(t, err)

	var decoded BotInfo
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, "", decoded.RobotID)
	assert.Equal(t, "", decoded.Name)
	assert.Equal(t, 0, decoded.Status)
}
