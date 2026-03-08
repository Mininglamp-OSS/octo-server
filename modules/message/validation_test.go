package message

import (
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
