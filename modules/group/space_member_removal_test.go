package group

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
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
	failFragment      string // 非空时，content 含该片段的下一条 /message/send 返回 500
}

// failSendOnce 让 content 含 fragment 的下一条消息发送失败一次，用于验证 best-effort。
func (s *groupIMStub) failSendOnce(fragment string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failFragment = fragment
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
		shouldFail := false
		if stub.failFragment != "" {
			if raw, ok := payload["payload"]; ok {
				if decoded, err := base64.StdEncoding.DecodeString(fmt.Sprint(raw)); err == nil &&
					strings.Contains(string(decoded), stub.failFragment) {
					shouldFail = true
					stub.failFragment = "" // 只失败一次
				}
			}
		}
		if !shouldFail {
			stub.sentMessages = append(stub.sentMessages, payload)
		}
		stub.mu.Unlock()
		if shouldFail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
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

// seedGroupMemberWithRemark 带群内备注的成员。remark 是本仓库展示名的第一优先级，
// 不带 remark 的 seedGroupMember 覆盖不到那条分支。
func seedGroupMemberWithRemark(t *testing.T, ctx *config.Context, groupNo, uid string, role int, remark string) {
	t.Helper()
	_, err := ctx.DB().Exec(
		"INSERT INTO group_member (group_no, uid, role, remark, is_deleted, version, created_at, updated_at) VALUES (?, ?, ?, ?, 0, 1, NOW(), NOW())",
		groupNo, uid, role, remark)
	require.NoError(t, err)
}

// seedUser 写一条 user 行，让 resolveDisplayName 能查到名字。
// 不写的话它一路回落到 uid，名字相关的断言就是在跟 uid 较劲，永远发现不了传错参数。
func seedUser(t *testing.T, ctx *config.Context, uid, name string) {
	t.Helper()
	// short_no 必须显式给且唯一：列上有 short_no_udx 唯一索引且默认值是 ''，
	// 不填的话第二个用户就撞 `Duplicate entry '' for key 'user.short_no_udx'`。
	_, err := ctx.DB().Exec(
		"INSERT INTO `user` (uid, name, username, short_no, password, status, created_at, updated_at) VALUES (?, ?, ?, ?, '', 1, NOW(), NOW())",
		uid, name, uid, uid)
	require.NoError(t, err)
}

// countGroupVisible 数**全群可见**且含指定文案的消息条数。
// 布尔式的 contains 对「同一条被发了 N 遍」毫无意见，而按成员数重复发正是
// 这类广播最容易犯的错。
func countGroupVisible(payloads []map[string]interface{}, fragment string) int {
	n := 0
	for _, p := range payloads {
		if payloadsContainGroupVisible([]map[string]interface{}{p}, fragment) {
			n++
		}
	}
	return n
}

// countGroupVisibleTips 数**全群可见且带可见文案**的消息条数。
//
// 为什么不能用 countGroupVisible(片段) 来断言「一条都不该有」：那个按 content 片段
// 匹配，而名字可能根本不在 content 里（SendGroupTransferGrouper 把名字放 extra、
// content 只有「“{0}”已成为新群主」）。用片段断言 0 条时，任何把名字挪进 extra 的
// 广播都会静默漏过——这正是变异检验抓到的洞。
//
// 排除 CMD：CMDGroupMemberUpdate 走同一个 /message/send，也没有收件人裁剪，
// 但它 type=99、没有 content，是静默刷新而非用户可见消息。
func countGroupVisibleTips(payloads []map[string]interface{}) int {
	n := 0
	for _, p := range payloads {
		if subs, ok := p["subscribers"]; ok && subs != nil {
			if list, isList := subs.([]interface{}); !isList || len(list) > 0 {
				continue
			}
		}
		raw, ok := p["payload"]
		if !ok {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(fmt.Sprint(raw))
		if err != nil {
			continue
		}
		var inner map[string]interface{}
		if json.Unmarshal(decoded, &inner) != nil {
			continue
		}
		if vis, ok := inner["visibles"]; ok && vis != nil {
			if list, isList := vis.([]interface{}); !isList || len(list) > 0 {
				continue
			}
		}
		if content, ok := inner["content"]; ok && content != nil && fmt.Sprint(content) != "" {
			n++
		}
	}
	return n
}

// payloadExtraHasUID / payloadExtraHasName 检查某条消息的 extra 里带的是谁。
//
// SendGroupTransferGrouper 的 content 是「“{0}”已成为新群主」——名字**不在** content
// 里，而在 extra[0]，由客户端替换占位符。所以断言"是谁成了新群主"必须看 extra；
// 只匹配 content 片段的话，把继任者传成离开者也照样绿。
func payloadExtraField(payloads []map[string]interface{}, fragment, field, want string) bool {
	for _, p := range payloads {
		raw, ok := p["payload"]
		if !ok {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(fmt.Sprint(raw))
		if err != nil {
			continue
		}
		var inner map[string]interface{}
		if json.Unmarshal(decoded, &inner) != nil {
			continue
		}
		if !strings.Contains(fmt.Sprint(inner["content"]), fragment) {
			continue
		}
		extras, _ := inner["extra"].([]interface{})
		for _, e := range extras {
			if m, ok := e.(map[string]interface{}); ok && fmt.Sprint(m[field]) == want {
				return true
			}
		}
	}
	return false
}

// payloadContentType 取含指定文案那条消息的 content type。
// 「与手动转让同一个 content type」是本改动明确主张的契约，此前无任何测试钉住它——
// 改成别的类型，所有断言照样绿（PR #804 review 指出）。
func payloadContentType(payloads []map[string]interface{}, fragment string) int {
	for _, p := range payloads {
		raw, ok := p["payload"]
		if !ok {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(fmt.Sprint(raw))
		if err != nil {
			continue
		}
		var inner map[string]interface{}
		if json.Unmarshal(decoded, &inner) != nil {
			continue
		}
		if !strings.Contains(fmt.Sprint(inner["content"]), fragment) {
			continue
		}
		if f, ok := inner["type"].(float64); ok {
			return int(f)
		}
	}
	return 0
}

func payloadExtraHasUID(payloads []map[string]interface{}, fragment, uid string) bool {
	return payloadExtraField(payloads, fragment, "uid", uid)
}

// payloadExtraUIDAt 取某条消息 extra 里第 idx 个元素的 uid。
// 两占位符的文案里位置是有语义的：{0} 是离开者、{1} 是新群主，两个传反了
// 「谁离开、谁接手」就整个说反，而只断言"两个 uid 都在 extra 里"抓不到。
func payloadExtraUIDAt(payloads []map[string]interface{}, fragment string, idx int) string {
	for _, p := range payloads {
		raw, ok := p["payload"]
		if !ok {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(fmt.Sprint(raw))
		if err != nil {
			continue
		}
		var inner map[string]interface{}
		if json.Unmarshal(decoded, &inner) != nil {
			continue
		}
		if !strings.Contains(fmt.Sprint(inner["content"]), fragment) {
			continue
		}
		extras, _ := inner["extra"].([]interface{})
		if idx >= len(extras) {
			return ""
		}
		if m, ok := extras[idx].(map[string]interface{}); ok {
			return fmt.Sprint(m["uid"])
		}
	}
	return ""
}

// payloadExtraNameAt 取某条消息 extra 里第 idx 个元素的 **name**。
// 位置有语义：{0}=离开者、{1}=新群主。只断言 uid 的位置挡不住「uid 对但 name 传反」——
// 三端客户端把 extra[i].name 渲染进 {i}，name 传反就播出一条完全说反的持久化系统消息
// （PR #804 round-7 review P2-7）。
func payloadExtraNameAt(payloads []map[string]interface{}, fragment string, idx int) string {
	for _, p := range payloads {
		raw, ok := p["payload"]
		if !ok {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(fmt.Sprint(raw))
		if err != nil {
			continue
		}
		var inner map[string]interface{}
		if json.Unmarshal(decoded, &inner) != nil {
			continue
		}
		if !strings.Contains(fmt.Sprint(inner["content"]), fragment) {
			continue
		}
		extras, _ := inner["extra"].([]interface{})
		if idx >= len(extras) {
			return ""
		}
		if m, ok := extras[idx].(map[string]interface{}); ok {
			return fmt.Sprint(m["name"])
		}
	}
	return ""
}

func payloadExtraHasName(payloads []map[string]interface{}, fragment, name string) bool {
	return payloadExtraField(payloads, fragment, "name", name)
}

// seedPendingRemovals 写真实的 pending 清理工单，模拟「这些人同属一次批量移除」。
// 交接通告要靠这张表判断继任者是不是也在待移除队列里，所以测试必须写真表，
// 不能只调 cleanupSpaceMemberGroups 了事。
func seedPendingRemovals(t *testing.T, ctx *config.Context, spaceID string, uids []string, reason string) {
	t.Helper()
	for _, uid := range uids {
		_, err := ctx.DB().Exec(
			"INSERT INTO space_member_removal_cleanup (space_id, uid, operator_uid, reason, status, created_at) "+
				"VALUES (?, ?, 'su', ?, 0, NOW(3))", spaceID, uid, reason)
		require.NoError(t, err)
	}
}

// markRemovalDone 把某人的工单置为终态，模拟 worker 跑完了这一条。
func markRemovalDone(t *testing.T, ctx *config.Context, spaceID, uid string) {
	t.Helper()
	_, err := ctx.DB().Exec(
		"UPDATE space_member_removal_cleanup SET status=1, finished_at=NOW(3) WHERE space_id=? AND uid=?",
		spaceID, uid)
	require.NoError(t, err)
}

// seedActiveSpaceMember 写一个活跃的 space + space_member，模拟「人已经回来了」。
func seedActiveSpaceMember(t *testing.T, ctx *config.Context, spaceID, uid string) {
	t.Helper()
	_, err := ctx.DB().Exec(
		"INSERT INTO space (space_id, name, creator, status, max_users, created_at, updated_at) "+
			"VALUES (?, ?, ?, 1, 1000, NOW(), NOW())", spaceID, spaceID, uid)
	require.NoError(t, err)
	_, err = ctx.DB().Exec(
		"INSERT INTO space_member (space_id, uid, role, status, created_at, updated_at) "+
			"VALUES (?, ?, 0, 1, NOW(), NOW())", spaceID, uid)
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
	// 自助退出走的是既有的 sendGroupExitTip（只给一位管理员看），不得升级成全群广播。
	assert.Equal(t, 0, countGroupVisibleTips(stub.sentPayloads()),
		"自助退出的提示只给一位管理员，不得产生全群可见消息")
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
	// 解散不得产生任何全群可见消息——N×M 的理由对广播同样成立。
	assert.Equal(t, 0, countGroupVisibleTips(stub.sentPayloads()),
		"解散场景不得产生任何带可见文案的全群消息")
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
		"被踢出 Space 导致的退群仍要发被移出通知")
}

// TestGroupCascadeKickSendsNoGroupBroadcast 普通成员被移出 Space 时**不**向全群广播。
//
// 刻意的产品取舍，不是遗漏：「某人走了」在成员列表里看得见，「群主换人了」看不见，
// 只有后者值得一条群消息。反过来做会让两个 200-uid 的批量入口
// （members/remove、管理端 removeMembers）变成消息洪水：200 人 × 50 群 = 一万条
// NoPersist=0 的永久群消息，量级与解散被抑制的理由相同。
//
// 断言用 payloadsContainGroupVisible（不带任何收件人裁剪的消息）而不是
// payloadsContain：被移除者本人仍会收到私人通知，那条**应该**发，不能被这里误伤。
func TestGroupCascadeKickSendsNoGroupBroadcast(t *testing.T) {
	ctx, g := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)
	const spaceID, victim = "sp-kicknobroadcast", "u-victim"

	seedUser(t, ctx, victim, "受害者小明")
	seedGroupInSpace(t, ctx, "g-kicknobroadcast", spaceID, "u-owner")
	seedGroupMember(t, ctx, "g-kicknobroadcast", "u-owner", MemberRoleCreator)
	seedGroupMember(t, ctx, "g-kicknobroadcast", victim, MemberRoleCommon)
	seedGroupMember(t, ctx, "g-kicknobroadcast", "u-bystander", MemberRoleCommon)

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: victim, OperatorUID: "u-owner",
		Reason: spacemod.MemberRemoveReasonKicked,
	}))

	// 清理本身照做
	_, stillIn := liveMemberRole(t, ctx, "g-kicknobroadcast", victim)
	assert.False(t, stillIn)
	assert.Contains(t, stub.unsubscribed("g-kicknobroadcast"), victim)

	assert.Equal(t, 0, countGroupVisibleTips(stub.sentPayloads()),
		"普通成员被移出 Space 不得产生任何带可见文案的全群消息")
	// 私人通知必须还在——它是被移除者知道自己为什么进不去群的唯一来源。
	assert.True(t, payloadsContain(stub.sentPayloads(), "移除群聊"),
		"被移除者本人的私人通知不得被一并砍掉")
}

// TestGroupCascadeCreatorHandoverIsAnnounced 群主被移出时，交接必须在群里通告。
//
// 交接本身 #795 就做了，但是**静默**的：群里凭空多出一个新群主，没有任何消息说明。
// 走的是 octo-lib 既有的 SendGroupTransferGrouper，与手动转让同一个 content type。
func TestGroupCascadeCreatorHandoverIsAnnounced(t *testing.T) {
	ctx, g := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)
	const spaceID, creator, successor = "sp-handover-tip", "u-creator", "u-successor"

	seedUser(t, ctx, creator, "老群主")
	seedUser(t, ctx, successor, "新群主")

	seedGroupInSpace(t, ctx, "g-handover-tip", spaceID, creator)
	seedGroupMember(t, ctx, "g-handover-tip", creator, MemberRoleCreator)
	seedGroupMember(t, ctx, "g-handover-tip", successor, MemberRoleCommon)

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: creator, OperatorUID: "su",
		Reason: spacemod.MemberRemoveReasonForceRemoved,
	}))

	// 交接确实发生
	role, stillIn := liveMemberRole(t, ctx, "g-handover-tip", successor)
	require.True(t, stillIn)
	assert.Equal(t, MemberRoleCreator, role, "第二元老应已被提升为群主")

	// 而且群里被告知了：全群可见（无 subscribers / visibles 裁剪）
	assert.True(t, payloadsContainGroupVisible(stub.sentPayloads(), "已成为新群主"),
		"群主交接必须全群可见，否则群里凭空多出一个新群主")
	// 级联场景要把**原因**一并交代，只说"换了群主"没头没尾。
	assert.True(t, payloadsContainGroupVisible(stub.sentPayloads(), "已离开当前空间"),
		"级联交接要说明是因为原群主离开了空间")
	// 名字走 extra 占位符而非拼进 content，且**位置有语义**：{0} 离开者、{1} 新群主。
	assert.Equal(t, creator, payloadExtraUIDAt(stub.sentPayloads(), "已成为新群主", 0),
		"extra[0] 必须是离开的老群主")
	assert.Equal(t, successor, payloadExtraUIDAt(stub.sentPayloads(), "已成为新群主", 1),
		"extra[1] 必须是继任者")
	// name 也必须按位置对上：只钉 uid 挡不住「name 传反」——三端把 extra[i].name
	// 渲染进 {i}，传反就播出一条说反的持久化系统消息（P2-7）。
	assert.Equal(t, "老群主", payloadExtraNameAt(stub.sentPayloads(), "已成为新群主", 0),
		"extra[0].name 必须是离开者的名字")
	assert.Equal(t, "新群主", payloadExtraNameAt(stub.sentPayloads(), "已成为新群主", 1),
		"extra[1].name 必须是继任者的名字")
	assert.Equal(t, 1, countGroupVisible(stub.sentPayloads(), "已成为新群主"),
		"每群只发一条交接通告")
	// 与手动转让同一个 content type —— 这是本改动明确主张的契约
	assert.Equal(t, int(common.GroupTransferGrouper), payloadContentType(stub.sentPayloads(), "已成为新群主"),
		"必须与手动转让路径用同一个 content type，否则同一件事会渲染成两种样子")
}

// failingRemoveService 让 RemoveGroupMembers 前 N 次调用失败，其余透传给真实实现。
// 内嵌 IService 拿到全部方法，只覆盖要注入故障的那一个。
type failingRemoveService struct {
	IService
	failures int
}

func (f *failingRemoveService) RemoveGroupMembers(req *RemoveGroupMembersServiceReq) (*RemoveGroupMembersServiceResp, error) {
	if f.failures > 0 {
		f.failures--
		return nil, errors.New("injected failure after handover commit")
	}
	return f.IService.RemoveGroupMembers(req)
}

// TestGroupCascadeHandoverAnnouncedOncePerRetry 交接通告在「交接已提交、后续失败重试」下
// 既不丢也不重发。
//
// 这条测试的前一版是**空的**（PR #804 review 实测）：它只是把整条清理跑两遍，而第二遍
// queryGroupsWithMemberUIDAndSpaceID 因 is_deleted=0 过滤已返回空集，根本没再进入
// exitSpaceMemberFromGroup —— 「行锁内看到已非 creator 直接返回」那条路径一次都没执行，
// count==1 在两种实现下都平凡成立。把通告挪回调用方（= 修复前的排布）时它照样绿。
//
// 现在用故障注入还原真实时序：第一轮 handOverGroupCreator 提交并通告，随后
// RemoveGroupMembers 失败 → 整步返回错误 → 工单重试；第二轮离开者角色已是
// MemberRoleCommon（仍在群里），交接分支不再进入。
//
// 这才能区分两种实现：
//   - 在提交处发（现状）：第一轮已发出 → 共 1 条
//   - 在调用方发（旧排布）：第一轮 RemoveGroupMembers 失败、发不到；第二轮不进交接分支
//     → 共 0 条，永久丢失
func TestGroupCascadeHandoverAnnouncedOncePerRetry(t *testing.T) {
	ctx, g := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)
	const spaceID, creator, successor = "sp-handover-retry", "u-creator", "u-successor"

	seedUser(t, ctx, creator, "老群主")
	seedUser(t, ctx, successor, "新群主")
	seedGroupInSpace(t, ctx, "g-handover-retry", spaceID, creator)
	seedGroupMember(t, ctx, "g-handover-retry", creator, MemberRoleCreator)
	seedGroupMember(t, ctx, "g-handover-retry", successor, MemberRoleCommon)

	removal := spacemod.MemberRemoval{
		SpaceID: spaceID, UID: creator, OperatorUID: "su",
		Reason: spacemod.MemberRemoveReasonForceRemoved,
	}

	// 第一轮：交接提交 + 通告，然后移除失败 → 整步报错
	g.groupService = &failingRemoveService{IService: g.groupService, failures: 1}
	require.Error(t, g.cleanupSpaceMemberGroups(ctx, removal),
		"注入的失败必须让整步返回错误，否则不构成重试场景")

	// 交接确实已提交，且离开者仍在群里（这正是重试要面对的中间态）
	role, stillIn := liveMemberRole(t, ctx, "g-handover-retry", successor)
	require.True(t, stillIn)
	require.Equal(t, MemberRoleCreator, role, "交接应已提交")
	leaverRole, leaverIn := liveMemberRole(t, ctx, "g-handover-retry", creator)
	require.True(t, leaverIn, "移除失败，离开者应还在群里")
	require.Equal(t, MemberRoleCommon, leaverRole, "离开者应已被降级")

	// 第二轮（重试）：这次移除成功
	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, removal))
	_, stillInAfter := liveMemberRole(t, ctx, "g-handover-retry", creator)
	assert.False(t, stillInAfter, "重试应完成移除")

	assert.Equal(t, 1, countGroupVisible(stub.sentPayloads(), "已成为新群主"),
		"跨重试恰好一条：不能丢（0）也不能重发（2）")
}

// TestGroupCascadeBatchHandoverAnnouncesOnce 批量移除时，一个群只发一条交接通告。
//
// 回归 PR #804 review 实测出的缺陷：批量移除为每个 uid 各建一条工单
// （逐条是 enqueueMemberRemovalCleanupTx，整批一次是 enqueueMemberRemovalCleanupBatchTx），
// 若被移除的几个人正好是群里连续的元老，交接会沿元老顺序连锁：C→S2、S2→S3、S3→S4，
// 一个群里三条「已成为新群主」，前两条在写下时就已作废。批量入口上限 200 uid。
//
// 这与解散被抑制的机制**完全相同**，只是触发方式不同。
//
// ⚠️ 本用例的前提是「同批工单在任何 worker 起跑前全部可见」，fixture 用循环前一次性
// seed 来建模。这个前提不是假设，它由 modules/space 的
// TestRemoveMembersLockedEnqueuesAtomically 证明——那条用一把竞争行锁把整批卡在中间，
// 断言此刻一条工单都不可见。两条合起来才覆盖真实路径；单看本用例会把前提当结论
// （见 learnings/pending/a-test-can-encode-the-premise.md）。
func TestGroupCascadeBatchHandoverAnnouncesOnce(t *testing.T) {
	ctx, g := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)
	const spaceID, gno = "sp-batch-handover", "g-batch-handover"

	for _, u := range []string{"u-c", "u-s2", "u-s3", "u-s4"} {
		seedUser(t, ctx, u, "名字"+u)
	}
	seedGroupInSpace(t, ctx, gno, spaceID, "u-c")
	seedGroupMember(t, ctx, gno, "u-c", MemberRoleCreator)
	// 逐条插入拉开 created_at，保证元老顺序确定（选主按 created_at ASC）
	for _, u := range []string{"u-s2", "u-s3", "u-s4"} {
		seedGroupMember(t, ctx, gno, u, MemberRoleCommon)
	}

	// 一次 3 人的批量移除：三条工单，且三人是元老前三
	batch := []string{"u-c", "u-s2", "u-s3"}
	seedPendingRemovals(t, ctx, spaceID, batch, spacemod.MemberRemoveReasonForceRemoved)
	for _, uid := range batch {
		require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
			SpaceID: spaceID, UID: uid, OperatorUID: "su",
			Reason: spacemod.MemberRemoveReasonForceRemoved,
		}))
		markRemovalDone(t, ctx, spaceID, uid)
	}

	// 群主最终落到 u-s4
	role, stillIn := liveMemberRole(t, ctx, gno, "u-s4")
	require.True(t, stillIn)
	assert.Equal(t, MemberRoleCreator, role, "群主应最终落到唯一留下的人")

	assert.Equal(t, 1, countGroupVisible(stub.sentPayloads(), "已成为新群主"),
		"一个群只发一条交接通告，链条中间那些当场作废的不发")
	// 发出的那条必须是**最终**结果，不是中间态
	assert.Equal(t, "u-s4", payloadExtraUIDAt(stub.sentPayloads(), "已成为新群主", 1),
		"通告里的新群主必须是最终群主")
}

// TestGroupCascadeHandoverPrefersGroupRemark 交接通告用群内 remark 而非全局名。
// 继任者那一行是交接事务里 FOR UPDATE 选出来的，Remark 直接可用。
func TestGroupCascadeHandoverPrefersGroupRemark(t *testing.T) {
	ctx, g := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)
	const spaceID, creator, successor = "sp-handover-remark", "u-creator", "u-successor"

	seedUser(t, ctx, successor, "全局大名")
	seedGroupInSpace(t, ctx, "g-handover-remark", spaceID, creator)
	seedGroupMember(t, ctx, "g-handover-remark", creator, MemberRoleCreator)
	seedGroupMemberWithRemark(t, ctx, "g-handover-remark", successor, MemberRoleCommon, "群里叫我老李")

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: creator, OperatorUID: "su",
		Reason: spacemod.MemberRemoveReasonForceRemoved,
	}))

	assert.True(t, payloadExtraHasName(stub.sentPayloads(), "已成为新群主", "群里叫我老李"),
		"应使用群内 remark")
	assert.False(t, payloadExtraHasName(stub.sentPayloads(), "已成为新群主", "全局大名"),
		"不得回落到全局 user.name")
}

// TestGroupCascadeHandoverLeaverPrefersGroupRemark 通告里**离开者**那一侧也用群内 remark。
//
// 补 PR #804 review 的 A1：继任者那侧有守卫（上一条测试），离开者那侧没有——
// 把 displayName 换成全局名，27 条 cascade 测试全绿。extra[0] 和 extra[1] 走的是
// 两条不同的解析路径（前者来自 exitSpaceMemberFromGroup 的 member.Remark，后者来自
// 交接事务里 FOR UPDATE 选出的 successor.Remark），钉住一侧不代表另一侧也对。
func TestGroupCascadeHandoverLeaverPrefersGroupRemark(t *testing.T) {
	ctx, g := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)
	const spaceID, creator, successor = "sp-handover-leaver-remark", "u-creator", "u-successor"

	seedUser(t, ctx, creator, "离开者全局名")
	seedUser(t, ctx, successor, "继任者全局名")
	seedGroupInSpace(t, ctx, "g-handover-leaver-remark", spaceID, creator)
	seedGroupMemberWithRemark(t, ctx, "g-handover-leaver-remark", creator, MemberRoleCreator, "离开者群内名")
	seedGroupMember(t, ctx, "g-handover-leaver-remark", successor, MemberRoleCommon)

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: creator, OperatorUID: "su",
		Reason: spacemod.MemberRemoveReasonForceRemoved,
	}))

	assert.True(t, payloadExtraHasName(stub.sentPayloads(), "已成为新群主", "离开者群内名"),
		"extra[0] 应使用离开者的群内 remark")
	assert.False(t, payloadExtraHasName(stub.sentPayloads(), "已成为新群主", "离开者全局名"),
		"extra[0] 不得回落到全局 user.name")
}

// TestGroupCascadeSelfExitTipPrefersGroupRemark 自助退群提示也用群内 remark。
//
// 这是 PR #804 review 指出的**越界改动**：把展示名解析改成 remark 优先时，
// sendGroupExitTip 顺带被改了（此前只解析全局 user.name），brief 没写、也没测试钉。
// 判断是保留它——与 groupExit / 花名册一致才是这个仓库的规则——但既然保留，就得钉住。
func TestGroupCascadeSelfExitTipPrefersGroupRemark(t *testing.T) {
	ctx, g := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)
	const spaceID, leaver = "sp-exit-remark", "u-leaver"

	seedUser(t, ctx, leaver, "全局大名")
	seedGroupInSpace(t, ctx, "g-exit-remark", spaceID, "u-owner")
	seedGroupMember(t, ctx, "g-exit-remark", "u-owner", MemberRoleCreator)
	seedGroupMemberWithRemark(t, ctx, "g-exit-remark", leaver, MemberRoleCommon, "群里叫我老李")

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: leaver, OperatorUID: leaver,
		Reason: spacemod.MemberRemoveReasonLeft,
	}))

	assert.True(t, payloadExtraHasName(stub.sentPayloads(), "退出群聊", "群里叫我老李"),
		"退群提示应使用群内 remark")
	assert.False(t, payloadExtraHasName(stub.sentPayloads(), "退出群聊", "全局大名"),
		"不得回落到全局 user.name")
}

// TestGroupCascadeHandoverNoticeIsBestEffort 通告发送失败不影响交接与移除。
//
// PR #804 review 的非阻塞提醒：best-effort 这个决定没有任何测试钉住（IM 桩永远返回
// 200）。有人把 g.Warn(...) 改成 return err，不会有测试变红——而那个改动并不能救回
// 通告（重试时锁内读到已非 creator 直接返回），只会把成员移除一起拖住。
func TestGroupCascadeHandoverNoticeIsBestEffort(t *testing.T) {
	ctx, g := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)
	stub.failSendOnce("已成为新群主") // 只让这一条通告发送失败

	const spaceID, creator, successor = "sp-besteffort", "u-creator", "u-successor"
	seedUser(t, ctx, creator, "老群主")
	seedUser(t, ctx, successor, "新群主")
	seedGroupInSpace(t, ctx, "g-besteffort", spaceID, creator)
	seedGroupMember(t, ctx, "g-besteffort", creator, MemberRoleCreator)
	seedGroupMember(t, ctx, "g-besteffort", successor, MemberRoleCommon)

	// 通告失败**不得**让整步报错
	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: creator, OperatorUID: "su",
		Reason: spacemod.MemberRemoveReasonForceRemoved,
	}), "通告是 best-effort，发不出去不该让工单重试")

	// 交接与移除照常完成
	role, stillIn := liveMemberRole(t, ctx, "g-besteffort", successor)
	require.True(t, stillIn)
	assert.Equal(t, MemberRoleCreator, role, "交接必须已提交")
	_, leaverIn := liveMemberRole(t, ctx, "g-besteffort", creator)
	assert.False(t, leaverIn, "移除必须完成")
}

// TestGroupCascadeLoneCreatorAnnouncesNoHandover 无人可继任时不得通告交接。
// handOverGroupCreator 此时把群留成无主群，硬发一条「…已成为新群主」会是假消息。
func TestGroupCascadeLoneCreatorAnnouncesNoHandover(t *testing.T) {
	ctx, g := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)
	const spaceID, creator = "sp-lonecreator-tip", "u-lone"

	seedUser(t, ctx, creator, "光杆司令")
	seedGroupInSpace(t, ctx, "g-lonecreator-tip", spaceID, creator)
	seedGroupMember(t, ctx, "g-lonecreator-tip", creator, MemberRoleCreator)

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: creator, OperatorUID: "su",
		Reason: spacemod.MemberRemoveReasonForceRemoved,
	}))

	assert.False(t, payloadsContain(stub.sentPayloads(), "已成为新群主"),
		"没有继任者就不得通告交接")
}

// TestGroupCascadeDisbandSuppressesHandoverAnnounce 解散时交接照做但不通告。
//
// 解散会把**每个**成员都移除，于是群主交接会沿着元老顺序连锁触发：C→S2、S2→S3…
// 一个 M 人群就是 M-1 条「已成为新群主」，且前 M-2 条在写下时就已作废。
// 空间没了、人也全走了，没有收信人。
func TestGroupCascadeDisbandSuppressesHandoverAnnounce(t *testing.T) {
	ctx, g := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)
	const spaceID, creator, successor = "sp-handover-disband", "u-creator", "u-successor"

	seedGroupInSpace(t, ctx, "g-handover-disband", spaceID, creator)
	seedGroupMember(t, ctx, "g-handover-disband", creator, MemberRoleCreator)
	seedGroupMember(t, ctx, "g-handover-disband", successor, MemberRoleCommon)

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: creator, OperatorUID: "su",
		Reason: spacemod.MemberRemoveReasonSpaceDisbanded,
	}))

	// 交接照做（否则 RemoveGroupMembers 会跳过 creator，人永远留在群里）
	role, stillIn := liveMemberRole(t, ctx, "g-handover-disband", successor)
	require.True(t, stillIn)
	assert.Equal(t, MemberRoleCreator, role, "解散场景交接仍要发生")
	// 但不通告
	assert.False(t, payloadsContain(stub.sentPayloads(), "已成为新群主"),
		"解散不得逐个通告群主交接")
}

// TestGroupCascadeForceRemovedSendsNoGroupBroadcast 超管强制移除同样不广播。
// 与 kicked 走同一条分支，但两个 reason 由不同入口产生，各钉一次。
func TestGroupCascadeForceRemovedSendsNoGroupBroadcast(t *testing.T) {
	ctx, g := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)
	const spaceID, victim = "sp-forcenobroadcast", "u-victim"

	seedUser(t, ctx, victim, "被超管移除者")
	seedGroupInSpace(t, ctx, "g-forcenobroadcast", spaceID, "u-owner")
	seedGroupMember(t, ctx, "g-forcenobroadcast", "u-owner", MemberRoleCreator)
	seedGroupMember(t, ctx, "g-forcenobroadcast", victim, MemberRoleCommon)

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: victim, OperatorUID: "su",
		Reason: spacemod.MemberRemoveReasonForceRemoved,
	}))

	_, stillIn := liveMemberRole(t, ctx, "g-forcenobroadcast", victim)
	assert.False(t, stillIn, "清理照做")
	assert.Equal(t, 0, countGroupVisibleTips(stub.sentPayloads()),
		"超管强制移除同样不得产生任何带可见文案的全群消息")
}

// payloadsContain 在录到的系统消息里找文案片段。
// SendGroupMemberBeRemove 的 content 是「你被{0}移除群聊」，
// SendGroupExit 的是「“{0}“退出群聊」。
//
// ⚠️ 只回答「有没有发出去」，**不**回答「谁能看见」。判断可见范围要用
// payloadsContainGroupVisible —— 见那里的注释。
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

// payloadsContainGroupVisible 找**全群可见**且含指定文案的系统消息。
//
// 为什么单列一个：WuKongIM 有两级收件人裁剪，任一级非空就不再是群内广播——
//   - 顶层 subscribers：只投递给这些人；
//   - payload.visibles：客户端只给这些人渲染。
//
// SendGroupMemberBeRemove 把**两级都**钉死成被移除者本人（octo-lib
// config/msg_group.go:203），所以它虽然"发出去了"，群里剩下的人一个字也看不到。
// 光用 payloadsContain 断言会让这个缺口一直是绿的：消息确实在 sentPayloads 里。
func payloadsContainGroupVisible(payloads []map[string]interface{}, fragment string) bool {
	for _, p := range payloads {
		if subs, ok := p["subscribers"]; ok && subs != nil {
			if list, isList := subs.([]interface{}); !isList || len(list) > 0 {
				continue // 定向投递，不是群内广播
			}
		}
		raw, ok := p["payload"]
		if !ok {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(fmt.Sprint(raw))
		if err != nil {
			continue
		}
		var inner map[string]interface{}
		if json.Unmarshal(decoded, &inner) != nil {
			continue
		}
		if vis, ok := inner["visibles"]; ok && vis != nil {
			if list, isList := vis.([]interface{}); !isList || len(list) > 0 {
				continue // 定向渲染，不是群内广播
			}
		}
		if content, ok := inner["content"]; ok &&
			strings.Contains(fmt.Sprint(content), fragment) {
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

// TestGroupCascadeSkipsRejoinedMember 工单跑到群级联这一步时，如果人已经重新
// 加入 Space，就必须什么都不做。
//
// worker 只在认领工单时查过一次成员身份，之后还要排队和跑完其它步骤；那段时间里
// 重新加入的人会被 joinPresetGroups 写进各个预置群，而这一步若照旧执行，
// 就把那些刚写好的行全删了——留下一个「Space 活跃成员、却不在任何群里」的人，
// 没有任何东西会补回来。
func TestGroupCascadeSkipsRejoinedMember(t *testing.T) {
	ctx, g := cascadeSetup(t)
	newGroupIMStub(t, ctx)
	const spaceID, victim = "sp-rejoin", "u-rejoined"

	seedGroupInSpace(t, ctx, "g-rejoin", spaceID, "u-owner")
	seedGroupMember(t, ctx, "g-rejoin", "u-owner", MemberRoleCreator)
	seedGroupMember(t, ctx, "g-rejoin", victim, MemberRoleCommon)

	// 已经重新加入：Space 活跃 + 成员行活跃，正是 CheckMembership 的口径。
	seedActiveSpaceMember(t, ctx, spaceID, victim)

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: victim, OperatorUID: "u-owner", Reason: spacemod.MemberRemoveReasonKicked,
	}))

	_, stillIn := liveMemberRole(t, ctx, "g-rejoin", victim)
	assert.True(t, stillIn, "重新入群的成员不能被上一轮的清理工单清掉")
}

// TestGroupCascadeStillRunsAfterSpaceDisbanded 解散场景下 space.status 已经是 0，
// 上面那个重新加入的判定必须判成「不是活跃成员」，级联照常进行。
//
// 单独钉住是因为这两条走的是同一个谓词：把它写成只看 space_member.status
// 就会让解散时成员行还没置 0 的那一刻误判成「已重新加入」，整条清理静默跳过。
func TestGroupCascadeStillRunsAfterSpaceDisbanded(t *testing.T) {
	ctx, g := cascadeSetup(t)
	newGroupIMStub(t, ctx)
	const spaceID, victim = "sp-disbanded", "u-victim"

	seedGroupInSpace(t, ctx, "g-disbanded", spaceID, "u-owner")
	seedGroupMember(t, ctx, "g-disbanded", "u-owner", MemberRoleCreator)
	seedGroupMember(t, ctx, "g-disbanded", victim, MemberRoleCommon)

	seedActiveSpaceMember(t, ctx, spaceID, victim)
	// Space 被解散：成员行可能还没来得及置 0，但空间本身已经没了。
	_, err := ctx.DB().Exec("UPDATE space SET status=0 WHERE space_id=?", spaceID)
	require.NoError(t, err)

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: victim, OperatorUID: "u-owner",
		Reason: spacemod.MemberRemoveReasonSpaceDisbanded,
	}))

	_, stillIn := liveMemberRole(t, ctx, "g-disbanded", victim)
	assert.False(t, stillIn, "空间已解散，级联必须照常把人清出群")
}

// seedInvitedBot 造一个「由 inviter 拉进群的 bot」，满足 QueryBotsInvitedByUIDTx 的三个条件：
// group_member.robot=1 且未删除，robot 行 status=1，且 robot.creator_uid=inviter。
func seedInvitedBot(t *testing.T, ctx *config.Context, groupNo, botUID, inviter, botName string) {
	t.Helper()
	_, err := ctx.DB().Exec(
		"INSERT INTO group_member (group_no, uid, role, robot, invite_uid, is_deleted, version, created_at, updated_at) "+
			"VALUES (?, ?, 0, 1, ?, 0, 1, NOW(), NOW())",
		groupNo, botUID, inviter)
	require.NoError(t, err)
	_, err = ctx.DB().Exec(
		"INSERT INTO robot (robot_id, status, creator_uid, created_at, updated_at) VALUES (?, 1, ?, NOW(), NOW())",
		botUID, inviter)
	require.NoError(t, err)
	_, err = ctx.DB().Exec(
		"INSERT INTO `user` (uid, name, created_at, updated_at) VALUES (?, ?, NOW(), NOW())",
		botUID, botName)
	require.NoError(t, err)
}

// TestGroupCascadeSelfExitBotTipSaysLeftNotRemoved 自助退出时，bot 连带移除的 Tip
// 必须说「退出了」，不能说「被移出」。
//
// 这条 Tip 与被 SuppressRemoveNotice 抑制的那条是**不同**的消息，走不同的分支，
// 而且是 NoPersist=0 的群可见持久化消息 —— 措辞错了会永久留在群历史里。
// 早先 SuppressRemoveNotice 只包住了前一条，这条在门外、动作词还硬编码成「被移出」，
// 于是主动退出的人在群历史里被写成「被移出群聊」，正是那个开关要挡的措辞。
//
// 既有的自助退出用例接不住这个：它断言的子串是「移除群聊」，而这条 Tip 发的是
// 「被移出群聊」；而且整个级联测试里一个 bot 都没种过。
func TestGroupCascadeSelfExitBotTipSaysLeftNotRemoved(t *testing.T) {
	ctx, g := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)
	const spaceID, leaver, bot = "sp-selfexit-bot", "u-leaver-bot", "bot-selfexit"

	seedGroupInSpace(t, ctx, "g-selfexit-bot", spaceID, "u-owner")
	seedGroupMember(t, ctx, "g-selfexit-bot", "u-owner", MemberRoleCreator)
	seedGroupMember(t, ctx, "g-selfexit-bot", leaver, MemberRoleCommon)
	seedInvitedBot(t, ctx, "g-selfexit-bot", bot, leaver, "助手A")

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: leaver,
		OperatorUID: leaver, // 自助退出：操作者就是本人
		Reason:      spacemod.MemberRemoveReasonLeft,
	}))

	// 前提：bot 确实被连带移除了，否则下面的断言是空转的。
	_, botStillIn := liveMemberRole(t, ctx, "g-selfexit-bot", bot)
	require.False(t, botStillIn, "前提：bot 应被连带移除")

	payloads := stub.sentPayloads()
	require.True(t, payloadsContain(payloads, "助手A"),
		"前提：应发出 bot 连带移除 Tip，否则本用例什么都没验证, payloads=%v", payloads)
	assert.False(t, payloadsContain(payloads, "被移出"),
		"自助退出不得在群历史里写成「被移出」, payloads=%v", payloads)
	assert.True(t, payloadsContain(payloads, "退出了群聊"),
		"应说「退出了群聊」, payloads=%v", payloads)
}

// TestGroupCascadeDisbandSuppressesBotTip 解散时，bot 连带移除的 Tip 也要一并抑制。
//
// 抑制默认移除通告的理由是 N 成员 × M 群的消息风暴；这条 Tip 走的是另一个分支，
// 早先不受抑制，于是同一场风暴照样发生，而且这条是持久化的。
func TestGroupCascadeDisbandSuppressesBotTip(t *testing.T) {
	ctx, g := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)
	const spaceID, leaver, bot = "sp-disband-bot", "u-leaver-db", "bot-disband"

	seedGroupInSpace(t, ctx, "g-disband-bot", spaceID, "u-owner")
	seedGroupMember(t, ctx, "g-disband-bot", "u-owner", MemberRoleCreator)
	seedGroupMember(t, ctx, "g-disband-bot", leaver, MemberRoleCommon)
	seedInvitedBot(t, ctx, "g-disband-bot", bot, leaver, "助手B")

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: leaver,
		OperatorUID: "u-admin",
		Reason:      spacemod.MemberRemoveReasonSpaceDisbanded,
	}))

	// 清理照做：人和 bot 都出群。
	_, stillIn := liveMemberRole(t, ctx, "g-disband-bot", leaver)
	assert.False(t, stillIn)
	_, botStillIn := liveMemberRole(t, ctx, "g-disband-bot", bot)
	require.False(t, botStillIn, "前提：bot 应被连带移除")

	// 但一条群消息都不该发。
	payloads := stub.sentPayloads()
	assert.False(t, payloadsContain(payloads, "助手B"),
		"解散时不得发 bot 连带移除 Tip, payloads=%v", payloads)
	assert.False(t, payloadsContain(payloads, "一并移除"),
		"解散时不得发 bot 连带移除 Tip, payloads=%v", payloads)
}

// TestGroupCascadeKickStillSendsBotTip 反面：普通踢出仍然要发，且说「被移出」。
// 没有这一条，把整个 Tip 删掉上面两个用例也会绿。
func TestGroupCascadeKickStillSendsBotTip(t *testing.T) {
	ctx, g := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)
	const spaceID, kicked, bot = "sp-kick-bot", "u-kicked-bot", "bot-kick"

	seedGroupInSpace(t, ctx, "g-kick-bot", spaceID, "u-owner")
	seedGroupMember(t, ctx, "g-kick-bot", "u-owner", MemberRoleCreator)
	seedGroupMember(t, ctx, "g-kick-bot", kicked, MemberRoleCommon)
	seedInvitedBot(t, ctx, "g-kick-bot", bot, kicked, "助手C")

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: kicked,
		OperatorUID: "u-owner",
		Reason:      spacemod.MemberRemoveReasonKicked,
	}))

	payloads := stub.sentPayloads()
	assert.True(t, payloadsContain(payloads, "助手C"),
		"普通踢出仍要告知群里 bot 为何消失, payloads=%v", payloads)
	assert.True(t, payloadsContain(payloads, "被移出群聊"),
		"普通踢出的动作词仍是「被移出」, payloads=%v", payloads)
}

// TestGroupCascadeSkipsMemberInBannedSpace 封禁 ≠ 解散。
//
// 级联步骤原本用 CheckMembership 做 rejoin 门，而它要求 space.status=1。封禁空间
// 的 status 是 2，于是一名完全在职的成员会被判成「不是活跃成员」，级联照常执行、
// 把他从该空间的每一个群里拆出去。Manager.addMembers 只挡 SpaceStatusDisbanded
// （modules/space/api_manager.go:638），往封禁空间加人是允许的，所以这个状态是
// 正常可达的，不是异常态。
func TestGroupCascadeSkipsMemberInBannedSpace(t *testing.T) {
	ctx, g := cascadeSetup(t)
	newGroupIMStub(t, ctx)
	const spaceID, victim = "sp-banned", "u-banned-victim"

	seedGroupInSpace(t, ctx, "g-banned", spaceID, "u-owner")
	seedGroupMember(t, ctx, "g-banned", "u-owner", MemberRoleCreator)
	seedGroupMember(t, ctx, "g-banned", victim, MemberRoleCommon)

	seedActiveSpaceMember(t, ctx, spaceID, victim)
	_, err := ctx.DB().Exec("UPDATE space SET status=2 WHERE space_id=?", spaceID)
	require.NoError(t, err)

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: victim, OperatorUID: "u-owner",
		Reason: spacemod.MemberRemoveReasonKicked,
	}))

	role, stillIn := liveMemberRole(t, ctx, "g-banned", victim)
	assert.True(t, stillIn, "空间只是被封禁、人还是成员，级联不得把他清出群")
	assert.Equal(t, MemberRoleCommon, role, "角色也不应被改动")
}

// lastHandoverNoticeSuccessor 取**最后一条**交接通告里的新群主 uid（extra[1]）。
// 不能用 payloadExtraUIDAt——它返回第一条命中的。链条里中间环被后面的取代是允许的，
// 真正不能错的是最后一条。
func lastHandoverNoticeSuccessor(payloads []map[string]interface{}) (string, bool) {
	found, uid := false, ""
	for _, p := range payloads {
		one := []map[string]interface{}{p}
		if !payloadsContainGroupVisible(one, "已成为新群主") {
			continue
		}
		found = true
		uid = payloadExtraUIDAt(one, "已成为新群主", 1)
	}
	return uid, found
}

// currentCreator 返回群当前的 creator uid。
func currentCreator(t *testing.T, ctx *config.Context, groupNo string) (string, bool) {
	t.Helper()
	var uids []string
	_, err := ctx.DB().SelectBySql(
		"SELECT uid FROM group_member WHERE group_no=? AND role=? AND is_deleted=0", groupNo, MemberRoleCreator).Load(&uids)
	require.NoError(t, err)
	if len(uids) == 0 {
		return "", false
	}
	return uids[0], true
}

// seedGroupMemberAt 显式指定 created_at。
//
// 必须有：seedGroupMember 用 NOW()，而 group_member.created_at 是秒级 TIMESTAMP，
// 同一秒内插入的几个成员在 `ORDER BY created_at ASC` 下**并列**，选主落到谁完全取决于
// InnoDB 恰好先返回哪一行。凡是依赖元老顺序的用例都必须自己拉开时间戳，否则测的是
// 存储引擎的返回次序而不是选主逻辑（PR #804 round-5 review）。
func seedGroupMemberAt(t *testing.T, ctx *config.Context, groupNo, uid string, role int, at string) {
	t.Helper()
	_, err := ctx.DB().Exec(
		"INSERT INTO group_member (group_no, uid, role, is_deleted, version, created_at, updated_at) VALUES (?, ?, ?, 0, 1, ?, ?)",
		groupNo, uid, role, at, at)
	require.NoError(t, err)
}

// TestGroupCascadeLastNoticeNamesActualOwner 复合缺陷回归守卫。
//
// PR #804 round-5 实测出的最坏情形，由两个各自「可容忍」的缺口复合而成：
//   - A：kicked 批量逐条提交 → 中间环照发通告（每条写下时为真，最后一条纠正全局）
//   - B：抑制只判定一次、永不重评 → 最终继任者若自己挂着 pending 工单，最后一环被抑制
//
// A 提供了一条错的消息，B 抽掉了那个纠正：群历史里**最后**一条群主消息指向一个已经
// 不在群里的人，而真正的群主从未被通告。
//
// A 已由 modules/space 的整批单事务修掉，并由
// TestRemoveMembersLockedEnqueuesAtomically 用一把竞争行锁守住（变异为逐条提交立刻变红）。
// 本用例守的是**在 A 已修的前提下**，B 单独存在时的两种结局都不说错话：
//
//	场景一（继任者挂着陈旧 pending）：一条都不发 —— 退回基线的静默，不比 main 差。
//	场景二（正常）：恰好一条，且点名的正是最终群主。
//
// 两条断言都是硬的，不走 if：上一版把场景一写成「若发了则必须正确」，而抑制生效时
// 一条都不发，整个断言块从不执行——fixture 又一次成了结论
// （见 learnings/pending/a-test-can-encode-the-premise.md）。
func TestGroupCascadeLastNoticeNamesActualOwner(t *testing.T) {
	seedChain := func(t *testing.T, ctx *config.Context, gno, spaceID string) (c, s2, s3, s4 string) {
		c, s2, s3, s4 = "u-c", "u-s2", "u-s3", "u-s4"
		for _, u := range []string{c, s2, s3, s4} {
			seedUser(t, ctx, u, "名字"+u)
		}
		seedGroupInSpace(t, ctx, gno, spaceID, c)
		// 显式拉开元老顺序，见 seedGroupMemberAt 的注释
		seedGroupMemberAt(t, ctx, gno, c, MemberRoleCreator, "2020-01-01 00:00:01")
		seedGroupMemberAt(t, ctx, gno, s2, MemberRoleCommon, "2020-01-01 00:00:02")
		seedGroupMemberAt(t, ctx, gno, s3, MemberRoleCommon, "2020-01-01 00:00:03")
		seedGroupMemberAt(t, ctx, gno, s4, MemberRoleCommon, "2020-01-01 00:00:04")
		return
	}
	runBatch := func(t *testing.T, ctx *config.Context, g *Group, spaceID string, batch []string) {
		// 整批原子入队 —— removeMembersLocked 修好后就是这个形状
		seedPendingRemovals(t, ctx, spaceID, batch, spacemod.MemberRemoveReasonKicked)
		for _, uid := range batch {
			require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
				SpaceID: spaceID, UID: uid, OperatorUID: "su",
				Reason: spacemod.MemberRemoveReasonKicked,
			}))
			markRemovalDone(t, ctx, spaceID, uid)
		}
	}

	t.Run("继任者挂着陈旧pending时保持静默而不是说错人", func(t *testing.T) {
		ctx, g := cascadeSetup(t)
		stub := newGroupIMStub(t, ctx)
		const spaceID, gno = "sp-compose-stale", "g-compose-stale"
		_, _, _, s4 := seedChain(t, ctx, gno, spaceID)

		// 缺口 B：最终继任者自己挂着一条 pending 工单（重试退避中的普通状态）
		seedPendingRemovals(t, ctx, spaceID, []string{s4}, spacemod.MemberRemoveReasonKicked)
		runBatch(t, ctx, g, spaceID, []string{"u-c", "u-s2", "u-s3"})

		owner, hasOwner := currentCreator(t, ctx, gno)
		require.True(t, hasOwner)
		require.Equal(t, s4, owner, "s4 是唯一留下的人，应当成为群主")

		assert.Equal(t, 0, countGroupVisible(stub.sentPayloads(), "已成为新群主"),
			"每一环的继任者都在待移除队列里，应当一条都不发；"+
				"发出来的必然点名一个已经离开的人——正是本 PR 要消灭的东西")
	})

	t.Run("正常情形恰好一条且点名最终群主", func(t *testing.T) {
		ctx, g := cascadeSetup(t)
		stub := newGroupIMStub(t, ctx)
		const spaceID, gno = "sp-compose-normal", "g-compose-normal"
		_, _, s3, _ := seedChain(t, ctx, gno, spaceID)

		// s3、s4 都不在队列里 → 链条应当收敛到 s2→s3 这一环
		runBatch(t, ctx, g, spaceID, []string{"u-c", "u-s2"})

		owner, hasOwner := currentCreator(t, ctx, gno)
		require.True(t, hasOwner)
		require.Equal(t, s3, owner)

		require.Equal(t, 1, countGroupVisible(stub.sentPayloads(), "已成为新群主"),
			"链条应当收敛成一条")
		claimed, emitted := lastHandoverNoticeSuccessor(stub.sentPayloads())
		require.True(t, emitted)
		assert.Equal(t, owner, claimed,
			"最后一条通告点名的新群主必须是群里当下真正的 creator")
		_, stillIn := liveMemberRole(t, ctx, gno, claimed)
		assert.True(t, stillIn, "通告点名的新群主必须还在群里")
	})
}
