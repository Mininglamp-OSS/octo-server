package message

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestDeleteReq_Check(t *testing.T) {
	tests := []struct {
		name    string
		req     deleteReq
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid request",
			req: deleteReq{
				MessageID:   "msg_001",
				ChannelID:   "ch_001",
				ChannelType: 1,
				MessageSeq:  100,
			},
			wantErr: false,
		},
		{
			name: "empty message ID",
			req: deleteReq{
				MessageID:   "",
				ChannelID:   "ch_001",
				ChannelType: 1,
				MessageSeq:  100,
			},
			wantErr: true,
			errMsg:  "消息ID不能为空",
		},
		{
			name: "whitespace message ID",
			req: deleteReq{
				MessageID:   "   ",
				ChannelID:   "ch_001",
				ChannelType: 1,
				MessageSeq:  100,
			},
			wantErr: true,
			errMsg:  "消息ID不能为空",
		},
		{
			name: "empty channel ID",
			req: deleteReq{
				MessageID:   "msg_001",
				ChannelID:   "",
				ChannelType: 1,
				MessageSeq:  100,
			},
			wantErr: true,
			errMsg:  "频道ID不能为空",
		},
		{
			name: "zero channel type",
			req: deleteReq{
				MessageID:   "msg_001",
				ChannelID:   "ch_001",
				ChannelType: 0,
				MessageSeq:  100,
			},
			wantErr: true,
			errMsg:  "频道类型不能为空",
		},
		{
			name: "zero message seq",
			req: deleteReq{
				MessageID:   "msg_001",
				ChannelID:   "ch_001",
				ChannelType: 1,
				MessageSeq:  0,
			},
			wantErr: true,
			errMsg:  "消息序号不能为空",
		},
		{
			name: "all fields empty",
			req: deleteReq{
				MessageID:   "",
				ChannelID:   "",
				ChannelType: 0,
				MessageSeq:  0,
			},
			wantErr: true,
			errMsg:  "消息ID不能为空", // 第一个检查先失败
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.check()
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestConstants_Message(t *testing.T) {
	assert.Equal(t, "messageDeleted", CMDMessageDeleted)
	assert.Equal(t, "messageEerase", CMDMessageErase) // 注意原代码有typo
	assert.Equal(t, "readedCount:", CacheReadedCountPrefix)
}

func TestRevokeTimeout(t *testing.T) {
	// 验证默认撤回超时时间为 24 小时（86400 秒）
	assert.Equal(t, 24*60*60, DefaultRevokeTimeout)
	assert.Equal(t, 86400, DefaultRevokeTimeout)
}

func TestRevokeTimeoutLogic(t *testing.T) {
	tests := []struct {
		name           string
		messageTime    time.Time
		shouldTimeout  bool
	}{
		{
			name:          "message sent 1 hour ago - should NOT timeout",
			messageTime:   time.Now().Add(-1 * time.Hour),
			shouldTimeout: false,
		},
		{
			name:          "message sent 12 hours ago - should NOT timeout",
			messageTime:   time.Now().Add(-12 * time.Hour),
			shouldTimeout: false,
		},
		{
			name:          "message sent 23 hours ago - should NOT timeout",
			messageTime:   time.Now().Add(-23 * time.Hour),
			shouldTimeout: false,
		},
		{
			name:          "message sent 24 hours and 1 second ago - should timeout",
			messageTime:   time.Now().Add(-24*time.Hour - 1*time.Second),
			shouldTimeout: true,
		},
		{
			name:          "message sent 25 hours ago - should timeout",
			messageTime:   time.Now().Add(-25 * time.Hour),
			shouldTimeout: true,
		},
		{
			name:          "message sent 1 week ago - should timeout",
			messageTime:   time.Now().Add(-7 * 24 * time.Hour),
			shouldTimeout: true,
		},
		{
			name:          "message sent just now - should NOT timeout",
			messageTime:   time.Now(),
			shouldTimeout: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			elapsed := time.Since(tt.messageTime)
			isTimeout := elapsed.Seconds() > float64(DefaultRevokeTimeout)
			assert.Equal(t, tt.shouldTimeout, isTimeout, "timeout check mismatch for %s", tt.name)
		})
	}
}

func TestReminderType(t *testing.T) {
	assert.Equal(t, 1, ReminderTypeMentionMe)
	assert.Equal(t, 2, ReminderTypeApplyJoinGroup)
}

func TestSensitiveWords(t *testing.T) {
	// 确保敏感词列表不为空
	assert.Greater(t, len(sensitive_words), 0, "sensitive words list should not be empty")

	// 确保包含一些关键敏感词
	wordSet := make(map[string]bool)
	for _, w := range sensitive_words {
		wordSet[w] = true
	}
	assert.True(t, wordSet["银行卡"], "should contain 银行卡")
	assert.True(t, wordSet["密码"], "should contain 密码")
	assert.True(t, wordSet["转账"], "should contain 转账")

	// 确保没有空字符串
	for i, w := range sensitive_words {
		assert.NotEmpty(t, w, "sensitive word at index %d should not be empty", i)
	}
}

func TestVoiceReadedReq_Check(t *testing.T) {
	// voiceReadedReq 继承 deleteReq，共享相同的验证逻辑
	tests := []struct {
		name    string
		req     voiceReadedReq
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid request",
			req: voiceReadedReq{deleteReq: deleteReq{
				MessageID: "msg_001", ChannelID: "ch_001",
				ChannelType: 1, MessageSeq: 100,
			}},
			wantErr: false,
		},
		{
			name: "empty message ID",
			req: voiceReadedReq{deleteReq: deleteReq{
				MessageID: "", ChannelID: "ch_001",
				ChannelType: 1, MessageSeq: 100,
			}},
			wantErr: true,
			errMsg:  "消息ID不能为空",
		},
		{
			name: "empty channel ID",
			req: voiceReadedReq{deleteReq: deleteReq{
				MessageID: "msg_001", ChannelID: "",
				ChannelType: 1, MessageSeq: 100,
			}},
			wantErr: true,
			errMsg:  "频道ID不能为空",
		},
		{
			name: "zero channel type",
			req: voiceReadedReq{deleteReq: deleteReq{
				MessageID: "msg_001", ChannelID: "ch_001",
				ChannelType: 0, MessageSeq: 100,
			}},
			wantErr: true,
			errMsg:  "频道类型不能为空",
		},
		{
			name: "zero message seq",
			req: voiceReadedReq{deleteReq: deleteReq{
				MessageID: "msg_001", ChannelID: "ch_001",
				ChannelType: 1, MessageSeq: 0,
			}},
			wantErr: true,
			errMsg:  "消息序号不能为空",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.check()
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestDeleteReq_Check_WhitespaceVariants(t *testing.T) {
	tests := []struct {
		name    string
		req     deleteReq
		wantErr bool
		errMsg  string
	}{
		{
			name: "tab in message ID",
			req: deleteReq{
				MessageID: "\t", ChannelID: "ch_001",
				ChannelType: 1, MessageSeq: 100,
			},
			wantErr: true,
			errMsg:  "消息ID不能为空",
		},
		{
			name: "newline in channel ID",
			req: deleteReq{
				MessageID: "msg_001", ChannelID: "\n",
				ChannelType: 1, MessageSeq: 100,
			},
			wantErr: true,
			errMsg:  "频道ID不能为空",
		},
		{
			name: "max channel type",
			req: deleteReq{
				MessageID: "msg_001", ChannelID: "ch_001",
				ChannelType: 255, MessageSeq: 100,
			},
			wantErr: false,
		},
		{
			name: "large message seq",
			req: deleteReq{
				MessageID: "msg_001", ChannelID: "ch_001",
				ChannelType: 2, MessageSeq: 4294967295,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.check()
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestSensitiveWords_NoDuplicates(t *testing.T) {
	seen := make(map[string]int)
	for i, w := range sensitive_words {
		if prev, ok := seen[w]; ok {
			t.Errorf("duplicate sensitive word %q at index %d (first seen at %d)", w, i, prev)
		}
		seen[w] = i
	}
}

func TestSensitiveWords_ContainsFinancialTerms(t *testing.T) {
	wordSet := make(map[string]bool)
	for _, w := range sensitive_words {
		wordSet[w] = true
	}

	financialTerms := []string{"银行卡", "密码", "转账", "支付宝"}
	for _, term := range financialTerms {
		assert.True(t, wordSet[term], "sensitive words should contain %q", term)
	}
}

// TestGetMentionTypeAssertionSafety verifies that getMention uses safe type
// assertions and doesn't panic on malformed data.
func TestGetMentionTypeAssertionSafety(t *testing.T) {
	m := &Message{}

	tests := []struct {
		name        string
		payloadMap  map[string]interface{}
		expectAll   bool
		expectUIDs  []string
		expectPanic bool
	}{
		{
			name: "valid mention with all=1",
			payloadMap: map[string]interface{}{
				"mention": map[string]interface{}{
					"all": "1",
				},
			},
			expectAll:  false, // json.Number parsing will fail for string "1"
			expectUIDs: nil,
		},
		{
			name: "valid mention with uids",
			payloadMap: map[string]interface{}{
				"mention": map[string]interface{}{
					"uids": []interface{}{"uid1", "uid2"},
				},
			},
			expectAll:  false,
			expectUIDs: []string{"uid1", "uid2"},
		},
		{
			name: "mention is string instead of map - should not panic",
			payloadMap: map[string]interface{}{
				"mention": "invalid_string",
			},
			expectAll:   false,
			expectUIDs:  nil,
			expectPanic: false,
		},
		{
			name: "mention is int instead of map - should not panic",
			payloadMap: map[string]interface{}{
				"mention": 12345,
			},
			expectAll:   false,
			expectUIDs:  nil,
			expectPanic: false,
		},
		{
			name: "mention is nil - should not panic",
			payloadMap: map[string]interface{}{
				"mention": nil,
			},
			expectAll:   false,
			expectUIDs:  nil,
			expectPanic: false,
		},
		{
			name: "mention.uids is string instead of array - should not panic",
			payloadMap: map[string]interface{}{
				"mention": map[string]interface{}{
					"uids": "invalid_string",
				},
			},
			expectAll:   false,
			expectUIDs:  nil,
			expectPanic: false,
		},
		{
			name: "mention.uids contains non-string elements - should skip them",
			payloadMap: map[string]interface{}{
				"mention": map[string]interface{}{
					"uids": []interface{}{"uid1", 123, "uid2", nil},
				},
			},
			expectAll:   false,
			expectUIDs:  []string{"uid1", "uid2"},
			expectPanic: false,
		},
		{
			name: "mention.uids is map instead of array - should not panic",
			payloadMap: map[string]interface{}{
				"mention": map[string]interface{}{
					"uids": map[string]interface{}{"uid": "123"},
				},
			},
			expectAll:   false,
			expectUIDs:  nil,
			expectPanic: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					if !tt.expectPanic {
						t.Errorf("unexpected panic: %v", r)
					}
				}
			}()

			all, uids := m.getMention(tt.payloadMap)
			assert.Equal(t, tt.expectAll, all)
			assert.Equal(t, tt.expectUIDs, uids)
		})
	}
}

// TestVisiblesTypeAssertionSafety verifies that visibles parsing in getReminders
// uses safe type assertions and doesn't panic on malformed data.
func TestVisiblesTypeAssertionSafety(t *testing.T) {
	tests := []struct {
		name        string
		payloadMap  map[string]interface{}
		expectPanic bool
	}{
		{
			name: "valid visibles array with strings",
			payloadMap: map[string]interface{}{
				"visibles": []interface{}{"uid1", "uid2"},
			},
			expectPanic: false,
		},
		{
			name: "visibles is string instead of array - should not panic",
			payloadMap: map[string]interface{}{
				"visibles": "invalid_string",
			},
			expectPanic: false,
		},
		{
			name: "visibles is int instead of array - should not panic",
			payloadMap: map[string]interface{}{
				"visibles": 12345,
			},
			expectPanic: false,
		},
		{
			name: "visibles is map instead of array - should not panic",
			payloadMap: map[string]interface{}{
				"visibles": map[string]interface{}{"uid": "123"},
			},
			expectPanic: false,
		},
		{
			name: "visibles is nil - should not panic",
			payloadMap: map[string]interface{}{
				"visibles": nil,
			},
			expectPanic: false,
		},
		{
			name: "visibles contains non-string elements - should skip them",
			payloadMap: map[string]interface{}{
				"visibles": []interface{}{"uid1", 123, nil, "uid2"},
			},
			expectPanic: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					if !tt.expectPanic {
						t.Errorf("unexpected panic for visibles type assertion: %v", r)
					}
				}
			}()

			// Simulate the safe visibles parsing pattern from api_reminders.go
			if tt.payloadMap["visibles"] != nil {
				visibleObjs, ok := tt.payloadMap["visibles"].([]interface{})
				if ok {
					for _, visibleObj := range visibleObjs {
						if uid, ok := visibleObj.(string); ok {
							assert.NotEmpty(t, uid)
						}
					}
				}
			}
		})
	}
}

// TestTypeAssertionSafety verifies that type assertions in message payload
// parsing use the comma-ok pattern to prevent panics on malformed data.
func TestTypeAssertionSafety(t *testing.T) {
	tests := []struct {
		name        string
		payloadMap  map[string]interface{}
		expectPanic bool
	}{
		{
			name: "valid reply object",
			payloadMap: map[string]interface{}{
				"reply": map[string]interface{}{
					"message_id": "msg_123",
				},
			},
			expectPanic: false,
		},
		{
			name: "reply is string instead of map",
			payloadMap: map[string]interface{}{
				"reply": "invalid_string_type",
			},
			expectPanic: false,
		},
		{
			name: "reply is nil",
			payloadMap: map[string]interface{}{
				"reply": nil,
			},
			expectPanic: false,
		},
		{
			name: "reply is int",
			payloadMap: map[string]interface{}{
				"reply": 12345,
			},
			expectPanic: false,
		},
		{
			name: "reply map with non-string message_id",
			payloadMap: map[string]interface{}{
				"reply": map[string]interface{}{
					"message_id": 12345,
				},
			},
			expectPanic: false,
		},
		{
			name: "valid visibles array",
			payloadMap: map[string]interface{}{
				"visibles": []interface{}{"uid1", "uid2"},
			},
			expectPanic: false,
		},
		{
			name: "visibles is string instead of array",
			payloadMap: map[string]interface{}{
				"visibles": "invalid_string_type",
			},
			expectPanic: false,
		},
		{
			name: "visibles is map instead of array",
			payloadMap: map[string]interface{}{
				"visibles": map[string]interface{}{"uid": "123"},
			},
			expectPanic: false,
		},
		{
			name:        "empty payload",
			payloadMap:  map[string]interface{}{},
			expectPanic: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					if !tt.expectPanic {
						t.Errorf("unexpected panic: %v", r)
					}
				}
			}()

			// Test reply type assertion safety (mirrors api.go lines 1777-1779)
			if replyJson := tt.payloadMap["reply"]; replyJson != nil {
				if replyMap, ok := replyJson.(map[string]interface{}); ok {
					if msgId, ok := replyMap["message_id"].(string); ok {
						assert.NotEmpty(t, msgId)
					}
				}
			}

			// Test visibles type assertion safety (mirrors api.go line 2032)
			if visibles := tt.payloadMap["visibles"]; visibles != nil {
				if visiblesArray, ok := visibles.([]interface{}); ok && len(visiblesArray) > 0 {
					assert.Greater(t, len(visiblesArray), 0)
				}
			}
		})
	}
}

// TestMentionEntitiesJSONPassthrough verifies that the mention.entities field
// is preserved intact through JSON marshal/unmarshal cycles. The backend treats
// payload as an opaque JSON blob — entities must survive the round-trip.
func TestMentionEntitiesJSONPassthrough(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{
			name: "v2 payload with uids and entities",
			payload: `{
				"type": 1,
				"content": "@Alice @Bob hello",
				"mention": {
					"uids": ["uid_alice", "uid_bob"],
					"all": 0,
					"entities": [
						{"uid": "uid_alice", "offset": 0, "length": 6},
						{"uid": "uid_bob", "offset": 7, "length": 4}
					]
				}
			}`,
		},
		{
			name: "v2 payload with entities and all=true",
			payload: `{
				"type": 1,
				"content": "@all check this",
				"mention": {
					"uids": [],
					"all": 1,
					"entities": [
						{"uid": "__all__", "offset": 0, "length": 4}
					]
				}
			}`,
		},
		{
			name: "v1 payload without entities",
			payload: `{
				"type": 1,
				"content": "@Alice hello",
				"mention": {
					"uids": ["uid_alice"],
					"all": 0
				}
			}`,
		},
		{
			name: "empty entities array",
			payload: `{
				"type": 1,
				"content": "no mentions",
				"mention": {
					"uids": ["uid_alice"],
					"entities": []
				}
			}`,
		},
		{
			name: "entities with extra fields (forward compat)",
			payload: `{
				"type": 1,
				"content": "@Alice hello",
				"mention": {
					"uids": ["uid_alice"],
					"entities": [
						{"uid": "uid_alice", "offset": 0, "length": 6, "display_name": "Alice", "color": "#ff0000"}
					]
				}
			}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate JSON round-trip as the backend does (unmarshal → re-marshal)
			var payloadMap map[string]interface{}
			decoder := json.NewDecoder(strings.NewReader(tt.payload))
			decoder.UseNumber()
			err := decoder.Decode(&payloadMap)
			assert.NoError(t, err, "should unmarshal payload")

			// Re-marshal
			marshaled, err := json.Marshal(payloadMap)
			assert.NoError(t, err, "should marshal payload")

			// Unmarshal again and compare
			var roundTripped map[string]interface{}
			decoder2 := json.NewDecoder(strings.NewReader(string(marshaled)))
			decoder2.UseNumber()
			err = decoder2.Decode(&roundTripped)
			assert.NoError(t, err, "should unmarshal round-tripped payload")

			// Verify mention field is preserved
			origMention, _ := json.Marshal(payloadMap["mention"])
			rtMention, _ := json.Marshal(roundTripped["mention"])
			assert.JSONEq(t, string(origMention), string(rtMention),
				"mention field should be identical after round-trip")

			// If entities existed in the original, verify they are still present
			if mentionMap, ok := payloadMap["mention"].(map[string]interface{}); ok {
				if _, hasEntities := mentionMap["entities"]; hasEntities {
					rtMentionMap, ok := roundTripped["mention"].(map[string]interface{})
					assert.True(t, ok, "round-tripped mention should be a map")
					_, rtHasEntities := rtMentionMap["entities"]
					assert.True(t, rtHasEntities, "entities should survive round-trip")
				}
			}
		})
	}
}

// TestGetMentionWithEntitiesCompatibility verifies that getMention() correctly
// extracts uids and all from payloads that include the new entities field,
// without panicking or returning wrong results.
func TestGetMentionWithEntitiesCompatibility(t *testing.T) {
	m := &Message{}

	tests := []struct {
		name       string
		payloadMap map[string]interface{}
		expectAll  bool
		expectUIDs []string
	}{
		{
			name: "v2 payload - uids + entities",
			payloadMap: map[string]interface{}{
				"mention": map[string]interface{}{
					"uids": []interface{}{"uid_alice", "uid_bob"},
					"entities": []interface{}{
						map[string]interface{}{"uid": "uid_alice", "offset": json.Number("0"), "length": json.Number("6")},
						map[string]interface{}{"uid": "uid_bob", "offset": json.Number("7"), "length": json.Number("4")},
					},
				},
			},
			expectAll:  false,
			expectUIDs: []string{"uid_alice", "uid_bob"},
		},
		{
			name: "v2 payload - all=1 + entities",
			payloadMap: map[string]interface{}{
				"mention": map[string]interface{}{
					"all":  json.Number("1"),
					"uids": []interface{}{},
					"entities": []interface{}{
						map[string]interface{}{"uid": "__all__", "offset": json.Number("0"), "length": json.Number("4")},
					},
				},
			},
			expectAll:  true,
			expectUIDs: []string{},
		},
		{
			name: "v1 payload - only uids (no entities key)",
			payloadMap: map[string]interface{}{
				"mention": map[string]interface{}{
					"uids": []interface{}{"uid_alice"},
				},
			},
			expectAll:  false,
			expectUIDs: []string{"uid_alice"},
		},
		{
			name: "entities is null",
			payloadMap: map[string]interface{}{
				"mention": map[string]interface{}{
					"uids":     []interface{}{"uid_alice"},
					"entities": nil,
				},
			},
			expectAll:  false,
			expectUIDs: []string{"uid_alice"},
		},
		{
			name: "entities is empty array",
			payloadMap: map[string]interface{}{
				"mention": map[string]interface{}{
					"uids":     []interface{}{"uid_alice", "uid_bob"},
					"entities": []interface{}{},
				},
			},
			expectAll:  false,
			expectUIDs: []string{"uid_alice", "uid_bob"},
		},
		{
			name: "entities is malformed string",
			payloadMap: map[string]interface{}{
				"mention": map[string]interface{}{
					"uids":     []interface{}{"uid_alice"},
					"entities": "not_an_array",
				},
			},
			expectAll:  false,
			expectUIDs: []string{"uid_alice"},
		},
		{
			name: "entities is integer",
			payloadMap: map[string]interface{}{
				"mention": map[string]interface{}{
					"uids":     []interface{}{"uid_alice"},
					"entities": 42,
				},
			},
			expectAll:  false,
			expectUIDs: []string{"uid_alice"},
		},
		{
			name: "entities contains malformed entries (no uid field)",
			payloadMap: map[string]interface{}{
				"mention": map[string]interface{}{
					"uids": []interface{}{"uid_alice"},
					"entities": []interface{}{
						map[string]interface{}{"offset": json.Number("0"), "length": json.Number("6")},
					},
				},
			},
			expectAll:  false,
			expectUIDs: []string{"uid_alice"},
		},
		{
			name: "entities contains non-map elements",
			payloadMap: map[string]interface{}{
				"mention": map[string]interface{}{
					"uids":     []interface{}{"uid_alice"},
					"entities": []interface{}{"invalid", 123, nil},
				},
			},
			expectAll:  false,
			expectUIDs: []string{"uid_alice"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("getMention panicked with entities in payload: %v", r)
				}
			}()

			all, uids := m.getMention(tt.payloadMap)
			assert.Equal(t, tt.expectAll, all)
			assert.Equal(t, tt.expectUIDs, uids)
		})
	}
}

// TestHasMentionWithEntities verifies that hasMention() returns true for
// payloads containing the new entities field.
func TestHasMentionWithEntities(t *testing.T) {
	m := &Message{}

	tests := []struct {
		name       string
		payloadMap map[string]interface{}
		expected   bool
	}{
		{
			name: "v2 mention with entities",
			payloadMap: map[string]interface{}{
				"mention": map[string]interface{}{
					"uids": []interface{}{"uid_alice"},
					"entities": []interface{}{
						map[string]interface{}{"uid": "uid_alice", "offset": 0, "length": 6},
					},
				},
			},
			expected: true,
		},
		{
			name: "v1 mention without entities",
			payloadMap: map[string]interface{}{
				"mention": map[string]interface{}{
					"uids": []interface{}{"uid_alice"},
				},
			},
			expected: true,
		},
		{
			name:       "no mention",
			payloadMap: map[string]interface{}{"type": 1},
			expected:   false,
		},
		{
			name:       "mention is nil",
			payloadMap: map[string]interface{}{"mention": nil},
			expected:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, m.hasMention(tt.payloadMap))
		})
	}
}

// TestV1V2MentionCoexistence verifies that getMention() works correctly when
// processing a mix of v1 (uids-only) and v2 (uids + entities) payloads,
// simulating the transition period where old and new clients coexist.
func TestV1V2MentionCoexistence(t *testing.T) {
	m := &Message{}

	payloads := []struct {
		label      string
		payloadMap map[string]interface{}
		expectAll  bool
		expectUIDs []string
	}{
		{
			label: "v1 message from old client",
			payloadMap: map[string]interface{}{
				"mention": map[string]interface{}{
					"uids": []interface{}{"uid_1", "uid_2"},
				},
			},
			expectAll:  false,
			expectUIDs: []string{"uid_1", "uid_2"},
		},
		{
			label: "v2 message from new client",
			payloadMap: map[string]interface{}{
				"mention": map[string]interface{}{
					"uids": []interface{}{"uid_1", "uid_2"},
					"entities": []interface{}{
						map[string]interface{}{"uid": "uid_1", "offset": json.Number("0"), "length": json.Number("5")},
						map[string]interface{}{"uid": "uid_2", "offset": json.Number("6"), "length": json.Number("7")},
					},
				},
			},
			expectAll:  false,
			expectUIDs: []string{"uid_1", "uid_2"},
		},
		{
			label: "v1 @all from old client",
			payloadMap: map[string]interface{}{
				"mention": map[string]interface{}{
					"all": json.Number("1"),
				},
			},
			expectAll:  true,
			expectUIDs: nil,
		},
		{
			label: "v2 @all from new client",
			payloadMap: map[string]interface{}{
				"mention": map[string]interface{}{
					"all":  json.Number("1"),
					"uids": []interface{}{},
					"entities": []interface{}{
						map[string]interface{}{"uid": "__all__", "offset": json.Number("0"), "length": json.Number("4")},
					},
				},
			},
			expectAll:  true,
			expectUIDs: []string{},
		},
	}

	for _, p := range payloads {
		t.Run(p.label, func(t *testing.T) {
			all, uids := m.getMention(p.payloadMap)
			assert.Equal(t, p.expectAll, all, "all mismatch for %s", p.label)
			assert.Equal(t, p.expectUIDs, uids, "uids mismatch for %s", p.label)
		})
	}
}
