package group

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	sentMessages      []map[string]interface{}
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
	// 必须单独记录 /message/send：系统 Tip 走的是它，若落进下面的 catch-all，
	// 「解散 / 自助退出时抑制被移出文案」这条行为就完全没有断言，改回去也不会红。
	mux.HandleFunc("/message/send", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		_ = json.Unmarshal(body, &payload)
		stub.mu.Lock()
		stub.sentMessages = append(stub.sentMessages, payload)
		stub.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
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

// sentContentTypes 返回所有被发出的系统消息的 content type。
// 群成员移除提示是 MessageContentTypeRemoveMembers(1003)，退群提示是 GroupExit。
func (s *groupIMStub) sentPayloads() []map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]interface{}, len(s.sentMessages))
	copy(out, s.sentMessages)
	return out
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

// TestGroupCascadeSelfExitSuppressesRemovedNotice 自助退出 Space（reason=left）时
// 操作者就是本人，默认的「被 X 移出群聊」会渲染成「X 被 X 移出群聊」。
// 断言此时不发被移出消息（改发退群提示），其余清理照常。
func TestGroupCascadeSelfExitSuppressesRemovedNotice(t *testing.T) {
	ctx, g := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)
	const spaceID, leaver = "sp-selfexit", "u-leaver"

	seedGroupInSpace(t, ctx, "g-selfexit", spaceID, "u-owner")
	seedGroupMember(t, ctx, "g-selfexit", "u-owner", MemberRoleCreator)
	seedGroupMember(t, ctx, "g-selfexit", leaver, MemberRoleCommon)

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: leaver,
		OperatorUID: leaver, // 自助退出：操作者就是本人
		Reason:      spacemod.MemberRemoveReasonLeft,
	}))

	// 清理照做
	_, stillIn := liveMemberRole(t, ctx, "g-selfexit", leaver)
	assert.False(t, stillIn)
	assert.Contains(t, stub.unsubscribed("g-selfexit"), leaver)

	// 关键断言：不得出现「被移出」文案，而应出现「退出群聊」文案。
	// 少了这一条，把 SuppressRemoveNotice 改回 false 测试照样绿。
	assert.False(t, payloadsContain(stub.sentPayloads(), "移除群聊"),
		"自助退出不得渲染成「你被 X 移除群聊」")
	assert.True(t, payloadsContain(stub.sentPayloads(), "退出群聊"),
		"应改发退群提示")
}

// TestGroupCascadeDisbandSuppressesPerMemberNotice 解散不会解散群，
// 于是每个成员在每个群里各触发一次移除。N 成员 × M 群条系统消息全堆给最后
// 被移除的人看，且空间已经没了，逐个通告毫无意义——必须抑制。
func TestGroupCascadeDisbandSuppressesPerMemberNotice(t *testing.T) {
	ctx, g := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)
	const spaceID, victim = "sp-disbandnotice", "u-victim"

	seedGroupInSpace(t, ctx, "g-disbandnotice", spaceID, "u-owner")
	seedGroupMember(t, ctx, "g-disbandnotice", "u-owner", MemberRoleCreator)
	seedGroupMember(t, ctx, "g-disbandnotice", victim, MemberRoleCommon)

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: victim, OperatorUID: "su",
		Reason: spacemod.MemberRemoveReasonSpaceDisbanded,
	}))

	_, stillIn := liveMemberRole(t, ctx, "g-disbandnotice", victim)
	assert.False(t, stillIn, "清理本身照做")
	assert.False(t, payloadsContain(stub.sentPayloads(), "移除群聊"),
		"解散场景不得逐成员逐群发被移出消息")
}

// TestGroupCascadeKickStillNotifies 反向保证：正常踢人仍要发被移出消息，
// 抑制逻辑不能把它一并关掉。
func TestGroupCascadeKickStillNotifies(t *testing.T) {
	ctx, g := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)
	const spaceID, victim = "sp-kicknotice", "u-victim"

	seedGroupInSpace(t, ctx, "g-kicknotice", spaceID, "u-owner")
	seedGroupMember(t, ctx, "g-kicknotice", "u-owner", MemberRoleCreator)
	seedGroupMember(t, ctx, "g-kicknotice", victim, MemberRoleCommon)

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: victim, OperatorUID: "u-owner",
		Reason: spacemod.MemberRemoveReasonKicked,
	}))

	assert.True(t, payloadsContain(stub.sentPayloads(), "移除群聊"),
		"被踢出 Space 导致的退群仍要在群里告知")
}

// payloadsContain 在录到的系统消息里找文案片段。
// SendGroupMemberBeRemove 的 content 是「你被{0}移除群聊」，
// SendGroupExit 的是「“{0}“退出群聊」。
func payloadsContain(payloads []map[string]interface{}, fragment string) bool {
	for _, p := range payloads {
		raw, ok := p["payload"]
		if !ok {
			continue
		}
		if decoded, err := base64.StdEncoding.DecodeString(fmt.Sprint(raw)); err == nil {
			if strings.Contains(string(decoded), fragment) {
				return true
			}
		}
		if strings.Contains(fmt.Sprint(raw), fragment) {
			return true
		}
	}
	return false
}

// TestGroupCascadeConcurrentCreatorAndSuccessor 阻塞项回归（PR #795 review）：
// 群主 C 与第二元老 S2 同批被移出 Space，两条工单可能在不同副本上并发执行。
//
// 两个失败分支都出自同一个无锁读窗口：
//   - 读落在交接提交**前** → S2 被当成普通成员通过过滤，而 DeleteMemberTx 没有
//     角色守卫 → 刚上任的群主被删掉，群里还剩着人却无主，且无人重新选主；
//   - 读落在交接提交**后** → S2 被识别为 creator 被静默跳过、返回 nil error →
//     工单标 done，人却永久留在群里。
//
// 修复后：删除前在事务内行锁重读角色，且调用方检查 Removed 计数并让工单重试。
// 断言最终态：C 和 S2 都不在群里，剩下的 C3 是唯一群主。
func TestGroupCascadeConcurrentCreatorAndSuccessor(t *testing.T) {
	ctx, g := cascadeSetup(t)
	newGroupIMStub(t, ctx)
	const spaceID, creator, second, third = "sp-concurrent", "u-creator", "u-second", "u-third"

	seedGroupInSpace(t, ctx, "g-concurrent", spaceID, creator)
	seedGroupMember(t, ctx, "g-concurrent", creator, MemberRoleCreator)
	seedGroupMember(t, ctx, "g-concurrent", second, MemberRoleCommon)
	seedGroupMember(t, ctx, "g-concurrent", third, MemberRoleCommon)

	run := func(uid string) error {
		return g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
			SpaceID: spaceID, UID: uid, OperatorUID: "su",
			Reason: spacemod.MemberRemoveReasonSpaceDisbanded,
		})
	}

	// 并发跑两条工单，模拟两个副本同时认领
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = run(creator) }()
	go func() { defer wg.Done(); errs[1] = run(second) }()
	wg.Wait()

	// 任一条失败都代表「并发导致这次没removed」，工单会重试——重跑一遍即可收敛。
	for i, uid := range []string{creator, second} {
		if errs[i] != nil {
			require.NoError(t, run(uid), "重试必须收敛：%s", uid)
		}
	}

	_, creatorIn := liveMemberRole(t, ctx, "g-concurrent", creator)
	assert.False(t, creatorIn, "群主必须被清出去")
	_, secondIn := liveMemberRole(t, ctx, "g-concurrent", second)
	assert.False(t, secondIn, "第二元老也被移出 Space，不得被留在群里")

	// 剩下的成员必须恰好有一个群主——既不能无主，也不能出现两个
	var creatorCount int
	_, err := ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM group_member WHERE group_no=? AND is_deleted=0 AND role=?",
		"g-concurrent", MemberRoleCreator).Load(&creatorCount)
	require.NoError(t, err)
	assert.Equal(t, 1, creatorCount, "群里必须恰好一个群主，不能无主也不能双主")

	role, thirdIn := liveMemberRole(t, ctx, "g-concurrent", third)
	require.True(t, thirdIn, "留下来的成员必须还在群里")
	assert.Equal(t, MemberRoleCreator, role, "群主应落到唯一剩下的成员身上")
}

// TestGroupCascadeRetriesWhenTargetBecameCreator 单副本也能触发：
// 群主转让接口在「无锁读角色」和「调用 RemoveGroupMembers」之间把目标提升为群主。
// RemoveGroupMembers 对群主是静默跳过 + nil error，不检查 Removed 就会把工单
// 标成 done，人却永久留在群里。这里直接构造那个中间态。
func TestGroupCascadeRetriesWhenTargetBecameCreator(t *testing.T) {
	ctx, g := cascadeSetup(t)
	newGroupIMStub(t, ctx)
	const spaceID, victim = "sp-became-creator", "u-victim"

	seedGroupInSpace(t, ctx, "g-became", spaceID, "u-owner")
	seedGroupMember(t, ctx, "g-became", "u-owner", MemberRoleCreator)
	seedGroupMember(t, ctx, "g-became", victim, MemberRoleCommon)
	// 让群里出现两个 creator 是不可能的中间态，所以改成：原群主先离场，
	// victim 被提升为 creator —— 这正是并发交接后 victim 的状态。
	_, err := ctx.DB().Exec(
		"UPDATE group_member SET is_deleted=1 WHERE group_no=? AND uid=?", "g-became", "u-owner")
	require.NoError(t, err)
	_, err = ctx.DB().Exec(
		"UPDATE group_member SET role=? WHERE group_no=? AND uid=?",
		MemberRoleCreator, "g-became", victim)
	require.NoError(t, err)

	// 此时 victim 是群主：级联必须先交接（无继任者则降级）再移除，最终人不在群里
	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: victim, OperatorUID: "su", Reason: spacemod.MemberRemoveReasonForceRemoved,
	}))

	_, stillIn := liveMemberRole(t, ctx, "g-became", victim)
	assert.False(t, stillIn, "已成为群主的目标也必须被清出去，不能静默跳过")
}
