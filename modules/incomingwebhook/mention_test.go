package incomingwebhook

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/mentionrewrite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mention 核心逻辑的纯单测（无 DB/Redis/IM 依赖）：装配决策矩阵、去重/上限、布尔助手、
// 配置列解析（parseMentionUIDs），以及【与 ExpandAisToBotUIDs 的 parity】——装配出的线协议
// 形态必须能被真实展开器消费。IO（成员闸 filterGroupMembers）由集成测试覆盖。

func memberSet(uids ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(uids))
	for _, u := range uids {
		m[u] = struct{}{}
	}
	return m
}

func TestAssembleMention(t *testing.T) {
	t.Run("nil when nothing requested", func(t *testing.T) {
		mention, ignored := assembleMention(nil, memberSet(), false, false, false, false)
		assert.Nil(t, mention)
		assert.Empty(t, ignored)
	})

	t.Run("targeted uids: keep members, drop non-members, preserve order", func(t *testing.T) {
		uids := []string{"u_alice", "u_ghost", "bot_ci", "u_bob"}
		mention, ignored := assembleMention(uids, memberSet("u_alice", "bot_ci", "u_bob"), false, false, false, false)
		require.NotNil(t, mention)
		assert.Empty(t, ignored)
		// kept 必须是 []interface{}（ExpandAisToBotUIDs 的要求），且顺序保持、非成员被剔除。
		got, ok := mention[mentionrewrite.UIDsKey].([]interface{})
		require.True(t, ok, "uids must be []interface{} for ExpandAisToBotUIDs compatibility")
		assert.Equal(t, []interface{}{"u_alice", "bot_ci", "u_bob"}, got)
		// 无广播位时不应出现 humans/ais。
		assert.NotContains(t, mention, mentionrewrite.HumansKey)
		assert.NotContains(t, mention, mentionrewrite.AIsKey)
	})

	t.Run("all uids are non-members: no uids key, nil mention", func(t *testing.T) {
		mention, ignored := assembleMention([]string{"u_ghost"}, memberSet("u_alice"), false, false, false, false)
		assert.Nil(t, mention)
		assert.Empty(t, ignored)
	})

	t.Run("@所有人 allowed -> humans=1", func(t *testing.T) {
		mention, ignored := assembleMention(nil, memberSet(), true, false, true, false)
		require.NotNil(t, mention)
		assert.Equal(t, 1, mention[mentionrewrite.HumansKey])
		assert.Empty(t, ignored)
	})

	t.Run("@所有人 denied -> ignored, no humans, nil mention", func(t *testing.T) {
		mention, ignored := assembleMention(nil, memberSet(), true, false, false, false)
		assert.Nil(t, mention)
		assert.Equal(t, []string{"all"}, ignored)
	})

	t.Run("@所有 AI allowed -> ais=1", func(t *testing.T) {
		mention, ignored := assembleMention(nil, memberSet(), false, true, false, true)
		require.NotNil(t, mention)
		assert.Equal(t, 1, mention[mentionrewrite.AIsKey])
		assert.Empty(t, ignored)
	})

	t.Run("@所有 AI denied -> ignored", func(t *testing.T) {
		mention, ignored := assembleMention(nil, memberSet(), false, true, false, false)
		assert.Nil(t, mention)
		assert.Equal(t, []string{"bots"}, ignored)
	})

	t.Run("both broadcasts denied -> ignored order [all, bots]", func(t *testing.T) {
		mention, ignored := assembleMention(nil, memberSet(), true, true, false, false)
		assert.Nil(t, mention)
		assert.Equal(t, []string{"all", "bots"}, ignored)
	})

	t.Run("uids + both broadcasts allowed -> full mention", func(t *testing.T) {
		mention, ignored := assembleMention([]string{"u_alice"}, memberSet("u_alice"), true, true, true, true)
		require.NotNil(t, mention)
		assert.Empty(t, ignored)
		assert.Equal(t, []interface{}{"u_alice"}, mention[mentionrewrite.UIDsKey])
		assert.Equal(t, 1, mention[mentionrewrite.HumansKey])
		assert.Equal(t, 1, mention[mentionrewrite.AIsKey])
	})

	t.Run("targeted uids kept but @所有人 denied -> mention has uids, ignored has all", func(t *testing.T) {
		mention, ignored := assembleMention([]string{"u_alice"}, memberSet("u_alice"), true, false, false, false)
		require.NotNil(t, mention)
		assert.Equal(t, []interface{}{"u_alice"}, mention[mentionrewrite.UIDsKey])
		assert.NotContains(t, mention, mentionrewrite.HumansKey)
		assert.Equal(t, []string{"all"}, ignored)
	})
}

// TestAssembleMentionFeedsExpandAis is the parity gate: a mention assembled with
// ais=1 must be consumable by the REAL pkg/mentionrewrite.ExpandAisToBotUIDs so
// `@所有 AI` from a webhook resolves to the same bot-UID list as the user / bot
// ingresses. Uses a stub fetchBotUIDs callback (the helper takes a callback, so
// no DB/services are needed) — this locks the wire shape, not the lookup.
func TestAssembleMentionFeedsExpandAis(t *testing.T) {
	groupNo := "g_123"
	mention, _ := assembleMention([]string{"u_alice"}, memberSet("u_alice"), false, true, false, true)
	require.NotNil(t, mention)
	require.Equal(t, 1, mention[mentionrewrite.AIsKey])

	payload := map[string]interface{}{mentionrewrite.MentionKey: mention}
	stub := func(string) ([]string, error) { return []string{"bot_x", "u_alice", "bot_y"}, nil }
	out := mentionrewrite.ExpandAisToBotUIDs(payload, common.ChannelTypeGroup.Uint8(), groupNo, stub)

	mm := out[mentionrewrite.MentionKey].(map[string]interface{})
	uids := mm[mentionrewrite.UIDsKey].([]interface{})
	// 原有 u_alice 保留、bot_x/bot_y 追加、u_alice 不重复（ExpandAisToBotUIDs 幂等去重）。
	assert.Equal(t, []interface{}{"u_alice", "bot_x", "bot_y"}, uids)
}

func TestDedupNonEmpty(t *testing.T) {
	t.Run("trim, drop blanks, dedup, preserve order", func(t *testing.T) {
		got := dedupNonEmpty([]string{" u1 ", "u2", "u1", "", "  ", "u3", "u2"}, 0)
		assert.Equal(t, []string{"u1", "u2", "u3"}, got)
	})
	t.Run("cap limits count after dedup", func(t *testing.T) {
		got := dedupNonEmpty([]string{"u1", "u2", "u3", "u4"}, 2)
		assert.Equal(t, []string{"u1", "u2"}, got)
	})
	t.Run("empty input -> nil", func(t *testing.T) {
		assert.Nil(t, dedupNonEmpty(nil, 50))
		assert.Nil(t, dedupNonEmpty([]string{"", "   "}, 50))
	})
}

// TestParseMentionUIDs pins the lenient decode of the config column mention_uids
// (a JSON array string). Empty / malformed → nil ("no targeted @"); valid → the
// list (config is validated at write time, so read-side leniency is just defense
// in depth — a bad row must never break a push).
func TestParseMentionUIDs(t *testing.T) {
	assert.Nil(t, parseMentionUIDs(""), "empty → no @")
	assert.Equal(t, []string{"u1", "bot_b"}, parseMentionUIDs(`["u1","bot_b"]`))
	assert.Equal(t, []string{}, parseMentionUIDs(`[]`), "empty array decodes to empty slice")
	for _, bad := range []string{`not json`, `{"uids":["a"]}`, `["a",1]`, `"a"`, `123`} {
		assert.Nilf(t, parseMentionUIDs(bad), "malformed %q → nil", bad)
	}
}

func TestBoolHelpers(t *testing.T) {
	tr, fa := true, false
	assert.False(t, boolPtrTrue(nil))
	assert.False(t, boolPtrTrue(&fa))
	assert.True(t, boolPtrTrue(&tr))
	assert.Equal(t, 0, boolToInt(false))
	assert.Equal(t, 1, boolToInt(true))
}

func TestMaxMentionUIDs(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv(envMaxMentionUIDs, "")
		assert.Equal(t, defaultMaxMentionUIDs, maxMentionUIDs())
	})
	t.Run("env override", func(t *testing.T) {
		t.Setenv(envMaxMentionUIDs, "7")
		assert.Equal(t, 7, maxMentionUIDs())
	})
	t.Run("invalid env falls back to default", func(t *testing.T) {
		t.Setenv(envMaxMentionUIDs, "-3")
		assert.Equal(t, defaultMaxMentionUIDs, maxMentionUIDs())
		t.Setenv(envMaxMentionUIDs, "abc")
		assert.Equal(t, defaultMaxMentionUIDs, maxMentionUIDs())
	})
}

func TestIsTextMention(t *testing.T) {
	assert.True(t, isTextMention(&pushPayloadReq{MsgType: ""}))
	assert.True(t, isTextMention(&pushPayloadReq{MsgType: "text"}))
	assert.True(t, isTextMention(&pushPayloadReq{MsgType: "  Text "}))
	assert.False(t, isTextMention(&pushPayloadReq{MsgType: "richtext"}))
}

// TestMentionEntitiesSurviveAisExpansion is the integration-seam guard: the way
// server-generated entities reach the wire is handlePush's `CloneForExpansion` →
// `ExpandAisToBotUIDs` chain. This proves entities pass through verbatim
// (offset/length/uid unchanged) even when ais-expansion appends bot UIDs to
// mention.uids, AND that the clone isolates the original payload (no in-place
// mutation). No DB/WuKongIM needed — it locks the only transformation between the
// generated entity and the serialized wire.
func TestMentionEntitiesSurviveAisExpansion(t *testing.T) {
	entity := map[string]interface{}{entityKeyUID: "u1", entityKeyOffset: 0, entityKeyLength: 3}
	mention := map[string]interface{}{
		mentionrewrite.UIDsKey:     []interface{}{"u1"},
		mentionrewrite.AIsKey:      1,
		mentionrewrite.EntitiesKey: []interface{}{entity},
	}
	payload := map[string]interface{}{mentionrewrite.MentionKey: mention}

	wire := mentionrewrite.CloneForExpansion(payload)
	wire = mentionrewrite.ExpandAisToBotUIDs(
		wire, common.ChannelTypeGroup.Uint8(), "g1",
		func(string) ([]string, error) { return []string{"bot_x"}, nil })

	mm, ok := wire[mentionrewrite.MentionKey].(map[string]interface{})
	require.True(t, ok)

	// entities survive verbatim through clone + ais expansion.
	ents, ok := mm[mentionrewrite.EntitiesKey].([]interface{})
	require.Truef(t, ok, "entities must survive clone+expand; mention=%v", mm)
	require.Len(t, ents, 1)
	assert.Equal(t, entity, ents[0])

	// ais expansion appended the bot uid; the directed uid is still there.
	uids, _ := mm[mentionrewrite.UIDsKey].([]interface{})
	assert.Contains(t, uids, "u1")
	assert.Contains(t, uids, "bot_x")

	// clone isolation: the original payload's uids are NOT mutated by the wire expansion.
	origUIDs, _ := mention[mentionrewrite.UIDsKey].([]interface{})
	assert.NotContains(t, origUIDs, "bot_x", "wire expansion must not mutate the original payload")
}
