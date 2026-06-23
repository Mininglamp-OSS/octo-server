package incomingwebhook

import (
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/mentionrewrite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mention 核心逻辑的纯单测（无 DB/Redis/IM 依赖）：装配决策矩阵、去重/上限、布尔助手、
// native body 解析，以及【与 ExpandAisToBotUIDs 的 parity】——装配出的线协议形态必须能被
// 真实展开器消费。IO（成员闸 filterGroupMembers / fetchBotMemberUIDs）由集成测试覆盖。

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

// TestParseNativePushMention verifies the `mention` field is captured as raw JSON
// by the native parser and — crucially — that a MALFORMED mention never fails the
// parse (acceptance #6: it must degrade to "no mention", not 400 the whole push).
func TestParseNativePushMention(t *testing.T) {
	t.Run("valid mention captured raw and decodes", func(t *testing.T) {
		req, skip, invalid := parseNativePush(nil,
			[]byte(`{"content":"hi","mention":{"uids":["u1","u2"],"all":true,"bots":true}}`))
		require.NotNil(t, req)
		assert.Empty(t, skip)
		assert.Empty(t, invalid)
		mr, ok := decodeMention(req.Mention)
		require.True(t, ok)
		assert.Equal(t, []string{"u1", "u2"}, mr.Uids)
		assert.True(t, mr.All)
		assert.True(t, mr.Bots)
	})
	t.Run("mention absent -> empty raw, decode not ok (backward compatible)", func(t *testing.T) {
		req, _, invalid := parseNativePush(nil, []byte(`{"content":"hi"}`))
		require.NotNil(t, req)
		assert.Empty(t, invalid)
		assert.Empty(t, req.Mention)
		_, ok := decodeMention(req.Mention)
		assert.False(t, ok)
	})
	t.Run("malformed mention does NOT fail the parse (acceptance #6)", func(t *testing.T) {
		for _, body := range []string{
			`{"content":"hi","mention":"please"}`,
			`{"content":"hi","mention":{"uids":"alice"}}`,
			`{"content":"hi","mention":{"uids":[1,2]}}`,
			`{"content":"hi","mention":{"all":1}}`,
			`{"content":"hi","mention":[]}`,
		} {
			req, _, invalid := parseNativePush(nil, []byte(body))
			require.NotNilf(t, req, "body=%s", body)
			assert.Emptyf(t, invalid, "malformed mention must not invalidate the parse: %s", body)
			_, ok := decodeMention(req.Mention)
			assert.Falsef(t, ok, "malformed mention must decode as not-ok: %s", body)
		}
	})
}

// TestDecodeMention pins the lenient decode contract used by buildMention.
func TestDecodeMention(t *testing.T) {
	_, ok := decodeMention(nil)
	assert.False(t, ok, "absent → not ok")

	mr, ok := decodeMention(json.RawMessage(`null`))
	assert.True(t, ok, "explicit null is valid JSON → ok with zero value")
	assert.Nil(t, mr.Uids)

	mr, ok = decodeMention(json.RawMessage(`{"uids":["a"],"all":true}`))
	require.True(t, ok)
	assert.Equal(t, []string{"a"}, mr.Uids)
	assert.True(t, mr.All)

	for _, bad := range []string{`"x"`, `[]`, `{"uids":3}`, `{"uids":["a",1]}`, `{"all":1}`} {
		_, ok := decodeMention(json.RawMessage(bad))
		assert.Falsef(t, ok, "malformed %s → not ok", bad)
	}
}

// TestOnlyNativeAdapterAllowsMention pins acceptance #4: mention is processed only
// by the native adapter; every sibling adapter must keep allowMention=false so a
// future edit to a platform adapter can't silently start emitting mentions.
func TestOnlyNativeAdapterAllowsMention(t *testing.T) {
	assert.True(t, nativeAdapter.allowMention, "native adapter must process mention")
	for name, ad := range map[string]pushAdapter{
		"wecom":   wecomAdapter,
		"github":  githubAdapter,
		"gitlab":  gitlabAdapter,
		"feishu":  feishuAdapter,
		"multica": multicaAdapter,
	} {
		assert.Falsef(t, ad.allowMention, "%s adapter must NOT process mention (acceptance #4)", name)
	}
}
