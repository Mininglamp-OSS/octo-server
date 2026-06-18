package space

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// seedFallbackUser 插入/更新 user 行（仅 uid + name，name 可为空字符串）。
func seedFallbackUser(t *testing.T, uid, name string) {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name) VALUES (?, ?) ON DUPLICATE KEY UPDATE name=VALUES(name)",
		uid, name,
	).Exec()
	assert.NoError(t, err)
}

// seedFallbackVerification 插入/更新 user_verification 行（uid + real_name）。
func seedFallbackVerification(t *testing.T, uid, realName string) {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO user_verification (user_id, real_name, source, source_sub, verified_at) VALUES (?, ?, 'test', ?, NOW()) "+
			"ON DUPLICATE KEY UPDATE real_name=VALUES(real_name)",
		uid, realName, uid,
	).Exec()
	assert.NoError(t, err)
}

func findMemberDetail(list []*MemberDetailModel, uid string) (*MemberDetailModel, bool) {
	for _, m := range list {
		if m.UID == uid {
			return m, true
		}
	}
	return nil, false
}

// TestQueryMembersNameFallback 验证 issue #344 的成员展示名兜底链：
//  1. user.name 空 + user_verification.real_name 有 → DisplayName() 返回 real_name
//  2. 两者皆空 → 返回稳定占位符（非空、不含 short_no/username）
//  3. user.name 正常 → 原样返回，不受影响
func TestQueryMembersNameFallback(t *testing.T) {
	_, _, err := setup(t)
	assert.NoError(t, err)

	const spaceId = "sp-name-fallback"
	// 登录用户即 owner，满足 listMembers 的成员校验前置（这里直查 DB）。
	seedMemberSearchSpace(t, spaceId, testutil.UID)

	// 1) name 空 + real_name 有
	const uidRealName = "u-realname"
	seedFallbackUser(t, uidRealName, "")
	seedFallbackVerification(t, uidRealName, "Zhang Wei")
	seedMemberSearchMember(t, spaceId, uidRealName, 0, 1)

	// 2) name 空 + real_name 空（无 verification 行 → LEFT JOIN 给空串）
	const uidPlaceholder = "u-placeholder"
	seedFallbackUser(t, uidPlaceholder, "")
	seedMemberSearchMember(t, spaceId, uidPlaceholder, 0, 1)

	// 3) name 正常
	const uidNamed = "u-named"
	seedFallbackUser(t, uidNamed, "Alice")
	seedFallbackVerification(t, uidNamed, "Should Not Win")
	seedMemberSearchMember(t, spaceId, uidNamed, 0, 1)

	members, err := testSpaceDB.queryMembers(spaceId, testutil.UID, 1, 100)
	assert.NoError(t, err)

	// case 1: real_name 兜底
	if m, ok := findMemberDetail(members, uidRealName); assert.True(t, ok, "real_name member missing") {
		assert.Equal(t, "", m.Name, "user.name should be empty")
		assert.Equal(t, "Zhang Wei", m.RealName)
		assert.Equal(t, "Zhang Wei", m.DisplayName(), "empty name must fall back to real_name")
	}

	// case 2: 占位符
	if m, ok := findMemberDetail(members, uidPlaceholder); assert.True(t, ok, "placeholder member missing") {
		assert.Equal(t, "", m.Name)
		assert.Equal(t, "", m.RealName)
		got := m.DisplayName()
		assert.NotEqual(t, "", got, "placeholder must never be empty")
		assert.Equal(t, memberDisplayNamePlaceholderPrefix+uidPlaceholder, got)
		// 隐私门禁：占位符不得使用 username（这里 username 列存在但兜底链绝不读它）。
		assert.NotContains(t, got, "short_no")
	}

	// case 3: 正常 name 不变
	if m, ok := findMemberDetail(members, uidNamed); assert.True(t, ok, "named member missing") {
		assert.Equal(t, "Alice", m.Name)
		assert.Equal(t, "Alice", m.DisplayName(), "populated name must be returned unchanged")
	}
}

// TestMemberDisplayNamePure 纯函数验证 DisplayName 兜底优先级，
// 不依赖 DB，明确禁止空串/隐私字段兜底。
func TestMemberDisplayNamePure(t *testing.T) {
	mk := func(uid, name, realName string) *MemberDetailModel {
		m := &MemberDetailModel{Name: name, RealName: realName}
		m.UID = uid
		return m
	}

	assert.Equal(t, "Alice", mk("u1", "Alice", "Real").DisplayName())
	assert.Equal(t, "Real", mk("u2", "", "Real").DisplayName())
	assert.Equal(t, memberDisplayNamePlaceholderPrefix+"u3", mk("u3", "", "").DisplayName())
	assert.NotEqual(t, "", mk("u4", "", "").DisplayName())
}
