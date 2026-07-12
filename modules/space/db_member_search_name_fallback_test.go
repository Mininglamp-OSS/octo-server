package space

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// Test_Issue434_SearchMemberDisplayNamePure 纯函数验证成员搜索路径复用了与成员
// 列表相同的兜底链（issue #434）：name 空 → real_name → 稳定占位符，任何分支非空，
// 绝不读 short_no/username。不依赖 DB。
func Test_Issue434_SearchMemberDisplayNamePure(t *testing.T) {
	mk := func(uid, name, realName string) *memberSearchModel {
		m := &memberSearchModel{Name: name, RealName: realName}
		m.UID = uid
		return m
	}

	assert.Equal(t, "Alice", mk("u1", "Alice", "Real").DisplayName())
	assert.Equal(t, "Real", mk("u2", "", "Real").DisplayName())
	assert.Equal(t, memberDisplayNamePlaceholderPrefix+"u3", mk("u3", "", "").DisplayName())
	assert.NotEqual(t, "", mk("u4", "", "").DisplayName(), "placeholder must never be empty")
}

// Test_Issue434_SearchMemberNameFallback 复现 #434 的 P1 缺口之一：成员搜索结果
// 对空 user.name 的成员未做兜底，会渲染成空白行。修复后 Name 应回退 real_name，
// 二者皆空时给稳定占位符。
func Test_Issue434_SearchMemberNameFallback(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)

	const spaceId = "sp-434-search-fallback"
	seedMemberSearchSpace(t, spaceId, testutil.UID) // 调用者即 owner，满足搜索前置校验

	// 1) name 空 + real_name 有 → 搜索结果回退 real_name
	const uidReal = "u434-real"
	seedFallbackUser(t, uidReal, "")
	seedFallbackVerification(t, uidReal, "Zhang Wei")
	seedMemberSearchMember(t, spaceId, uidReal, 0, 1)

	// 2) name 空 + real_name 空 → 稳定占位符，绝不空白
	const uidPlaceholder = "u434-ph"
	seedFallbackUser(t, uidPlaceholder, "")
	seedMemberSearchMember(t, spaceId, uidPlaceholder, 0, 1)

	w := getMembersSearch(t, srv, testCtx, spaceId, url.Values{"page_size": {"50"}})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp := decodeMembersSearchResp(t, w)

	real, ok := findMemberSearchItem(resp.List, uidReal)
	if assert.True(t, ok, "real_name member missing from search results") {
		assert.Equal(t, "Zhang Wei", real.Name, "empty name must fall back to real_name in search results")
	}
	ph, ok := findMemberSearchItem(resp.List, uidPlaceholder)
	if assert.True(t, ok, "placeholder member missing from search results") {
		assert.NotEqual(t, "", ph.Name, "blank name+real_name must never render as an empty string")
		assert.Equal(t, memberDisplayNamePlaceholderPrefix+uidPlaceholder, ph.Name)
	}
}

// Test_Issue434_SearchByRealName 复现 #434 的 P1 缺口之二：空 user.name 的成员
// 无法通过实名被搜到。关键词只匹配 user_verification.real_name（不匹配
// name/username/email/phone/uid），修复前 count=0，修复后应命中该成员并回退展示实名。
func Test_Issue434_SearchByRealName(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)

	const spaceId = "sp-434-search-realname"
	seedMemberSearchSpace(t, spaceId, testutil.UID)

	const uid = "u434-byreal"
	seedFallbackUser(t, uid, "") // 无 user.name / username / email / phone
	seedFallbackVerification(t, uid, "Ouyang Feng")
	seedMemberSearchMember(t, spaceId, uid, 0, 1)

	w := getMembersSearch(t, srv, testCtx, spaceId, url.Values{"keyword": {"Ouyang"}})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp := decodeMembersSearchResp(t, w)

	assert.EqualValues(t, 1, resp.Count, "member with blank name must be searchable by real_name")
	if assert.Len(t, resp.List, 1) {
		assert.Equal(t, uid, resp.List[0].UID)
		assert.Equal(t, "Ouyang Feng", resp.List[0].Name, "search result must display the real_name fallback")
	}
}

// Test_Issue434_NamedMemberNotSearchableByRealName pins the privacy invariant flagged
// by the #537 review (three-reviewer consensus): real_name is searchable ONLY when it is
// the actual display value (user.name blank). A member with a non-empty user.name displays
// that name and never returns real_name, so their verification real_name must NOT be a
// search oracle — otherwise an in-space admin could substring-probe a hidden legal name and
// confirm it via the hit/count. This is the "可检索粒度 == 可见粒度" boundary the file also
// holds for phone (maskPhone). Without the name-empty gate this test fails (member matched).
func Test_Issue434_NamedMemberNotSearchableByRealName(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)

	const spaceId = "sp-434-realname-oracle"
	seedMemberSearchSpace(t, spaceId, testutil.UID)

	// Named member: a visible user.name AND a distinct (hidden) verification real_name.
	const uid = "u434-named-hidden"
	seedFallbackUser(t, uid, "Visible Name")
	seedFallbackVerification(t, uid, "Secret Legal Name")
	seedMemberSearchMember(t, spaceId, uid, 0, 1)

	// Probing the hidden real_name must not match — searchable granularity == visible granularity.
	w := getMembersSearch(t, srv, testCtx, spaceId, url.Values{"keyword": {"Secret Legal"}})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp := decodeMembersSearchResp(t, w)
	assert.EqualValues(t, 0, resp.Count, "named member must not be discoverable by their hidden real_name")
	_, found := findMemberSearchItem(resp.List, uid)
	assert.False(t, found, "named member's real_name must not act as a search oracle")

	// Sanity: the same member is still findable by their visible name (fallback gate is one-way).
	w2 := getMembersSearch(t, srv, testCtx, spaceId, url.Values{"keyword": {"Visible"}})
	assert.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
	resp2 := decodeMembersSearchResp(t, w2)
	if item, ok := findMemberSearchItem(resp2.List, uid); assert.True(t, ok, "named member must remain findable by visible name") {
		assert.Equal(t, "Visible Name", item.Name)
	}
}
