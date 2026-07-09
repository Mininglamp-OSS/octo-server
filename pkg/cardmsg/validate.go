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
// false)。接受集 = **{octo/v1, octo/v2}**（P1 Decision 10 + P2 D2）：octo/v1 是
// 展示档（interactive=false），octo/v2 放行 Action.Submit 与 Input.*
// （interactive=true，P2 D1）。其余 profile 一律未知 → 400（分期继承：P1-only
// 客户端见 octo/v2 走 P1 降级链）。
func interactiveByProfile(profile string) (interactive, ok bool) {
	// 接受集从 acceptedProfiles（profiles.go）派生 —— 与 D12 能力清单 AcceptedProfiles
	// 同源，两者不可能漂移。
	for i := range acceptedProfiles {
		if acceptedProfiles[i].name == profile {
			return acceptedProfiles[i].interactive, true
		}
	}
	return false, false
}

// walker 携带遍历状态（递归节点计数 + profile 能力档位 + 帧内 id 去重集）。
// 深度经参数传递。
type walker struct {
	nodes int
	// interactive octo/v2 档位放行 Action.Submit 与 Input.*（P2 D1）。octo/v1 恒为
	// false（interactiveByProfile 只对 octo/v2 置 true）。
	interactive bool
	// seenIDs P2 D1（round-3 nit）：Action.Submit / Input.* 的 id 必须帧内唯一 ——
	// D3 action 寻址、D4 幂等键、D11 inputs 声明匹配都以 id 为键，重复 id 会让这三处
	// 语义歧义。懒初始化（仅 octo/v2 帧才有交互元素）。
	seenIDs map[string]struct{}
}

// registerID 记录一个 Action.Submit / Input.* 的 id 并强制帧内唯一（P2 D1）。
func (w *walker) registerID(kind, id string) error {
	if w.seenIDs == nil {
		w.seenIDs = make(map[string]struct{})
	}
	if _, dup := w.seenIDs[id]; dup {
		return fmt.Errorf("%w: %s.id %q 帧内重复", ErrCardBadShape, kind, id)
	}
	w.seenIDs[id] = struct{}{}
	return nil
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
	// selectAction 是任意元素都可携带的动作面，且派发侧 findSubmitInElements 对每种
	// 元素都读 el["selectAction"] —— 必须在类型分派前无条件校验，否则 TextBlock /
	// FactSet 等「叶子」上的 selectAction 会「派发期可解析、发送期没校验」，Action.Submit
	// 的 D1 帧内唯一 id / data-必须是对象被旁路（PR#548 review：校验面必须 ≥ 派发面）。
	if err := w.selectAction(el); err != nil {
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
				// Fact 是叶子（title/value）—— 不得携带任何子集合字段（PR#556 review：flat-validated 子位置
				// 同一纪律，堵伪装容器夹带未走查子树）。
				if err := checkConstrainedChild(fact, "FactSet.facts[]", ""); err != nil {
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
	case "ImageSet":
		// AC 1.0：一组 Image。images[] 每项是 Image 对象，逐个走与顶层 "Image" 同款纪律
		// （url 必填 + 正向 allowlist；backgroundImage/selectAction 等 URL/动作面同校验）。
		if imgs, present := el["images"]; present {
			list, ok := imgs.([]interface{})
			if !ok {
				return fmt.Errorf("%w: ImageSet.images 必须是数组", ErrCardBadShape)
			}
			for _, it := range list {
				img, ok := it.(map[string]interface{})
				if !ok {
					return fmt.Errorf("%w: ImageSet.images 元素必须是对象", ErrCardBadShape)
				}
				if err := w.imageChild(img, depth+1); err != nil {
					return err
				}
			}
		}
	case "RichTextBlock":
		// AC 1.2：inlines[] 是字符串或 TextRun 对象。TextRun 不渲染 markdown（AC 语义，
		// 故 text 无 markdown URL 面），但可携带 selectAction —— 那是动作/URL 面，逐个走
		// allowlist（校验面 ≥ 派发面；element() 顶部只校验 RichTextBlock 自身的 selectAction）。
		if inlines, present := el["inlines"]; present {
			list, ok := inlines.([]interface{})
			if !ok {
				return fmt.Errorf("%w: RichTextBlock.inlines 必须是数组", ErrCardBadShape)
			}
			for _, it := range list {
				switch inl := it.(type) {
				case string:
					// 纯文本 run，无 URL/动作面。
				case map[string]interface{}:
					if err := w.bump(depth + 1); err != nil {
						return err
					}
					// AC：inlines[] 的对象必须是**叶子 TextRun**：type 显式给出时必须是 "TextRun"，且
					// 不得携带子集合字段 —— 堵住伪类型 / typeless 的「伪装容器」把未校验 URL 面藏在
					// items 等子树里绕过（PR#556 review P1 + residual：校验面 ≥ 渲染面）。
					if err := checkConstrainedChild(inl, "RichTextBlock.inlines[]", "TextRun"); err != nil {
						return err
					}
					if err := w.selectAction(inl); err != nil {
						return err
					}
				default:
					return fmt.Errorf("%w: RichTextBlock.inlines 元素必须是字符串或对象", ErrCardBadShape)
				}
			}
		}
	case "Table":
		// AC 1.5：rows[] → TableRow.cells[] → TableCell.items[]（递归标准元素）。行/单元格
		// 计入节点预算；行/单元格的 backgroundImage、单元格的 selectAction 都是 URL/动作面。
		if rows, present := el["rows"]; present {
			list, ok := rows.([]interface{})
			if !ok {
				return fmt.Errorf("%w: Table.rows 必须是数组", ErrCardBadShape)
			}
			for _, r := range list {
				row, ok := r.(map[string]interface{})
				if !ok {
					return fmt.Errorf("%w: TableRow 必须是对象", ErrCardBadShape)
				}
				if err := w.bump(depth + 1); err != nil {
					return err
				}
				if err := checkNodeURLs(row); err != nil {
					return err
				}
				// TableRow.selectAction 与其它一切节点的 selectAction 同权威过 allowlist
				// （element() 顶部对每个 body 元素、column()/cell 都无条件校验 selectAction；row
				// 原先漏了这一层 —— 补齐使「每个节点的 selectAction 都被校验」不留缺口，OpenUrl 的
				// javascript: 面在 row 上也被拒；派发侧 findSubmitInElements 对齐读 row.selectAction，
				// PR#556 review P2）。
				if err := w.selectAction(row); err != nil {
					return err
				}
				// TableRow 必须是 TableRow 且只带 cells 子集合（同 column()/imageChild 纪律）——堵伪类型
				//（{"type":"Container","items":[…]}）/ typeless（{"items":[…]}）伪装行把未走查子树藏进
				// items 等绕过（PR#556 review P1：校验面 ≥ 渲染面）。
				if err := checkConstrainedChild(row, "Table.rows[]", "TableRow", "cells"); err != nil {
					return err
				}
				cells, present := row["cells"]
				if !present {
					continue
				}
				clist, ok := cells.([]interface{})
				if !ok {
					return fmt.Errorf("%w: TableRow.cells 必须是数组", ErrCardBadShape)
				}
				for _, c := range clist {
					cell, ok := c.(map[string]interface{})
					if !ok {
						return fmt.Errorf("%w: TableCell 必须是对象", ErrCardBadShape)
					}
					if err := w.bump(depth + 2); err != nil {
						return err
					}
					// TableCell 必须是 TableCell 且只带 items 子集合 —— 堵伪类型单元格
					//（{"type":"Image","url":"javascript:"} 会被当扁平 cell、其 url 永不走查）及 typeless
					// 伪装容器（PR#556 review P1：校验面 ≥ 渲染面）。
					if err := checkConstrainedChild(cell, "Table.rows[].cells[]", "TableCell", "items"); err != nil {
						return err
					}
					if err := checkNodeURLs(cell); err != nil {
						return err
					}
					if err := w.selectAction(cell); err != nil {
						return err
					}
					if items, present := cell["items"]; present {
						ilist, ok := items.([]interface{})
						if !ok {
							return fmt.Errorf("%w: TableCell.items 必须是数组", ErrCardBadShape)
						}
						if err := w.elements(ilist, depth+3); err != nil {
							return err
						}
					}
				}
			}
		}
	case "ActionSet":
		// AC 1.2：body 内的动作组。actions[] 每项走同一动作 allowlist（Action.OpenUrl；
		// Action.Submit 仍受 octo/v2 门控）。Submit 由此可出现在 body（非仅 card.actions /
		// selectAction），派发侧 findSubmitInElements 已对齐遍历 ActionSet.actions。
		if acts, present := el["actions"]; present {
			list, ok := acts.([]interface{})
			if !ok {
				return fmt.Errorf("%w: ActionSet.actions 必须是数组", ErrCardBadShape)
			}
			for _, a := range list {
				if err := w.action(a); err != nil {
					return err
				}
			}
		}
	default:
		// 校验器不枚举字面量，新增输入元素只改 inputElements 一处，发送期放行 / inputs 采集
		// / D12 清单三方自动同步（D12.2 反漂移）。非输入类型即未知元素。
		// 六个输入元素共享发送期纪律：id 必填且帧内唯一、label/errorMessage 的 markdown URL
		// 面走正向 allowlist、inlineAction 路由。Number/Date/Time 为 P3-3 追加（AC 1.0，落在
		// 固定 card_version="1.5" 内，白名单增量而非版本升级）。
		if !isInputElement(t) {
			return fmt.Errorf("%w: %q", ErrCardUnknownElement, t)
		}
		// octo/v1 一律拒绝：正常情况 octo/v2 信封已被 profile 协商挡在前面，此分支拦截
		// 「octo/v1 信封携带交互元素」的越级形状。
		if !w.interactive {
			return fmt.Errorf("%w: %q（octo/v2 起）", ErrCardUnknownElement, t)
		}
		// D1：输入控件 id 必填且帧内唯一（提交时 inputs 以 id 为键）。
		id, _ := el["id"].(string)
		if id == "" {
			return fmt.Errorf("%w: %s.id 必填", ErrCardBadShape, t)
		}
		if err := w.registerID(t, id); err != nil {
			return err
		}
		// Input.label / Input.errorMessage 与 TextBlock.text 同为 AC markdown 渲染面
		// （AC 1.3+：label 富文本标签、errorMessage 校验失败提示），其 markdown 链接
		// 目标必须走同一正向 allowlist（Decision 6；PR#548 review 补强：放开 Input.*
		// 后这两处是新增的 markdown URL 面）。仅在字符串时检查，不新增类型拒绝面。
		for _, k := range [2]string{"label", "errorMessage"} {
			if s, ok := el[k].(string); ok {
				for _, target := range markdownLinkTargets(s) {
					if err := checkURL(target); err != nil {
						return err
					}
				}
			}
		}
		// Input.ChoiceSet 的 choices 若出现必须是数组（值级枚举在 D11 提交期按
		// 声明 choices 校验；此处只保结构合法，与 send 期白名单纪律一致）。
		if t == "Input.ChoiceSet" {
			if choices, present := el["choices"]; present {
				if _, ok := choices.([]interface{}); !ok {
					return fmt.Errorf("%w: Input.ChoiceSet.choices 必须是数组", ErrCardBadShape)
				}
			}
		}
		// Input.* 特有的 inlineAction（AC 1.2+，端上渲染成贴附输入右侧的可点动作）也是
		// 动作/URL 面，走同一正向 allowlist；不显式路由即给它开天窗 —— 可夹带 javascript:
		// 的 Action.OpenUrl 或 P3 的 Action.Execute 绕过校验，Submit 也会逃过 id/registerID
		// 纪律（PR#548 review：校验面必须 ≥ 渲染/派发面，Decision 3d/6）。selectAction
		// 已由 element() 顶部对所有元素无条件校验，此处不再重复。
		if err := w.inlineAction(el); err != nil {
			return err
		}
	}
	return nil
}

// imageChild 校验 ImageSet 内的单个 Image。ImageSet.images 不经 element() 分派（其项
// 常省略 type），故这里补齐顶层 "Image" case 依赖 element() 提供的公共校验：节点预算、
// backgroundImage 等 URL 面（checkNodeURLs）、selectAction，再加 Image 自身的 url 必填 +
// 正向 allowlist。
// childCollectionFields 是 octo 支持的容器/集合类元素的全部「子集合」字段名 —— 承载子元素的
// 数组字段。任何按位置约束的子节点（column / imageChild / inline / table row·cell / fact）只允许
// 其类型契约内的那个（或零个）子集合字段，其余一律视为「伪装容器」夹带未走查子树。
var childCollectionFields = [...]string{
	"items", "columns", "rows", "cells", "inlines", "actions", "facts", "images",
}

// childTypeMatches 报告 node 的显式 type 是否为 expected（type 缺省视为相符 —— AC 允许省略 type，
// 显式给出则必须相符，同 column() 纪律）。**校验期与派发期共用同一判定**：校验期据此拒（reject
// foreign child），派发期据此跳过（skip），两面结构上不可能漂移（PR#556 review：shared predicate）。
func childTypeMatches(node map[string]interface{}, expected string) bool {
	t, present := node["type"]
	if !present {
		return true
	}
	s, _ := t.(string)
	return s == expected
}

// rejectForeignSubtree 拒绝一个按位置约束的子节点携带其类型契约之外的子集合字段。allowed 是该
// 节点合法的子集合字段（叶子 Image/TextRun/Fact 传空；TableRow 传 "cells"；TableCell/Column 传
// "items"）。堵住伪类型 / typeless 的「伪装容器」把未走查的 URL 面藏在越界子树里绕过发送期
// allowlist —— 校验面 ≥ 渲染面（类型门只挡显式伪类型，此层兜 typeless 及「类型合法但夹带越界
// 子树」，PR#556 review P1 + residual）。
func rejectForeignSubtree(node map[string]interface{}, where string, allowed ...string) error {
	for _, f := range childCollectionFields {
		if _, present := node[f]; !present {
			continue
		}
		permitted := false
		for _, a := range allowed {
			if f == a {
				permitted = true
				break
			}
		}
		if !permitted {
			return fmt.Errorf("%w: %s 携带非法子集合字段 %q（超出其类型契约）", ErrCardUnknownElement, where, f)
		}
	}
	return nil
}

// checkConstrainedChild 校验一个「按位置约束」的子节点（ColumnSet.columns[] / ImageSet.images[] /
// RichTextBlock.inlines[] / Table.rows[]·cells[] / FactSet.facts[]）：显式 type 必须是 expectedType
// （缺省放行；expectedType=="" 表示该位置无类型标签，如 Fact），且除 allowedCollections 外不得携带
// 任何子集合字段。是「校验面 ≥ 渲染面」在所有 flat-validated 子位置上的统一纪律（PR#556 review）。
func checkConstrainedChild(node map[string]interface{}, where, expectedType string, allowedCollections ...string) error {
	if expectedType != "" && !childTypeMatches(node, expectedType) {
		return fmt.Errorf("%w: %s 内元素类型 %v", ErrCardUnknownElement, where, node["type"])
	}
	return rejectForeignSubtree(node, where, allowedCollections...)
}

func (w *walker) imageChild(img map[string]interface{}, depth int) error {
	if err := w.bump(depth); err != nil {
		return err
	}
	// AC：ImageSet.images[] 每项必须是**叶子 Image**：type 显式给出时必须是 "Image"，且不得携带
	// 任何子集合字段。否则伪类型（{"type":"Container",…}）或 typeless（{"url":ok,"items":[…]}）的
	// 「伪装容器」会被当扁平 Image 只校 url、其 items 子树永不走查 —— 夹带 javascript: 链接绕过发送
	// 期校验（PR#556 review P1 + residual：校验面必须 ≥ 渲染面，同 column()/PR#543）。
	if err := checkConstrainedChild(img, "ImageSet.images[]", "Image"); err != nil {
		return err
	}
	if err := checkNodeURLs(img); err != nil {
		return err
	}
	if err := w.selectAction(img); err != nil {
		return err
	}
	u, _ := img["url"].(string)
	if u == "" {
		return fmt.Errorf("%w: ImageSet.images[].url 必填", ErrCardBadShape)
	}
	return checkURL(u)
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
	// Column 必须是 Column 且只带 items 子集合（type 缺省放行 —— AC 允许省略）——堵伪类型/typeless
	// 伪装列把未走查子树藏进 rows/columns 等越界字段绕过（PR#556 review：全 flat-validated 子位置同一
	// 纪律，校验面 ≥ 渲染面）。
	if err := checkConstrainedChild(col, "ColumnSet.columns[]", "Column", "items"); err != nil {
		return err
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

// inlineAction 校验 Input.* 上的可选 inlineAction（AC 1.2+：ISelectAction 型，端上
// 渲染成贴附输入右侧的可点动作，可载 Action.OpenUrl/Submit/Execute/ToggleVisibility）。
// 与 selectAction 对称 —— 继承所载动作的分期，走同一 w.action 正向 allowlist
// （checkURL + 动作类型白名单 + Submit id/data/registerID）。PR#548 review：Input
// 白名单放开后 inlineAction 是新增的动作/URL 面，原先完全绕过 w.action（校验面
// 必须 ≥ 渲染面，Decision 3d/6）。
func (w *walker) inlineAction(el map[string]interface{}) error {
	ia, present := el["inlineAction"]
	if !present {
		return nil
	}
	return w.action(ia)
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
		// P2 动作（octo/v2，sibling brief D1）。octo/v1 一律拒绝 —— selectAction
		// 携带时同样走到这里（分期继承：selectAction 继承所载动作的分期）。
		if !w.interactive {
			return fmt.Errorf("%w: %q（octo/v2 起）", ErrCardUnknownAction, t)
		}
		// D1：Action.Submit.id 必填 —— card/action 端点按 id 寻址且 D4 幂等键含 id；
		// 帧内唯一。
		id, _ := act["id"].(string)
		if id == "" {
			return fmt.Errorf("%w: Action.Submit.id 必填", ErrCardBadShape)
		}
		if err := w.registerID(t, id); err != nil {
			return err
		}
		// data 是作者静态上下文对象（D11：服务端在 card/action 时从生效帧提取塞进
		// event_data.data）。出现时必须是对象 —— 端点不做形状再校验，只信任发送期
		// 已校验过的这份 data，故此处钉住类型。
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
