package thread

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
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

// ==================== parsePayloadContent 测试 ====================

func TestParsePayloadContent(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    string
	}{
		{
			name:    "normal_text_message",
			payload: []byte(`{"type":1,"content":"你好世界"}`),
			want:    "你好世界",
		},
		{
			name:    "empty_content",
			payload: []byte(`{"type":1,"content":""}`),
			want:    "",
		},
		{
			name:    "no_content_field",
			payload: []byte(`{"type":1}`),
			want:    "",
		},
		{
			name:    "content_is_number",
			payload: []byte(`{"type":1,"content":123}`),
			want:    "",
		},
		{
			name:    "nil_payload",
			payload: nil,
			want:    "",
		},
		{
			name:    "empty_payload",
			payload: []byte{},
			want:    "",
		},
		{
			name:    "invalid_json",
			payload: []byte(`not json`),
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parsePayloadContent(tt.payload)
			assert.Equal(t, tt.want, got)
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

// ==================== DB 层 UpdateMessageStats 测试 ====================

func TestUpdateMessageStats(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)

	// 插入 thread
	shortID := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
	err = db.Insert(&Model{
		ShortID:    shortID,
		GroupNo:    "00000000000000000000000000000001",
		Name:       "统计测试",
		CreatorUID: "u1",
		Status:     ThreadStatusActive,
		Version:    1,
	})
	assert.NoError(t, err)

	// 初始状态
	thread, err := db.QueryByShortID(shortID)
	assert.NoError(t, err)
	assert.Equal(t, int64(0), thread.MessageCount)
	assert.Nil(t, thread.LastMessageAt)
	assert.Empty(t, thread.LastMessageContent)
	assert.Empty(t, thread.LastMessageSenderUID)

	// 更新一次
	err = db.UpdateMessageStats(shortID, "你好世界", "sender1")
	assert.NoError(t, err)

	thread, err = db.QueryByShortID(shortID)
	assert.NoError(t, err)
	assert.Equal(t, int64(1), thread.MessageCount)
	assert.NotNil(t, thread.LastMessageAt)
	assert.Equal(t, "你好世界", thread.LastMessageContent)
	assert.Equal(t, "sender1", thread.LastMessageSenderUID)

	// 再更新一次，message_count 应递增
	err = db.UpdateMessageStats(shortID, "第二条消息", "sender2")
	assert.NoError(t, err)

	thread, err = db.QueryByShortID(shortID)
	assert.NoError(t, err)
	assert.Equal(t, int64(2), thread.MessageCount)
	assert.Equal(t, "第二条消息", thread.LastMessageContent)
	assert.Equal(t, "sender2", thread.LastMessageSenderUID)
}

// ==================== RemoveUserFromGroupThreads 测试 ====================

func setupServiceTestData(t *testing.T) (*Service, string) {
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 创建测试用户
	userDB := user.NewDB(ctx)
	err = userDB.Insert(&user.Model{UID: testutil.UID, Name: "用户1", ShortNo: "u10000"})
	assert.NoError(t, err)
	err = userDB.Insert(&user.Model{UID: "user2", Name: "用户2", ShortNo: "u10002"})
	assert.NoError(t, err)

	// 创建测试群
	groupNo := strings.ReplaceAll(util.GenerUUID(), "-", "")
	groupDB := group.NewDB(ctx)
	err = groupDB.Insert(&group.Model{GroupNo: groupNo, Name: "测试群", Creator: testutil.UID, Status: 1, Version: 1})
	assert.NoError(t, err)
	err = groupDB.InsertMember(&group.MemberModel{GroupNo: groupNo, UID: testutil.UID, Role: group.MemberRoleCreator, Status: 1, Version: 1, Vercode: util.GenerUUID()})
	assert.NoError(t, err)
	err = groupDB.InsertMember(&group.MemberModel{GroupNo: groupNo, UID: "user2", Role: group.MemberRoleCommon, Status: 1, Version: 1, Vercode: util.GenerUUID()})
	assert.NoError(t, err)

	svc := NewService(ctx).(*Service)
	return svc, groupNo
}

func TestRemoveUserFromGroupThreads(t *testing.T) {
	svc, groupNo := setupServiceTestData(t)

	// 创建两个子区
	thread1, err := svc.CreateThread(&CreateThreadReq{GroupNo: groupNo, Name: "子区1", CreatorUID: testutil.UID, CreatorName: "用户1"})
	assert.NoError(t, err)
	thread2, err := svc.CreateThread(&CreateThreadReq{GroupNo: groupNo, Name: "子区2", CreatorUID: testutil.UID, CreatorName: "用户1"})
	assert.NoError(t, err)

	// user2 加入两个子区
	err = svc.JoinThread(groupNo, thread1.ShortID, "user2")
	assert.NoError(t, err)
	err = svc.JoinThread(groupNo, thread2.ShortID, "user2")
	assert.NoError(t, err)

	// 确认 user2 是两个子区的成员
	isMember1, _ := svc.IsMember(groupNo, thread1.ShortID, "user2")
	isMember2, _ := svc.IsMember(groupNo, thread2.ShortID, "user2")
	assert.True(t, isMember1)
	assert.True(t, isMember2)

	// 执行批量移除
	err = svc.RemoveUserFromGroupThreads(groupNo, "user2")
	assert.NoError(t, err)

	// 验证 user2 已从所有子区移除
	isMember1, _ = svc.IsMember(groupNo, thread1.ShortID, "user2")
	isMember2, _ = svc.IsMember(groupNo, thread2.ShortID, "user2")
	assert.False(t, isMember1)
	assert.False(t, isMember2)

	// 验证创建者(testutil.UID)不受影响
	isCreator1, _ := svc.IsMember(groupNo, thread1.ShortID, testutil.UID)
	isCreator2, _ := svc.IsMember(groupNo, thread2.ShortID, testutil.UID)
	assert.True(t, isCreator1)
	assert.True(t, isCreator2)
}

func TestRemoveUserFromGroupThreads_NoThreads(t *testing.T) {
	svc, groupNo := setupServiceTestData(t)

	// user2 没加入任何子区，调用应无副作用
	err := svc.RemoveUserFromGroupThreads(groupNo, "user2")
	assert.NoError(t, err)
}

func TestRemoveUserFromGroupThreads_OnlyAffectsTargetGroup(t *testing.T) {
	svc, groupNo1 := setupServiceTestData(t)

	// 创建第二个群
	groupNo2 := strings.ReplaceAll(util.GenerUUID(), "-", "")
	groupDB := group.NewDB(svc.ctx)
	err := groupDB.Insert(&group.Model{GroupNo: groupNo2, Name: "群2", Creator: testutil.UID, Status: 1, Version: 1})
	assert.NoError(t, err)
	err = groupDB.InsertMember(&group.MemberModel{GroupNo: groupNo2, UID: testutil.UID, Role: group.MemberRoleCreator, Status: 1, Version: 1, Vercode: util.GenerUUID()})
	assert.NoError(t, err)
	err = groupDB.InsertMember(&group.MemberModel{GroupNo: groupNo2, UID: "user2", Role: group.MemberRoleCommon, Status: 1, Version: 1, Vercode: util.GenerUUID()})
	assert.NoError(t, err)

	// 两个群各创建一个子区，user2 都加入
	t1, err := svc.CreateThread(&CreateThreadReq{GroupNo: groupNo1, Name: "群1子区", CreatorUID: testutil.UID, CreatorName: "用户1"})
	assert.NoError(t, err)
	t2, err := svc.CreateThread(&CreateThreadReq{GroupNo: groupNo2, Name: "群2子区", CreatorUID: testutil.UID, CreatorName: "用户1"})
	assert.NoError(t, err)

	err = svc.JoinThread(groupNo1, t1.ShortID, "user2")
	assert.NoError(t, err)
	err = svc.JoinThread(groupNo2, t2.ShortID, "user2")
	assert.NoError(t, err)

	// 只移除群1的子区成员
	err = svc.RemoveUserFromGroupThreads(groupNo1, "user2")
	assert.NoError(t, err)

	// 群1子区已移除
	isMember1, _ := svc.IsMember(groupNo1, t1.ShortID, "user2")
	assert.False(t, isMember1)

	// 群2子区不受影响
	isMember2, _ := svc.IsMember(groupNo2, t2.ShortID, "user2")
	assert.True(t, isMember2)
}
