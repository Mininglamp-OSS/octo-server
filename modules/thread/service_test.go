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

// ==================== DB 层 QueryThreadMetaByShortIDs 测试 ====================

func TestQueryThreadMetaByShortIDs(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)

	// 插入三个 thread：两个有 source_message_id，一个没有
	shortID1 := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
	shortID2 := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
	shortID3 := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())

	srcMsgID1 := int64(100001)
	srcMsgID2 := int64(100002)

	err = db.Insert(&Model{
		ShortID:         shortID1,
		GroupNo:         "00000000000000000000000000000001",
		Name:            "有源消息1",
		CreatorUID:      "u1",
		SourceMessageID: &srcMsgID1,
		Status:          ThreadStatusActive,
		Version:         1,
	})
	assert.NoError(t, err)

	err = db.Insert(&Model{
		ShortID:         shortID2,
		GroupNo:         "00000000000000000000000000000001",
		Name:            "有源消息2",
		CreatorUID:      "u1",
		SourceMessageID: &srcMsgID2,
		Status:          ThreadStatusActive,
		Version:         1,
	})
	assert.NoError(t, err)

	err = db.Insert(&Model{
		ShortID:    shortID3,
		GroupNo:    "00000000000000000000000000000001",
		Name:       "无源消息",
		CreatorUID: "u1",
		Status:     ThreadStatusActive,
		Version:    1,
	})
	assert.NoError(t, err)

	// 模拟消息统计更新
	err = db.UpdateMessageStats(shortID1, "hello", "u1")
	assert.NoError(t, err)
	err = db.UpdateMessageStats(shortID1, "world", "u2")
	assert.NoError(t, err)

	// 批量查询元数据
	result, err := db.QueryThreadMetaByShortIDs([]string{shortID1, shortID2, shortID3})
	assert.NoError(t, err)
	assert.Len(t, result, 3)

	// shortID1: 有 source_message_id，message_count=2
	assert.NotNil(t, result[shortID1].SourceMessageID)
	assert.Equal(t, srcMsgID1, *result[shortID1].SourceMessageID)
	assert.Equal(t, int64(2), result[shortID1].MessageCount)

	// shortID2: 有 source_message_id，message_count=0
	assert.NotNil(t, result[shortID2].SourceMessageID)
	assert.Equal(t, srcMsgID2, *result[shortID2].SourceMessageID)
	assert.Equal(t, int64(0), result[shortID2].MessageCount)

	// shortID3: 无 source_message_id，message_count=0
	assert.Nil(t, result[shortID3].SourceMessageID)
	assert.Equal(t, int64(0), result[shortID3].MessageCount)

	// 空列表不报错
	emptyResult, err := db.QueryThreadMetaByShortIDs([]string{})
	assert.NoError(t, err)
	assert.Len(t, emptyResult, 0)

	// 不存在的 shortID 不在结果中
	unknownResult, err := db.QueryThreadMetaByShortIDs([]string{"999999999999999"})
	assert.NoError(t, err)
	assert.Len(t, unknownResult, 0)

	// 向后兼容：QuerySourceMessageIDsByShortIDs 仍然工作
	srcResult, err := db.QuerySourceMessageIDsByShortIDs([]string{shortID1, shortID3})
	assert.NoError(t, err)
	assert.Len(t, srcResult, 2)
	assert.Equal(t, srcMsgID1, *srcResult[shortID1])
	assert.Nil(t, srcResult[shortID3])
}

// ==================== QueryThreadMetaByShortIDs 过滤已删除子区 ====================

func TestQueryThreadMetaByShortIDs_ExcludeDeleted(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)

	shortIDActive := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
	shortIDDeleted := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
	shortIDArchived := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())

	srcMsg1 := int64(200001)
	srcMsg2 := int64(200002)
	srcMsg3 := int64(200003)

	err = db.Insert(&Model{
		ShortID:         shortIDActive,
		GroupNo:         "00000000000000000000000000000001",
		Name:            "活跃子区",
		CreatorUID:      "u1",
		SourceMessageID: &srcMsg1,
		Status:          ThreadStatusActive,
		Version:         1,
	})
	assert.NoError(t, err)

	err = db.Insert(&Model{
		ShortID:         shortIDDeleted,
		GroupNo:         "00000000000000000000000000000001",
		Name:            "已删除子区",
		CreatorUID:      "u1",
		SourceMessageID: &srcMsg2,
		Status:          ThreadStatusDeleted,
		Version:         1,
	})
	assert.NoError(t, err)

	err = db.Insert(&Model{
		ShortID:         shortIDArchived,
		GroupNo:         "00000000000000000000000000000001",
		Name:            "已归档子区",
		CreatorUID:      "u1",
		SourceMessageID: &srcMsg3,
		Status:          ThreadStatusArchived,
		Version:         1,
	})
	assert.NoError(t, err)

	result, err := db.QueryThreadMetaByShortIDs([]string{shortIDActive, shortIDDeleted, shortIDArchived})
	assert.NoError(t, err)

	// 已删除的子区不应出现在结果中
	assert.Len(t, result, 2)
	assert.Contains(t, result, shortIDActive)
	assert.Contains(t, result, shortIDArchived)
	assert.NotContains(t, result, shortIDDeleted)
}

// ==================== DB 层 QueryNonDeletedShortIDs 测试 ====================

func TestQueryNonDeletedShortIDs(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)

	shortIDActive := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
	shortIDDeleted := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
	shortIDArchived := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())

	err = db.Insert(&Model{
		ShortID:    shortIDActive,
		GroupNo:    "00000000000000000000000000000001",
		Name:       "活跃",
		CreatorUID: "u1",
		Status:     ThreadStatusActive,
		Version:    1,
	})
	assert.NoError(t, err)

	err = db.Insert(&Model{
		ShortID:    shortIDDeleted,
		GroupNo:    "00000000000000000000000000000001",
		Name:       "已删除",
		CreatorUID: "u1",
		Status:     ThreadStatusDeleted,
		Version:    1,
	})
	assert.NoError(t, err)

	err = db.Insert(&Model{
		ShortID:    shortIDArchived,
		GroupNo:    "00000000000000000000000000000001",
		Name:       "已归档",
		CreatorUID: "u1",
		Status:     ThreadStatusArchived,
		Version:    1,
	})
	assert.NoError(t, err)

	activeIDs, err := db.QueryNonDeletedShortIDs([]string{shortIDActive, shortIDDeleted, shortIDArchived})
	assert.NoError(t, err)
	assert.Len(t, activeIDs, 2)
	assert.Contains(t, activeIDs, shortIDActive)
	assert.Contains(t, activeIDs, shortIDArchived)
	assert.NotContains(t, activeIDs, shortIDDeleted)

	// 空列表不报错
	emptyResult, err := db.QueryNonDeletedShortIDs([]string{})
	assert.NoError(t, err)
	assert.Len(t, emptyResult, 0)
}

// TestQueryActiveShortIDs PR-B：sync 过滤路径只保留 active 子区（archived/deleted 都丢）
func TestQueryActiveShortIDs(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)
	shortIDActive := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
	shortIDArchived := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
	shortIDDeleted := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())

	for _, m := range []*Model{
		{ShortID: shortIDActive, GroupNo: "00000000000000000000000000000001", Name: "active", CreatorUID: "u1", Status: ThreadStatusActive, Version: 1},
		{ShortID: shortIDArchived, GroupNo: "00000000000000000000000000000001", Name: "archived", CreatorUID: "u1", Status: ThreadStatusArchived, Version: 1},
		{ShortID: shortIDDeleted, GroupNo: "00000000000000000000000000000001", Name: "deleted", CreatorUID: "u1", Status: ThreadStatusDeleted, Version: 1},
	} {
		assert.NoError(t, db.Insert(m))
	}

	got, err := db.QueryActiveShortIDs([]string{shortIDActive, shortIDArchived, shortIDDeleted})
	assert.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Contains(t, got, shortIDActive)
	assert.NotContains(t, got, shortIDArchived)
	assert.NotContains(t, got, shortIDDeleted)

	empty, err := db.QueryActiveShortIDs(nil)
	assert.NoError(t, err)
	assert.Len(t, empty, 0)
}

// TestQueryByGroupNoWithStatus PR-B：群子区列表支持按 status 集合筛选
func TestQueryByGroupNoWithStatus(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)
	groupNo := "00000000000000000000000000000010"
	mk := func(name string, status int) *Model {
		return &Model{
			ShortID:    fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64()),
			GroupNo:    groupNo,
			Name:       name,
			CreatorUID: "u1",
			Status:     status,
			Version:    1,
		}
	}
	for _, m := range []*Model{
		mk("a1", ThreadStatusActive),
		mk("a2", ThreadStatusActive),
		mk("r1", ThreadStatusArchived),
		mk("r2", ThreadStatusArchived),
		mk("d1", ThreadStatusDeleted),
	} {
		assert.NoError(t, db.Insert(m))
	}

	cases := []struct {
		name     string
		statuses []int
		want     int
	}{
		{"default active only", []int{ThreadStatusActive}, 2},
		{"archived only", []int{ThreadStatusArchived}, 2},
		{"active+archived", []int{ThreadStatusActive, ThreadStatusArchived}, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, qerr := db.QueryByGroupNoWithStatus(groupNo, tc.statuses, 0, 100)
			assert.NoError(t, qerr)
			assert.Len(t, rows, tc.want)
			total, cerr := db.CountByGroupNoWithStatus(groupNo, tc.statuses)
			assert.NoError(t, cerr)
			assert.Equal(t, int64(tc.want), total)
		})
	}

	// 空 status 列表：返回空（保护性，避免误拉所有数据）
	rows, err := db.QueryByGroupNoWithStatus(groupNo, []int{}, 0, 100)
	assert.NoError(t, err)
	assert.Len(t, rows, 0)
	total, err := db.CountByGroupNoWithStatus(groupNo, nil)
	assert.NoError(t, err)
	assert.Equal(t, int64(0), total)
}

// TestSanitizeListStatuses defense-in-depth：service 层 status 白名单归一。
// 即便 service 被旁路调用，deleted / 未知码也不会被传到 DB。
func TestSanitizeListStatuses(t *testing.T) {
	cases := []struct {
		name string
		in   []int
		want []int
	}{
		{"nil → active", nil, []int{ThreadStatusActive}},
		{"empty → active", []int{}, []int{ThreadStatusActive}},
		{"active passthrough", []int{ThreadStatusActive}, []int{ThreadStatusActive}},
		{"archived passthrough", []int{ThreadStatusArchived}, []int{ThreadStatusArchived}},
		{"active+archived passthrough", []int{ThreadStatusActive, ThreadStatusArchived}, []int{ThreadStatusActive, ThreadStatusArchived}},
		{"deleted stripped → active", []int{ThreadStatusDeleted}, []int{ThreadStatusActive}},
		{"unknown code stripped → active", []int{99}, []int{ThreadStatusActive}},
		{"mixed legit + deleted → only legit", []int{ThreadStatusActive, ThreadStatusDeleted}, []int{ThreadStatusActive}},
		{"dedupe preserves order", []int{ThreadStatusArchived, ThreadStatusActive, ThreadStatusArchived}, []int{ThreadStatusArchived, ThreadStatusActive}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeListStatuses(tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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

// ==================== UpdateName 测试 ====================

func TestUpdateName(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	svc, groupNo := setupServiceTestData(t)

	// 创建子区
	thread, err := svc.CreateThread(&CreateThreadReq{GroupNo: groupNo, Name: "原始名称", CreatorUID: testutil.UID, CreatorName: "用户1"})
	assert.NoError(t, err)

	// 创建者修改名称
	err = svc.UpdateName(groupNo, thread.ShortID, testutil.UID, "新名称")
	assert.NoError(t, err)

	// 验证名称已更新
	updated, err := svc.GetThread(groupNo, thread.ShortID, testutil.UID)
	assert.NoError(t, err)
	assert.Equal(t, "新名称", updated.Name)
}

func TestUpdateName_NotFound(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	svc, groupNo := setupServiceTestData(t)

	err := svc.UpdateName(groupNo, "999999999999999", testutil.UID, "新名称")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestUpdateName_Deleted(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	svc, groupNo := setupServiceTestData(t)

	thread, err := svc.CreateThread(&CreateThreadReq{GroupNo: groupNo, Name: "待删除", CreatorUID: testutil.UID, CreatorName: "用户1"})
	assert.NoError(t, err)

	// 删除子区
	err = svc.DeleteThread(groupNo, thread.ShortID, testutil.UID)
	assert.NoError(t, err)

	// 删除后不能修改名称
	err = svc.UpdateName(groupNo, thread.ShortID, testutil.UID, "新名称")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "deleted")
}

func TestUpdateName_NoPermission(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	svc, groupNo := setupServiceTestData(t)

	// testutil.UID 创建子区
	thread, err := svc.CreateThread(&CreateThreadReq{GroupNo: groupNo, Name: "原始名称", CreatorUID: testutil.UID, CreatorName: "用户1"})
	assert.NoError(t, err)

	// user2 不是创建者也不是管理员，不能修改
	err = svc.UpdateName(groupNo, thread.ShortID, "user2", "新名称")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no permission")
}

func TestUpdateName_EmptyName(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	svc, groupNo := setupServiceTestData(t)

	thread, err := svc.CreateThread(&CreateThreadReq{GroupNo: groupNo, Name: "原始名称", CreatorUID: testutil.UID, CreatorName: "用户1"})
	assert.NoError(t, err)

	err = svc.UpdateName(groupNo, thread.ShortID, testutil.UID, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "name")
}

func TestUpdateName_TooLong(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	svc, groupNo := setupServiceTestData(t)

	thread, err := svc.CreateThread(&CreateThreadReq{GroupNo: groupNo, Name: "原始名称", CreatorUID: testutil.UID, CreatorName: "用户1"})
	assert.NoError(t, err)

	longName := strings.Repeat("a", 101)
	err = svc.UpdateName(groupNo, thread.ShortID, testutil.UID, longName)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "name")
}

func TestRemoveUserFromGroupThreads_OnlyAffectsTargetGroup(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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

// ==================== DB 层 ThreadMd 测试 ====================

func TestQueryThreadMd_NotSet(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)

	groupNo := "00000000000000000000000000000001"
	shortID := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
	err = db.Insert(&Model{
		ShortID:    shortID,
		GroupNo:    groupNo,
		Name:       "md测试",
		CreatorUID: "u1",
		Status:     ThreadStatusActive,
		Version:    1,
	})
	assert.NoError(t, err)

	// 未设置时应返回空内容
	result, err := db.QueryThreadMd(groupNo, shortID)
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, "", result.Content)
	assert.Equal(t, int64(0), result.Version)
	assert.Nil(t, result.UpdatedAt)
	assert.Equal(t, "", result.UpdatedBy)
}

func TestQueryThreadMd_NonExistentThread(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)

	// 查询不存在的子区
	result, err := db.QueryThreadMd("00000000000000000000000000000001", "999999999999999")
	assert.NoError(t, err)
	assert.Nil(t, result)
}

func TestQueryThreadMd_DeletedThread(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)

	groupNo := "00000000000000000000000000000001"
	shortID := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
	err = db.Insert(&Model{
		ShortID:    shortID,
		GroupNo:    groupNo,
		Name:       "已删除",
		CreatorUID: "u1",
		Status:     ThreadStatusDeleted,
		Version:    1,
	})
	assert.NoError(t, err)

	// 已删除的子区不应返回
	result, err := db.QueryThreadMd(groupNo, shortID)
	assert.NoError(t, err)
	assert.Nil(t, result)
}

func TestUpdateThreadMd(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)

	groupNo := "00000000000000000000000000000001"
	shortID := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
	err = db.Insert(&Model{
		ShortID:    shortID,
		GroupNo:    groupNo,
		Name:       "更新测试",
		CreatorUID: "u1",
		Status:     ThreadStatusActive,
		Version:    1,
	})
	assert.NoError(t, err)

	// 第一次更新
	v1, err := db.UpdateThreadMd(groupNo, shortID, "# 子区规范 v1", "u1")
	assert.NoError(t, err)
	assert.Equal(t, int64(1), v1)

	// 验证内容
	result, err := db.QueryThreadMd(groupNo, shortID)
	assert.NoError(t, err)
	assert.Equal(t, "# 子区规范 v1", result.Content)
	assert.Equal(t, int64(1), result.Version)
	assert.NotNil(t, result.UpdatedAt)
	assert.Equal(t, "u1", result.UpdatedBy)

	// 第二次更新（版本号递增）
	v2, err := db.UpdateThreadMd(groupNo, shortID, "# 子区规范 v2", "u2")
	assert.NoError(t, err)
	assert.Equal(t, int64(2), v2)

	result, err = db.QueryThreadMd(groupNo, shortID)
	assert.NoError(t, err)
	assert.Equal(t, "# 子区规范 v2", result.Content)
	assert.Equal(t, int64(2), result.Version)
	assert.Equal(t, "u2", result.UpdatedBy)
}

func TestUpdateThreadMd_NonExistentThread(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)

	// 更新不存在的子区
	_, err = db.UpdateThreadMd("00000000000000000000000000000001", "999999999999999", "content", "u1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "thread not found or already deleted")
}

func TestUpdateThreadMd_DeletedThread(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)

	groupNo := "00000000000000000000000000000001"
	shortID := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
	err = db.Insert(&Model{
		ShortID:    shortID,
		GroupNo:    groupNo,
		Name:       "已删除",
		CreatorUID: "u1",
		Status:     ThreadStatusDeleted,
		Version:    1,
	})
	assert.NoError(t, err)

	// 更新已删除的子区
	_, err = db.UpdateThreadMd(groupNo, shortID, "content", "u1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "thread not found or already deleted")
}

func TestDeleteThreadMd(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)

	groupNo := "00000000000000000000000000000001"
	shortID := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
	err = db.Insert(&Model{
		ShortID:    shortID,
		GroupNo:    groupNo,
		Name:       "删除测试",
		CreatorUID: "u1",
		Status:     ThreadStatusActive,
		Version:    1,
	})
	assert.NoError(t, err)

	// 先设置内容
	_, err = db.UpdateThreadMd(groupNo, shortID, "# 待删除内容", "u1")
	assert.NoError(t, err)

	// 删除
	v, err := db.DeleteThreadMd(groupNo, shortID, "u2")
	assert.NoError(t, err)
	assert.Equal(t, int64(2), v)

	// 验证内容已被清空
	result, err := db.QueryThreadMd(groupNo, shortID)
	assert.NoError(t, err)
	assert.Equal(t, "", result.Content) // IFNULL 转换后为空字符串
	assert.Equal(t, int64(2), result.Version)
	assert.Equal(t, "u2", result.UpdatedBy) // 保留删除者 UID
}

func TestDeleteThreadMd_NonExistentThread(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)

	// 删除不存在的子区
	_, err = db.DeleteThreadMd("00000000000000000000000000000001", "999999999999999", "u1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "thread not found or already deleted")
}

func TestThreadMd_VersionAutoIncrement(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)

	groupNo := "00000000000000000000000000000001"
	shortID := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
	err = db.Insert(&Model{
		ShortID:    shortID,
		GroupNo:    groupNo,
		Name:       "版本测试",
		CreatorUID: "u1",
		Status:     ThreadStatusActive,
		Version:    1,
	})
	assert.NoError(t, err)

	// 连续更新和删除，版本号应连续递增
	v1, err := db.UpdateThreadMd(groupNo, shortID, "v1", "u1")
	assert.NoError(t, err)
	assert.Equal(t, int64(1), v1)

	v2, err := db.UpdateThreadMd(groupNo, shortID, "v2", "u1")
	assert.NoError(t, err)
	assert.Equal(t, int64(2), v2)

	v3, err := db.DeleteThreadMd(groupNo, shortID, "u1")
	assert.NoError(t, err)
	assert.Equal(t, int64(3), v3)

	v4, err := db.UpdateThreadMd(groupNo, shortID, "v4", "u1")
	assert.NoError(t, err)
	assert.Equal(t, int64(4), v4)
}

func TestQueryThreadMd_ArchivedThread(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)

	groupNo := "00000000000000000000000000000001"
	shortID := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
	err = db.Insert(&Model{
		ShortID:    shortID,
		GroupNo:    groupNo,
		Name:       "归档测试",
		CreatorUID: "u1",
		Status:     ThreadStatusArchived,
		Version:    1,
	})
	assert.NoError(t, err)

	// 归档子区的 GROUP.md 仍可读取
	_, err = db.UpdateThreadMd(groupNo, shortID, "归档内容", "u1")
	assert.NoError(t, err)

	result, err := db.QueryThreadMd(groupNo, shortID)
	assert.NoError(t, err)
	assert.Equal(t, "归档内容", result.Content)
}

// ==================== Service 层 ThreadMd 测试 ====================

func TestServiceCanEditThreadMd(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	svc, groupNo := setupServiceTestData(t)

	// 创建子区（testutil.UID 是创建者）
	thread1, err := svc.CreateThread(&CreateThreadReq{GroupNo: groupNo, Name: "权限测试", CreatorUID: testutil.UID, CreatorName: "用户1"})
	assert.NoError(t, err)

	// 子区创建者可以编辑
	canEdit, err := svc.CanEditThreadMd(groupNo, thread1.ShortID, testutil.UID)
	assert.NoError(t, err)
	assert.True(t, canEdit)

	// 群创建者也是 testutil.UID（同上），已测试

	// 普通群成员不能编辑
	canEdit, err = svc.CanEditThreadMd(groupNo, thread1.ShortID, "user2")
	assert.NoError(t, err)
	assert.False(t, canEdit)
}

func TestServiceGetThreadMd(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	svc, groupNo := setupServiceTestData(t)

	thread1, err := svc.CreateThread(&CreateThreadReq{GroupNo: groupNo, Name: "服务测试", CreatorUID: testutil.UID, CreatorName: "用户1"})
	assert.NoError(t, err)

	// 未设置时
	result, err := svc.GetThreadMd(groupNo, thread1.ShortID)
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, "", result.Content)

	// 设置后
	v, err := svc.UpdateThreadMd(groupNo, thread1.ShortID, "# 服务层测试", testutil.UID)
	assert.NoError(t, err)
	assert.Equal(t, int64(1), v)

	result, err = svc.GetThreadMd(groupNo, thread1.ShortID)
	assert.NoError(t, err)
	assert.Equal(t, "# 服务层测试", result.Content)
	assert.Equal(t, int64(1), result.Version)
}

func TestServiceDeleteThreadMd(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	svc, groupNo := setupServiceTestData(t)

	thread1, err := svc.CreateThread(&CreateThreadReq{GroupNo: groupNo, Name: "删除测试", CreatorUID: testutil.UID, CreatorName: "用户1"})
	assert.NoError(t, err)

	// 设置内容
	_, err = svc.UpdateThreadMd(groupNo, thread1.ShortID, "# 待删除", testutil.UID)
	assert.NoError(t, err)

	// 删除
	v, err := svc.DeleteThreadMd(groupNo, thread1.ShortID, testutil.UID)
	assert.NoError(t, err)
	assert.Equal(t, int64(2), v)

	// 验证已删除
	result, err := svc.GetThreadMd(groupNo, thread1.ShortID)
	assert.NoError(t, err)
	assert.Equal(t, "", result.Content)
}
