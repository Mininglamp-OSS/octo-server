package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PR-2 of project-p2-product-surfaces: project_id on the sidebar.
//
// #855 put project_id on GroupResp and the group detail, which answers "which
// project does this group belong to" for a client that already has the group. The
// sidebar is the surface that does NOT — it renders the conversation list, and
// without this field it cannot group those conversations by project.

// fakeProjectGroupService counts GetGroups calls so the "no extra query" property
// is asserted rather than asserted-about-in-a-comment.
type fakeProjectGroupService struct {
	group.IService
	calls  int
	groups []*group.InfoResp
}

func (f *fakeProjectGroupService) GetGroups(groupNos []string) ([]*group.InfoResp, error) {
	f.calls++
	return f.groups, nil
}

// TestSidebarProjectIDMatchesTheSpaceIDSplit pins the three-way contract the
// SpaceID comment declares, one case per target type.
//
// Asserted together rather than in three cases because the property is that the
// three AGREE with SpaceID's split — a group carries its own, a topic carries its
// parent's, a DM carries none. Split apart, each half can drift without the test
// noticing.
func TestSidebarProjectIDMatchesTheSpaceIDSplit(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		makeIMConv("g_project", common.ChannelTypeGroup.Uint8(), nowRecent()),
		makeIMConv("g_direct", common.ChannelTypeGroup.Uint8(), nowRecent()),
		makeIMConv("u_friend", common.ChannelTypePerson.Uint8(), nowRecent()),
	}
	groupSpaceMap := map[string]string{"g_project": "space_a", "g_direct": "space_a"}
	// g_direct is Space-direct, so it is ABSENT from the map rather than present
	// with an empty value — the collector only records project groups.
	groupProjectMap := map[string]string{"g_project": "p_1"}

	items := buildRecentItems(convs, recentCutoffs{}, nil, groupSpaceMap, groupProjectMap, nil, "space_a")

	byID := map[string]*SidebarItem{}
	for _, it := range items {
		byID[it.TargetID] = it
	}
	require.Contains(t, byID, "g_project")
	require.Contains(t, byID, "g_direct")

	assert.Equal(t, "p_1", byID["g_project"].ProjectID,
		"a project group carries its own project_id")
	assert.Empty(t, byID["g_direct"].ProjectID,
		"a Space-direct group carries none; '' is group.project_id's own sentinel")
	if dm, ok := byID["u_friend"]; ok {
		assert.Empty(t, dm.ProjectID,
			"a DM belongs to no project, the same way it carries no space_id")
	}
}

// TestSidebarTopicInheritsTheParentProjectID is the case a naive implementation
// gets wrong: a COMMUNITY_TOPIC's channel id is not a group number, so looking the
// project up by the item's own id would silently yield "" for every thread in a
// project group. SpaceID already resolves via the parent; this must use the same
// key, at the same site, or the two can disagree about one conversation.
func TestSidebarTopicInheritsTheParentProjectID(t *testing.T) {
	parent := "g_parent"
	convs := []*config.SyncUserConversationResp{
		makeIMConv(parent+"____1", common.ChannelTypeCommunityTopic.Uint8(), nowRecent()),
	}
	groupSpaceMap := map[string]string{parent: "space_a"}
	groupProjectMap := map[string]string{parent: "p_1"}

	items := buildRecentItems(convs, recentCutoffs{}, nil, groupSpaceMap, groupProjectMap, nil, "space_a")
	require.Len(t, items, 1)
	assert.Equal(t, "space_a", items[0].SpaceID, "precondition: the parent lookup works at all")
	assert.Equal(t, "p_1", items[0].ProjectID,
		"a topic must inherit its parent group's project, resolved by the same key "+
			"SpaceID uses — keyed on the topic's own channel id it would read empty")
}

// TestFollowItemsCarryTheProjectID covers the other builder, so the field cannot be
// present on one sidebar tab and missing from the next.
func TestFollowItemsCarryTheProjectID(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		makeIMConv("g1", common.ChannelTypeGroup.Uint8(), nowRecent()),
	}
	categorySetting := map[string]*GroupCategorySetting{
		"g1": {GroupNo: "g1", CategoryID: strPtr("cat1"), CategorySort: 1, CategoryGroupSort: 1},
	}
	items := buildFollowItems(convs, categorySetting, map[string]struct{}{}, nil, nil, nil, nil,
		map[string]string{"g1": "space_a"}, map[string]string{"g1": "p_1"}, nil, "space_a")
	require.Len(t, items, 1)
	assert.Equal(t, "p_1", items[0].ProjectID)
}

// TestProjectMapCostsNoExtraQuery is the performance property stated as a test
// rather than as a comment.
//
// The sidebar is the hottest read path in the product. project_id is free only
// because GetGroups already returns whole group rows and the collector takes a
// second pass over the SAME result; a separate collector would double the batch for
// a field that was already in the response.
func TestProjectMapCostsNoExtraQuery(t *testing.T) {
	svc := &fakeProjectGroupService{groups: []*group.InfoResp{
		{GroupNo: "g_project", SpaceID: "space_a", ProjectID: "p_1"},
		{GroupNo: "g_direct", SpaceID: "space_a"},
	}}
	convs := []*config.SyncUserConversationResp{
		makeIMConv("g_project", common.ChannelTypeGroup.Uint8(), nowRecent()),
		makeIMConv("g_direct", common.ChannelTypeGroup.Uint8(), nowRecent()),
	}

	spaceMap, projectMap, ok := CollectGroupSpaceAndProjectMaps(convs, nil, svc)
	require.True(t, ok)

	assert.Equal(t, 1, svc.calls,
		"both maps must come from ONE GetGroups call; a second collector would "+
			"double the batch on the hottest read path for a field already in the response")
	assert.Equal(t, "space_a", spaceMap["g_project"])
	assert.Equal(t, "p_1", projectMap["g_project"])
	assert.NotContains(t, projectMap, "g_direct",
		"a Space-direct group is absent rather than present with an empty value, so "+
			"the map stays proportional to project groups rather than to conversations")
}

// TestCollectGroupSpaceMapStillWorks pins the compatibility wrapper. It has one
// caller today, but deleting it silently would be a source-compatible change that
// stops being source-compatible for anything outside this package.
func TestCollectGroupSpaceMapStillWorks(t *testing.T) {
	svc := &fakeProjectGroupService{groups: []*group.InfoResp{
		{GroupNo: "g1", SpaceID: "space_a", ProjectID: "p_1"},
	}}
	convs := []*config.SyncUserConversationResp{
		makeIMConv("g1", common.ChannelTypeGroup.Uint8(), nowRecent()),
	}
	m, ok := CollectGroupSpaceMap(convs, nil, svc)
	require.True(t, ok)
	assert.Equal(t, "space_a", m["g1"])
}
