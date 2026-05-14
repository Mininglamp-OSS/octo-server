package channel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// TestGetStoryline_NotGroupMember tests that non-group members cannot access storyline
func TestGetStoryline_NotGroupMember(t *testing.T) {
	s, ctx := testutil.NewTestServer()

	// Create login user
	userService := user.NewService(ctx)
	err := userService.AddUser(&user.AddUserReq{
		UID:  testutil.UID,
		Name: "Login User",
	})
	assert.NoError(t, err)

	// Create a group without adding the user as member
	groupNo := "test_group_storyline"
	groupService := group.NewService(ctx)
	err = groupService.AddGroup(&group.AddGroupReq{
		GroupNo: groupNo,
		Name:    "Test Group",
	})
	assert.NoError(t, err)

	// Try to access storyline without being a member
	w := httptest.NewRecorder()
	channelType := common.ChannelTypeGroup.Uint8()
	req, err := http.NewRequest("GET",
		fmt.Sprintf("/v1/channels/%s/%d/storyline", groupNo, channelType),
		nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)

	s.GetRoute().ServeHTTP(w, req)

	// Should return error because not a group member
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "非群成员无法查询故事线")
}

// TestGetStoryline_OnlyGroupChannel tests that storyline only works for group channels
func TestGetStoryline_OnlyGroupChannel(t *testing.T) {
	s, ctx := testutil.NewTestServer()

	// Create login user
	userService := user.NewService(ctx)
	err := userService.AddUser(&user.AddUserReq{
		UID:  testutil.UID,
		Name: "Login User",
	})
	assert.NoError(t, err)

	// Try to access storyline for personal channel
	w := httptest.NewRecorder()
	channelType := common.ChannelTypePerson.Uint8()
	req, err := http.NewRequest("GET",
		fmt.Sprintf("/v1/channels/some_user/%d/storyline", channelType),
		nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)

	s.GetRoute().ServeHTTP(w, req)

	// Should return error because storyline only supports group channels
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "故事线功能仅支持群聊")
}

// TestGetStoryline_EmptyChannel tests storyline response for empty channel
func TestGetStoryline_EmptyChannel(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()

	// Create login user
	userService := user.NewService(ctx)
	err := userService.AddUser(&user.AddUserReq{
		UID:  testutil.UID,
		Name: "Login User",
	})
	assert.NoError(t, err)

	// Create a group and add the user as member
	groupNo := "test_group_empty_storyline"
	groupService := group.NewService(ctx)
	err = groupService.AddGroup(&group.AddGroupReq{
		GroupNo: groupNo,
		Name:    "Test Group",
	})
	assert.NoError(t, err)

	err = groupService.AddMember(&group.AddMemberReq{
		GroupNo:   groupNo,
		MemberUID: testutil.UID,
	})
	assert.NoError(t, err)

	// Access storyline - should return empty segments
	w := httptest.NewRecorder()
	channelType := common.ChannelTypeGroup.Uint8()
	req, err := http.NewRequest("GET",
		fmt.Sprintf("/v1/channels/%s/%d/storyline", groupNo, channelType),
		nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)

	s.GetRoute().ServeHTTP(w, req)

	// Should return OK with empty segments
	assert.Equal(t, http.StatusOK, w.Code)

	var resp StorylineResp
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Empty(t, resp.Segments)
}

// TestAggregateStorylineSegments tests the segment aggregation logic
func TestAggregateStorylineSegments(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name             string
		messages         []*storylineMessage
		expectedSegments int
	}{
		{
			name:             "empty messages",
			messages:         []*storylineMessage{},
			expectedSegments: 0,
		},
		{
			name: "single message",
			messages: []*storylineMessage{
				{MessageID: 1, FromUID: "user1", Timestamp: now.Unix()},
			},
			expectedSegments: 1,
		},
		{
			name: "messages within 5 minutes - same segment",
			messages: []*storylineMessage{
				{MessageID: 1, FromUID: "user1", Timestamp: now.Unix()},
				{MessageID: 2, FromUID: "user2", Timestamp: now.Add(1 * time.Minute).Unix()},
				{MessageID: 3, FromUID: "user1", Timestamp: now.Add(2 * time.Minute).Unix()},
			},
			expectedSegments: 1,
		},
		{
			name: "messages with gap > 5 minutes - multiple segments",
			messages: []*storylineMessage{
				{MessageID: 1, FromUID: "user1", Timestamp: now.Unix()},
				{MessageID: 2, FromUID: "user2", Timestamp: now.Add(1 * time.Minute).Unix()},
				// Gap of 10 minutes
				{MessageID: 3, FromUID: "user1", Timestamp: now.Add(11 * time.Minute).Unix()},
				{MessageID: 4, FromUID: "user2", Timestamp: now.Add(12 * time.Minute).Unix()},
			},
			expectedSegments: 2,
		},
		{
			name: "three separate segments",
			messages: []*storylineMessage{
				{MessageID: 1, FromUID: "user1", Timestamp: now.Unix()},
				// Gap 1
				{MessageID: 2, FromUID: "user1", Timestamp: now.Add(10 * time.Minute).Unix()},
				// Gap 2
				{MessageID: 3, FromUID: "user1", Timestamp: now.Add(20 * time.Minute).Unix()},
			},
			expectedSegments: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			segments := aggregateStorylineSegments(tt.messages, 0)
			assert.Equal(t, tt.expectedSegments, len(segments))
		})
	}
}

// TestAggregateStorylineSegments_Participants tests participant aggregation
func TestAggregateStorylineSegments_Participants(t *testing.T) {
	now := time.Now()

	messages := []*storylineMessage{
		{MessageID: 1, FromUID: "user1", Timestamp: now.Unix()},
		{MessageID: 2, FromUID: "user2", Timestamp: now.Add(1 * time.Minute).Unix()},
		{MessageID: 3, FromUID: "user3", Timestamp: now.Add(2 * time.Minute).Unix()},
		{MessageID: 4, FromUID: "user1", Timestamp: now.Add(3 * time.Minute).Unix()}, // user1 again
	}

	segments := aggregateStorylineSegments(messages, 0)
	assert.Equal(t, 1, len(segments))
	assert.Equal(t, 4, segments[0].MessageCount)
	assert.Equal(t, 3, len(segments[0].Participants)) // 3 unique participants
}

// TestAggregateStorylineSegments_StartTimestamp tests filtering by start timestamp
func TestAggregateStorylineSegments_StartTimestamp(t *testing.T) {
	now := time.Now()

	messages := []*storylineMessage{
		{MessageID: 1, FromUID: "user1", Timestamp: now.Unix()},
		{MessageID: 2, FromUID: "user1", Timestamp: now.Add(1 * time.Minute).Unix()},
		{MessageID: 3, FromUID: "user1", Timestamp: now.Add(2 * time.Minute).Unix()},
	}

	// Start from after the first message
	startTimestamp := now.Add(30 * time.Second).Unix()
	segments := aggregateStorylineSegments(messages, startTimestamp)

	assert.Equal(t, 1, len(segments))
	assert.Equal(t, 2, segments[0].MessageCount) // Only messages 2 and 3
}

// TestFilterStorylineMessages tests message filtering logic
func TestFilterStorylineMessages(t *testing.T) {
	loginUID := "login_user"

	messages := []*storylineMessage{
		{MessageID: 1, FromUID: loginUID, Timestamp: 1000},
		{MessageID: 2, FromUID: "other_user", Timestamp: 2000},
		{MessageID: 3, FromUID: loginUID, Timestamp: 3000},
		{MessageID: 4, FromUID: "another_user", Timestamp: 4000},
	}

	// Test "all" filter - should only include messages from login user
	filtered := filterStorylineMessages(messages, loginUID, "all")
	assert.Equal(t, 2, len(filtered))
	assert.Equal(t, int64(1), filtered[0].MessageID)
	assert.Equal(t, int64(3), filtered[1].MessageID)

	// Test "with_user:other_user" filter
	filtered = filterStorylineMessages(messages, loginUID, "with_user:other_user")
	assert.Equal(t, 3, len(filtered)) // login_user messages + other_user messages
}
