package cardmsg

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// Validate 是 InteractiveCard(=17) 发送入口的 write-strict 校验 gate。
//   - 非 type=17 的 payload 直接通过（no-op），老消息路径不变；
//   - type=17 时依次执行：完整 payload 512KiB 上限（与 richtext 同口径，marshal
//     整个 map 而非子集，未知顶层字段一并计入）→ profile/card_version 协商
//     （Decision 10）→ card 结构白名单遍历（元素/动作/URL/节点数/深度）。
//
// Validate 不修改 payload，只做 gate；plain 的权威生成在所有 enrich 之后由
// Finalize 完成（与 pkg/richtext 的两步纪律对称）。
func Validate(payload map[string]interface{}) error {
	if !IsCardPayload(payload) {
		return nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCardBadShape, err)
	}
	if len(raw) > MaxPayloadBytes {
		return ErrCardPayloadTooLarge
	}
	profile, _ := payload["profile"].(string)
	interactive, known := interactiveByProfile(profile)
	if !known {
		return fmt.Errorf("%w: profile=%q", ErrCardProfileUnsupported, profile)
	}
	if ver, _ := payload["card_version"].(string); ver != CardVersion {
		return fmt.Errorf("%w: card_version=%q", ErrCardProfileUnsupported, ver)
	}
	card, ok := payload["card"].(map[string]interface{})
	if !ok || len(card) == 0 {
		return ErrCardMissing
	}
	return validateCard(card, interactive)
}

// validateCard 遍历标准 AC 卡片对象，按 profile 档位（interactive=octo/v2）执行
// 白名单 + 结构上限校验。卡片根上的未知标量字段（$schema/speak/lang 等）保持
// 宽容（前向兼容，与信封顶层字段同口径）；body/actions/selectAction/type/version
// 严格校验。
func validateCard(card map[string]interface{}, interactive bool) error {
	if t, present := card["type"]; present {
		if s, _ := t.(string); s != "AdaptiveCard" {
			return fmt.Errorf("%w: card.type=%v", ErrCardBadShape, t)
		}
	}
	if v, present := card["version"]; present {
		if s, _ := v.(string); s != CardVersion {
			return fmt.Errorf("%w: card.version=%v", ErrCardProfileUnsupported, v)
		}
	}
	w := &walker{interactive: interactive}
	// 卡片根上的 backgroundImage 等 URL 面（AdaptiveCard.backgroundImage）。
	if err := checkNodeURLs(card); err != nil {
		return err
	}
	if body, present := card["body"]; present {
		items, ok := body.([]interface{})
		if !ok {
			return fmt.Errorf("%w: body 必须是数组", ErrCardBadShape)
		}
		if err := w.elements(items, 1); err != nil {
			return err
		}
	}
	if actions, present := card["actions"]; present {
		list, ok := actions.([]interface{})
		if !ok {
			return fmt.Errorf("%w: actions 必须是数组", ErrCardBadShape)
		}
		for _, a := range list {
			if err := w.action(a); err != nil {
				return err
			}
		}
	}
	// 整卡 selectAction（端上以单容器包裹实现「点整卡跳转」时也可能直接落根上；
	// 与容器级同口径校验，P1 仅 OpenUrl）。
	if sa, present := card["selectAction"]; present {
		if err := w.action(sa); err != nil {
			return err
		}
	}
	return nil
}

// interactiveByProfile 报告 profile 对应的能力档位；未知 profile 返回 (false,
// false)。P1 接受集 = **{octo/v1}**（Decision 10）—— octo/v2 在 P1 是「未知
// profile」→ 400，正是分期继承要求的形状。P2 sibling（card-message-interaction）
// 把 octo/v2 加入接受集并置 interactive=true。
func interactiveByProfile(profile string) (interactive, ok bool) {
	if profile == ProfileV1 {
		return false, true
	}
	return false, false
}

// walker 携带遍历状态（递归节点计数 + profile 能力档位）。深度经参数传递。
type walker struct {
	nodes int
	// interactive octo/v2 档位放行 Action.Submit 与 Input.*（P2 D1）。P1 恒为
	// false（interactiveByProfile 只认 octo/v1）—— 字段保留使 P2 成为增量 diff。
	interactive bool
}

func (w *walker) bump(depth int) error {
	w.nodes++
	if w.nodes > MaxNodes {
		return ErrCardTooManyNodes
	}
	if depth > MaxDepth {
		return ErrCardTooDeep
	}
	return nil
}

func (w *walker) elements(items []interface{}, depth int) error {
	for _, it := range items {
		el, ok := it.(map[string]interface{})
		if !ok {
			return fmt.Errorf("%w: body 元素必须是对象", ErrCardBadShape)
		}
		if err := w.element(el, depth); err != nil {
			return err
		}
	}
	return nil
}

func (w *walker) element(el map[string]interface{}, depth int) error {
	if err := w.bump(depth); err != nil {
		return err
	}
	// 容器/元素级 URL 面（backgroundImage 等）——先于类型分派统一校验，覆盖
	// Container/ColumnSet 等承载 backgroundImage 的元素（PR#543 review）。
	if err := checkNodeURLs(el); err != nil {
		return err
	}
	t, _ := el["type"].(string)
	switch t {
	case "TextBlock":
		if txt, present := el["text"]; present {
			s, ok := txt.(string)
			if !ok {
				return fmt.Errorf("%w: TextBlock.text 必须是字符串", ErrCardBadShape)
			}
			// Decision 6：markdown 链接是额外的 URL 面，走同一正向 allowlist。
			for _, target := range markdownLinkTargets(s) {
				if err := checkURL(target); err != nil {
					return err
				}
			}
		}
	case "Image":
		u, _ := el["url"].(string)
		if u == "" {
			return fmt.Errorf("%w: Image.url 必填", ErrCardBadShape)
		}
		if err := checkURL(u); err != nil {
			return err
		}
		if err := w.selectAction(el); err != nil {
			return err
		}
	case "Container":
		if items, present := el["items"]; present {
			list, ok := items.([]interface{})
			if !ok {
				return fmt.Errorf("%w: Container.items 必须是数组", ErrCardBadShape)
			}
			if err := w.elements(list, depth+1); err != nil {
				return err
			}
		}
		if err := w.selectAction(el); err != nil {
			return err
		}
	case "ColumnSet":
		if cols, present := el["columns"]; present {
			list, ok := cols.([]interface{})
			if !ok {
				return fmt.Errorf("%w: ColumnSet.columns 必须是数组", ErrCardBadShape)
			}
			for _, c := range list {
				col, ok := c.(map[string]interface{})
				if !ok {
					return fmt.Errorf("%w: Column 必须是对象", ErrCardBadShape)
				}
				if err := w.column(col, depth+1); err != nil {
					return err
				}
			}
		}
		if err := w.selectAction(el); err != nil {
			return err
		}
	case "FactSet":
		facts, present := el["facts"]
		if present {
			list, ok := facts.([]interface{})
			if !ok {
				return fmt.Errorf("%w: FactSet.facts 必须是数组", ErrCardBadShape)
			}
			for _, f := range list {
				fact, ok := f.(map[string]interface{})
				if !ok {
					return fmt.Errorf("%w: Fact 必须是对象", ErrCardBadShape)
				}
				if err := w.bump(depth + 1); err != nil {
					return err
				}
				for _, k := range [2]string{"title", "value"} {
					if v, ok := fact[k]; ok {
						s, isStr := v.(string)
						if !isStr {
							return fmt.Errorf("%w: Fact.%s 必须是字符串", ErrCardBadShape, k)
						}
						// Fact.title/value 同样渲染 AC markdown 子集，是与 TextBlock
						// 对等的 URL 面 —— markdown 链接目标走同一正向 allowlist
						// （Decision 6；PR#543 review：FactSet 曾漏这层，校验面必须
						// ≥ 渲染面）。
						for _, target := range markdownLinkTargets(s) {
							if err := checkURL(target); err != nil {
								return err
							}
						}
					}
				}
			}
		}
	case "Input.Text", "Input.Toggle", "Input.ChoiceSet":
		// P2 元素（octo/v2 白名单，sibling brief D1）。P1 一律拒绝：正常情况
		// octo/v2 信封已被 profile 协商挡在前面，此分支拦截「octo/v1 信封携带
		// P2 元素」的越级形状。P2 在此分支落 id 必填/帧内唯一/choices 校验。
		if !w.interactive {
			return fmt.Errorf("%w: %q（octo/v2 起）", ErrCardUnknownElement, t)
		}
	default:
		return fmt.Errorf("%w: %q", ErrCardUnknownElement, t)
	}
	return nil
}

// column 校验 ColumnSet 中的单列。AC 允许 Column 省略 type 字段；显式给出时必须
// 是 "Column"。
func (w *walker) column(col map[string]interface{}, depth int) error {
	if err := w.bump(depth); err != nil {
		return err
	}
	// Column.backgroundImage 等 URL 面。
	if err := checkNodeURLs(col); err != nil {
		return err
	}
	if t, present := col["type"]; present {
		if s, _ := t.(string); s != "Column" {
			return fmt.Errorf("%w: columns 内元素类型 %v", ErrCardUnknownElement, t)
		}
	}
	if items, present := col["items"]; present {
		list, ok := items.([]interface{})
		if !ok {
			return fmt.Errorf("%w: Column.items 必须是数组", ErrCardBadShape)
		}
		if err := w.elements(list, depth+1); err != nil {
			return err
		}
	}
	return w.selectAction(col)
}

// selectAction 校验元素上的可选 selectAction（Decision：selectAction 继承所载
// 动作的分期 —— P1 仅 Action.OpenUrl，携带 Action.Submit 属 octo/v2，此处拒绝）。
func (w *walker) selectAction(el map[string]interface{}) error {
	sa, present := el["selectAction"]
	if !present {
		return nil
	}
	return w.action(sa)
}

// action 校验单个动作对象。octo/v1 仅 Action.OpenUrl；Action.Submit 属 octo/v2
// （P2 sibling 解锁）；Action.Execute 两档均拒（P3）。
func (w *walker) action(a interface{}) error {
	if err := w.bump(1); err != nil {
		return err
	}
	act, ok := a.(map[string]interface{})
	if !ok {
		return fmt.Errorf("%w: action 必须是对象", ErrCardBadShape)
	}
	// Action 上的 URL 面（Action.OpenUrl.iconUrl 等）——独立于 url 字段（PR#543
	// review）。
	if err := checkNodeURLs(act); err != nil {
		return err
	}
	switch t, _ := act["type"].(string); t {
	case "Action.OpenUrl":
		u, _ := act["url"].(string)
		if u == "" {
			return fmt.Errorf("%w: Action.OpenUrl.url 必填", ErrCardBadShape)
		}
		return checkURL(u)
	case "Action.Submit":
		// P2 动作（octo/v2，sibling brief D1）。P1 一律拒绝 —— selectAction 携带
		// 时同样走到这里（分期继承：selectAction 继承所载动作的分期）。
		if !w.interactive {
			return fmt.Errorf("%w: %q（octo/v2 起）", ErrCardUnknownAction, t)
		}
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrCardUnknownAction, t)
	}
}

// checkURL 执行 Decision 3d 的正向 allowlist：仅接受「绝对」http/https URL。
// 相对路径、data:/javascript:/vbscript:/intent:/file: 等一律拒绝（正向名单
// 天然覆盖未来出现的新危险 scheme）。
func checkURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("%w: %q", ErrCardBadURLScheme, raw)
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: %q", ErrCardBadURLScheme, raw)
	}
	return nil
}

// checkNodeURLs 校验一个卡片节点（card 根 / 元素 / 列 / 动作）上、除已单独处理的
// `url` 外其余会被端上渲染成资源/图标的 URL 承载字段：
//   - backgroundImage：AC 允许字符串简写或 {url:...} 对象全写，出现在 AdaptiveCard
//     根 / Container / Column / ColumnSet；
//   - iconUrl：Action.OpenUrl 的图标。
//
// walker 对未知属性宽容（前向兼容），但**不能给这些"会被渲染"的 URL 面开天窗** ——
// 校验面必须 ≥ 渲染面（Decision 3d，与 markdown 链接、Image.url 同一正向 allowlist）。
// PR#543 review：backgroundImage/iconUrl 原先完全绕过 checkURL。新的 URL 承载字段
// 随 profile 演进补进本表即可。
func checkNodeURLs(node map[string]interface{}) error {
	if bg, present := node["backgroundImage"]; present {
		switch v := bg.(type) {
		case string:
			if v != "" {
				if err := checkURL(v); err != nil {
					return err
				}
			}
		case map[string]interface{}:
			if u, _ := v["url"].(string); u != "" {
				if err := checkURL(u); err != nil {
					return err
				}
			}
		}
	}
	if icon, _ := node["iconUrl"].(string); icon != "" {
		if err := checkURL(icon); err != nil {
			return err
		}
	}
	return nil
}

// markdownParser 是提取 markdown 链接面用的 CommonMark 解析器。goldmark 默认
// 配置即 CommonMark 合规（含内联链接、引用式链接、图片、`<scheme:…>` autolink）。
// Parser.Parse 每次解析创建独立解析上下文，可并发安全复用单例。
var markdownParser = goldmark.New().Parser()

// markdownLinkTargets 提取一段 AC markdown 文本里**会被 CommonMark 渲染成活链接
// 的全部目标 URL**：内联链接 `[t](url)`、引用式链接 `[t][l]`+`[l]: url`、图片
// `![alt](url)`、autolink `<scheme:url>`。调用方对每个目标一律过 checkURL 的正向
// http(s) allowlist。
//
// 用真正的 CommonMark 解析器而非正则，是 Decision 3d/6「校验面必须 ≥ 渲染面」的
// 结构性保证：正则无法覆盖 CommonMark 会渲染、而模式匹配漏抽的形态 —— 嵌套/转义
// 方括号 label（`[a [b]](url)`、`[x\]](url)`）、转义 scheme 引用定义（`[l]:
// javascript\:…` 配 `[x][l]`）等（PR#543 round-4：yujiawei/Jerry-Xin byte-verified
// 两处正则绕过）。因 checkURL 是正向名单，任何非 http(s)（含反斜杠破坏 scheme 的
// `javascript\:`）都会被拒，故此处**不预判 scheme**（预判正是旧 refDefRe 被绕过的
// 根因），把全部提取目标交给 checkURL 判定。
//
// 只提取「真正成为链接」的目标:未被引用的孤立引用定义(如正文 `[Note]: do this`)
// 不产出链接节点 → 不误伤(优于旧 refDefRe 的无差别行提取)。空 destination
// (`[t]()`)跳过 —— 空 href 不承载任何 scheme,拒之只会误伤合法「同页/占位」链接。
func markdownLinkTargets(s string) []string {
	if s == "" {
		return nil
	}
	src := []byte(s)
	doc := markdownParser.Parse(text.NewReader(src))
	var targets []string
	add := func(dest string) {
		if strings.TrimSpace(dest) != "" {
			targets = append(targets, dest)
		}
	}
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.Link:
			add(string(v.Destination))
		case *ast.Image:
			add(string(v.Destination))
		case *ast.AutoLink:
			add(string(v.URL(src)))
		}
		return ast.WalkContinue, nil
	})
	return targets
}
