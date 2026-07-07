package cardmsg

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Finalize 是 InteractiveCard(=17) 派发出口的权威收尾：必须在所有 server 端
// enrich（space_id 注入 / mention 顶层键改写等）之后调用。
//   - 非 type=17 的 payload 原样返回（no-op）；
//   - type=17 时按 Decision 8 派生规则重算顶层 plain 覆盖端上不可信 plain
//     （永不为空），并对真正出站的「完整」payload（含 server 注入顶层字段）复检
//     512KiB 上限。
//
// 就地修改传入的 payload map（写入 payload["plain"]）。Decision 9 保证 enrich
// 只发生在信封顶层键 —— card 树本身永不被 server 改写,故 plain 派生对 enrich
// 顺序不敏感,这里晚于 enrich 只为让大小复检覆盖真实出站字节（与 richtext 的
// PR#232 口径一致）。
func Finalize(payload map[string]interface{}) error {
	if !IsCardPayload(payload) {
		return nil
	}
	card, ok := payload["card"].(map[string]interface{})
	if !ok {
		return ErrCardMissing
	}
	payload["plain"] = BuildPlain(card)
	out, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if len(out) > MaxPayloadBytes {
		return ErrCardPayloadTooLarge
	}
	return nil
}

// BuildPlain 按 Decision 8 从卡片派生权威纯文本：
//   - 按文档序遍历 body：TextBlock 取剥离 markdown 后的文本；FactSet 逐条产出
//     "title: value" 行；Image 产出 [图片] 占位；容器类（Container/ColumnSet/
//     Column）递归；
//   - 动作（按钮）不参与 —— 按钮是操作面不是内容；
//   - 段落以换行拼接；结果为空时兜底 [卡片] —— plain 永不为空,推送/搜索/摘要/
//     置顶永远有可显示文本。
func BuildPlain(card map[string]interface{}) string {
	var segs []string
	if body, ok := card["body"].([]interface{}); ok {
		collectPlain(body, &segs)
	}
	out := strings.TrimSpace(strings.Join(segs, "\n"))
	if out == "" {
		return PlaceholderCard
	}
	return out
}

func collectPlain(items []interface{}, segs *[]string) {
	for _, it := range items {
		el, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		switch el["type"] {
		case "TextBlock":
			if s, _ := el["text"].(string); s != "" {
				if t := strings.TrimSpace(stripMarkdown(s)); t != "" {
					*segs = append(*segs, t)
				}
			}
		case "Image":
			*segs = append(*segs, PlaceholderImage)
		case "FactSet":
			if facts, ok := el["facts"].([]interface{}); ok {
				for _, f := range facts {
					fact, ok := f.(map[string]interface{})
					if !ok {
						continue
					}
					title, _ := fact["title"].(string)
					value, _ := fact["value"].(string)
					// 与 TextBlock 对等：Fact.title/value 渲染 markdown，plain 须剥离
					// 语法字符（Decision 8：plain 不含原始 markdown；PR#543 review：
					// FactSet 曾直接拼接 raw title/value，把 `[x](url)`/`**b**` 泄进
					// 权威 plain）。
					title = stripMarkdown(title)
					value = stripMarkdown(value)
					line := strings.TrimSpace(strings.TrimSuffix(title+": "+value, ": "))
					if line != "" && line != ":" {
						*segs = append(*segs, strings.TrimPrefix(line, ": "))
					}
				}
			}
		case "Container":
			if sub, ok := el["items"].([]interface{}); ok {
				collectPlain(sub, segs)
			}
		case "ColumnSet":
			if cols, ok := el["columns"].([]interface{}); ok {
				for _, c := range cols {
					col, ok := c.(map[string]interface{})
					if !ok {
						continue
					}
					if sub, ok := col["items"].([]interface{}); ok {
						collectPlain(sub, segs)
					}
				}
			}
		}
	}
}

// stripMarkdown 剥离 AC 基础 markdown 子集的语法字符（Decision 8：plain 不含
// 原始 `**`/`[]()`）：链接降为链接文本，粗体/斜体星号与反引号移除。下划线不动
// —— snake_case 文本比 `_斜体_` 常见得多,误伤成本更高（AC 强调以 * 为主）。
func stripMarkdown(s string) string {
	s = markdownLinkTextRe.ReplaceAllString(s, "$1")
	return markdownEmphasisReplacer.Replace(s)
}

var (
	// markdownLinkTextRe 捕获组是「链接文本」（validate.go 的 markdownLinkRe
	// 捕获的是链接目标 —— 两处捕获对象不同,不能复用）。
	markdownLinkTextRe       = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	markdownEmphasisReplacer = strings.NewReplacer("**", "", "*", "", "`", "")
)

// DisplayTextFor 是「按 plain 描述卡片消息」的所有服务端展示面（离线推送、
// 搜索命中投影、会话摘要、引用预览）的**单一执法点**（Decision 2 residual-risk
// 规则，round-3 P1-2 统一化）：
//   - senderTrusted（存储行 from_uid 是 bot / webhook 身份）→ 信任 server 权威
//     plain（PushDisplayText）；
//   - 否则（直连长连接发送者，plain 攻击者可控 —— Finalize 从未跑过）→ 一律
//     [卡片] 占位，绝不透出存储 plain。
//
// 调用方负责解析 sender 身份（robot 表 / iwh_ 前缀），不得在各展示面私开
// type-17 分支。置顶 tips 等「内容类型占位」文案面不经过本函数 —— 用
// DisplayText()（恒 [卡片]，无 plain 信任问题）。
func DisplayTextFor(senderTrusted bool, payloadRaw []byte) string {
	if !senderTrusted {
		return PlaceholderCard
	}
	return PushDisplayText(payloadRaw)
}

// PushDisplayText 为「可信路径」产出卡片文案：优先信任 payload 里 server 已
// 生成的权威 plain（Decision 8 保证经校验路径永不为空）；缺失/为空时现场从
// card 重算兜底；再不行回退 [卡片]。展示面不要直接调它 —— 走 DisplayTextFor
// （sender 身份门在那里）。
func PushDisplayText(payloadRaw []byte) string {
	var p struct {
		Plain string                 `json:"plain"`
		Card  map[string]interface{} `json:"card"`
	}
	if err := json.Unmarshal(payloadRaw, &p); err != nil {
		return PlaceholderCard
	}
	if s := strings.TrimSpace(p.Plain); s != "" {
		return s
	}
	if len(p.Card) > 0 {
		return BuildPlain(p.Card)
	}
	return PlaceholderCard
}
