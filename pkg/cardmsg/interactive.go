package cardmsg

// card-message-interaction P2（spec: .octospec/tasks/card-message-interaction/
// brief.md）：octo/v2 profile 的交互能力面 + 交互闭环所需的纯函数 helper。
// ⚠️ POC 说明：本分支同时携带 P1+P2 行为（正式实现按 brief 分两个 PR 交付，
// P1 服务端只接受 octo/v1）。

import (
	"encoding/json"
)

// ProfileV2 octo/v2：在 v1 展示集之上增加 Action.Submit（含 selectAction 携带）
// 与 Input.Text / Input.Toggle / Input.ChoiceSet（P2 D1/D2）。Action.Execute /
// auto-refresh 仍拒绝（P3）。
const ProfileV2 = "octo/v2"

// EventTypeCardAction bot 事件队列里的卡片动作事件类型（P2 D5，增量事件类型，
// event_data 形状由 brief 冻结：只许增字段）。
const EventTypeCardAction = "card_action"

// interactiveByProfile 报告 profile 对应的能力档位；未知 profile 返回 (false,
// false)。P1 服务端接受集 = {v1}；P2 = {v1, v2}（本 POC 直接实现 P2 集合）。
func interactiveByProfile(profile string) (interactive, ok bool) {
	switch profile {
	case ProfileV1:
		return false, true
	case ProfileV2:
		return true, true
	}
	return false, false
}

// IsCardRawPayload 是 IsCardPayload 的 []byte 便捷形态（消息表 / IM 查询返回的
// 原始 payload 字节）。解析失败视为非卡片。
func IsCardRawPayload(raw []byte) bool {
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false
	}
	return IsCardPayload(payload)
}

// SubmitActionIDs 从「生效卡片」信封字节提取所有 Action.Submit 的 id 集合，
// 供 card/action 端点做 D3 防伪造校验（action_id 必须存在于生效帧 —— content_edit
// 优先于原始 payload，由调用方决定传入哪份字节；据此过期帧按钮天然 fail-closed）。
// 解析失败返回空集（fail-closed：查无 action 即拒绝）。
func SubmitActionIDs(envelopeRaw []byte) map[string]struct{} {
	ids := make(map[string]struct{})
	var payload struct {
		Card map[string]interface{} `json:"card"`
	}
	if err := json.Unmarshal(envelopeRaw, &payload); err != nil || payload.Card == nil {
		return ids
	}
	collectSubmitIDs(payload.Card["actions"], ids)
	collectSubmitIDs(payload.Card["selectAction"], ids)
	if body, ok := payload.Card["body"].([]interface{}); ok {
		collectSubmitIDsFromElements(body, ids)
	}
	return ids
}

func collectSubmitIDsFromElements(items []interface{}, ids map[string]struct{}) {
	for _, it := range items {
		el, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		collectSubmitIDs(el["selectAction"], ids)
		if sub, ok := el["items"].([]interface{}); ok {
			collectSubmitIDsFromElements(sub, ids)
		}
		if cols, ok := el["columns"].([]interface{}); ok {
			for _, c := range cols {
				if col, ok := c.(map[string]interface{}); ok {
					collectSubmitIDs(col["selectAction"], ids)
					if sub, ok := col["items"].([]interface{}); ok {
						collectSubmitIDsFromElements(sub, ids)
					}
				}
			}
		}
	}
}

func collectSubmitIDs(v interface{}, ids map[string]struct{}) {
	switch t := v.(type) {
	case []interface{}:
		for _, a := range t {
			collectSubmitIDs(a, ids)
		}
	case map[string]interface{}:
		if t["type"] == "Action.Submit" {
			if id, _ := t["id"].(string); id != "" {
				ids[id] = struct{}{}
			}
		}
	}
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
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(contentEdit), &payload); err != nil {
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
func CardSeq(payload map[string]interface{}) (int64, bool) {
	switch v := payload["card_seq"].(type) {
	case float64:
		return int64(v), true
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
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(contentEdit), &payload); err != nil {
		return 0, false
	}
	return CardSeq(payload)
}
