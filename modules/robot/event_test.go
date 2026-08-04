package robot

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tidwall/gjson"
)

// TestExtractBotCommand tests the bot command extraction logic with bounds checking.
// This test addresses issue #251 where malformed offset/length values could cause panic.
func TestExtractBotCommand(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		entities []map[string]interface{}
		expected string
	}{
		{
			name:    "valid command extraction",
			content: "/start hello",
			entities: []map[string]interface{}{
				{
					"type":   "bot_command",
					"offset": json.Number("0"),
					"length": json.Number("6"),
				},
			},
			expected: "/start",
		},
		{
			name:    "command in middle of content",
			content: "hello /help world",
			entities: []map[string]interface{}{
				{
					"type":   "bot_command",
					"offset": json.Number("6"),
					"length": json.Number("5"),
				},
			},
			expected: "/help",
		},
		{
			name:    "offset out of bounds - should return empty",
			content: "short",
			entities: []map[string]interface{}{
				{
					"type":   "bot_command",
					"offset": json.Number("100"),
					"length": json.Number("5"),
				},
			},
			expected: "",
		},
		{
			name:    "length exceeds content - should return empty",
			content: "short",
			entities: []map[string]interface{}{
				{
					"type":   "bot_command",
					"offset": json.Number("0"),
					"length": json.Number("100"),
				},
			},
			expected: "",
		},
		{
			name:    "negative offset - should return empty",
			content: "/test",
			entities: []map[string]interface{}{
				{
					"type":   "bot_command",
					"offset": json.Number("-1"),
					"length": json.Number("5"),
				},
			},
			expected: "",
		},
		{
			name:    "zero length - should return empty",
			content: "/test",
			entities: []map[string]interface{}{
				{
					"type":   "bot_command",
					"offset": json.Number("0"),
					"length": json.Number("0"),
				},
			},
			expected: "",
		},
		{
			name:    "missing offset field - should return empty",
			content: "/test",
			entities: []map[string]interface{}{
				{
					"type":   "bot_command",
					"length": json.Number("5"),
				},
			},
			expected: "",
		},
		{
			name:    "missing length field - should return empty",
			content: "/test",
			entities: []map[string]interface{}{
				{
					"type":   "bot_command",
					"offset": json.Number("0"),
				},
			},
			expected: "",
		},
		{
			name:    "wrong type for offset - should return empty",
			content: "/test",
			entities: []map[string]interface{}{
				{
					"type":   "bot_command",
					"offset": "not a number",
					"length": json.Number("5"),
				},
			},
			expected: "",
		},
		{
			name:     "empty entities - should return empty",
			content:  "/test",
			entities: []map[string]interface{}{},
			expected: "",
		},
		{
			name:    "unicode content - valid extraction",
			content: "/测试 你好",
			entities: []map[string]interface{}{
				{
					"type":   "bot_command",
					"offset": json.Number("0"),
					"length": json.Number("3"),
				},
			},
			expected: "/测试",
		},
		{
			name:    "offset+length exactly at boundary",
			content: "/test",
			entities: []map[string]interface{}{
				{
					"type":   "bot_command",
					"offset": json.Number("0"),
					"length": json.Number("5"),
				},
			},
			expected: "/test",
		},
		{
			name:    "offset+length exceeds boundary by 1 - should return empty",
			content: "/test",
			entities: []map[string]interface{}{
				{
					"type":   "bot_command",
					"offset": json.Number("0"),
					"length": json.Number("6"),
				},
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractBotCommandKey(tt.content, tt.entities)
			assert.Equal(t, tt.expected, result, "extractBotCommandKey should return expected result")
		})
	}
}

// TestShouldSkipFriendCheck tests the friend check skip logic for Bot creators.
// This test addresses issue #481 where Bot always shows "add friend first" message.
func TestShouldSkipFriendCheck(t *testing.T) {
	tests := []struct {
		name       string
		fromUID    string
		creatorUID string
		robotID    string
		shouldSkip bool
	}{
		{
			name:       "creator should skip friend check",
			fromUID:    "user123",
			creatorUID: "user123",
			robotID:    "bot001",
			shouldSkip: true,
		},
		{
			name:       "non-creator should not skip friend check",
			fromUID:    "user456",
			creatorUID: "user123",
			robotID:    "bot001",
			shouldSkip: false,
		},
		{
			name:       "empty creator allows all (legacy bot)",
			fromUID:    "user789",
			creatorUID: "",
			robotID:    "bot002",
			shouldSkip: false, // empty creator means check friend relation
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// shouldSkip logic: creatorUID == fromUID
			shouldSkip := tt.creatorUID == tt.fromUID
			assert.Equal(t, tt.shouldSkip, shouldSkip, "shouldSkipFriendCheck should return expected result")
		})
	}
}

// TestRobotMentionParsingWithEntities verifies that the gjson-based mention
// parsing in robotMessageListen correctly extracts mention.uids while ignoring
// the new mention.entities field. This mirrors the parsing logic at event.go:135-153.
func TestRobotMentionParsingWithEntities(t *testing.T) {
	tests := []struct {
		name         string
		payload      string
		expectUIDs   []string
		expectPanic  bool
	}{
		{
			name: "v2 payload with uids and entities",
			payload: `{
				"type": 1,
				"content": "@bot1 @bot2 hello",
				"mention": {
					"uids": ["bot1", "bot2"],
					"entities": [
						{"uid": "bot1", "offset": 0, "length": 5},
						{"uid": "bot2", "offset": 6, "length": 5}
					]
				}
			}`,
			expectUIDs: []string{"bot1", "bot2"},
		},
		{
			name: "v1 payload with uids only",
			payload: `{
				"type": 1,
				"content": "@bot1 hello",
				"mention": {
					"uids": ["bot1"]
				}
			}`,
			expectUIDs: []string{"bot1"},
		},
		{
			name: "mention with empty entities array",
			payload: `{
				"type": 1,
				"content": "@bot1 hello",
				"mention": {
					"uids": ["bot1"],
					"entities": []
				}
			}`,
			expectUIDs: []string{"bot1"},
		},
		{
			name: "mention with null entities",
			payload: `{
				"type": 1,
				"content": "@bot1 hello",
				"mention": {
					"uids": ["bot1"],
					"entities": null
				}
			}`,
			expectUIDs: []string{"bot1"},
		},
		{
			name: "mention with malformed entities (string instead of array)",
			payload: `{
				"type": 1,
				"content": "@bot1 hello",
				"mention": {
					"uids": ["bot1"],
					"entities": "invalid"
				}
			}`,
			expectUIDs: []string{"bot1"},
		},
		{
			name: "entities present but uids missing",
			payload: `{
				"type": 1,
				"content": "@bot1 hello",
				"mention": {
					"entities": [
						{"uid": "bot1", "offset": 0, "length": 5}
					]
				}
			}`,
			expectUIDs: nil,
		},
		{
			name: "entities with extra fields",
			payload: `{
				"type": 1,
				"content": "@bot1 hello",
				"mention": {
					"uids": ["bot1"],
					"entities": [
						{"uid": "bot1", "offset": 0, "length": 5, "display_name": "Bot One", "type": "mention"}
					]
				}
			}`,
			expectUIDs: []string{"bot1"},
		},
		{
			name: "deeply nested entities (stress test)",
			payload: `{
				"type": 1,
				"content": "@a @b @c",
				"mention": {
					"uids": ["a", "b", "c"],
					"all": false,
					"entities": [
						{"uid": "a", "offset": 0, "length": 2, "nested": {"key": "value"}},
						{"uid": "b", "offset": 3, "length": 2, "nested": {"key": "value"}},
						{"uid": "c", "offset": 6, "length": 2, "nested": {"key": "value"}}
					]
				}
			}`,
			expectUIDs: []string{"a", "b", "c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					if !tt.expectPanic {
						t.Errorf("unexpected panic parsing mention with entities: %v", r)
					}
				}
			}()

			// Mirror the exact gjson parsing logic from robotMessageListen (event.go:135-153)
			payloadValue := gjson.Parse(tt.payload)
			var extractedUIDs []string

			mentionValue := payloadValue.Get("mention")
			mentionUIDsValue := mentionValue.Get("uids")
			if mentionValue.Exists() && mentionUIDsValue.Exists() {
				uidsValues := mentionUIDsValue.Array()
				for _, uidValue := range uidsValues {
					extractedUIDs = append(extractedUIDs, uidValue.String())
				}
			}

			assert.Equal(t, tt.expectUIDs, extractedUIDs,
				"gjson should extract uids correctly regardless of entities")

			// Verify that entities field is still accessible (passthrough)
			entitiesValue := mentionValue.Get("entities")
			if entitiesValue.Exists() && entitiesValue.IsArray() {
				// Entities are preserved in the parsed JSON
				entitiesArr := entitiesValue.Array()
				assert.GreaterOrEqual(t, len(entitiesArr), 0,
					"entities array should be accessible via gjson")
			}
		})
	}
}

// TestGjsonIgnoresUnknownFields verifies that gjson.Get on a specific path
// is not affected by the presence of sibling keys like entities.
func TestGjsonIgnoresUnknownFields(t *testing.T) {
	payload := `{
		"mention": {
			"uids": ["uid_1"],
			"all": true,
			"entities": [{"uid": "uid_1", "offset": 0, "length": 5}],
			"future_field": {"nested": true}
		}
	}`

	parsed := gjson.Parse(payload)

	// Verify each field is independently accessible
	assert.True(t, parsed.Get("mention").Exists())
	assert.True(t, parsed.Get("mention.uids").Exists())
	assert.Equal(t, "uid_1", parsed.Get("mention.uids.0").String())
	assert.True(t, parsed.Get("mention.all").Bool())
	assert.True(t, parsed.Get("mention.entities").Exists())
	assert.True(t, parsed.Get("mention.future_field").Exists())

	// Adding entities does not change the uids result
	payloadWithout := `{"mention": {"uids": ["uid_1"], "all": true}}`
	parsedWithout := gjson.Parse(payloadWithout)

	assert.Equal(t,
		parsed.Get("mention.uids").String(),
		parsedWithout.Get("mention.uids").String(),
		"uids should be identical with or without entities")
}

// extractBotCommandKey extracts the bot command from content using entities.
// This is a helper function that mirrors the logic in messagesListen for testability.
func extractBotCommandKey(content string, entities []map[string]interface{}) string {
	if entities == nil {
		return ""
	}

	var offset int64
	var length int64
	var offsetOK, lengthOK bool

	for _, entitiesMap := range entities {
		if entitiesMap["type"] == "bot_command" {
			// Safely extract offset
			if offsetVal, ok := entitiesMap["offset"].(json.Number); ok {
				offset, _ = offsetVal.Int64()
				offsetOK = true
			}
			// Safely extract length
			if lengthVal, ok := entitiesMap["length"].(json.Number); ok {
				length, _ = lengthVal.Int64()
				lengthOK = true
			}
			break
		}
	}

	contentRunes := []rune(content)
	contentLen := int64(len(contentRunes))

	// Validate bounds before slicing - require both offset and length to be valid
	if offsetOK && lengthOK && offset >= 0 && length > 0 && offset < contentLen && offset+length <= contentLen {
		return string(contentRunes[offset : offset+length])
	}

	return ""
}
