package thread

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ==================== 验证函数测试 (RED -> GREEN) ====================

func TestIsValidShortID(t *testing.T) {
	tests := []struct {
		name     string
		shortID  string
		expected bool
	}{
		// 有效的 shortID (snowflake ID: 15-20位纯数字)
		{"valid_19_digits", "1489104291682713601", true},
		{"valid_15_digits", "148910429168271", true},
		{"valid_20_digits", "14891042916827136019", true},
		{"valid_all_zeros", "000000000000000", true},

		// 无效的 shortID
		{"empty", "", false},
		{"too_short", "12345", false},
		{"too_long", "123456789012345678901", false},
		{"contains_letter", "148910429168a713", false},
		{"contains_hyphen", "1489104291-82713", false},
		{"contains_special", "148910429168271!", false},
		{"contains_space", "148910429 682713", false},
		{"hex_string", "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4", false},
		{"sql_injection", "'; DROP TABLE thread; --", false},
		{"path_traversal", "../../../etc/passwd", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsValidShortID(tt.shortID)
			assert.Equal(t, tt.expected, result, "shortID: %s", tt.shortID)
		})
	}
}

func TestParseChannelID(t *testing.T) {
	tests := []struct {
		name          string
		channelID     string
		expectGroupNo string
		expectShortID string
		expectError   bool
	}{
		// 有效的 channelID
		{
			name:          "valid",
			channelID:     "abc12345678901234567890123456789a____1489104291682713601",
			expectGroupNo: "abc12345678901234567890123456789a",
			expectShortID: "1489104291682713601",
			expectError:   false,
		},

		// 无效的 channelID
		{
			name:        "no_separator",
			channelID:   "abc123def456",
			expectError: true,
		},
		{
			name:        "multiple_separators",
			channelID:   "abc____123____def",
			expectError: true,
		},
		{
			name:        "empty",
			channelID:   "",
			expectError: true,
		},
		{
			name:        "only_separator",
			channelID:   "____",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groupNo, shortID, err := ParseChannelID(tt.channelID)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectGroupNo, groupNo)
				assert.Equal(t, tt.expectShortID, shortID)
			}
		})
	}
}

func TestBuildChannelID(t *testing.T) {
	groupNo := "abc12345678901234567890123456789a"
	shortID := "1489104291682713601"
	expected := "abc12345678901234567890123456789a____1489104291682713601"

	result := BuildChannelID(groupNo, shortID)
	assert.Equal(t, expected, result)

	// 验证 Parse 和 Build 是互逆的
	parsedGroupNo, parsedShortID, err := ParseChannelID(result)
	assert.NoError(t, err)
	assert.Equal(t, groupNo, parsedGroupNo)
	assert.Equal(t, shortID, parsedShortID)
}

func TestIsValidGroupNo(t *testing.T) {
	tests := []struct {
		name     string
		groupNo  string
		expected bool
	}{
		// 有效的 groupNo (32位十六进制，与 shortID 格式相同)
		{"valid_lowercase", "151960c60144482684d816eb469de867", true},
		{"valid_uppercase", "A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4", true},
		{"valid_mixed", "a1B2c3D4e5F6a1B2c3D4e5F6a1B2c3D4", true},
		{"valid_all_zeros", "00000000000000000000000000000000", true},

		// 无效的 groupNo
		{"empty", "", false},
		{"too_short", "a1b2c3d4e5f6", false},
		{"too_long", "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6", false},
		{"contains_hyphen", "a1b2c3d4-e5f6-a1b2-c3d4-e5f6a1b2c3d4", false},
		{"contains_g", "g1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4", false},
		{"contains_special", "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d!", false},
		{"sql_injection", "'; DROP TABLE thread; --", false},
		{"path_traversal", "../../../etc/passwd", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsValidGroupNo(tt.groupNo)
			assert.Equal(t, tt.expected, result, "groupNo: %s", tt.groupNo)
		})
	}
}

// ==================== 状态常量测试 ====================

func TestThreadStatusConstants(t *testing.T) {
	// 确保状态常量值正确
	assert.Equal(t, 1, ThreadStatusActive)
	assert.Equal(t, 2, ThreadStatusArchived)
	assert.Equal(t, 3, ThreadStatusDeleted)
}
