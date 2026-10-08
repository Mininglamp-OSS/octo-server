package imreconcile

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestSnapshotMemberRangesCoverBothDenyScopes(t *testing.T) {
	var members, parentDeny, childDeny []string
	for i := 0; i < 270; i++ {
		members = append(members, fmt.Sprintf("u%03d", i))
	}
	parentDeny = []string{"A", "a", "u129", "拒绝", "🙂"}
	childDeny = []string{"A", "拒绝"}
	pages := splitMemberPages(members, parentDeny, childDeny)
	require.Len(t, pages, 3)
	var gotMembers, gotParent, gotChild []string
	for i, p := range pages {
		if i == 0 {
			require.Empty(t, p.Start)
		} else {
			require.Equal(t, pages[i-1].End, p.Start)
		}
		for _, list := range [][]string{p.Subscribers, p.ParentDeny, p.ChildDeny} {
			for _, uid := range list {
				require.True(t, uid >= p.Start && (p.End == "" || uid < p.End))
			}
		}
		gotMembers = append(gotMembers, p.Subscribers...)
		gotParent = append(gotParent, p.ParentDeny...)
		gotChild = append(gotChild, p.ChildDeny...)
	}
	sort.Strings(members)
	sort.Strings(parentDeny)
	sort.Strings(childDeny)
	require.Equal(t, members, gotMembers)
	require.Equal(t, parentDeny, gotParent)
	require.Equal(t, childDeny, gotChild)
	require.Empty(t, pages[len(pages)-1].End)
}

func TestSnapshotEmptyAuthorityStillCoversOldMembers(t *testing.T) {
	pages := splitMemberPages(nil, nil, nil)
	require.Len(t, pages, 1)
	require.Empty(t, pages[0].Start)
	require.Empty(t, pages[0].End)
}

func TestSnapshotHTTPResponsePreservesSpecificConflict(t *testing.T) {
	for _, tc := range []struct{ body, code string }{
		{`{"msg":"snapshot_upgrade_required","data":{"error":"snapshot_upgrade_required"}}`, "snapshot_upgrade_required"},
		{`{"msg":"revision_conflict"}`, "revision_conflict"},
		{`{"msg":"stale_revision"}`, "stale_revision"},
		{`unparseable upstream error`, ""},
	} {
		var response *snapshotHTTPError
		require.ErrorAs(t, snapshotResponseError(http.StatusConflict, []byte(tc.body)), &response)
		require.Equal(t, tc.code, response.Code)
		require.Equal(t, http.StatusConflict, response.Status)
	}
	require.NoError(t, snapshotResponseError(http.StatusOK, nil))
}

func TestSnapshotRetryErrorPreservesUTF8(t *testing.T) {
	message := errorMessage(fmt.Errorf("%s", strings.Repeat("中文🙂", 300)))
	require.True(t, utf8.ValidString(message))
	require.Len(t, []rune(message), 512)
	require.EqualValues(t, 1, retrySeconds(0))
	require.EqualValues(t, 256, retrySeconds(31))
}
