package incomingwebhook

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/mentionrewrite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 广播补文案（#448 item ②）的测试：把获批的 canonical 广播字面量(@所有人/@所有AI)前置到
// content，使三端渲染广播气泡。核心 compose / offset-shift 是纯函数（无 DB）；buildMention
// 的「仅 text 路径 + 同条件 + 返回改写后 content」的接线用 bare-w 单测覆盖（广播 only 不触
// 成员闸，无需 infra）；广播与定向 entities 共存的 offset 右移用一条 infra 集成测试钉死。

func TestComposeBroadcastContent(t *testing.T) {
	const content = "deploy done"
	// broadcast-only compose (render off): assert no entities are generated, return (content, prefixLen).
	compose := func(c string, all, bots, allowAll, allowBots bool) (string, int) {
		got, n, ents := composeMentionContent(c, all, bots, allowAll, allowBots, false, nil, nil, 0)
		require.Nil(t, ents, "broadcast-only compose must not generate entities")
		return got, n
	}

	t.Run("permitted all, no literal -> prepend @所有人 + space (5 utf16)", func(t *testing.T) {
		got, n := compose(content, true, false, true, false)
		assert.Equal(t, "@所有人 "+content, got)
		assert.Equal(t, 5, n) // @所有人 + space = 5 UTF-16 code units
		assert.True(t, strings.HasPrefix(got, broadcastTokenAll+broadcastTokenSep))
	})

	t.Run("permitted bots, no literal -> prepend @所有AI + space (6 utf16)", func(t *testing.T) {
		got, n := compose(content, false, true, false, true)
		assert.Equal(t, "@所有AI "+content, got)
		assert.Equal(t, 6, n)
	})

	t.Run("both permitted -> humans first then ais (11 utf16)", func(t *testing.T) {
		got, n := compose(content, true, true, true, true)
		assert.Equal(t, "@所有人 @所有AI "+content, got)
		assert.Equal(t, 11, n)
	})

	t.Run("wanted but not permitted -> no prepend, zero shift", func(t *testing.T) {
		got, n := compose(content, true, true, false, false)
		assert.Equal(t, content, got)
		assert.Equal(t, 0, n)
	})

	t.Run("permitted but not wanted -> no prepend", func(t *testing.T) {
		got, n := compose(content, false, false, true, true)
		assert.Equal(t, content, got)
		assert.Equal(t, 0, n)
	})

	t.Run("idempotent: literal already present anywhere -> token not re-prepended", func(t *testing.T) {
		c := "ping @所有人 now"
		got, n := compose(c, true, false, true, false)
		assert.Equal(t, c, got)
		assert.Equal(t, 0, n)
	})

	t.Run("idempotent per token: all present, ais absent -> prepend only @所有AI", func(t *testing.T) {
		c := "ping @所有人 now"
		got, n := compose(c, true, true, true, true)
		assert.Equal(t, "@所有AI "+c, got)
		assert.Equal(t, 6, n)
		assert.Equal(t, 1, strings.Count(got, broadcastTokenAll), "existing @所有人 not duplicated")
	})

	t.Run("no-op returns original (byte-identical, backward compat)", func(t *testing.T) {
		got, n := compose(content, false, false, false, false)
		assert.Equal(t, content, got)
		assert.Equal(t, 0, n)
	})
}

func TestShiftEntityOffsets(t *testing.T) {
	mk := func(uid string, off, length int) map[string]interface{} {
		return map[string]interface{}{entityKeyUID: uid, entityKeyOffset: off, entityKeyLength: length}
	}

	t.Run("shifts every offset by N, leaves uid/length", func(t *testing.T) {
		ents := []interface{}{mk("u1", 0, 3), mk("u2", 4, 3)}
		shiftEntityOffsets(ents, 5)
		assert.Equal(t, 5, ents[0].(map[string]interface{})[entityKeyOffset])
		assert.Equal(t, 9, ents[1].(map[string]interface{})[entityKeyOffset])
		assert.Equal(t, 3, ents[0].(map[string]interface{})[entityKeyLength])
		assert.Equal(t, "u1", ents[0].(map[string]interface{})[entityKeyUID])
	})

	t.Run("zero / negative shift is a no-op", func(t *testing.T) {
		ents := []interface{}{mk("u1", 2, 3)}
		shiftEntityOffsets(ents, 0)
		shiftEntityOffsets(ents, -1)
		assert.Equal(t, 2, ents[0].(map[string]interface{})[entityKeyOffset])
	})

	t.Run("nil / empty safe", func(t *testing.T) {
		shiftEntityOffsets(nil, 5)
		shiftEntityOffsets([]interface{}{}, 5)
	})
}

// TestBuildMentionBroadcastCompose covers the buildMention wiring for broadcast-only
// pushes (no uids/entities → the group-member gate is never queried, so a bare *IncomingWebhook
// suffices — no DB/Redis/IM needed): compose runs only on the text path and only for a flag that
// survives the capability gate, and the (possibly rewritten) content is returned for the caller
// to write back.
func TestBuildMentionBroadcastCompose(t *testing.T) {
	w := &IncomingWebhook{Log: log.NewTLog("test")}
	model := &incomingWebhookModel{WebhookID: "iwh_t", GroupNo: "g", AllowMentionAll: 1, AllowMentionBots: 1}
	newReq := func(mention string) *pushPayloadReq {
		return &pushPayloadReq{Content: "deploy done", Mention: json.RawMessage(mention)}
	}

	t.Run("permitted all -> content prepended, humans=1", func(t *testing.T) {
		mention, content, ignored := w.buildMention(model, newReq(`{"all":true}`), true)
		assert.Equal(t, "@所有人 deploy done", content)
		require.NotNil(t, mention)
		assert.Equal(t, 1, mention[mentionrewrite.HumansKey])
		assert.Empty(t, ignored)
	})

	t.Run("permitted both -> humans-first prefix, humans+ais set", func(t *testing.T) {
		mention, content, _ := w.buildMention(model, newReq(`{"all":true,"bots":true}`), true)
		assert.Equal(t, "@所有人 @所有AI deploy done", content)
		assert.Equal(t, 1, mention[mentionrewrite.HumansKey])
		assert.Equal(t, 1, mention[mentionrewrite.AIsKey])
	})

	t.Run("broadcast not permitted -> no prepend, both reported ignored", func(t *testing.T) {
		mention, content, ignored := w.buildMention(model, newReq(`{"all":true,"bots":true}`), false)
		assert.Equal(t, "deploy done", content)
		assert.Nil(t, mention)
		assert.ElementsMatch(t, []string{"all", "bots"}, ignored)
	})

	t.Run("capability bit off -> no prepend even when policy permits", func(t *testing.T) {
		off := &incomingWebhookModel{WebhookID: "iwh_off", GroupNo: "g"} // AllowMention* default 0
		_, content, ignored := w.buildMention(off, newReq(`{"all":true}`), true)
		assert.Equal(t, "deploy done", content)
		assert.Equal(t, []string{"all"}, ignored)
	})

	t.Run("richtext path -> content never composed, flags still assembled", func(t *testing.T) {
		req := &pushPayloadReq{MsgType: msgTypeRichText, Content: "deploy done",
			Mention: json.RawMessage(`{"all":true,"bots":true}`)}
		mention, content, _ := w.buildMention(model, req, true)
		assert.Equal(t, "deploy done", content, "richtext has no top-level content to compose")
		assert.Equal(t, 1, mention[mentionrewrite.HumansKey])
		assert.Equal(t, 1, mention[mentionrewrite.AIsKey])
	})

	t.Run("idempotent: caller already wrote @所有人 -> not duplicated", func(t *testing.T) {
		req := &pushPayloadReq{Content: "@所有人 ship it", Mention: json.RawMessage(`{"all":true}`)}
		_, content, _ := w.buildMention(model, req, true)
		assert.Equal(t, "@所有人 ship it", content)
		assert.Equal(t, 1, strings.Count(content, broadcastTokenAll))
	})

	t.Run("no broadcast flags -> content byte-identical (backward compat)", func(t *testing.T) {
		_, content, ignored := w.buildMention(model, newReq(`{}`), true)
		assert.Equal(t, "deploy done", content)
		assert.Empty(t, ignored)
	})

	t.Run("malformed mention -> content unchanged, nil mention, no panic", func(t *testing.T) {
		req := &pushPayloadReq{Content: "deploy done", Mention: json.RawMessage(`"garbage"`)}
		mention, content, ignored := w.buildMention(model, req, true)
		assert.Nil(t, mention)
		assert.Equal(t, "deploy done", content)
		assert.Empty(t, ignored)
	})
}

// TestBuildMentionBroadcastShiftsEntities is the integration seam (needs MySQL): when a broadcast
// prepend and a directed entity (#449) coexist, the entity offset must shift by the prefix's UTF-16
// length so web/Android keep binding the right member. Asserts buildMention's returned content +
// mention directly — no WuKongIM read-back (the env does not register subscribers; see #449).
func TestBuildMentionBroadcastShiftsEntities(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	w := newIncomingWebhook(ctx)

	const groupNo = "g_bcast"
	const uid = "u_member_bcast"
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO group_member(group_no, uid, role, status, is_deleted, version) VALUES(?, ?, 0, 1, 0, 1)",
		groupNo, uid).Exec()
	require.NoError(t, err)

	m := &incomingWebhookModel{WebhookID: "iwh_bcast", GroupNo: groupNo, AllowMentionAll: 1}
	// content "@张三 hi": @(0)张(1)三(2) space(3) h(4)i(5); entity offset0 length3 -> "@张三".
	req := &pushPayloadReq{
		Content: "@张三 hi",
		Mention: json.RawMessage(fmt.Sprintf(
			`{"all":true,"uids":["%s"],"entities":[{"uid":"%s","offset":0,"length":3}]}`, uid, uid)),
	}
	mention, content, ignored := w.buildMention(m, req, true)
	require.NotNil(t, mention)
	assert.Empty(t, ignored)

	// 广播补文案前置 "@所有人 "（5 UTF-16 码元）。
	assert.Equal(t, "@所有人 @张三 hi", content)
	assert.Equal(t, 1, mention[mentionrewrite.HumansKey])

	// 定向 entity 的 offset 由 0 右移到 5，正好指向最终 content 里 @张三 的 '@'。
	ents, ok := mention[mentionrewrite.EntitiesKey].([]interface{})
	require.Truef(t, ok, "entities present; mention=%v", mention)
	require.Len(t, ents, 1)
	e := ents[0].(map[string]interface{})
	assert.Equal(t, uid, e[entityKeyUID])
	assert.Equal(t, 5, e[entityKeyOffset])
	assert.Equal(t, 3, e[entityKeyLength])
	u16 := utf16.Encode([]rune(content))
	require.Greater(t, len(u16), 5)
	assert.Equal(t, uint16('@'), u16[5], "shifted offset must still anchor a '@' in the final content")
}
