package cardmsg

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// v2Envelope 构造 octo/v2 信封。
func v2Envelope(card map[string]interface{}) map[string]interface{} {
	env := envelope(card)
	env["profile"] = ProfileV2
	return env
}

func approvalCard() map[string]interface{} {
	return map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5",
		"body": []interface{}{
			map[string]interface{}{"type": "TextBlock", "weight": "Bolder", "text": "审批单 #42"},
			map[string]interface{}{"type": "Input.Text", "id": "comment", "placeholder": "审批意见"},
			map[string]interface{}{"type": "Input.ChoiceSet", "id": "priority", "choices": []interface{}{
				map[string]interface{}{"title": "高", "value": "high"},
			}},
			map[string]interface{}{"type": "Input.Toggle", "id": "notify"},
		},
		"actions": []interface{}{
			map[string]interface{}{"type": "Action.Submit", "id": "approve_btn", "title": "通过",
				"data": map[string]interface{}{"verb": "approve"}},
			map[string]interface{}{"type": "Action.Submit", "id": "reject_btn", "title": "拒绝"},
			map[string]interface{}{"type": "Action.OpenUrl", "title": "详情", "url": "https://e.com/42"},
		},
	}
}

// 验收(P2 D1/D2):octo/v2 白名单矩阵。
func TestValidateV2Whitelist(t *testing.T) {
	// v2 下审批卡(Submit+Inputs)合法
	if err := Validate(v2Envelope(approvalCard())); err != nil {
		t.Fatalf("octo/v2 审批卡被拒: %v", err)
	}
	// 同一张卡在 v1 下被拒(分期继承)
	if err := Validate(envelope(approvalCard())); !errors.Is(err, ErrCardUnknownElement) {
		t.Errorf("v1 收到 Input.* 应拒, err=%v", err)
	}
	// Execute 在 v2 仍拒(P3)
	c := map[string]interface{}{"actions": []interface{}{
		map[string]interface{}{"type": "Action.Execute", "verb": "x"},
	}}
	if err := Validate(v2Envelope(c)); !errors.Is(err, ErrCardUnknownAction) {
		t.Errorf("v2 收到 Action.Execute 应拒(P3), err=%v", err)
	}
	// Submit 缺 id 拒(寻址/幂等键依赖 id)
	c2 := map[string]interface{}{"actions": []interface{}{
		map[string]interface{}{"type": "Action.Submit", "title": "OK"},
	}}
	if err := Validate(v2Envelope(c2)); !errors.Is(err, ErrCardBadShape) {
		t.Errorf("Submit 缺 id 应拒, err=%v", err)
	}
	// Input 缺 id 拒
	c3 := cardWithBody(map[string]interface{}{"type": "Input.Text"})
	if err := Validate(v2Envelope(c3)); !errors.Is(err, ErrCardBadShape) {
		t.Errorf("Input.Text 缺 id 应拒, err=%v", err)
	}
	// selectAction 携带 Submit 在 v2 合法
	c4 := cardWithBody(map[string]interface{}{
		"type":         "Container",
		"selectAction": map[string]interface{}{"type": "Action.Submit", "id": "tap_card"},
	})
	if err := Validate(v2Envelope(c4)); err != nil {
		t.Errorf("v2 selectAction=Submit 应放行: %v", err)
	}
}

// 验收(P2 D3):生效卡片的 Submit action_id 提取。
func TestSubmitActionIDs(t *testing.T) {
	env := v2Envelope(approvalCard())
	raw, _ := json.Marshal(env)
	ids := SubmitActionIDs(raw)
	for _, want := range []string{"approve_btn", "reject_btn"} {
		if _, ok := ids[want]; !ok {
			t.Errorf("缺少 action_id %q, got %v", want, ids)
		}
	}
	if _, ok := ids["详情"]; ok || len(ids) != 2 {
		t.Errorf("OpenUrl 不应参与寻址, got %v", ids)
	}
	// 嵌套容器/列内的 selectAction=Submit 也要收集
	nested := v2Envelope(cardWithBody(map[string]interface{}{
		"type": "ColumnSet", "columns": []interface{}{
			map[string]interface{}{"items": []interface{}{}, "selectAction": map[string]interface{}{
				"type": "Action.Submit", "id": "col_tap"}},
		},
	}))
	rawNested, _ := json.Marshal(nested)
	if _, ok := SubmitActionIDs(rawNested)["col_tap"]; !ok {
		t.Error("列上的 selectAction=Submit 未被收集")
	}
	// 解析失败 → 空集(fail-closed)
	if len(SubmitActionIDs([]byte("not-json"))) != 0 {
		t.Error("坏字节应返回空集")
	}
}

// 验收(P2 D6):编辑体收敛 gate。
func TestNormalizeContentEditCard(t *testing.T) {
	env := v2Envelope(approvalCard())
	env["plain"] = "client forged"
	raw, _ := json.Marshal(env)
	out, err := NormalizeContentEdit(string(raw))
	if err != nil {
		t.Fatalf("合法 v2 编辑体被拒: %v", err)
	}
	var normalized map[string]interface{}
	if err := json.Unmarshal([]byte(out), &normalized); err != nil {
		t.Fatalf("canonical 输出非 JSON: %v", err)
	}
	plain, _ := normalized["plain"].(string)
	if plain == "client forged" || !strings.Contains(plain, "审批单 #42") {
		t.Errorf("plain 应被服务端重算, got %q", plain)
	}
	// 脏卡片编辑体 → 错误
	dirty := v2Envelope(cardWithBody(map[string]interface{}{"type": "Image", "url": "javascript:x"}))
	rawDirty, _ := json.Marshal(dirty)
	if _, err := NormalizeContentEdit(string(rawDirty)); !errors.Is(err, ErrCardBadURLScheme) {
		t.Errorf("脏编辑体应拒, err=%v", err)
	}
	// 非卡片编辑体原样通过(richtext / 老路径不变)
	passthrough := `{"type":14,"content":[{"type":"text","text":"x"}]}`
	if out, err := NormalizeContentEdit(passthrough); err != nil || out != passthrough {
		t.Errorf("非卡片编辑体应原样通过, out=%q err=%v", out, err)
	}
}

// 验收(P2 D9):card_seq 读取。
func TestCardSeq(t *testing.T) {
	for _, tc := range []struct {
		v    interface{}
		want int64
		ok   bool
	}{
		{float64(3), 3, true},
		{json.Number("7"), 7, true},
		{"3", 0, false}, // string 不识别
		{nil, 0, false},
	} {
		p := map[string]interface{}{}
		if tc.v != nil {
			p["card_seq"] = tc.v
		}
		got, ok := CardSeq(p)
		if got != tc.want || ok != tc.ok {
			t.Errorf("CardSeq(%v)=(%d,%v) want (%d,%v)", tc.v, got, ok, tc.want, tc.ok)
		}
	}
	if seq, ok := CardSeqFromContentEdit(`{"type":17,"card_seq":5,"card":{}}`); !ok || seq != 5 {
		t.Errorf("CardSeqFromContentEdit=(%d,%v)", seq, ok)
	}
}

func TestIsCardRawPayload(t *testing.T) {
	if !IsCardRawPayload([]byte(`{"type":17,"card":{}}`)) {
		t.Error("type-17 字节应命中")
	}
	if IsCardRawPayload([]byte(`{"type":1,"content":"hi"}`)) || IsCardRawPayload([]byte(`bad`)) {
		t.Error("非卡片/坏字节不应命中")
	}
}
