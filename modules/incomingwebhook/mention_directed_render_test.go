package incomingwebhook

import (
	"encoding/json"
	"fmt"
	"testing"
	"unicode/utf16"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/mentionrewrite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 定向 @ 昵称渲染（#448 ① b, mention.render）的测试：把【本群成员】uid 解析成展示昵称、
// 前置 "@<昵称> " 并【生成】对应 entity。compose 是纯函数(给定 namesByUID,无 DB);buildMention
// 的「成员闸取昵称 → 渲染」端到端走 infra(种 group_member + user 行,LEFT JOIN user.name)。

func TestComposeMentionContentDirected(t *testing.T) {
	names := map[string]string{"u1": "我的天", "u2": "Bob", "u3": ""}

	t.Run("single member -> @name prepended + entity generated (UTF-16)", func(t *testing.T) {
		c, n, ents := composeMentionContent("执行吧", false, false, false, false, true, []string{"u1"}, names, 0)
		assert.Equal(t, "@我的天 执行吧", c)
		assert.Equal(t, 5, n) // "@我的天 " = @我的天(4) + space(1) = 5 UTF-16 code units
		require.Len(t, ents, 1)
		assert.Equal(t, map[string]interface{}{entityKeyUID: "u1", entityKeyOffset: 0, entityKeyLength: 4}, ents[0])
	})

	t.Run("multiple members -> caller order, each offset anchors its own '@'", func(t *testing.T) {
		c, n, ents := composeMentionContent("hi", false, false, false, false, true, []string{"u1", "u2"}, names, 0)
		assert.Equal(t, "@我的天 @Bob hi", c)
		assert.Equal(t, utf16Len("@我的天 @Bob "), n)
		require.Len(t, ents, 2)
		assert.Equal(t, map[string]interface{}{entityKeyUID: "u1", entityKeyOffset: 0, entityKeyLength: 4}, ents[0])
		assert.Equal(t, map[string]interface{}{entityKeyUID: "u2", entityKeyOffset: 5, entityKeyLength: 4}, ents[1])
		u16 := utf16.Encode([]rune(c))
		assert.Equal(t, uint16('@'), u16[0])
		assert.Equal(t, uint16('@'), u16[5])
	})

	t.Run("non-member / empty name skipped (never composes '@ ')", func(t *testing.T) {
		// u3 has empty name; "uX" is absent from the map (non-member) -> both skipped, only u1 renders.
		c, n, ents := composeMentionContent("body", false, false, false, false, true, []string{"u3", "uX", "u1"}, names, 0)
		assert.Equal(t, "@我的天 body", c)
		assert.Equal(t, 5, n)
		require.Len(t, ents, 1)
		assert.Equal(t, "u1", ents[0].(map[string]interface{})[entityKeyUID])
	})

	t.Run("idempotent: @name already in content -> not re-prepended", func(t *testing.T) {
		c, n, ents := composeMentionContent("cc @我的天 ok", false, false, false, false, true, []string{"u1"}, names, 0)
		assert.Equal(t, "cc @我的天 ok", c)
		assert.Equal(t, 0, n)
		assert.Nil(t, ents)
	})

	t.Run("same uid twice -> one pill (dedup)", func(t *testing.T) {
		c, _, ents := composeMentionContent("x", false, false, false, false, true, []string{"u1", "u1"}, names, 0)
		assert.Equal(t, "@我的天 x", c)
		require.Len(t, ents, 1)
	})

	t.Run("render off -> no compose, no entities", func(t *testing.T) {
		c, n, ents := composeMentionContent("y", false, false, false, false, false, []string{"u1"}, names, 0)
		assert.Equal(t, "y", c)
		assert.Equal(t, 0, n)
		assert.Nil(t, ents)
	})

	t.Run("broadcast + directed compose together: broadcast first, directed offset accounts for it", func(t *testing.T) {
		c, n, ents := composeMentionContent("go", true, false, true, false, true, []string{"u1"}, names, 0)
		assert.Equal(t, "@所有人 @我的天 go", c)
		assert.Equal(t, utf16Len("@所有人 @我的天 "), n)
		require.Len(t, ents, 1)
		assert.Equal(t, map[string]interface{}{entityKeyUID: "u1", entityKeyOffset: 5, entityKeyLength: 4}, ents[0])
		u16 := utf16.Encode([]rune(c))
		assert.Equal(t, uint16('@'), u16[5], "directed '@' anchors correctly after the broadcast prefix")
	})

	t.Run("budget: stop adding @names before composed content exceeds maxRunes", func(t *testing.T) {
		nm := map[string]string{"a": "AAAA", "b": "BBBB"} // each "@AAAA " = 6 runes
		// content "body" = 4 runes; maxRunes 10 fits exactly one "@AAAA " (6) + body (4); the 2nd breaks.
		c, _, ents := composeMentionContent("body", false, false, false, false, true, []string{"a", "b"}, nm, 10)
		require.Len(t, ents, 1, "only the first @name fits the budget; the rest still route via mention.uids")
		assert.Equal(t, "@AAAA body", c)
	})

	t.Run("broadcast-like names skipped (exact + boundary + embedded '@'); real names still render", func(t *testing.T) {
		// Skipped: exact label (所有人 / All AIs), label + non-word boundary (所有人 X / 所有人: / all-hands —
		// iOS @-token scanning would emit a standalone @所有人 / @all broadcast token), and any name with '@'.
		// Rendered: a label that continues into a longer word (所有人事部 = HR dept; allen) is a real name.
		nm := map[string]string{
			"a": "所有人", "b": "所有人 X", "c": "所有人:", "d": "All AIs", "e": "@x", "f": "all-hands",
			"g": "所有人事部", "h": "allen",
		}
		c, _, ents := composeMentionContent("body", false, false, false, false, true,
			[]string{"a", "b", "c", "d", "e", "f", "g", "h"}, nm, 0)
		assert.Equal(t, "@所有人事部 @allen body", c, "only non-broadcast-like names render, in caller order")
		require.Len(t, ents, 2)
		assert.Equal(t, "g", ents[0].(map[string]interface{})[entityKeyUID])
		assert.Equal(t, "h", ents[1].(map[string]interface{})[entityKeyUID])
	})

	t.Run("invisible/bidi/full-width confusables: folded for the guard, stripped from the pill", func(t *testing.T) {
		// zwsp/rlo are built via rune() so there are no literal invisible bytes in source. A ZWSP *inside*
		// a label, an RLO bidi prefix, and a full-width spelling all fold to a broadcast label and are
		// skipped; a real name still renders but with the invisible rune stripped from the pill.
		zwsp, rlo := string(rune(0x200B)), string(rune(0x202E))
		nm := map[string]string{
			"a": "所有" + zwsp + "人", // ZWSP (U+200B) inside the label -> folds to 所有人 -> skipped
			"b": rlo + "所有AI",      // RLO (U+202E) bidi prefix -> 所有AI -> skipped
			"c": "ａｌｌ",             // full-width letters -> NFKC "all" -> skipped
			"d": "Bob" + zwsp,      // ZWSP in a real name -> renders, invisible stripped from the pill
		}
		c, _, ents := composeMentionContent("body", false, false, false, false, true,
			[]string{"a", "b", "c", "d"}, nm, 0)
		assert.Equal(t, "@Bob body", c, "confusable broadcast-likes skipped; real name renders with the invisible rune stripped")
		require.Len(t, ents, 1)
		e := ents[0].(map[string]interface{})
		assert.Equal(t, "d", e[entityKeyUID])
		assert.Equal(t, 4, e[entityKeyLength]) // "@Bob" = 4 UTF-16 (ZWSP gone)
	})
}

// TestBuildMentionDirectedRender is the end-to-end seam (needs MySQL): a bare uid + render:true
// resolves the member's display name (group_member LEFT JOIN user.name), prepends "@<name> ", and
// generates the entity — asserting buildMention's returned content/mention directly.
func TestBuildMentionDirectedRender(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	w := newIncomingWebhook(ctx)

	const groupNo = "g_render"
	const uid = "u_render_bot"
	const name = "我的天"
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO group_member(group_no, uid, role, status, is_deleted, version) VALUES(?, ?, 0, 1, 0, 1)",
		groupNo, uid).Exec()
	require.NoError(t, err)
	// display name resolves from user.name via the gate's LEFT JOIN.
	_, err = ctx.DB().InsertInto("user").Columns("uid", "name", "username", "short_no", "status").
		Values(uid, name, uid, "sn_"+uid, 1).Exec()
	require.NoError(t, err)

	m := &incomingWebhookModel{WebhookID: "iwh_render", GroupNo: groupNo}

	t.Run("uid + render:true -> @name composed + entity generated; uid still routes", func(t *testing.T) {
		req := &pushPayloadReq{
			Content: "执行吧",
			Mention: json.RawMessage(fmt.Sprintf(`{"uids":["%s"],"render":true}`, uid)),
		}
		// render is NOT capability-gated (directed, not broadcast) -> broadcastPermitted=false is fine.
		mention, content, ignored := w.buildMention(m, req, false)
		require.NotNil(t, mention)
		assert.Empty(t, ignored)
		assert.Equal(t, "@我的天 执行吧", content)

		uidsOut, _ := mention[mentionrewrite.UIDsKey].([]interface{})
		assert.Contains(t, uidsOut, uid, "uid still carried for routing/red-dot")

		ents, ok := mention[mentionrewrite.EntitiesKey].([]interface{})
		require.Truef(t, ok, "entities generated; mention=%v", mention)
		require.Len(t, ents, 1)
		e := ents[0].(map[string]interface{})
		assert.Equal(t, uid, e[entityKeyUID])
		assert.Equal(t, 0, e[entityKeyOffset])
		assert.Equal(t, 4, e[entityKeyLength]) // "@我的天" = 4 UTF-16
		u16 := utf16.Encode([]rune(content))
		assert.Equal(t, uint16('@'), u16[0])
	})

	t.Run("render + caller entities -> caller entities win, no auto-generate, no prepend", func(t *testing.T) {
		// caller wrote their own "@x" + supplied the entity; #449 path is authoritative -> render is ignored.
		req := &pushPayloadReq{
			Content: "@x hi",
			Mention: json.RawMessage(fmt.Sprintf(
				`{"uids":["%s"],"render":true,"entities":[{"uid":"%s","offset":0,"length":2}]}`, uid, uid)),
		}
		mention, content, _ := w.buildMention(m, req, false)
		require.NotNil(t, mention)
		assert.Equal(t, "@x hi", content, "caller entities present -> render does not prepend names")
		ents, ok := mention[mentionrewrite.EntitiesKey].([]interface{})
		require.True(t, ok)
		require.Len(t, ents, 1)
		e := ents[0].(map[string]interface{})
		assert.Equal(t, 0, e[entityKeyOffset], "caller offset preserved (no prepend, no shift)")
		assert.Equal(t, 2, e[entityKeyLength])
	})

	t.Run("render:true but uid is a non-member -> no pill, nothing routed", func(t *testing.T) {
		req := &pushPayloadReq{
			Content: "执行吧",
			Mention: json.RawMessage(`{"uids":["ghost_not_member"],"render":true}`),
		}
		mention, content, ignored := w.buildMention(m, req, false)
		assert.Equal(t, "执行吧", content, "non-member resolves no name -> no compose")
		assert.Nil(t, mention)
		assert.Empty(t, ignored)
	})
}

// TestAssemblePushPayload_DirectedRenderReachesWire is the handler→wire seam (needs MySQL, no
// WuKongIM): it asserts the composed content + generated entities actually land in the payload
// map that handlePush hands to SendMessageWithResult — closing the wire-bytes gap reviewers
// flagged on #450 (buildMention's return was asserted one layer below this).
func TestAssemblePushPayload_DirectedRenderReachesWire(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	w := newIncomingWebhook(ctx)

	const groupNo = "g_wire"
	const uid = "u_wire_bot"
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO group_member(group_no, uid, role, status, is_deleted, version) VALUES(?, ?, 0, 1, 0, 1)",
		groupNo, uid).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertInto("user").Columns("uid", "name", "username", "short_no", "status").
		Values(uid, "我的天", uid, "sn_"+uid, 1).Exec()
	require.NoError(t, err)

	m := &incomingWebhookModel{WebhookID: "iwh_wire", GroupNo: groupNo}
	req := &pushPayloadReq{
		Content: "执行吧",
		Mention: json.RawMessage(fmt.Sprintf(`{"uids":["%s"],"render":true}`, uid)),
	}
	// base payload mirrors the text-path payload handlePush builds before mention assembly.
	base := map[string]interface{}{payloadContentKey: req.Content}

	payload, ignored := w.assemblePushPayload(m, req, base, false)
	assert.Empty(t, ignored)

	// composed content reached the wire payload under the shared content key.
	assert.Equal(t, "@我的天 执行吧", payload[payloadContentKey])

	mention, ok := payload[mentionrewrite.MentionKey].(map[string]interface{})
	require.Truef(t, ok, "mention attached to payload; got %v", payload)
	uids, _ := mention[mentionrewrite.UIDsKey].([]interface{})
	assert.Contains(t, uids, uid, "uid carried for routing")
	ents, ok := mention[mentionrewrite.EntitiesKey].([]interface{})
	require.True(t, ok, "generated entities reached the wire")
	require.Len(t, ents, 1)
	e := ents[0].(map[string]interface{})
	assert.Equal(t, uid, e[entityKeyUID])
	assert.Equal(t, 0, e[entityKeyOffset])
	assert.Equal(t, 4, e[entityKeyLength])
}
