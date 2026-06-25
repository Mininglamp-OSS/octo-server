package incomingwebhook

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/pkg/mentionrewrite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 广播补文案的测试：把获批的 canonical 广播字面量(@所有人/@所有AI)前置到 content，使三端
// 渲染广播气泡。核心 compose 是纯函数（无 DB）；buildMention 的「仅 text 路径 + 同条件 +
// 返回改写后 content」的接线用 bare-w 单测覆盖（广播 only 不配 uid → 不触成员闸，无需 infra）。
// 广播由 webhook 配置的 AllowMention* 开关驱动（push body 不再传 all/bots）。

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

// TestBuildMentionBroadcastCompose covers the buildMention wiring for broadcast-only
// webhooks (no configured uids → the group-member gate is never queried, so a bare
// *IncomingWebhook suffices — no DB/Redis/IM). Broadcast is driven by the webhook's
// AllowMention* switches (the push body no longer carries all/bots); compose runs
// only on the text path and only for a switch that survives the capability+policy
// gate; the (possibly rewritten) content is returned for the caller to write back.
func TestBuildMentionBroadcastCompose(t *testing.T) {
	w := &IncomingWebhook{Log: log.NewTLog("test")}
	req := func() *pushPayloadReq { return &pushPayloadReq{Content: "deploy done"} }

	t.Run("switch all on + permitted -> content prepended, humans=1", func(t *testing.T) {
		m := &incomingWebhookModel{WebhookID: "iwh_t", GroupNo: "g", AllowMentionAll: 1}
		mention, content, ignored := w.buildMention(m, req(), true)
		assert.Equal(t, "@所有人 deploy done", content)
		require.NotNil(t, mention)
		assert.Equal(t, 1, mention[mentionrewrite.HumansKey])
		assert.Empty(t, ignored)
	})

	t.Run("both switches on + permitted -> humans-first prefix, humans+ais set", func(t *testing.T) {
		m := &incomingWebhookModel{WebhookID: "iwh_t", GroupNo: "g", AllowMentionAll: 1, AllowMentionBots: 1}
		mention, content, _ := w.buildMention(m, req(), true)
		assert.Equal(t, "@所有人 @所有AI deploy done", content)
		assert.Equal(t, 1, mention[mentionrewrite.HumansKey])
		assert.Equal(t, 1, mention[mentionrewrite.AIsKey])
	})

	t.Run("switches on but broadcast not permitted -> no prepend, both reported ignored", func(t *testing.T) {
		m := &incomingWebhookModel{WebhookID: "iwh_t", GroupNo: "g", AllowMentionAll: 1, AllowMentionBots: 1}
		mention, content, ignored := w.buildMention(m, req(), false)
		assert.Equal(t, "deploy done", content)
		assert.Nil(t, mention)
		assert.ElementsMatch(t, []string{"all", "bots"}, ignored)
	})

	t.Run("switches off -> no prepend, no ignored (nothing wanted)", func(t *testing.T) {
		off := &incomingWebhookModel{WebhookID: "iwh_off", GroupNo: "g"} // AllowMention* default 0
		mention, content, ignored := w.buildMention(off, req(), true)
		assert.Equal(t, "deploy done", content)
		assert.Nil(t, mention)
		assert.Empty(t, ignored)
	})

	t.Run("richtext path -> content never composed, flags still assembled", func(t *testing.T) {
		m := &incomingWebhookModel{WebhookID: "iwh_t", GroupNo: "g", AllowMentionAll: 1, AllowMentionBots: 1}
		rt := &pushPayloadReq{MsgType: msgTypeRichText, Content: "deploy done"}
		mention, content, _ := w.buildMention(m, rt, true)
		assert.Equal(t, "deploy done", content, "richtext has no top-level content to compose")
		assert.Equal(t, 1, mention[mentionrewrite.HumansKey])
		assert.Equal(t, 1, mention[mentionrewrite.AIsKey])
	})

	t.Run("idempotent: content already has @所有人 -> not duplicated", func(t *testing.T) {
		m := &incomingWebhookModel{WebhookID: "iwh_t", GroupNo: "g", AllowMentionAll: 1}
		rq := &pushPayloadReq{Content: "@所有人 ship it"}
		_, content, _ := w.buildMention(m, rq, true)
		assert.Equal(t, "@所有人 ship it", content)
		assert.Equal(t, 1, strings.Count(content, broadcastTokenAll))
	})
}
