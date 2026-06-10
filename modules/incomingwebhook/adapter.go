package incomingwebhook

// 推送形态适配层（#297 Phase 3）。
//
// 三种推送形态共享同一条鉴权 / 限流 / 群校验 / 投递 / 审计流水线（api.go handlePush），
// 彼此只差「如何把请求 body 翻译成 native 推送请求」这一步：
//
//   - native（历史契约）   POST /v1/incoming-webhooks/:webhook_id/:token
//   - GitHub 事件          POST .../:token/github   （adapter_github.go）
//   - 企业微信群机器人格式  POST .../:token/wecom    （adapter_wecom.go）
//
// 适配器不是新的攻击面：URL token 鉴权、四层限流、群 Normal 校验、payload 白名单
// 构造（buildPayload / buildRichTextPayload 注入 from.kind=webhook 与服务端 space_id）
// 全部复用，适配器只产出 pushPayloadReq（content / blocks），不直接触达消息 payload。

import (
	"encoding/json"
	"net/http"
	"strings"
)

// pushAdapter 描述一种推送形态。
type pushAdapter struct {
	// name 写入审计 adapter 列（adapterNative / adapterGitHub / adapterWeCom）。
	name string
	// parse 把平台原始 body 翻译成 native 推送请求。三个返回值恰有一个生效：
	//   - req     非 nil：照常走 msg_type 构造 / 投递；
	//   - skip    非空：请求合法但刻意不投递（GitHub ping / 渲染子集之外的事件），
	//     返回 200 并以 auditSkipped 落审计，供管理端 deliveries 观察链路；
	//   - invalid 非空：解析失败原因码，映射 400 invalid(reason=...) 并落审计。
	parse func(header http.Header, body []byte) (req *pushPayloadReq, skip string, invalid string)
	// successExtra 合并进成功 / skip 响应体的平台兼容字段（如企业微信的 errcode /
	// errmsg），让按平台 SDK 校验响应的既有工具不改代码即可迁移。key 与 native 的
	// status / message_id 不重叠，纯追加。
	successExtra map[string]interface{}
	// bodyLimit 该形态的请求体字节上限。native / wecom 的 body 由调用方编写，沿用
	// 8KiB 的 maxBytes()——上限本就是约束调用方的；github 的 body 是平台生成的事件
	// JSON，真实 push / PR 事件普遍 >8KiB 且发送方无法修短，必须用更宽的专属上限
	//（githubMaxBytes，见 adapter_github.go；PR #330 review 阻断项）。
	bodyLimit func() int
}

var (
	nativeAdapter = pushAdapter{name: adapterNative, parse: parseNativePush, bodyLimit: maxBytes}
	githubAdapter = pushAdapter{name: adapterGitHub, parse: parseGitHubPush, bodyLimit: githubMaxBytes}
	wecomAdapter  = pushAdapter{
		name:      adapterWeCom,
		parse:     parseWeComPush,
		bodyLimit: maxBytes,
		// 企业微信调用方普遍校验 errcode==0，附带平台习惯字段降低迁移摩擦。
		successExtra: map[string]interface{}{"errcode": 0, "errmsg": "ok"},
	}
)

// parseNativePush 是 native 形态的 parse：body 即 pushPayloadReq JSON 本身。
func parseNativePush(_ http.Header, body []byte) (*pushPayloadReq, string, string) {
	var req pushPayloadReq
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, "", "json"
	}
	return &req, "", ""
}

// successBody 构造推送成功（或 skip）的 200 响应体；skipped 非空时附带说明字段，
// 让调用方能区分「已投递」与「已接收但刻意不投递」。
func successBody(ad pushAdapter, msgID int64, skipped string) map[string]interface{} {
	body := map[string]interface{}{
		"status":     0,
		"message_id": msgID,
	}
	if skipped != "" {
		body["skipped"] = skipped
	}
	for k, v := range ad.successExtra {
		body[k] = v
	}
	return body
}

// ============================================================
// 渲染文本工具（GitHub / WeCom 适配器共用）
// ============================================================

// clipRunes 按 rune 数截断并以省略号收尾。平台事件里的标题 / 提交信息 / 评论长度
// 不受我们控制，渲染前必须钳制——adapter 产出的 content 一旦超过 maxContentRunes
// 会被 push 路径按 413 拒绝，而平台调用方没有任何手段「修短」一个 GitHub 事件。
func clipRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}

// firstLine 取首行（提交信息惯例：首行即摘要）。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// oneLine 把多行文本压成单行，避免标题 / 评论里的换行破坏 markdown 链接结构。
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}
