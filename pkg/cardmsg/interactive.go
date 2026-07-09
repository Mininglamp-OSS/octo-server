package cardmsg

// card-message-interaction P2（spec: .octospec/tasks/card-message-interaction/
// brief.md）：octo/v2 profile 的交互能力面 + 交互闭环所需的纯函数 helper。
//
// 本文件是 P2（PR-B）在已合并 P1 之上的增量：ProfileV2 常量、card_action 事件
// 类型、以及 card/action 端点 / bot 编辑路径要用的无副作用 helper。profile 档位
// 判定（interactiveByProfile）与 walker 的 Submit/Input 校验在 validate.go 里就地
// 扩展，不在此重复定义（避免与 P1 的同名符号冲突）。

import (
	"encoding/json"
	"errors"
	"strings"
)

const (
	// ProfileV2 octo/v2：在 v1 展示集之上增加 Action.Submit（含 selectAction 携带）
	// 与 Input.Text / Input.Toggle / Input.ChoiceSet（P2 D1/D2）。Action.Execute /
	// auto-refresh 仍拒绝（P3）。
	ProfileV2 = "octo/v2"

	// EventTypeCardAction bot 事件队列里的卡片动作事件类型（P2 D5，增量事件类型，
	// event_data 形状由 brief 冻结：只许增字段）。bot SDK 必须容忍未知 event_type。
	EventTypeCardAction = "card_action"
)

// SubmitAction 在「生效卡片」信封字节里按 id 查找一个 Action.Submit，返回其静态
// data 对象与是否命中（P2 D3 防伪造 + D11 data 提取合一）：
//   - found=false：该 id 不是生效帧里任何 Action.Submit 的 id —— 调用方 fail-closed
//     拒绝（伪造 / 被重写移除的过期按钮天然落此分支）。
//   - found=true, data=nil：命中但该动作未声明 data。
//   - found=true, data≠nil：命中且带作者静态 data —— 服务端原样塞进 event_data.data
//     （anti-forgery：绝不取请求里的 data）。
//
// envelopeRaw 由调用方决定传哪份（content_edit 优先于原始 payload），据此过期帧
// 按钮天然 fail-closed。解析失败 / 无 card 返回 (nil, false)。
func SubmitAction(envelopeRaw []byte, actionID string) (data map[string]interface{}, found bool) {
	if actionID == "" {
		return nil, false
	}
	var payload struct {
		Card map[string]interface{} `json:"card"`
	}
	if err := json.Unmarshal(envelopeRaw, &payload); err != nil || payload.Card == nil {
		return nil, false
	}
	if d, ok := findSubmitAction(payload.Card["actions"], actionID); ok {
		return d, true
	}
	if d, ok := findSubmitAction(payload.Card["selectAction"], actionID); ok {
		return d, true
	}
	if body, ok := payload.Card["body"].([]interface{}); ok {
		return findSubmitInElements(body, actionID)
	}
	return nil, false
}

func findSubmitInElements(items []interface{}, actionID string) (map[string]interface{}, bool) {
	for _, it := range items {
		el, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		t, _ := el["type"].(string)
		// selectAction：任意元素都可携带（发送期 element() 顶部无条件校验），派发侧对齐。
		if d, ok := findSubmitAction(el["selectAction"], actionID); ok {
			return d, true
		}
		// inlineAction：仅 Input.*（发送期只在输入分支校验）。派发面用与校验器同一个
		// isInputElement 谓词，保证「校验面 == 派发面」—— 否则 Input.Number/Date/Time 的
		// inlineAction Submit 会「发送通过、点击 not-found」死按钮（P3-3）。
		if isInputElement(t) {
			if d, ok := findSubmitAction(el["inlineAction"], actionID); ok {
				return d, true
			}
		}
		// 容器/动作面递归，与 Validate 完全一致（发送期递归的位置：Container.items、
		// ColumnSet→Column.items、以及 P3-3 Tier 1 的 Table→cells.items、ActionSet.actions、
		// ImageSet.images[].selectAction、RichTextBlock.inlines[].selectAction）——否则藏在
		// 未遍历位置的 Submit 会「派发可解析、发送期没校验」，或反之成为死按钮（派发面 == 校验面）。
		switch t {
		case "Container":
			if sub, ok := el["items"].([]interface{}); ok {
				if d, ok := findSubmitInElements(sub, actionID); ok {
					return d, true
				}
			}
		case "ColumnSet":
			if cols, ok := el["columns"].([]interface{}); ok {
				for _, c := range cols {
					col, ok := c.(map[string]interface{})
					if !ok {
						continue
					}
					if d, ok := findSubmitAction(col["selectAction"], actionID); ok {
						return d, true
					}
					if sub, ok := col["items"].([]interface{}); ok {
						if d, ok := findSubmitInElements(sub, actionID); ok {
							return d, true
						}
					}
				}
			}
		case "ActionSet":
			// P3-3 Tier 1：Submit 可出现在 body 的 ActionSet.actions（发送期 element() 走
			// w.action 校验），派发侧对齐遍历。
			if d, ok := findSubmitAction(el["actions"], actionID); ok {
				return d, true
			}
		case "ImageSet":
			// ImageSet.images[].selectAction 可载 Submit（发送期 imageChild 校验），对齐。
			if imgs, ok := el["images"].([]interface{}); ok {
				for _, it := range imgs {
					if img, ok := it.(map[string]interface{}); ok {
						// 派发面 == 校验面：非 Image 子元素发送期已拒，派发侧同 childTypeMatches 跳过，两面不漂移（PR#556 review P2）。
						if !childTypeMatches(img, "Image") {
							continue
						}
						if d, ok := findSubmitAction(img["selectAction"], actionID); ok {
							return d, true
						}
					}
				}
			}
		case "RichTextBlock":
			// RichTextBlock.inlines[] 里的 TextRun.selectAction 可载 Submit（发送期校验），对齐。
			if inls, ok := el["inlines"].([]interface{}); ok {
				for _, it := range inls {
					if inl, ok := it.(map[string]interface{}); ok {
						// 同上：非 TextRun 内联对象发送期已拒，派发侧同判定跳过（PR#556 review P2）。
						if !childTypeMatches(inl, "TextRun") {
							continue
						}
						if d, ok := findSubmitAction(inl["selectAction"], actionID); ok {
							return d, true
						}
					}
				}
			}
		case "Table":
			// Table rows→cells：行 selectAction + 单元格 selectAction + 递归 cell.items（与发送
			// 期 Table 校验完全一致，派发面 == 校验面）。
			if rows, ok := el["rows"].([]interface{}); ok {
				for _, r := range rows {
					row, ok := r.(map[string]interface{})
					if !ok {
						continue
					}
					// 非 TableRow 发送期已拒，派发侧同判定跳过（PR#556 review P2）。
					if !childTypeMatches(row, "TableRow") {
						continue
					}
					// 行级 selectAction 可载 Submit（发送期 w.selectAction(row) 已对齐校验，
					// PR#556 review P2）。
					if d, ok := findSubmitAction(row["selectAction"], actionID); ok {
						return d, true
					}
					cells, ok := row["cells"].([]interface{})
					if !ok {
						continue
					}
					for _, c := range cells {
						cell, ok := c.(map[string]interface{})
						if !ok {
							continue
						}
						// 非 TableCell 发送期已拒，派发侧同判定跳过（PR#556 review P2）。
						if !childTypeMatches(cell, "TableCell") {
							continue
						}
						if d, ok := findSubmitAction(cell["selectAction"], actionID); ok {
							return d, true
						}
						if sub, ok := cell["items"].([]interface{}); ok {
							if d, ok := findSubmitInElements(sub, actionID); ok {
								return d, true
							}
						}
					}
				}
			}
		}
	}
	return nil, false
}

// findSubmitAction 从一个 actions 数组或单个 selectAction 对象里匹配指定 id 的
// Action.Submit，返回其 data。
func findSubmitAction(v interface{}, actionID string) (map[string]interface{}, bool) {
	switch t := v.(type) {
	case []interface{}:
		for _, a := range t {
			if d, ok := findSubmitAction(a, actionID); ok {
				return d, true
			}
		}
	case map[string]interface{}:
		if t["type"] == "Action.Submit" {
			if id, _ := t["id"].(string); id == actionID {
				data, _ := t["data"].(map[string]interface{})
				return data, true
			}
		}
	}
	return nil, false
}

// NormalizeContentEdit 是 bot 编辑路径的卡片收敛 gate（P2 D6 解锁；user/robot
// 编辑路径对卡片永久关闭，不得调用本函数放行）。与 richtext.NormalizeContentEdit
// 对称：
//   - 非 type=17 编辑体：原样返回（老路径 / richtext 路径不变）；
//   - type=17：跑与 send 同一套 Validate（白名单/大小/URL/profile 协商，v2 帧
//     在此合法），随后 Finalize 重算权威 plain，返回 canonical JSON 供落库。
//
// 跨类型变异（D6 不变量 (a)）由调用方比对原消息类型后拒绝 —— 本函数只看编辑体。
func NormalizeContentEdit(contentEdit string) (string, error) {
	payload, err := decodeEnvelope(contentEdit)
	if err != nil {
		return contentEdit, nil
	}
	if !IsCardPayload(payload) {
		return contentEdit, nil
	}
	if err := Validate(payload); err != nil {
		return "", err
	}
	if err := Finalize(payload); err != nil {
		return "", err
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// CardSeq 读取信封可选的单调帧序号 card_seq（P2 D9 乱序防护）。缺失/非数值
// 返回 (0, false) —— 无 card_seq 时行为退化为 last-write-wins（单写者 bot 零迁移）。
//
// 刻意不接受 float64（PR#548 review P2-d）：float64 尾数仅 52 位,>2^53 的雪花号 /
// 纳秒 epoch 会被静默坍缩,令 D9 CAS 失真。所有实时调用方经 decodeEnvelope(UseNumber)
// → json.Number(见 CardSeqFromContentEdit);int/int64 仅供程序化构造,三者皆 int64
// 精确。若未来有调用方误用裸 json.Unmarshal(→float64),这里返回 (0,false) 退化为 LWW,
// 而非接受一个被截断的错误序号 —— fail-safe。
func CardSeq(payload map[string]interface{}) (int64, bool) {
	switch v := payload["card_seq"].(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i, true
		}
	}
	return 0, false
}

// CardSeqFromContentEdit 从编辑体 JSON 读取 card_seq。
func CardSeqFromContentEdit(contentEdit string) (int64, bool) {
	payload, err := decodeEnvelope(contentEdit)
	if err != nil {
		return 0, false
	}
	return CardSeq(payload)
}

// decodeEnvelope 用 json.Decoder + UseNumber 解析信封 map：JSON 整数保留为 json.Number
// 而非先坍缩到 float64。card_seq（D9）等 64 位整数因此在 normalize 往返与 CAS 读取
// 全程保精 —— 普通 json.Unmarshal 会把 >2^53 的整数量化到 float64 的 53 位尾数，令
// 相邻帧坍缩为相等：D9 的 card_seq CAS（stored ≥ incoming 即拒）会把有效推进误判为
// stale 或接受迟到帧，正是它要防的 lost-update（PR#548 review；纳秒时间戳 ~1.75e18、
// 雪花号等合法 producer 天然 > 2^53）。整卡其余数值字段一并保精；Validate / Finalize /
// IsCardPayload / CardSeq 均不做 float64 断言（数值读取处已兼容 json.Number），故换用
// UseNumber 无副作用。尾部多余数据按与 json.Unmarshal 一致的严格度拒绝。
func decodeEnvelope(raw string) (map[string]interface{}, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var payload map[string]interface{}
	if err := dec.Decode(&payload); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("cardmsg: 信封 JSON 尾部有多余数据")
	}
	return payload, nil
}

// Transient 读取信封可选的 transient 标记（P2 D10.4）。transient 帧照常应用到
// content_edit（D6/D9 不变），但不进修订历史 —— 进度帧（thinking/tool-state）不
// 淹没审批态变更。缺失/非布尔按 false（非 transient，入历史）。
func Transient(payload map[string]interface{}) bool {
	t, _ := payload["transient"].(bool)
	return t
}

// TransientFromContentEdit 从编辑体 JSON 读取 transient。
func TransientFromContentEdit(contentEdit string) bool {
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(contentEdit), &payload); err != nil {
		return false
	}
	return Transient(payload)
}

// PlainFromContentEdit 从编辑体 JSON 读取服务端权威 plain（Finalize 重算后的值），
// 供修订历史存列表摘要。缺失返回空串。
func PlainFromContentEdit(contentEdit string) string {
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(contentEdit), &payload); err != nil {
		return ""
	}
	s, _ := payload["plain"].(string)
	return s
}
