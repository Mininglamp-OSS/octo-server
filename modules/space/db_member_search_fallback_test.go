package space

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

func findSearchModel(list []*memberSearchModel, uid string) (*memberSearchModel, bool) {
	for _, m := range list {
		if m.UID == uid {
			return m, true
		}
	}
	return nil, false
}

// Test_Issue434_SearchMemberNameFallback 验证 issue #434（#344 的 follow-up）：
// 成员搜索路径（searchMembers / countSearchMembers）此前未做 #420 给成员列表
// 加的 name 兜底——空 user.name 的成员在搜索结果里展示空名，且无法按
// user_verification.real_name 检索。本测试锁定与成员列表一致的行为：
//  1. name 空 + real_name 有 → 搜索结果 DisplayName() 回退到 real_name；
//  2. name 空 + real_name 空 → 稳定占位符（非空、不含隐私字段）；
//  3. 可按 real_name 检索到 blank-name 成员，且 count 与 list 口径一致。
func Test_Issue434_SearchMemberNameFallback(t *testing.T) {
	_, _, err := setup(t)
	assert.NoError(t, err)

	const spaceId = "sp-search-namefallback"
	seedMemberSearchSpace(t, spaceId, testutil.UID)

	// 1) name 空 + real_name 有
	const uidRealName = "u-search-realname"
	seedFallbackUser(t, uidRealName, "")
	seedFallbackVerification(t, uidRealName, "Zhang Wei")
	seedMemberSearchMember(t, spaceId, uidRealName, 0, 1)

	// 2) name 空 + real_name 空（无 verification 行 → LEFT JOIN 给空串）
	const uidPlaceholder = "u-search-placeholder"
	seedFallbackUser(t, uidPlaceholder, "")
	seedMemberSearchMember(t, spaceId, uidPlaceholder, 0, 1)

	// 空 keyword：全部成员，real_name 兜底生效
	all, err := testSpaceDB.searchMembers(spaceId, "", 1, 100)
	assert.NoError(t, err)
	if m, ok := findSearchModel(all, uidRealName); assert.True(t, ok, "real_name member missing in search") {
		assert.Equal(t, "", m.Name, "user.name should be empty")
		assert.Equal(t, "Zhang Wei", m.RealName)
		assert.Equal(t, "Zhang Wei", m.DisplayName(), "empty name must fall back to real_name in search")
	}
	if m, ok := findSearchModel(all, uidPlaceholder); assert.True(t, ok, "placeholder member missing in search") {
		assert.Equal(t, "", m.Name)
		assert.Equal(t, "", m.RealName)
		assert.Equal(t, memberDisplayNamePlaceholderPrefix+uidPlaceholder, m.DisplayName(),
			"empty name+real_name must fall back to stable placeholder")
	}

	// 按 real_name 检索：应命中 blank-name 成员（此前搜不到 → 本 issue 的核心缺陷）
	byReal, err := testSpaceDB.searchMembers(spaceId, "Zhang", 1, 100)
	assert.NoError(t, err)
	_, ok := findSearchModel(byReal, uidRealName)
	assert.True(t, ok, "member must be searchable by real_name")

	// count 与 list 口径一致（同一 keyword 两侧都要含 uv JOIN，否则分页错位）
	cnt, err := testSpaceDB.countSearchMembers(spaceId, "Zhang")
	assert.NoError(t, err)
	assert.Equal(t, int64(1), cnt, "count must match real_name search list")
}
