package incomingwebhook

import (
	"errors"
	"os"
	"strconv"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/richtext"
)

// 推送消息形态（pushPayloadReq.MsgType 取值）。缺省与 "text" 等价，保持历史纯文本
// 契约不变；"richtext" 走图文混排 blocks 路径。
const (
	msgTypeText     = "text"
	msgTypeRichText = "richtext"
)

// 富文本 block 数量上限。8KB 的 body cap 已是天然约束，这里再加一道显式上限，避免
// 调用方用海量空块构造病态 payload（每块都要进 Validate 遍历）。可经 env 调整。
const (
	envMaxBlocks     = "DM_INCOMINGWEBHOOK_MAX_BLOCKS"
	defaultMaxBlocks = 50
)

// errTooManyBlocks blocks 数量超过 maxBlocks 上限。映射为 400 invalid（reason=blocks），
// 而非 413：413 语义是「字节/体积过大」，块数超限是结构性非法，归 invalid 更准确。
var errTooManyBlocks = errors.New("incomingwebhook: too many richtext blocks")

func maxBlocks() int {
	if v := os.Getenv(envMaxBlocks); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxBlocks
}

// buildRichTextPayload 把对外的 webhookBlock 数组翻译为 octo 原生 RichText(=14)
// 消息 payload，并在发送前做权威校验/收尾：
//
//   - 逐块按白名单复制字段（text 块只取 text；image 块只取 url/width/height），
//     绝不透传调用方的其它任意字段——与 buildPayload 丢弃 req.Extra 的安全基线一致；
//   - from.kind=webhook + 服务端派生的 space_id 注入方式与纯文本路径完全相同，
//     保证客户端识别 webhook 发送者、且消息不可被伪造到其它 Space；
//   - richtext.Validate 跑 write-strict 校验（缺/空 content、空 text 块、非 http(s)
//     图片 URL、缺图片宽高、未知 block 类型、>1MB 等一律拒绝），是块合法性的权威闸；
//   - richtext.Finalize 在注入完所有服务端字段后用 content 重算权威顶层 plain（供
//     搜索/推送/摘要/复制复用），并对真正出站的完整 payload 复检 1MB 上限。
//
// 校验失败返回错误，由 push 路径映射：common.ErrRichTextPayloadTooLarge → 413，
// 其余（含 errTooManyBlocks 与 Validate 的各类结构错误）→ 400 invalid(reason=blocks)。
func buildRichTextPayload(m *incomingWebhookModel, req *pushPayloadReq, allowOverride bool) (map[string]interface{}, error) {
	if len(req.Blocks) > maxBlocks() {
		return nil, errTooManyBlocks
	}
	// 空 blocks 不在此处单独报错：构造空 content 交由 richtext.Validate 统一拦
	// （common.ErrRichTextEmptyContent），保持「块合法性单一权威 = Validate」。
	content := make([]map[string]interface{}, 0, len(req.Blocks))
	for _, b := range req.Blocks {
		switch b.Type {
		case common.RichTextBlockText:
			content = append(content, map[string]interface{}{
				"type": common.RichTextBlockText,
				"text": b.Text,
			})
		case common.RichTextBlockImage:
			content = append(content, map[string]interface{}{
				"type":   common.RichTextBlockImage,
				"url":    b.URL,
				"width":  b.Width,
				"height": b.Height,
			})
		default:
			// 未知块类型：原样带上 type 交给 Validate 拒绝（ErrRichTextUnknownBlock），
			// 不在此处复制任何其它字段，避免未知块夹带任意字段。
			content = append(content, map[string]interface{}{"type": b.Type})
		}
	}

	name, avatar := resolveFromIdentity(m, req, allowOverride)
	payload := map[string]interface{}{
		"type":    int(common.RichText),
		"content": content,
		"from": map[string]interface{}{
			"kind":       extraKindValue,
			"webhook_id": m.WebhookID,
			"name":       name,
			"avatar":     avatar,
		},
		// space_id 由服务端从 group 派生，不接受调用方覆盖（与 buildPayload 一致）。
		"space_id": m.SpaceID,
	}

	// Validate（入站 write-strict）→ Finalize（注入后重算 plain + 完整 payload 复检
	// 1MB）。push 路径无后续 enrich，两步紧邻即可。
	if err := richtext.Validate(payload); err != nil {
		return nil, err
	}
	if err := richtext.Finalize(payload); err != nil {
		return nil, err
	}
	return payload, nil
}
