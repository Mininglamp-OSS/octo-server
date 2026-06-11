package incomingwebhook

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 这些是 Phase 1（图文混排）的纯单测：buildRichTextPayload 是无 DB 依赖的纯翻译/
// 校验函数，可本地直接 go test，不需要 MySQL/Redis/WuKongIM。push 端到端行为另由
// richtext_push_test.go 的集成用例覆盖（CI）。

func textBlock(s string) webhookBlock { return webhookBlock{Type: "text", Text: s} }
func imageBlock(u string, w, h int) webhookBlock {
	return webhookBlock{Type: "image", URL: u, Width: w, Height: h}
}

func sampleWebhook() *incomingWebhookModel {
	return &incomingWebhookModel{
		WebhookID: "iwh_abc123",
		Name:      "CI Bot",
		Avatar:    "https://example.com/ci.png",
		GroupNo:   "g_123",
		SpaceID:   "space_42",
		TokenHash: "SECRET_SHOULD_NOT_LEAK",
	}
}

// 纯文本块：翻译为 RichText(=14)，注入 from.kind=webhook + 服务端 space_id，
// plain 由 server 权威生成（== 文本内容）。
func TestBuildRichTextPayload_TextOnly(t *testing.T) {
	m := sampleWebhook()
	req := &pushPayloadReq{MsgType: msgTypeRichText, Blocks: []webhookBlock{textBlock("Build #123 passed")}}

	payload, err := buildRichTextPayload(m, req, true)
	require.NoError(t, err)

	assert.Equal(t, int(common.RichText), payload["type"])
	assert.Equal(t, m.SpaceID, payload["space_id"])
	assert.Equal(t, "Build #123 passed", payload["plain"], "server 应权威重算 plain")

	from, _ := payload["from"].(map[string]interface{})
	require.NotNil(t, from)
	assert.Equal(t, extraKindValue, from["kind"])
	assert.Equal(t, m.WebhookID, from["webhook_id"])
	assert.Equal(t, "CI Bot", from["name"])

	content, _ := payload["content"].([]map[string]interface{})
	require.Len(t, content, 1)
	assert.Equal(t, common.RichTextBlockText, content[0]["type"])
	assert.Equal(t, "Build #123 passed", content[0]["text"])
}

// 图片块：保留 url/width/height，plain 注入 [图片] 占位符。
func TestBuildRichTextPayload_Image(t *testing.T) {
	m := sampleWebhook()
	req := &pushPayloadReq{MsgType: msgTypeRichText, Blocks: []webhookBlock{
		imageBlock("https://example.com/chart.png", 800, 400),
	}}

	payload, err := buildRichTextPayload(m, req, true)
	require.NoError(t, err)

	content, _ := payload["content"].([]map[string]interface{})
	require.Len(t, content, 1)
	assert.Equal(t, common.RichTextBlockImage, content[0]["type"])
	assert.Equal(t, "https://example.com/chart.png", content[0]["url"])
	assert.Equal(t, 800, content[0]["width"])
	assert.Equal(t, 400, content[0]["height"])
	assert.Equal(t, common.RichTextImagePlaceholder, payload["plain"])
}

// 图文混排：数组顺序即穿插顺序，plain 拼接保序（文本 + [图片] + 文本）。
func TestBuildRichTextPayload_MixedOrderPreserved(t *testing.T) {
	m := sampleWebhook()
	req := &pushPayloadReq{MsgType: msgTypeRichText, Blocks: []webhookBlock{
		textBlock("before "),
		imageBlock("https://example.com/a.png", 10, 10),
		textBlock(" after"),
	}}

	payload, err := buildRichTextPayload(m, req, true)
	require.NoError(t, err)

	content, _ := payload["content"].([]map[string]interface{})
	require.Len(t, content, 3)
	assert.Equal(t, common.RichTextBlockText, content[0]["type"])
	assert.Equal(t, common.RichTextBlockImage, content[1]["type"])
	assert.Equal(t, common.RichTextBlockText, content[2]["type"])
	assert.Equal(t, "before "+common.RichTextImagePlaceholder+" after", payload["plain"])
}

// 调用方覆盖 username/avatar_url 优先于 webhook 自身配置。
func TestBuildRichTextPayload_FromOverride(t *testing.T) {
	m := sampleWebhook()
	req := &pushPayloadReq{
		MsgType:   msgTypeRichText,
		Blocks:    []webhookBlock{textBlock("hi")},
		Username:  "Override Name",
		AvatarURL: "https://example.com/override.png",
	}
	payload, err := buildRichTextPayload(m, req, true)
	require.NoError(t, err)
	from, _ := payload["from"].(map[string]interface{})
	assert.Equal(t, "Override Name", from["name"])
	assert.Equal(t, "https://example.com/override.png", from["avatar"])
}

// 调用方写 space_id 不能覆盖服务端派生值（防伪造到其它 Space）。本路径压根不读
// 调用方 space_id：webhookBlock 无该字段，且 from/space_id 全由服务端注入。
func TestBuildRichTextPayload_SpaceIDNotForgeable(t *testing.T) {
	m := sampleWebhook()
	req := &pushPayloadReq{MsgType: msgTypeRichText, Blocks: []webhookBlock{textBlock("hi")}}
	payload, err := buildRichTextPayload(m, req, true)
	require.NoError(t, err)
	assert.Equal(t, "space_42", payload["space_id"])
}

// 不泄漏 token_hash。
func TestBuildRichTextPayload_NoTokenLeak(t *testing.T) {
	m := sampleWebhook()
	req := &pushPayloadReq{MsgType: msgTypeRichText, Blocks: []webhookBlock{textBlock("hi")}}
	payload, err := buildRichTextPayload(m, req, true)
	require.NoError(t, err)
	assert.NotContains(t, util.ToJson(payload), "SECRET_SHOULD_NOT_LEAK")
}

// ---- 拒绝路径（write-strict，权威闸 = richtext.Validate） ----

func TestBuildRichTextPayload_Rejects(t *testing.T) {
	m := sampleWebhook()
	cases := []struct {
		name   string
		blocks []webhookBlock
	}{
		{"empty blocks", nil},
		{"empty text block", []webhookBlock{textBlock("   ")}},
		{"unknown block type", []webhookBlock{{Type: "video", URL: "https://x/y"}}},
		{"image missing url", []webhookBlock{{Type: "image", Width: 10, Height: 10}}},
		{"image non-http scheme", []webhookBlock{imageBlock("data:image/png;base64,AAAA", 10, 10)}},
		{"image missing size", []webhookBlock{{Type: "image", URL: "https://example.com/a.png"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildRichTextPayload(m, &pushPayloadReq{MsgType: msgTypeRichText, Blocks: tc.blocks}, true)
			assert.Error(t, err, "应拒绝非法 blocks: %s", tc.name)
		})
	}
}

// 块数超过上限 → errTooManyBlocks（映射 400 invalid，而非 413）。
func TestBuildRichTextPayload_TooManyBlocks(t *testing.T) {
	t.Setenv(envMaxBlocks, "2")
	m := sampleWebhook()
	blocks := []webhookBlock{textBlock("a"), textBlock("b"), textBlock("c")}
	_, err := buildRichTextPayload(m, &pushPayloadReq{MsgType: msgTypeRichText, Blocks: blocks}, true)
	assert.ErrorIs(t, err, errTooManyBlocks)
}

// 未知块只带 type、不夹带任意字段：未知块被 Validate 拒绝，确认不会因夹带字段而误放行。
func TestBuildRichTextPayload_UnknownBlockDropsExtraFields(t *testing.T) {
	m := sampleWebhook()
	// type=text 合法块 + 一个未知块（携带看似合法的 url/width/height）：整体必须被拒。
	blocks := []webhookBlock{textBlock("ok"), {Type: "image_v2", URL: "https://x/y", Width: 1, Height: 1}}
	_, err := buildRichTextPayload(m, &pushPayloadReq{MsgType: msgTypeRichText, Blocks: blocks}, true)
	assert.Error(t, err)
}

// resolveFromIdentity：管理员（allowOverride=true）覆盖优先、回落 webhook 配置、
// 超长裁剪到字节上限；成员/bot（allowOverride=false）覆盖一律忽略——否则管理面的
// Webhook- 前缀与头像锁会被 push 路径整体绕过（PR #340 review，yujiawei P1）。
func TestResolveFromIdentity(t *testing.T) {
	m := &incomingWebhookModel{Name: "WH", Avatar: "https://a/x.png"}

	name, avatar := resolveFromIdentity(m, &pushPayloadReq{}, true)
	assert.Equal(t, "WH", name)
	assert.Equal(t, "https://a/x.png", avatar)

	name, _ = resolveFromIdentity(m, &pushPayloadReq{Username: "Override"}, true)
	assert.Equal(t, "Override", name)

	longName := strings.Repeat("x", maxFromNameBytes+10)
	name, _ = resolveFromIdentity(m, &pushPayloadReq{Username: longName}, true)
	assert.LessOrEqual(t, len(name), maxFromNameBytes)

	// 成员/bot 的 webhook：覆盖被忽略，展示固定为存量（已带前缀的）配置。
	spoof := &pushPayloadReq{Username: "HR 公告", AvatarURL: "https://evil/ceo.png"}
	lockedM := &incomingWebhookModel{Name: "Webhook-abc123", Avatar: ""}
	name, avatar = resolveFromIdentity(lockedM, spoof, false)
	assert.Equal(t, "Webhook-abc123", name)
	assert.Equal(t, "", avatar)
}
