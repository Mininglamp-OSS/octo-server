package group

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Space 成员被移除后的群级联。用录制桩替掉 WuKongIM：这里要验证的是
// 「哪些群被退、群主怎么交接、幂等性」，这些都落在 MySQL 上可断言，
// IM 侧只需确认退订阅确实发出去了。

type groupIMStub struct {
	server *httptest.Server

	mu                sync.Mutex
	subscriberRemoves []subscriberRemoveCall
}

type subscriberRemoveCall struct {
	ChannelID   string   `json:"channel_id"`
	ChannelType uint8    `json:"channel_type"`
	Subscribers []string `json:"subscribers"`
}

func newGroupIMStub(t *testing.T, ctx *config.Context) *groupIMStub {
	t.Helper()
	stub := &groupIMStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/channel/subscriber_remove", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call subscriberRemoveCall
		_ = json.Unmarshal(body, &call)
		stub.mu.Lock()
		stub.subscriberRemoves = append(stub.subscriberRemoves, call)
		stub.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	stub.server = httptest.NewServer(mux)

	cfg := ctx.GetConfig()
	previous := cfg.WuKongIM.APIURL
	cfg.WuKongIM.APIURL = stub.server.URL
	t.Cleanup(func() {
		cfg.WuKongIM.APIURL = previous
		stub.server.Close()
	})
	return stub
}

func (s *groupIMStub) unsubscribed(groupNo string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, call := range s.subscriberRemoves {
		if call.ChannelID == groupNo {
			out = append(out, call.Subscribers...)
		}
	}
	return out
}

// ---------- fixture ----------

func cascadeSetup(t *testing.T) (*config.Context, *Group) {
	t.Helper()
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	return ctx, New(ctx)
}

func seedGroupInSpace(t *testing.T, ctx *config.Context, groupNo, spaceID, creator string) {
	t.Helper()
	_, err := ctx.DB().Exec(
		"INSERT INTO `group` (group_no, name, creator, status, space_id, created_at, updated_at) VALUES (?, ?, ?, 1, ?, NOW(), NOW())",
		groupNo, groupNo, creator, spaceID)
	require.NoError(t, err)
}

func seedGroupMember(t *testing.T, ctx *config.Context, groupNo, uid string, role int) {
	t.Helper()
	_, err := ctx.DB().Exec(
		"INSERT INTO group_member (group_no, uid, role, is_deleted, version, created_at, updated_at) VALUES (?, ?, ?, 0, 1, NOW(), NOW())",
		groupNo, uid, role)
	require.NoError(t, err)
}

func liveMemberRole(t *testing.T, ctx *config.Context, groupNo, uid string) (int, bool) {
	t.Helper()
	var roles []int
	_, err := ctx.DB().SelectBySql(
		"SELECT role FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0", groupNo, uid).Load(&roles)
	require.NoError(t, err)
	if len(roles) == 0 {
		return 0, false
	}
	return roles[0], true
}

// ---------- 用例 ----------

// TestGroupCascadeRemovesMemberFromSpaceGroupsOnly 只清理本 Space 下的群，
// 别的 Space 的群必须原封不动 —— 越界会造成跨 Space 的数据破坏。
func TestGroupCascadeRemovesMemberFromSpaceGroupsOnly(t *testing.T) {
	ctx, g := cascadeSetup(t)
	newGroupIMStub(t, ctx)
	const spaceA, spaceB, victim = "sp-a", "sp-b", "u-victim"

	seedGroupInSpace(t, ctx, "g-a1", spaceA, "u-owner")
	seedGroupMember(t, ctx, "g-a1", "u-owner", MemberRoleCreator)
	seedGroupMember(t, ctx, "g-a1", victim, MemberRoleCommon)

	seedGroupInSpace(t, ctx, "g-b1", spaceB, "u-owner")
	seedGroupMember(t, ctx, "g-b1", "u-owner", MemberRoleCreator)
	seedGroupMember(t, ctx, "g-b1", victim, MemberRoleCommon)

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceA, UID: victim, OperatorUID: "u-owner", Reason: spacemod.MemberRemoveReasonKicked,
	}))

	_, inA := liveMemberRole(t, ctx, "g-a1", victim)
	assert.False(t, inA, "本 Space 的群必须退出")
	_, inB := liveMemberRole(t, ctx, "g-b1", victim)
	assert.True(t, inB, "其它 Space 的群绝不能被牵连")
}

// TestGroupCascadeUnsubscribesFromIM 退群必须同时摘掉 WuKongIM 订阅，
// 否则人虽然不在成员表里，仍然收得到群消息。
func TestGroupCascadeUnsubscribesFromIM(t *testing.T) {
	ctx, g := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)
	const spaceID, victim = "sp-unsub", "u-victim"

	seedGroupInSpace(t, ctx, "g-unsub", spaceID, "u-owner")
	seedGroupMember(t, ctx, "g-unsub", "u-owner", MemberRoleCreator)
	seedGroupMember(t, ctx, "g-unsub", victim, MemberRoleCommon)

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: victim, OperatorUID: "u-owner", Reason: spacemod.MemberRemoveReasonKicked,
	}))

	assert.Contains(t, stub.unsubscribed("g-unsub"), victim)
}

// TestGroupCascadeTransfersCreator 被移除者是群主时先交接再移除。
// RemoveGroupMembers 会静默跳过 creator，不交接就等于人根本没被清出去。
func TestGroupCascadeTransfersCreator(t *testing.T) {
	ctx, g := cascadeSetup(t)
	newGroupIMStub(t, ctx)
	const spaceID, victim, successor = "sp-transfer", "u-creator", "u-second"

	seedGroupInSpace(t, ctx, "g-transfer", spaceID, victim)
	seedGroupMember(t, ctx, "g-transfer", victim, MemberRoleCreator)
	seedGroupMember(t, ctx, "g-transfer", successor, MemberRoleCommon)

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: victim, OperatorUID: "su", Reason: spacemod.MemberRemoveReasonForceRemoved,
	}))

	_, stillIn := liveMemberRole(t, ctx, "g-transfer", victim)
	assert.False(t, stillIn, "群主也必须被清出去")
	role, ok := liveMemberRole(t, ctx, "g-transfer", successor)
	require.True(t, ok, "继任者必须还在群里")
	assert.Equal(t, MemberRoleCreator, role, "第二元老必须被提升为群主")
}

// TestGroupCascadeRemovesLoneCreator 群里只剩群主一个人时没有继任者，
// 仍然要把他清出去（降级后再移除），否则人永远留在群里。
func TestGroupCascadeRemovesLoneCreator(t *testing.T) {
	ctx, g := cascadeSetup(t)
	newGroupIMStub(t, ctx)
	const spaceID, victim = "sp-lone", "u-lone"

	seedGroupInSpace(t, ctx, "g-lone", spaceID, victim)
	seedGroupMember(t, ctx, "g-lone", victim, MemberRoleCreator)

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: victim, OperatorUID: "su", Reason: spacemod.MemberRemoveReasonForceRemoved,
	}))

	_, stillIn := liveMemberRole(t, ctx, "g-lone", victim)
	assert.False(t, stillIn, "无继任者也必须把群主清出去，群成为无主空群")
}

// TestGroupCascadeSkipsDisbandedGroup 已解散的群没有可清理的东西，
// 且 RemoveGroupMembers 对解散群直接报错——必须提前跳过，否则整条工单会一直重试。
func TestGroupCascadeSkipsDisbandedGroup(t *testing.T) {
	ctx, g := cascadeSetup(t)
	newGroupIMStub(t, ctx)
	const spaceID, victim = "sp-disband", "u-victim"

	seedGroupInSpace(t, ctx, "g-disband", spaceID, "u-owner")
	seedGroupMember(t, ctx, "g-disband", "u-owner", MemberRoleCreator)
	seedGroupMember(t, ctx, "g-disband", victim, MemberRoleCommon)
	_, err := ctx.DB().Exec("UPDATE `group` SET status=? WHERE group_no=?", GroupStatusDisband, "g-disband")
	require.NoError(t, err)

	assert.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: victim, OperatorUID: "u-owner", Reason: spacemod.MemberRemoveReasonKicked,
	}), "解散群必须被静默跳过，不能变成永久失败")
}

// TestGroupCascadeIsIdempotent 工单失败会整条重跑，已完成的群必须是 no-op。
func TestGroupCascadeIsIdempotent(t *testing.T) {
	ctx, g := cascadeSetup(t)
	newGroupIMStub(t, ctx)
	const spaceID, victim = "sp-idem", "u-victim"

	seedGroupInSpace(t, ctx, "g-idem", spaceID, "u-owner")
	seedGroupMember(t, ctx, "g-idem", "u-owner", MemberRoleCreator)
	seedGroupMember(t, ctx, "g-idem", victim, MemberRoleCommon)

	removal := spacemod.MemberRemoval{
		SpaceID: spaceID, UID: victim, OperatorUID: "u-owner", Reason: spacemod.MemberRemoveReasonKicked,
	}
	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, removal))
	// 第二遍必须干净返回，且不改变群主归属
	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, removal))

	role, ok := liveMemberRole(t, ctx, "g-idem", "u-owner")
	require.True(t, ok)
	assert.Equal(t, MemberRoleCreator, role, "重跑不得动到留下来的群主")
}

// TestGroupCascadeNoGroupsIsNoop 成员在本 Space 下没有任何群时安静返回。
func TestGroupCascadeNoGroupsIsNoop(t *testing.T) {
	ctx, g := cascadeSetup(t)
	newGroupIMStub(t, ctx)
	assert.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: "sp-empty", UID: "nobody", OperatorUID: "op", Reason: spacemod.MemberRemoveReasonLeft,
	}))
}
