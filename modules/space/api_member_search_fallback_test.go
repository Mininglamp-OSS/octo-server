package space

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// Test_Issue434_SearchBlankNameMemberFallsBackToRealName pins the #434 P1 defect:
// the display-name fallback shipped in #420 for the member LIST was never applied
// to the sibling member SEARCH path, so blank-name members rendered as empty rows.
// After the fix the search result must reuse the same #344 fallback chain:
//  1. user.name empty + user_verification.real_name set → real_name is returned;
//  2. both empty → the stable "User <uid>" placeholder, never an empty string;
//  3. a populated user.name is returned unchanged (real_name must not override it).
func Test_Issue434_SearchBlankNameMemberFallsBackToRealName(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)

	const spaceId = "sp-434-search-fallback"
	seedMemberSearchSpace(t, spaceId, testutil.UID)

	// case 1: name empty + real_name present → real_name
	const uidRealName = "u434-realname"
	seedFallbackUser(t, uidRealName, "")
	seedFallbackVerification(t, uidRealName, "Zhang Wei")
	seedMemberSearchMember(t, spaceId, uidRealName, 0, 1)

	// case 2: name empty + real_name empty → stable placeholder
	const uidPlaceholder = "u434-placeholder"
	seedFallbackUser(t, uidPlaceholder, "")
	seedMemberSearchMember(t, spaceId, uidPlaceholder, 0, 1)

	// case 3: populated name wins over a present real_name
	const uidNamed = "u434-named"
	seedFallbackUser(t, uidNamed, "Alice")
	seedFallbackVerification(t, uidNamed, "Should Not Win")
	seedMemberSearchMember(t, spaceId, uidNamed, 0, 1)

	w := getMembersSearch(t, srv, testCtx, spaceId, url.Values{"page_size": {"50"}})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp := decodeMembersSearchResp(t, w)

	if it, ok := findMemberSearchItem(resp.List, uidRealName); assert.True(t, ok, "real_name member missing from search") {
		assert.Equal(t, "Zhang Wei", it.Name, "blank name must fall back to real_name in search results")
	}
	if it, ok := findMemberSearchItem(resp.List, uidPlaceholder); assert.True(t, ok, "placeholder member missing from search") {
		assert.NotEqual(t, "", it.Name, "blank-name member must never render an empty name")
		assert.Equal(t, memberDisplayNamePlaceholderPrefix+uidPlaceholder, it.Name)
		// privacy gate: the placeholder must not leak short_no / username.
		assert.NotContains(t, it.Name, "short_no")
	}
	if it, ok := findMemberSearchItem(resp.List, uidNamed); assert.True(t, ok, "named member missing from search") {
		assert.Equal(t, "Alice", it.Name, "a populated name must be returned unchanged")
	}
}

// Test_Issue434_MembersSearchableByRealName pins the other half of the #434 P1
// defect: members were not searchable by their verified real_name. After the fix
// a keyword matching only user_verification.real_name must surface the member, and
// list + count must stay in lockstep (both join user_verification) so pagination
// does not drift.
func Test_Issue434_MembersSearchableByRealName(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)

	const spaceId = "sp-434-search-by-realname"
	seedMemberSearchSpace(t, spaceId, testutil.UID)

	// Target: blank user.name, real_name only. A keyword hitting real_name must find it.
	const uidTarget = "u434-search-target"
	seedFallbackUser(t, uidTarget, "")
	seedFallbackVerification(t, uidTarget, "Ouyang Distinctive")
	seedMemberSearchMember(t, spaceId, uidTarget, 0, 1)

	// Decoy: unrelated member that must not match the real_name keyword.
	seedMemberSearchUser(t, "u434-decoy", "Bob Decoy", "bob_decoy", "", "")
	seedMemberSearchMember(t, spaceId, "u434-decoy", 0, 1)

	w := getMembersSearch(t, srv, testCtx, spaceId, url.Values{"keyword": {"Ouyang"}})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp := decodeMembersSearchResp(t, w)

	assert.EqualValues(t, 1, resp.Count, "count must reflect the real_name match (list/count must share the uv join)")
	if assert.Len(t, resp.List, 1, "member must be searchable by real_name") {
		assert.Equal(t, uidTarget, resp.List[0].UID)
		assert.Equal(t, "Ouyang Distinctive", resp.List[0].Name)
	}
}
