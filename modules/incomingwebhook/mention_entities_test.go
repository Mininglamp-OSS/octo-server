package incomingwebhook

import (
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/mentionrewrite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 渲染层 mention.entities 的纯单测（无 DB 依赖）：逐条宽松解码 + 成员闸/越界/指向'@' 校验，
// 以及【offset 单位是 UTF-16 码元】的关键 parity（emoji 前缀场景下 rune/byte 偏移必须被拒）。
// 成员闸的真实查询（filterGroupMembers）与端到端透传由集成测试覆盖。

func rawEntities(jsons ...string) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(jsons))
	for _, j := range jsons {
		out = append(out, json.RawMessage(j))
	}
	return out
}

func TestDecodeEntities(t *testing.T) {
	t.Run("valid entities decode in order", func(t *testing.T) {
		ents := decodeEntities(rawEntities(
			`{"uid":"u1","offset":0,"length":3}`,
			`{"uid":"u2","offset":4,"length":3}`,
		), 0)
		require.Len(t, ents, 2)
		assert.Equal(t, mentionEntity{UID: "u1", Offset: 0, Length: 3}, ents[0])
		assert.Equal(t, mentionEntity{UID: "u2", Offset: 4, Length: 3}, ents[1])
	})

	t.Run("per-entity leniency: bad ones skipped, good kept (acceptance #6)", func(t *testing.T) {
		ents := decodeEntities(rawEntities(
			`{"uid":"u1","offset":0,"length":3}`,   // good
			`"garbage"`,                            // not an object
			`{"uid":"","offset":0,"length":3}`,     // empty uid
			`{"uid":"u3","offset":-1,"length":3}`,  // negative offset
			`{"uid":"u4","offset":0,"length":0}`,   // zero length
			`{"uid":"u5","offset":"x","length":3}`, // offset wrong type
			`{"uid":"u6","offset":1,"length":2}`,   // good
		), 0)
		require.Len(t, ents, 2)
		assert.Equal(t, "u1", ents[0].UID)
		assert.Equal(t, "u6", ents[1].UID)
	})

	t.Run("cap limits count", func(t *testing.T) {
		ents := decodeEntities(rawEntities(
			`{"uid":"u1","offset":0,"length":1}`,
			`{"uid":"u2","offset":1,"length":1}`,
			`{"uid":"u3","offset":2,"length":1}`,
		), 2)
		require.Len(t, ents, 2)
	})

	t.Run("empty / all-bad input -> nil", func(t *testing.T) {
		assert.Nil(t, decodeEntities(nil, 0))
		assert.Nil(t, decodeEntities(rawEntities(), 0))
		assert.Nil(t, decodeEntities(rawEntities(`"x"`, `{"uid":""}`), 0))
	})
}

func TestEntityUIDsOf(t *testing.T) {
	assert.Nil(t, entityUIDsOf(nil))
	got := entityUIDsOf([]mentionEntity{{UID: "a"}, {UID: "b"}})
	assert.Equal(t, []string{"a", "b"}, got)
}

func TestFinalizeEntities(t *testing.T) {
	members := memberSet("u1", "u2") // helper from mention_test.go

	t.Run("keeps member entities pointing at '@', builds wire maps", func(t *testing.T) {
		// "@张三 @李四": @(0)张(1)三(2) space(3) @(4)李(5)四(6)
		content := "@张三 @李四"
		ents := []mentionEntity{
			{UID: "u1", Offset: 0, Length: 3},
			{UID: "u2", Offset: 4, Length: 3},
		}
		got := finalizeEntities(ents, members, content)
		require.Len(t, got, 2)
		assert.Equal(t, map[string]interface{}{entityKeyUID: "u1", entityKeyOffset: 0, entityKeyLength: 3}, got[0])
		assert.Equal(t, map[string]interface{}{entityKeyUID: "u2", entityKeyOffset: 4, entityKeyLength: 3}, got[1])
	})

	t.Run("drops non-member uid (anti-enumeration, silent)", func(t *testing.T) {
		got := finalizeEntities([]mentionEntity{{UID: "ghost", Offset: 0, Length: 3}}, members, "@张三")
		assert.Nil(t, got)
	})

	t.Run("drops out-of-range offset/length (UTF-16 bounds)", func(t *testing.T) {
		content := "@张三" // utf16 len = 3
		assert.Nil(t, finalizeEntities([]mentionEntity{{UID: "u1", Offset: 0, Length: 4}}, members, content))
		assert.Nil(t, finalizeEntities([]mentionEntity{{UID: "u1", Offset: 3, Length: 1}}, members, content))
		assert.Nil(t, finalizeEntities([]mentionEntity{{UID: "u1", Offset: 99, Length: 1}}, members, content))
	})

	t.Run("drops entity not pointing at '@'", func(t *testing.T) {
		// "x@张三": x(0)@(1)张(2)三(3)
		content := "x@张三"
		assert.Nil(t, finalizeEntities([]mentionEntity{{UID: "u1", Offset: 0, Length: 3}}, members, content))
		got := finalizeEntities([]mentionEntity{{UID: "u1", Offset: 1, Length: 3}}, members, content)
		require.Len(t, got, 1)
		assert.Equal(t, 1, got[0].(map[string]interface{})[entityKeyOffset])
	})

	// 关键 parity：offset 单位必须是 UTF-16 码元，不是 rune、也不是字节。
	// "👍@张三" 的 UTF-16: 👍=2 码元(代理对) → @张三 起于 offset 2；rune 偏移会是 1、UTF-8 字节偏移会是 4。
	t.Run("offset unit is UTF-16 code units, not rune or byte (emoji prefix)", func(t *testing.T) {
		content := "👍@张三"
		ok := finalizeEntities([]mentionEntity{{UID: "u1", Offset: 2, Length: 3}}, members, content)
		require.Len(t, ok, 1, "UTF-16 offset 2 must point at '@'")
		assert.Equal(t, map[string]interface{}{entityKeyUID: "u1", entityKeyOffset: 2, entityKeyLength: 3}, ok[0])

		assert.Nil(t, finalizeEntities([]mentionEntity{{UID: "u1", Offset: 1, Length: 3}}, members, content),
			"rune-based offset 1 lands inside the surrogate pair -> must be rejected (proves not rune)")
		assert.Nil(t, finalizeEntities([]mentionEntity{{UID: "u1", Offset: 4, Length: 3}}, members, content),
			"byte-based offset 4 is out of range -> must be rejected (proves not byte)")
	})

	t.Run("mixed valid + invalid: keep only valid", func(t *testing.T) {
		content := "@张三 @李四"
		ents := []mentionEntity{
			{UID: "u1", Offset: 0, Length: 3},    // valid
			{UID: "ghost", Offset: 4, Length: 3}, // non-member -> drop
		}
		got := finalizeEntities(ents, members, content)
		require.Len(t, got, 1)
		assert.Equal(t, "u1", got[0].(map[string]interface{})[entityKeyUID])
	})

	t.Run("exact-fit span at content end is accepted (boundary)", func(t *testing.T) {
		// "@a": @(0)a(1), utf16 len 2 → offset0 length2 fits exactly to the end.
		got := finalizeEntities([]mentionEntity{{UID: "u1", Offset: 0, Length: 2}}, members, "@a")
		require.Len(t, got, 1)
		assert.Equal(t, map[string]interface{}{entityKeyUID: "u1", entityKeyOffset: 0, entityKeyLength: 2}, got[0])
	})

	t.Run("rejects duplicate + overlapping spans, keeps disjoint (first-wins)", func(t *testing.T) {
		// "@a @b": @(0)a(1) space(2) @(3)b(4)
		content := "@a @b"
		ents := []mentionEntity{
			{UID: "u1", Offset: 0, Length: 2}, // accepted "@a"
			{UID: "u1", Offset: 0, Length: 2}, // exact duplicate -> drop
			{UID: "u2", Offset: 0, Length: 2}, // overlaps accepted [0,2) (even though at '@') -> drop
			{UID: "u2", Offset: 3, Length: 2}, // accepted "@b" (disjoint)
		}
		got := finalizeEntities(ents, members, content)
		require.Len(t, got, 2)
		assert.Equal(t, map[string]interface{}{entityKeyUID: "u1", entityKeyOffset: 0, entityKeyLength: 2}, got[0])
		assert.Equal(t, map[string]interface{}{entityKeyUID: "u2", entityKeyOffset: 3, entityKeyLength: 2}, got[1])
	})

	t.Run("non-member and bad-offset drops are indistinguishable in output (anti-enumeration)", func(t *testing.T) {
		// "@张三 @李四": valid keep (u1@0); non-member drop (ghost@4); bad-'@' drop (u2@3 is the space)
		content := "@张三 @李四"
		ents := []mentionEntity{
			{UID: "u1", Offset: 0, Length: 3},    // valid keep
			{UID: "ghost", Offset: 4, Length: 3}, // non-member -> drop
			{UID: "u2", Offset: 3, Length: 1},    // offset 3 is the space (not '@') -> drop
		}
		got := finalizeEntities(ents, members, content)
		require.Len(t, got, 1)
		assert.Equal(t, "u1", got[0].(map[string]interface{})[entityKeyUID])
	})

	t.Run("empty -> nil", func(t *testing.T) {
		assert.Nil(t, finalizeEntities(nil, members, "@张三"))
		assert.Nil(t, finalizeEntities([]mentionEntity{}, members, "@张三"))
	})
}

func TestIsTextMention(t *testing.T) {
	assert.True(t, isTextMention(&pushPayloadReq{MsgType: ""}))
	assert.True(t, isTextMention(&pushPayloadReq{MsgType: "text"}))
	assert.True(t, isTextMention(&pushPayloadReq{MsgType: "  Text "}))
	assert.False(t, isTextMention(&pushPayloadReq{MsgType: "richtext"}))
}

// TestMentionEntitiesSurviveAisExpansion is the integration-seam guard: the way
// entities reach the wire is handlePush's `CloneForExpansion` → `ExpandAisToBotUIDs`
// chain. This proves entities pass through that chain verbatim (offset/length/uid
// unchanged) even when ais-expansion appends bot UIDs to mention.uids, AND that the
// clone isolates the original payload (no in-place mutation). No DB/WuKongIM needed —
// it locks the only transformation between finalizeEntities and the serialized wire.
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

// TestDecodeMentionWithEntities pins that adding the Entities field keeps the
// lenient decode contract: valid entities decode; a malformed entities VALUE
// (non-array) degrades the whole mention to not-ok (message still delivered),
// while a malformed single entity is handled later by decodeEntities, not here.
func TestDecodeMentionWithEntities(t *testing.T) {
	mr, ok := decodeMention(json.RawMessage(
		`{"uids":["a"],"entities":[{"uid":"a","offset":0,"length":2}]}`))
	require.True(t, ok)
	assert.Equal(t, []string{"a"}, mr.Uids)
	require.Len(t, mr.Entities, 1)

	// entities as a non-array scalar -> whole mention fails to decode (acceptance
	// #6: degrade to no-mention, never 400). decodeEntities never even runs.
	_, ok = decodeMention(json.RawMessage(`{"uids":["a"],"entities":5}`))
	assert.False(t, ok)
}
