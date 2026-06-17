package webhook

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildThreadTitle(t *testing.T) {
	tests := []struct {
		name       string
		channelID  string
		threadName string
		groupName  string
		expected   string
	}{
		{
			name:       "both thread name and group name",
			channelID:  "groupNo____shortID",
			threadName: "日常聊天",
			groupName:  "Octo",
			expected:   "#日常聊天,Octo",
		},
		{
			name:       "only thread name",
			channelID:  "groupNo____shortID",
			threadName: "日常聊天",
			groupName:  "",
			expected:   "#日常聊天",
		},
		{
			name:       "only group name",
			channelID:  "groupNo____shortID",
			threadName: "",
			groupName:  "Octo",
			expected:   "Octo",
		},
		{
			name:       "neither thread name nor group name",
			channelID:  "groupNo____shortID",
			threadName: "",
			groupName:  "",
			expected:   "groupNo____shortID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := BuildThreadTitle(tt.channelID, tt.threadName, tt.groupName)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParseThreadChannelID(t *testing.T) {
	tests := []struct {
		name      string
		channelID string
		ok        bool
		groupNo   string
		shortID   string
	}{
		{
			name:      "valid thread channel ID",
			channelID: "abc12345678901234567890123456789a____1489104291682713601",
			ok:        true,
			groupNo:   "abc12345678901234567890123456789a",
			shortID:   "1489104291682713601",
		},
		{
			name:      "no separator",
			channelID: "plain_group_no",
			ok:        false,
		},
		{
			name:      "empty group no",
			channelID: "____shortID",
			ok:        false,
		},
		{
			name:      "empty short ID",
			channelID: "groupNo____",
			ok:        false,
		},
		{
			name:      "only separator",
			channelID: "____",
			ok:        false,
		},
		{
			name:      "empty string",
			channelID: "",
			ok:        false,
		},
		{
			name:      "multiple separators takes first split",
			channelID: "groupNo____shortID____extra",
			ok:        true,
			groupNo:   "groupNo",
			shortID:   "shortID____extra",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groupNo, shortID, ok := parseThreadChannelID(tt.channelID)
			assert.Equal(t, tt.ok, ok)
			if ok {
				assert.Equal(t, tt.groupNo, groupNo)
				assert.Equal(t, tt.shortID, shortID)
			}
		})
	}
}
