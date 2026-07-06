package cardmsg

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
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

// walker 携带遍历状态（递归节点计数 + profile 能力档位）。深度经参数传递。
type walker struct {
	nodes       int
	interactive bool // octo/v2：放行 Action.Submit 与 Input.*（P2 D1）
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
						if _, isStr := v.(string); !isStr {
							return fmt.Errorf("%w: Fact.%s 必须是字符串", ErrCardBadShape, k)
						}
					}
				}
			}
		}
	case "Input.Text", "Input.Toggle", "Input.ChoiceSet":
		// P2 D1：输入控件仅 octo/v2；id 必填（提交时 inputs 以 id 为键）。
		if !w.interactive {
			return fmt.Errorf("%w: %q（需要 octo/v2）", ErrCardUnknownElement, t)
		}
		if id, _ := el["id"].(string); id == "" {
			return fmt.Errorf("%w: %s.id 必填", ErrCardBadShape, t)
		}
		if t == "Input.ChoiceSet" {
			if choices, present := el["choices"]; present {
				list, ok := choices.([]interface{})
				if !ok {
					return fmt.Errorf("%w: choices 必须是数组", ErrCardBadShape)
				}
				for _, ch := range list {
					if err := w.bump(depth + 1); err != nil {
						return err
					}
					if _, ok := ch.(map[string]interface{}); !ok {
						return fmt.Errorf("%w: choice 必须是对象", ErrCardBadShape)
					}
				}
			}
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

// action 校验单个动作对象。octo/v1 仅 Action.OpenUrl；octo/v2 增加
// Action.Submit（id 必填 —— card/action 端点按 id 寻址且 D4 幂等键含 id；
// data 可选对象）。Action.Execute 两档均拒（P3）。
func (w *walker) action(a interface{}) error {
	if err := w.bump(1); err != nil {
		return err
	}
	act, ok := a.(map[string]interface{})
	if !ok {
		return fmt.Errorf("%w: action 必须是对象", ErrCardBadShape)
	}
	switch t, _ := act["type"].(string); t {
	case "Action.OpenUrl":
		u, _ := act["url"].(string)
		if u == "" {
			return fmt.Errorf("%w: Action.OpenUrl.url 必填", ErrCardBadShape)
		}
		return checkURL(u)
	case "Action.Submit":
		if !w.interactive {
			return fmt.Errorf("%w: %q（需要 octo/v2）", ErrCardUnknownAction, t)
		}
		if id, _ := act["id"].(string); id == "" {
			return fmt.Errorf("%w: Action.Submit.id 必填", ErrCardBadShape)
		}
		if data, present := act["data"]; present {
			if _, ok := data.(map[string]interface{}); !ok {
				return fmt.Errorf("%w: Action.Submit.data 必须是对象", ErrCardBadShape)
			}
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

// markdownLinkRe 提取 AC 基础 markdown 子集里的链接目标 [text](target)。
// target 取到右括号前的第一段（容忍 "(url title)" 形态取 url 部分）。
var markdownLinkRe = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)[^)]*\)`)

func markdownLinkTargets(text string) []string {
	ms := markdownLinkRe.FindAllStringSubmatch(text, -1)
	if len(ms) == 0 {
		return nil
	}
	targets := make([]string, 0, len(ms))
	for _, m := range ms {
		targets = append(targets, m[1])
	}
	return targets
}
