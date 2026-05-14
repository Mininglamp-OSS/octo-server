// Package message 单测：enrichPayloadWithSpaceIDCore (YUJ-219-A / GH#1283)。
//
// 对应 analysis-report.md §4.5 / §7.4：sendMessage 派发前注入权威 space_id，
// 让客户端 SpaceFilter 在消息级有可信字段，race 窗口 fail-open 可降级为 fail-closed。
package message

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// groupSpaceLookupStub 记录调用历史，返回固定映射。
type groupSpaceLookupStub struct {
	spaces  map[string]string
	errOn   map[string]error
	calls   []string
}

func (s *groupSpaceLookupStub) lookup(groupNo string) (string, error) {
	s.calls = append(s.calls, groupNo)
	if err, ok := s.errOn[groupNo]; ok {
		return "", err
	}
	return s.spaces[groupNo], nil
}

// silentLog 是 logWarn 的 no-op 实现，避免测试日志噪音。
func silentLog(string, ...zap.Field) {}

func TestEnrichPayloadWithSpaceIDCore_GroupInjects(t *testing.T) {
	stub := &groupSpaceLookupStub{spaces: map[string]string{"g1": "spaceA"}}
	payload := map[string]interface{}{"content": "hi"}
	got := enrichPayloadWithSpaceIDCore("g1", common.ChannelTypeGroup.Uint8(), payload, "", stub.lookup, silentLog)
	assert.Equal(t, "spaceA", got["space_id"])
	assert.Equal(t, []string{"g1"}, stub.calls)
}

func TestEnrichPayloadWithSpaceIDCore_GroupEmptySpaceNoInject(t *testing.T) {
	// 旧群（SpaceID 为空）→ 不写 space_id。调用方可以据此识别"老群消息"，
	// 走原有向前兼容路径。
	stub := &groupSpaceLookupStub{spaces: map[string]string{"g1": ""}}
	payload := map[string]interface{}{"content": "hi"}
	got := enrichPayloadWithSpaceIDCore("g1", common.ChannelTypeGroup.Uint8(), payload, "", stub.lookup, silentLog)
	_, ok := got["space_id"]
	assert.False(t, ok, "旧群无 SpaceID 不应注入 payload.space_id")
}

func TestEnrichPayloadWithSpaceIDCore_GroupLookupErrorSilentlySkips(t *testing.T) {
	// DB 查询失败不应阻断发送，payload 保持原样，调用方走老路径。
	stub := &groupSpaceLookupStub{errOn: map[string]error{"g1": errors.New("db fail")}}
	payload := map[string]interface{}{"content": "hi"}
	got := enrichPayloadWithSpaceIDCore("g1", common.ChannelTypeGroup.Uint8(), payload, "", stub.lookup, silentLog)
	_, ok := got["space_id"]
	assert.False(t, ok)
}

func TestEnrichPayloadWithSpaceIDCore_PersonalLeavesPayloadUntouched(t *testing.T) {
	// PERSONAL 不注入 space_id，尊重发送端上送。确保没有无意的 lookup 调用。
	stub := &groupSpaceLookupStub{}
	payload := map[string]interface{}{"content": "hi"}
	got := enrichPayloadWithSpaceIDCore("peer_uid", common.ChannelTypePerson.Uint8(), payload, "", stub.lookup, silentLog)
	_, ok := got["space_id"]
	assert.False(t, ok, "PERSONAL 不应注入 space_id")
	assert.Empty(t, stub.calls, "PERSONAL 路径不应查群表")
}

func TestEnrichPayloadWithSpaceIDCore_GroupOverwritesForgedClientSpaceID(t *testing.T) {
	// YUJ-226 / lml P1-2 regression：客户端给 GROUP 消息塞错误的 payload.space_id，
	// 服务端必须无条件以群表值覆盖，不再"尊重客户端上送"。
	stub := &groupSpaceLookupStub{spaces: map[string]string{"g1": "spaceB"}}
	payload := map[string]interface{}{"content": "hi", "space_id": "spaceA_forged"}
	got := enrichPayloadWithSpaceIDCore("g1", common.ChannelTypeGroup.Uint8(), payload, "", stub.lookup, silentLog)
	assert.Equal(t, "spaceB", got["space_id"], "GROUP 消息 space_id 必须以群表权威值覆盖客户端上送值")
	assert.Equal(t, []string{"g1"}, stub.calls, "GROUP 路径每次都应查群表，不短路")
}

func TestEnrichPayloadWithSpaceIDCore_GroupLegacyDropsForgedClientSpaceID(t *testing.T) {
	// 老群 SpaceID 为空 → 删除客户端上送的 space_id（否则 sender 可以对老群
	// 伪造任意 Space tag）。这是对 P1-2 的补强：override 必须覆盖 null 情况。
	stub := &groupSpaceLookupStub{spaces: map[string]string{"g_legacy": ""}}
	payload := map[string]interface{}{"content": "hi", "space_id": "spaceX_forged"}
	got := enrichPayloadWithSpaceIDCore("g_legacy", common.ChannelTypeGroup.Uint8(), payload, "", stub.lookup, silentLog)
	_, ok := got["space_id"]
	assert.False(t, ok, "老群无 SpaceID 时客户端上送的 space_id 必须被剥离")
}

func TestEnrichPayloadWithSpaceIDCore_CommunityTopicOverwritesForgedClientSpaceID(t *testing.T) {
	// YUJ-226 / lml P1-2 regression：CommunityTopic 也走强制覆盖路径。
	parentNo := "g42"
	topicCID := thread.BuildChannelID(parentNo, "short_t1")
	stub := &groupSpaceLookupStub{spaces: map[string]string{parentNo: "spaceC"}}
	payload := map[string]interface{}{"content": "hi", "space_id": "spaceA_forged"}
	got := enrichPayloadWithSpaceIDCore(topicCID, common.ChannelTypeCommunityTopic.Uint8(), payload, "", stub.lookup, silentLog)
	assert.Equal(t, "spaceC", got["space_id"], "CommunityTopic 消息 space_id 必须以父群权威值覆盖客户端上送值")
	assert.Equal(t, []string{parentNo}, stub.calls)
}

func TestEnrichPayloadWithSpaceIDCore_PersonalEmptySenderStripsClientSpaceID(t *testing.T) {
	// YUJ-660 High-3 fail-open fix：senderSpaceID 为空（SpaceMiddleware 未注入）
	// 时，payload.space_id 被无条件剥离，避免攻击者用 forged payload.space_id +
	// 省略 X-Space-ID 的方式绕过权威覆盖。
	stub := &groupSpaceLookupStub{}
	payload := map[string]interface{}{"content": "hi", "space_id": "spaceA_forged"}
	got := enrichPayloadWithSpaceIDCore("peer_uid", common.ChannelTypePerson.Uint8(), payload, "", stub.lookup, silentLog)
	_, ok := got["space_id"]
	assert.False(t, ok, "PERSONAL senderSpaceID 为空时必须剥离客户端 payload.space_id")
	assert.Empty(t, stub.calls, "PERSONAL 路径不应查群表")
}

// ---------------- YUJ-644 / Mininglamp-OSS#33 PERSONAL 权威覆盖 ----------------

func TestEnrichPayloadWithSpaceIDCore_PersonalAuthoritativeOverridesClient(t *testing.T) {
	// senderSpaceID 来自 SpaceMiddleware 已校验的发送方 SpaceID。客户端任何
	// payload.space_id（包括伪造值）必须被无条件覆盖，因为客户端 SpaceFilter 的
	// 唯一可信信号源就是 payload.space_id。
	stub := &groupSpaceLookupStub{}
	payload := map[string]interface{}{"content": "hi", "space_id": "spaceB_forged"}
	got := enrichPayloadWithSpaceIDCore("peer_uid", common.ChannelTypePerson.Uint8(), payload, "spaceA", stub.lookup, silentLog)
	assert.Equal(t, "spaceA", got["space_id"], "PERSONAL senderSpaceID 必须覆盖客户端伪造值")
	assert.Empty(t, stub.calls, "PERSONAL 路径不查群表")
}

func TestEnrichPayloadWithSpaceIDCore_PersonalAuthoritativeInjectsWhenAbsent(t *testing.T) {
	// senderSpaceID 非空，客户端 payload 没有 space_id —— 服务端注入。
	stub := &groupSpaceLookupStub{}
	payload := map[string]interface{}{"content": "hi"}
	got := enrichPayloadWithSpaceIDCore("peer_uid", common.ChannelTypePerson.Uint8(), payload, "spaceA", stub.lookup, silentLog)
	assert.Equal(t, "spaceA", got["space_id"], "PERSONAL senderSpaceID 注入到无 space_id 的 payload")
}

func TestEnrichPayloadWithSpaceIDCore_PersonalEmptySenderEmitsObservabilityWarn(t *testing.T) {
	// senderSpaceID 为空，payload 也没有 space_id —— 派发后客户端走 fail-open 兼容
	// 分支。本测试锁住可观测性 warn 日志的发射条件，作为日志告警的稳态指标。
	stub := &groupSpaceLookupStub{}
	var captured []string
	captureLog := func(msg string, _ ...zap.Field) {
		captured = append(captured, msg)
	}
	payload := map[string]interface{}{"content": "hi"}
	got := enrichPayloadWithSpaceIDCore("peer_uid", common.ChannelTypePerson.Uint8(), payload, "", stub.lookup, captureLog)
	_, ok := got["space_id"]
	assert.False(t, ok, "senderSpaceID 为空且客户端未上送 → 不注入，保持兼容语义")
	assert.Contains(t, captured, "enrich_payload_space_id_empty", "应发出 empty-space_id 监控 warn")
}

func TestEnrichPayloadWithSpaceIDCore_PersonalEmptySenderStripsAndWarns(t *testing.T) {
	// YUJ-660 High-3：senderSpaceID 为空 + 客户端上送了 space_id —— 必须剥离，
	// 同时发监控 warn 标记 client_space_id_stripped=true，便于运维识别 fail-open
	// 绕过尝试。
	stub := &groupSpaceLookupStub{}
	var captured []string
	captureLog := func(msg string, _ ...zap.Field) {
		captured = append(captured, msg)
	}
	payload := map[string]interface{}{"content": "hi", "space_id": "spaceA_forged"}
	got := enrichPayloadWithSpaceIDCore("peer_uid", common.ChannelTypePerson.Uint8(), payload, "", stub.lookup, captureLog)
	_, ok := got["space_id"]
	assert.False(t, ok, "客户端上送的 space_id 必须被剥离")
	assert.Contains(t, captured, "enrich_payload_space_id_empty",
		"剥离时仍应发出 empty-space_id 监控 warn")
}

func TestEnrichPayloadWithSpaceIDCore_ExistingEmptyStringOverwritten(t *testing.T) {
	// payload["space_id"] 存在但为空串 → 视为未上送，按规则推导。保证
	// 老客户端显式传空字符串的边界行为可被修复。
	stub := &groupSpaceLookupStub{spaces: map[string]string{"g1": "spaceA"}}
	payload := map[string]interface{}{"space_id": ""}
	got := enrichPayloadWithSpaceIDCore("g1", common.ChannelTypeGroup.Uint8(), payload, "", stub.lookup, silentLog)
	assert.Equal(t, "spaceA", got["space_id"])
}

func TestEnrichPayloadWithSpaceIDCore_CommunityTopicInjectsParentSpace(t *testing.T) {
	// 子区 channel_id 由 thread.BuildChannelID 构造，space_id 取自父群。
	// 用生产端 Build/Parse 对称构造，避免硬编码分隔符。
	parentNo := "g42"
	topicCID := thread.BuildChannelID(parentNo, "short_t1")
	gotParent, _, err := thread.ParseChannelID(topicCID)
	assert.NoError(t, err)
	assert.Equal(t, parentNo, gotParent, "thread.BuildChannelID / ParseChannelID 对称性保证")

	stub := &groupSpaceLookupStub{spaces: map[string]string{parentNo: "spaceC"}}
	payload := map[string]interface{}{"content": "hi"}
	got := enrichPayloadWithSpaceIDCore(topicCID, common.ChannelTypeCommunityTopic.Uint8(), payload, "", stub.lookup, silentLog)
	assert.Equal(t, "spaceC", got["space_id"])
	assert.Equal(t, []string{parentNo}, stub.calls)
}

func TestEnrichPayloadWithSpaceIDCore_CommunityTopicLegacyParentNoSpace(t *testing.T) {
	// 父群无 SpaceID（老群）→ 子区也不注入 space_id，保持老行为。
	parentNo := "g_legacy"
	topicCID := thread.BuildChannelID(parentNo, "short_t2")
	stub := &groupSpaceLookupStub{spaces: map[string]string{parentNo: ""}}
	got := enrichPayloadWithSpaceIDCore(topicCID, common.ChannelTypeCommunityTopic.Uint8(), map[string]interface{}{"content": "hi"}, "", stub.lookup, silentLog)
	_, ok := got["space_id"]
	assert.False(t, ok)
}

func TestEnrichPayloadWithSpaceIDCore_CommunityTopicParentLookupError(t *testing.T) {
	// 父群查询 DB 失败 → 静默跳过，不注入不阻断。
	parentNo := "g_err"
	topicCID := thread.BuildChannelID(parentNo, "short_t3")
	stub := &groupSpaceLookupStub{errOn: map[string]error{parentNo: errors.New("db fail")}}
	got := enrichPayloadWithSpaceIDCore(topicCID, common.ChannelTypeCommunityTopic.Uint8(), map[string]interface{}{"content": "hi"}, "", stub.lookup, silentLog)
	_, ok := got["space_id"]
	assert.False(t, ok)
}

func TestEnrichPayloadWithSpaceIDCore_CommunityTopicInvalidChannelIDSkips(t *testing.T) {
	// 解析失败 → 不注入，不查 DB，不阻断发送。
	stub := &groupSpaceLookupStub{}
	payload := map[string]interface{}{"content": "hi"}
	got := enrichPayloadWithSpaceIDCore("not-a-thread-id", common.ChannelTypeCommunityTopic.Uint8(), payload, "", stub.lookup, silentLog)
	_, ok := got["space_id"]
	assert.False(t, ok)
	assert.Empty(t, stub.calls)
}

func TestEnrichPayloadWithSpaceIDCore_NilPayloadBecomesMap(t *testing.T) {
	// payload 为 nil 时函数内部 make 一个空 map 返回，调用方可以安全序列化。
	stub := &groupSpaceLookupStub{spaces: map[string]string{"g1": "spaceA"}}
	got := enrichPayloadWithSpaceIDCore("g1", common.ChannelTypeGroup.Uint8(), nil, "", stub.lookup, silentLog)
	assert.NotNil(t, got)
	assert.Equal(t, "spaceA", got["space_id"])
}

// buildValidThreadChannelID removed: tests now use thread.BuildChannelID
// directly so changes to the separator constant in modules/thread/const.go
// propagate without manual test-side updates.
