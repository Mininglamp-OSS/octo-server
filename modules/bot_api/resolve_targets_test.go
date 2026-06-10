package bot_api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// resolve/targets test fixtures. One User Bot (bot_token auth) that is a member
// of a few groups; non-member groups/threads must never surface. Seeding helpers
// build the membership-scoped visibility the handler relies on.
const (
	rtRobotID  = "bot_rt_1"
	rtBotToken = "bf_rt_token_1"
)

func setupBotResolveTargets(t *testing.T) (http.Handler, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	assert.NoError(t, testutil.CleanAllTables(ctx))

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid, bot_token) VALUES (?, 1, ?, ?)",
		rtRobotID, "owner_rt", rtBotToken,
	).Exec()
	assert.NoError(t, err)

	return s.GetRoute(), ctx
}

// seedGroup inserts a group row in the given space.
func seedGroup(t *testing.T, ctx *config.Context, groupNo, name, spaceID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, status, version, space_id) VALUES (?, ?, 1, 1, ?)",
		groupNo, name, spaceID,
	).Exec()
	assert.NoError(t, err)
}

// seedMember adds uid to a group (active membership).
func seedMember(t *testing.T, ctx *config.Context, groupNo, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, vercode, is_deleted, status, version) VALUES (?, ?, ?, 0, 1, 1)",
		groupNo, uid, util.GenerUUID(),
	).Exec()
	assert.NoError(t, err)
}

// seedThread inserts a thread row under a group with a given status.
func seedThread(t *testing.T, ctx *config.Context, groupNo, shortID, name string, status int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO thread (short_id, group_no, name, creator_uid, status, version) VALUES (?, ?, ?, ?, ?, 1)",
		shortID, groupNo, name, "owner_rt", status,
	).Exec()
	assert.NoError(t, err)
}

type resolveTargetsResp struct {
	Candidates []resolveTargetCandidate `json:"candidates"`
	Total      int                      `json:"total"`
	Truncated  bool                     `json:"truncated"`
}

func callResolveTargets(t *testing.T, handler http.Handler, token, query string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/bot/resolve/targets?"+query, nil)
	assert.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(w, req)
	return w
}

func decodeResolveTargets(t *testing.T, w *httptest.ResponseRecorder) resolveTargetsResp {
	t.Helper()
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var b resolveTargetsResp
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &b))
	return b
}

// TestResolveTargets_GroupAndThreadSameName verifies a same-named group and
// thread both come back as distinct candidates (kind disambiguates them).
func TestResolveTargets_GroupAndThreadSameName(t *testing.T) {
	handler, ctx := setupBotResolveTargets(t)

	seedGroup(t, ctx, "g_rt_a", "研发", "space_1")
	seedMember(t, ctx, "g_rt_a", rtRobotID)
	seedThread(t, ctx, "g_rt_a", "tp_a1", "研发", 1)

	b := decodeResolveTargets(t, callResolveTargets(t, handler, rtBotToken, "name=研发"))
	assert.Equal(t, 2, b.Total)
	assert.False(t, b.Truncated)

	var group, thread *resolveTargetCandidate
	for i := range b.Candidates {
		switch b.Candidates[i].Kind {
		case resolveKindGroup:
			group = &b.Candidates[i]
		case resolveKindThread:
			thread = &b.Candidates[i]
		}
	}
	if assert.NotNil(t, group) {
		assert.Equal(t, "g_rt_a", group.ChannelID)
		assert.Equal(t, common.ChannelTypeGroup.Uint8(), group.ChannelType)
		assert.Equal(t, "g_rt_a", group.GroupNo)
	}
	if assert.NotNil(t, thread) {
		assert.Equal(t, "g_rt_a____tp_a1", thread.ChannelID)
		assert.Equal(t, common.ChannelTypeCommunityTopic.Uint8(), thread.ChannelType)
		assert.Equal(t, "g_rt_a", thread.GroupNo)
		assert.Equal(t, "tp_a1", thread.ShortID)
		assert.Equal(t, "研发", thread.ParentName)
	}
}

// TestResolveTargets_MultipleSameNameThreadsAcrossGroups verifies same-named
// threads under different parent groups all return.
func TestResolveTargets_MultipleSameNameThreadsAcrossGroups(t *testing.T) {
	handler, ctx := setupBotResolveTargets(t)

	seedGroup(t, ctx, "g_rt_b1", "大群一", "space_1")
	seedMember(t, ctx, "g_rt_b1", rtRobotID)
	seedThread(t, ctx, "g_rt_b1", "tp_b1", "周报", 1)

	seedGroup(t, ctx, "g_rt_b2", "大群二", "space_1")
	seedMember(t, ctx, "g_rt_b2", rtRobotID)
	seedThread(t, ctx, "g_rt_b2", "tp_b2", "周报", 1)

	b := decodeResolveTargets(t, callResolveTargets(t, handler, rtBotToken, "name=周报&kind=thread"))
	assert.Equal(t, 2, b.Total)
	parents := map[string]string{}
	for _, cand := range b.Candidates {
		assert.Equal(t, resolveKindThread, cand.Kind)
		parents[cand.GroupNo] = cand.ParentName
	}
	assert.Equal(t, "大群一", parents["g_rt_b1"])
	assert.Equal(t, "大群二", parents["g_rt_b2"])
}

// TestResolveTargets_NonMemberNotVisible verifies groups/threads the bot is not
// a member of never surface.
func TestResolveTargets_NonMemberNotVisible(t *testing.T) {
	handler, ctx := setupBotResolveTargets(t)

	// Bot is a member here.
	seedGroup(t, ctx, "g_rt_mine", "项目组", "space_1")
	seedMember(t, ctx, "g_rt_mine", rtRobotID)
	seedThread(t, ctx, "g_rt_mine", "tp_mine", "项目组", 1)

	// Bot is NOT a member here (some other group with the same name).
	seedGroup(t, ctx, "g_rt_other", "项目组", "space_1")
	seedThread(t, ctx, "g_rt_other", "tp_other", "项目组", 1)

	b := decodeResolveTargets(t, callResolveTargets(t, handler, rtBotToken, "name=项目组"))
	for _, cand := range b.Candidates {
		assert.Equal(t, "g_rt_mine", cand.GroupNo, "only the member group/thread must surface")
	}
	assert.Equal(t, 2, b.Total) // one group + one thread, both from g_rt_mine
}

// TestResolveTargets_CrossSpaceExternalGroup verifies that a group in a different
// space, that the bot was pulled into as a member, is still resolvable (v2: no
// space filter).
func TestResolveTargets_CrossSpaceExternalGroup(t *testing.T) {
	handler, ctx := setupBotResolveTargets(t)

	// External group lives in space_2; bot is still a member.
	seedGroup(t, ctx, "g_rt_ext", "外部协作", "space_2")
	seedMember(t, ctx, "g_rt_ext", rtRobotID)
	seedThread(t, ctx, "g_rt_ext", "tp_ext", "外部协作", 1)

	b := decodeResolveTargets(t, callResolveTargets(t, handler, rtBotToken, "name=外部协作"))
	assert.Equal(t, 2, b.Total)
	for _, cand := range b.Candidates {
		assert.Equal(t, "g_rt_ext", cand.GroupNo)
	}
}

// TestResolveTargets_KindFilter verifies kind=group and kind=thread filters.
func TestResolveTargets_KindFilter(t *testing.T) {
	handler, ctx := setupBotResolveTargets(t)

	seedGroup(t, ctx, "g_rt_k", "客服", "space_1")
	seedMember(t, ctx, "g_rt_k", rtRobotID)
	seedThread(t, ctx, "g_rt_k", "tp_k", "客服", 1)

	gOnly := decodeResolveTargets(t, callResolveTargets(t, handler, rtBotToken, "name=客服&kind=group"))
	assert.Equal(t, 1, gOnly.Total)
	assert.Equal(t, resolveKindGroup, gOnly.Candidates[0].Kind)

	tOnly := decodeResolveTargets(t, callResolveTargets(t, handler, rtBotToken, "name=客服&kind=thread"))
	assert.Equal(t, 1, tOnly.Total)
	assert.Equal(t, resolveKindThread, tOnly.Candidates[0].Kind)
}

// TestResolveTargets_InvalidKind rejects an unknown kind value.
func TestResolveTargets_InvalidKind(t *testing.T) {
	handler, _ := setupBotResolveTargets(t)
	w := callResolveTargets(t, handler, rtBotToken, "name=x&kind=bogus")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestResolveTargets_ExactMatchFirst verifies an exact name match sorts ahead of
// a partial (wildcard) match.
func TestResolveTargets_ExactMatchFirst(t *testing.T) {
	handler, ctx := setupBotResolveTargets(t)

	seedGroup(t, ctx, "g_rt_e1", "产研中心", "space_1")
	seedMember(t, ctx, "g_rt_e1", rtRobotID)
	seedGroup(t, ctx, "g_rt_e2", "产研", "space_1")
	seedMember(t, ctx, "g_rt_e2", rtRobotID)

	b := decodeResolveTargets(t, callResolveTargets(t, handler, rtBotToken, "name=产研&kind=group"))
	assert.Equal(t, 2, b.Total)
	assert.Equal(t, "产研", b.Candidates[0].Name, "exact match must sort first")
}

// TestResolveTargets_LimitClampAndTruncated verifies limit>50 clamps to 50, and
// that filling the group limit still returns the thread and sets truncated.
func TestResolveTargets_LimitClampAndTruncated(t *testing.T) {
	handler, ctx := setupBotResolveTargets(t)

	// 3 groups + 3 threads matching "区", limit=2 → each class returns 2,
	// both classes truncated.
	for i, gn := range []string{"g_rt_l1", "g_rt_l2", "g_rt_l3"} {
		seedGroup(t, ctx, gn, "区"+string(rune('A'+i)), "space_1")
		seedMember(t, ctx, gn, rtRobotID)
		seedThread(t, ctx, gn, "tp_l"+string(rune('1'+i)), "区"+string(rune('X'+i)), 1)
	}

	b := decodeResolveTargets(t, callResolveTargets(t, handler, rtBotToken, "name=区&limit=2"))
	groupCount, threadCount := 0, 0
	for _, cand := range b.Candidates {
		if cand.Kind == resolveKindGroup {
			groupCount++
		} else {
			threadCount++
		}
	}
	assert.Equal(t, 2, groupCount, "group class clamped to limit")
	assert.Equal(t, 2, threadCount, "thread class clamped to limit, not squeezed out by groups")
	assert.True(t, b.Truncated)
	assert.Equal(t, 4, b.Total)
}

// TestResolveTargets_GroupFullStillReturnsThread is the BLOCKER#4 guard: a full
// group class must not squeeze out the (only) matching thread.
func TestResolveTargets_GroupFullStillReturnsThread(t *testing.T) {
	handler, ctx := setupBotResolveTargets(t)

	// 2 matching groups, limit=1 → group class fills and truncates; the single
	// matching thread must still come back.
	seedGroup(t, ctx, "g_rt_f1", "同名", "space_1")
	seedMember(t, ctx, "g_rt_f1", rtRobotID)
	seedGroup(t, ctx, "g_rt_f2", "同名", "space_1")
	seedMember(t, ctx, "g_rt_f2", rtRobotID)
	seedThread(t, ctx, "g_rt_f1", "tp_f1", "同名", 1)

	b := decodeResolveTargets(t, callResolveTargets(t, handler, rtBotToken, "name=同名&limit=1"))
	groupCount, threadCount := 0, 0
	for _, cand := range b.Candidates {
		if cand.Kind == resolveKindGroup {
			groupCount++
		} else {
			threadCount++
		}
	}
	assert.Equal(t, 1, groupCount)
	assert.Equal(t, 1, threadCount, "thread must not be squeezed out by a full group class")
	assert.True(t, b.Truncated)
}

// TestResolveTargets_WildcardEscaped verifies % and _ in the name are treated as
// literals, not LIKE wildcards.
func TestResolveTargets_WildcardEscaped(t *testing.T) {
	handler, ctx := setupBotResolveTargets(t)

	seedGroup(t, ctx, "g_rt_w1", "50%达成", "space_1")
	seedMember(t, ctx, "g_rt_w1", rtRobotID)
	// A group that would match if % were a wildcard but must NOT match literally.
	seedGroup(t, ctx, "g_rt_w2", "50abc达成", "space_1")
	seedMember(t, ctx, "g_rt_w2", rtRobotID)

	b := decodeResolveTargets(t, callResolveTargets(t, handler, rtBotToken, "name=50%25&kind=group"))
	assert.Equal(t, 1, b.Total, "%% must be matched literally, not as a wildcard")
	assert.Equal(t, "50%达成", b.Candidates[0].Name)
}

// TestResolveTargets_EmptyName returns 400 for a blank name.
func TestResolveTargets_EmptyName(t *testing.T) {
	handler, _ := setupBotResolveTargets(t)
	w := callResolveTargets(t, handler, rtBotToken, "name=%20%20")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestResolveTargets_ArchivedThreadVisibleDeletedHidden verifies status IN (1,2)
// — archived (2) is searchable, deleted (3) is not.
func TestResolveTargets_ArchivedThreadVisibleDeletedHidden(t *testing.T) {
	handler, ctx := setupBotResolveTargets(t)

	seedGroup(t, ctx, "g_rt_s", "归档群", "space_1")
	seedMember(t, ctx, "g_rt_s", rtRobotID)
	seedThread(t, ctx, "g_rt_s", "tp_active", "状态区", 1)
	seedThread(t, ctx, "g_rt_s", "tp_archived", "状态区", 2)
	seedThread(t, ctx, "g_rt_s", "tp_deleted", "状态区", 3)

	b := decodeResolveTargets(t, callResolveTargets(t, handler, rtBotToken, "name=状态区&kind=thread"))
	shortIDs := map[string]bool{}
	for _, cand := range b.Candidates {
		shortIDs[cand.ShortID] = true
	}
	assert.True(t, shortIDs["tp_active"], "active thread must be visible")
	assert.True(t, shortIDs["tp_archived"], "archived thread must be visible")
	assert.False(t, shortIDs["tp_deleted"], "deleted thread must be hidden")
	assert.Equal(t, 2, b.Total)
}

// TestResolveTargets_AppBotReturnsEmpty verifies an App Bot (no group_member
// rows) resolves to an empty candidate set rather than being rejected.
func TestResolveTargets_AppBotReturnsEmpty(t *testing.T) {
	handler, ctx := setupBotResolveTargets(t)

	const appToken = "app_rt_token_1"
	// Register the App Bot in the in-memory registry so authBot resolves it via
	// the O(1) path without needing the app_bot table in the test schema.
	reg := NewAppBotRegistryAdapter()
	reg.Add(appToken, &AppBotRegistrySpec{UID: "appbot_rt_uid", Scope: "platform"})
	prev := GetAppBotRegistry()
	SetAppBotRegistry(reg)
	t.Cleanup(func() {
		if prev != nil {
			SetAppBotRegistry(prev)
		} else {
			SetAppBotRegistry(NewAppBotRegistryAdapter())
		}
	})

	// Seed a group with a matching name but no app-bot membership.
	seedGroup(t, ctx, "g_rt_app", "应用群", "space_1")
	seedThread(t, ctx, "g_rt_app", "tp_app", "应用群", 1)

	b := decodeResolveTargets(t, callResolveTargets(t, handler, appToken, "name=应用群"))
	assert.Equal(t, 0, b.Total)
	assert.Empty(t, b.Candidates)
	assert.False(t, b.Truncated)
}
