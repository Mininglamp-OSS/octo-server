package incomingwebhook

// 推送形态适配层（#297 Phase 3/4 / #426）。
//
// 多种推送形态共享同一条鉴权 / 限流 / 群校验 / 投递 / 审计流水线（api.go handlePush），
// 彼此只差「如何把请求 body 翻译成 native 推送请求」这一步。下列路径同时挂在 canonical
// 前缀 /v1/incoming-webhooks 与短别名 /v1/webhooks 上（#455，两前缀共用同一套 handler/
// 中间件，见 api.go 的 mountPush）：
//
//   - native（历史契约）   POST /v1/incoming-webhooks/:webhook_id/:token
//   - GitHub 事件          POST .../:token/github   （adapter_github.go）
//   - 企业微信群机器人格式  POST .../:token/wecom    （adapter_wecom.go）
//   - Multica 出站事件     POST .../:token/multica  （adapter_multica.go）
//   - GitLab 事件          POST .../:token/gitlab   （adapter_gitlab.go）
//   - 飞书自定义机器人格式  POST .../:token/feishu   （adapter_feishu.go）
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
	// name 写入审计 adapter 列（adapterNative / adapterGitHub / adapterWeCom /
	// adapterMultica）。新增适配器时把对应常量加进列表。
	name string
	// parse 把平台原始 body 翻译成 native 推送请求。三个返回值恰有一个生效：
	//   - req     非 nil：照常走 msg_type 构造 / 投递；
	//   - skip    非空：请求合法但刻意不投递（GitHub ping / 渲染子集之外的事件），
	//     返回 200 并以 auditSkipped 落审计，供管理端 deliveries 观察链路；
	//   - invalid 非空：解析失败原因码，映射 400 invalid(reason=...) 并落审计。
	parse func(header http.Header, body []byte) (req *pushPayloadReq, skip string, invalid string)
	// successExtra 合并进成功 / skip 响应体的平台兼容字段（如企业微信的 errcode /
	// errmsg、飞书的 code / msg），让按平台 SDK 校验响应的既有工具不改代码即可迁移。
	// key 与 native 的 status / message_id 不重叠，纯追加。
	successExtra map[string]interface{}
	// bodyLimit 该形态的请求体字节上限。native / wecom / feishu 的 body 由调用方编写，
	// 沿用 8KiB 的 maxBytes()——上限本就是约束调用方的；github / gitlab 的 body 是平台
	// 生成的事件 JSON，真实事件普遍 >8KiB 且发送方无法修短，必须用更宽的专属上限
	//（githubMaxBytes / gitlabMaxBytes）。
	bodyLimit func() int
	// verifyToken（可选）在 URL token 已校验通过后，对平台在 header 里回传的 token 再做
	// 一次常量时间比对（目前仅 GitLab 的 X-Gitlab-Token）。返回 false → 401。能走到这里
	// 说明 URL token 已验证、调用方已持有 webhook 真正密钥，故不匹配是配置错误而非枚举
	// 探测（见 handlePush）。nil 表示该形态无需 header token 二次校验。
	verifyToken func(header http.Header, urlToken string) bool
	// allowMention 标记该形态是否处理请求里的 mention（@ 群成员/广播）。仅 native 置真：
	// @ 由调用方在 native payload 里显式描述（mentionReq），平台适配器(github/wecom/…)
	// 的 body 是平台生成的事件、没有 octo 成员 UID 语义，强行支持 @ 需要平台身份↔群成员
	// 的映射表，超出本期范围。置假的形态即便 parse 出 req.Mention 也一律忽略（防御纵深）。
	allowMention bool
}

var (
	// 仅 native 形态支持 @：调用方在 payload 里用 mention{uids,all,bots} 显式描述。
	nativeAdapter = pushAdapter{name: adapterNative, parse: parseNativePush, bodyLimit: maxBytes, allowMention: true}
	githubAdapter = pushAdapter{name: adapterGitHub, parse: parseGitHubPush, bodyLimit: githubMaxBytes}
	wecomAdapter  = pushAdapter{
		name:      adapterWeCom,
		parse:     parseWeComPush,
		bodyLimit: maxBytes,
		// 企业微信调用方普遍校验 errcode==0，附带平台习惯字段降低迁移摩擦。
		successExtra: map[string]interface{}{"errcode": 0, "errmsg": "ok"},
	}
	// multicaAdapter 接收 multica 出站 webhook（issue.status_changed 等事件
	// 的固定 JSON envelope）。multica envelope 比 GitHub 事件紧凑（不嵌入
	// repository 对象），8 KiB 足够，沿用 native 的 bodyLimit。
	multicaAdapter = pushAdapter{name: adapterMultica, parse: parseMulticaPush, bodyLimit: maxBytes}
	gitlabAdapter  = pushAdapter{
		name:      adapterGitLab,
		parse:     parseGitLabPush,
		bodyLimit: gitlabMaxBytes,
		// GitLab 额外要求把项目 Secret token 设为 URL token，经 X-Gitlab-Token 回传。
		verifyToken: verifyGitLabToken,
	}
	feishuAdapter = pushAdapter{
		name:      adapterFeishu,
		parse:     parseFeishuPush,
		bodyLimit: maxBytes,
		// 飞书调用方普遍校验 code==0，附带平台习惯字段降低迁移摩擦。
		successExtra: map[string]interface{}{"code": 0, "msg": "success"},
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

// isHTTPURL 判断字符串是否是 http(s) URL（大小写不敏感）。用于把外部提供的链接
// 目标限制到安全 scheme——非 http(s)（如 `javascript:` / `data:`）的「链接」会被降级
// 为纯文本，杜绝 scheme 注入（#423 review）。
func isHTTPURL(s string) bool {
	l := strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")
}

// oneLine 把多行文本压成单行，避免标题 / 评论里的换行破坏 markdown 链接结构。
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}

// mdLinkTextEscaper 转义会破坏 markdown 链接结构的字符：放进 `[text](url)` 的文本里
// 出现 `]` 会提前闭合链接、`[` 会引入嵌套——而 PR / issue 标题、评论、release 名乃至
// 仓库名都来自公开仓库的外部输入，必须转义后再拼进链接文本，否则攻击者可用一个带 `]`
// 的标题把后续 URL 暴露成可见文本甚至错位渲染（PR #330 review 跟进）。`\` 先于括号
// 转义，避免二次转义。
var mdLinkTextEscaper = strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`)

// mdLinkText 把外部文本钳到 max rune 后转义为安全的 markdown 链接文本。先钳后转义：
// 钳约束的是可见长度，转义引入的反斜杠不该挤占可见字符预算。
func mdLinkText(s string, max int) string {
	return mdLinkTextEscaper.Replace(clipRunes(oneLine(s), max))
}

// mdInertTextEscaper 转义会让外部文本"越界"激活 markdown 语法的字符。覆盖：
//   - 链接相关 `\` / `[` / `]`：与 mdLinkText 同源，避免拼成意外链接；
//   - 强调相关 `*` / `_`：放进 `**X**`/`__X__` 包装或纯文本时不能让自身字符提前闭合
//     或反生成强调；
//   - HTML / 自动链接 `<` / `>`：CommonMark 会把 `<http://…>` 渲染成链接，
//     `<script>` 在部分宽松渲染器里也会被转 HTML；
//   - 表格管道 `|`：进表格单元格的字段不能裸传 `|`。
//
// 反引号「不转义、直接剥离」：CommonMark 单 backtick span 里 `\` 是字面反斜杠，
// 用 `\“ 不能把反引号嵌进去；而双 backtick fence 又会引入二次转义难题。本模块
// 用 mdInertText 处理的字段（multica 的 identifier / actor.type 等短标识符）按
// 契约都是不含反引号的，剥离比转义更稳——与 GitHub adapter 处理短字段
// （branch、sender.login）的口径一致。
//
// ⚠️ 不要把 mdInertText 用在 `...` code span 的内部：code span 内 `_` / `*`
// 不被 markdown 解释，反斜杠转义会让客户端把 `\_` 当字面显示出来（in\_progress
// 而非 in_progress）。code span 内只需 strip 反引号，请用 mdCodeSpanText。
var mdInertTextEscaper = strings.NewReplacer(
	`\`, `\\`,
	"`", "",
	`*`, `\*`,
	`_`, `\_`,
	`[`, `\[`,
	`]`, `\]`,
	`<`, `\<`,
	`>`, `\>`,
	`|`, `\|`,
)

// mdInertText 把不该激活 markdown 语法的短字段（identifier / 标签 / actor 类型）
// 钳到 max rune 后转义为「inert」文本——拼到 `**X**` 或 `(by X)` 纯文本都不能
// 逃出去开新 span / 链接。先钳后转义同 mdLinkText。
//
// 仅用于 **非** code span 上下文；code span 内字段请用 mdCodeSpanText（否则
// `\_` 等转义序列会以字面形式回显，破坏显示）。
func mdInertText(s string, max int) string {
	return mdInertTextEscaper.Replace(clipRunes(oneLine(s), max))
}

// mdCodeSpanText 专用于 `...` 单反引号 code span 内部字段：只剥离反引号
// （没法在单 backtick span 内安全转义反引号），其余字符在 code span 内不被
// markdown 解释（`_` `*` `\` 都会按字面显示），无须转义——也绝不可转义，否则
// 客户端会回显 `\_` 而非 `_`，破坏 status 等可读性。
//
// 与 mdInertText 不同：用法严格限定到 “ `X` “ 上下文，不要拿出去拼别处。
var mdCodeSpanStripper = strings.NewReplacer("`", "")

func mdCodeSpanText(s string, max int) string {
	return mdCodeSpanStripper.Replace(clipRunes(oneLine(s), max))
}
