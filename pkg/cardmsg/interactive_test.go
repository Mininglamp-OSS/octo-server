package cardmsg

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// v2Envelope 构造一个 octo/v2 信封，card body/actions 由调用方给。
func v2Envelope(card map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"type":         float64(17),
		"card":         card,
		"plain":        "client-forged",
		"card_version": "1.5",
		"profile":      "octo/v2",
	}
}

func TestValidateV2WhitelistGating(t *testing.T) {
	submit := map[string]interface{}{"type": "Action.Submit", "id": "approve", "title": "通过"}
	inputText := map[string]interface{}{"type": "Input.Text", "id": "comment"}

	// octo/v2 放行 Action.Submit + Input.*
	env := v2Envelope(map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5",
		"body":    []interface{}{inputText},
		"actions": []interface{}{submit},
	})
	if err := Validate(env); err != nil {
		t.Fatalf("octo/v2 应放行 Action.Submit + Input.Text, err=%v", err)
	}

	// octo/v1 携带 Action.Submit → 拒绝（越级）
	v1 := envelope(map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5",
		"body":    []interface{}{map[string]interface{}{"type": "TextBlock", "text": "x"}},
		"actions": []interface{}{submit},
	})
	if err := Validate(v1); !errors.Is(err, ErrCardUnknownAction) {
		t.Errorf("octo/v1 携带 Action.Submit 应拒, err=%v", err)
	}

	// octo/v1 携带 Input.Text → 拒绝
	v1in := envelope(map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5",
		"body": []interface{}{inputText},
	})
	if err := Validate(v1in); !errors.Is(err, ErrCardUnknownElement) {
		t.Errorf("octo/v1 携带 Input.Text 应拒, err=%v", err)
	}

	// Action.Execute 两档均拒（P3）
	exec := v2Envelope(map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5",
		"body":    []interface{}{map[string]interface{}{"type": "TextBlock", "text": "x"}},
		"actions": []interface{}{map[string]interface{}{"type": "Action.Execute", "id": "e"}},
	})
	if err := Validate(exec); !errors.Is(err, ErrCardUnknownAction) {
		t.Errorf("Action.Execute 应拒(P3), err=%v", err)
	}
}

func TestValidateV2SelectActionSubmit(t *testing.T) {
	// selectAction 携带 Action.Submit：octo/v2 放行，octo/v1 拒绝（分期继承）。
	body := []interface{}{map[string]interface{}{
		"type": "Container", "items": []interface{}{},
		"selectAction": map[string]interface{}{"type": "Action.Submit", "id": "tap"},
	}}
	if err := Validate(v2Envelope(cardWithBody(body...))); err != nil {
		t.Errorf("octo/v2 selectAction=Submit 应放行, err=%v", err)
	}
	if err := Validate(envelope(cardWithBody(body...))); !errors.Is(err, ErrCardUnknownAction) {
		t.Errorf("octo/v1 selectAction=Submit 应拒, err=%v", err)
	}
}

func TestValidateV2InputInlineAction(t *testing.T) {
	// PR#548 review：Input.* 白名单放开后，inlineAction / selectAction 携带的动作
	// 必须走同一正向 allowlist，不能借「walker 宽容未知属性」绕过（校验面 ≥ 渲染面）。
	inputWith := func(prop string, action map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{"type": "Input.Text", "id": "comment", prop: action}
	}

	// 合法 http inlineAction 放行（正向名单不误伤真实卡片）。
	ok := v2Envelope(cardWithBody(inputWith("inlineAction",
		map[string]interface{}{"type": "Action.OpenUrl", "url": "https://example.com/go"})))
	if err := Validate(ok); err != nil {
		t.Errorf("合法 http inlineAction 应放行, err=%v", err)
	}

	// javascript: 的 inlineAction OpenUrl 必须走 checkURL 拒绝。
	js := v2Envelope(cardWithBody(inputWith("inlineAction",
		map[string]interface{}{"type": "Action.OpenUrl", "url": "javascript:alert(1)"})))
	if err := Validate(js); !errors.Is(err, ErrCardBadURLScheme) {
		t.Errorf("inlineAction 的 javascript: OpenUrl 应拒, err=%v", err)
	}

	// Action.Execute（P3）经 inlineAction 也必须拒。
	exec := v2Envelope(cardWithBody(inputWith("inlineAction",
		map[string]interface{}{"type": "Action.Execute", "id": "e"})))
	if err := Validate(exec); !errors.Is(err, ErrCardUnknownAction) {
		t.Errorf("inlineAction 的 Action.Execute 应拒(P3), err=%v", err)
	}

	// inlineAction 携带的 Action.Submit 必须走 id 纪律：缺 id → 拒。
	noID := v2Envelope(cardWithBody(inputWith("inlineAction",
		map[string]interface{}{"type": "Action.Submit"})))
	if err := Validate(noID); !errors.Is(err, ErrCardBadShape) {
		t.Errorf("inlineAction 的无 id Action.Submit 应拒, err=%v", err)
	}

	// inlineAction Submit 与顶层 action id 撞车 → 同一 seenIDs 命名空间拒（证明确实
	// 走了 registerID，未逃过帧内唯一，否则 D3/D4 的 id 寻址/幂等会被旁路）。
	dup := v2Envelope(map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5",
		"body": []interface{}{inputWith("inlineAction",
			map[string]interface{}{"type": "Action.Submit", "id": "dup"})},
		"actions": []interface{}{map[string]interface{}{"type": "Action.Submit", "id": "dup"}},
	})
	if err := Validate(dup); !errors.Is(err, ErrCardBadShape) {
		t.Errorf("inlineAction Submit 与 action id 撞车应拒, err=%v", err)
	}

	// selectAction 面同样被路由：Input.Text.selectAction 的 javascript: OpenUrl → 拒。
	sel := v2Envelope(cardWithBody(inputWith("selectAction",
		map[string]interface{}{"type": "Action.OpenUrl", "url": "javascript:alert(1)"})))
	if err := Validate(sel); !errors.Is(err, ErrCardBadURLScheme) {
		t.Errorf("Input.selectAction 的 javascript: OpenUrl 应拒, err=%v", err)
	}
}

func TestValidateV2InputLabelMarkdown(t *testing.T) {
	// PR#548 review 补强：Input.label / errorMessage 是 AC markdown 渲染面，其链接目标
	// 与 TextBlock.text 同走正向 allowlist（校验面 ≥ 渲染面）。
	inputWith := func(field, val string) map[string]interface{} {
		return map[string]interface{}{"type": "Input.Text", "id": "c", field: val}
	}
	// label 里 javascript: 的 markdown 链接 → 拒。
	if err := Validate(v2Envelope(cardWithBody(inputWith("label", "点[x](javascript:steal)")))); !errors.Is(err, ErrCardBadURLScheme) {
		t.Errorf("label markdown 的 javascript: 链接应拒, err=%v", err)
	}
	// errorMessage 里非 http scheme 的 markdown 链接 → 拒。
	if err := Validate(v2Envelope(cardWithBody(inputWith("errorMessage", "[e](vbscript:y)")))); !errors.Is(err, ErrCardBadURLScheme) {
		t.Errorf("errorMessage markdown 的 vbscript: 链接应拒, err=%v", err)
	}
	// 合法 https markdown 链接 / 纯文本 label 放行（不误伤，也不新增类型拒绝面）。
	if err := Validate(v2Envelope(cardWithBody(inputWith("label", "见[文档](https://example.com)")))); err != nil {
		t.Errorf("label 里合法 https markdown 链接应放行, err=%v", err)
	}
	if err := Validate(v2Envelope(cardWithBody(inputWith("label", "备注")))); err != nil {
		t.Errorf("纯文本 label 应放行, err=%v", err)
	}
}

func TestValidateSelectActionAllElements(t *testing.T) {
	// PR#548 review P1：selectAction 现由 element() 顶部对**所有**元素无条件校验 ——
	// TextBlock / FactSet 上的 selectAction 也走 w.action，不能借「叶子不校验 selectAction」
	// 夹带未过 id/checkURL 纪律的动作（否则派发面 > 校验面）。
	// TextBlock.selectAction 的无 id Submit → 拒（证明走了 registerID/id 纪律）。
	tb := v2Envelope(cardWithBody(map[string]interface{}{
		"type": "TextBlock", "text": "x",
		"selectAction": map[string]interface{}{"type": "Action.Submit"},
	}))
	if err := Validate(tb); !errors.Is(err, ErrCardBadShape) {
		t.Errorf("TextBlock.selectAction 无 id Submit 应拒, err=%v", err)
	}
	// FactSet.selectAction 的 javascript: OpenUrl → 拒（证明走了 checkURL）。
	fs := v2Envelope(cardWithBody(map[string]interface{}{
		"type": "FactSet", "facts": []interface{}{},
		"selectAction": map[string]interface{}{"type": "Action.OpenUrl", "url": "javascript:x"},
	}))
	if err := Validate(fs); !errors.Is(err, ErrCardBadURLScheme) {
		t.Errorf("FactSet.selectAction 的 javascript: OpenUrl 应拒, err=%v", err)
	}
	// TextBlock.selectAction 的合法 Submit（带 id）→ 放行（不误伤）。
	ok := v2Envelope(cardWithBody(map[string]interface{}{
		"type": "TextBlock", "text": "x",
		"selectAction": map[string]interface{}{"type": "Action.Submit", "id": "tap"},
	}))
	if err := Validate(ok); err != nil {
		t.Errorf("TextBlock.selectAction 合法 Submit 应放行, err=%v", err)
	}
}

func TestSubmitActionDispatchMatchesValidation(t *testing.T) {
	// PR#548 review P1：派发面必须 ≤ 校验面。
	mk := func(card map[string]interface{}) []byte {
		raw, _ := json.Marshal(v2Envelope(card))
		return raw
	}
	// inlineAction Submit 现在派发期可解析（消除死按钮：发送期校验通过的面必须可派发）。
	inlineCard := cardWithBody(map[string]interface{}{
		"type": "Input.Text", "id": "c",
		"inlineAction": map[string]interface{}{"type": "Action.Submit", "id": "go", "data": map[string]interface{}{"k": "v"}},
	})
	if d, found := SubmitAction(mk(inlineCard), "go"); !found || d["k"] != "v" {
		t.Errorf("inlineAction Submit 应可派发解析并取回 data, found=%v d=%v", found, d)
	}
	// 藏在叶子 TextBlock.items[] 内某元素 selectAction 下的 Submit：Validate 不递归
	// TextBlock.items（发送期不校验），派发侧也不得解析 —— 否则派发面 > 校验面。修复前
	// findSubmitInElements 无条件递归 items 会进到内层、命中其 selectAction Submit；修复
	// 后仅容器类递归，TextBlock.items 不进。用 selectAction 承载才有区分度（裸 Submit
	// 元素本就不被 findSubmitInElements 匹配，无论是否递归 —— 那样断言是空跑）。
	leafCard := cardWithBody(map[string]interface{}{
		"type": "TextBlock", "text": "x",
		"items": []interface{}{map[string]interface{}{
			"type": "Container",
			"selectAction": map[string]interface{}{
				"type": "Action.Submit", "id": "sneaky", "data": map[string]interface{}{"x": float64(1)},
			},
		}},
	})
	if _, found := SubmitAction(mk(leafCard), "sneaky"); found {
		t.Error("藏在 TextBlock.items[] 内元素 selectAction 下的 Submit 不得被派发解析（派发面 ≤ 校验面）")
	}
}

func TestValidateFrameUniqueIDs(t *testing.T) {
	// D1：Action.Submit / Input.* 的 id 帧内唯一。
	dupActions := v2Envelope(map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5",
		"body": []interface{}{map[string]interface{}{"type": "TextBlock", "text": "x"}},
		"actions": []interface{}{
			map[string]interface{}{"type": "Action.Submit", "id": "a"},
			map[string]interface{}{"type": "Action.Submit", "id": "a"},
		},
	})
	if err := Validate(dupActions); !errors.Is(err, ErrCardBadShape) {
		t.Errorf("重复 Action.Submit id 应拒, err=%v", err)
	}

	// Input id 与 Action id 撞车也算重复（同一 seenIDs 命名空间）。
	dupMixed := v2Envelope(map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5",
		"body":    []interface{}{map[string]interface{}{"type": "Input.Text", "id": "x"}},
		"actions": []interface{}{map[string]interface{}{"type": "Action.Submit", "id": "x"}},
	})
	if err := Validate(dupMixed); !errors.Is(err, ErrCardBadShape) {
		t.Errorf("Input/Action id 撞车应拒, err=%v", err)
	}

	// Action.Submit 缺 id → 拒绝。
	noID := v2Envelope(map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5",
		"body":    []interface{}{map[string]interface{}{"type": "TextBlock", "text": "x"}},
		"actions": []interface{}{map[string]interface{}{"type": "Action.Submit"}},
	})
	if err := Validate(noID); !errors.Is(err, ErrCardBadShape) {
		t.Errorf("Action.Submit 缺 id 应拒, err=%v", err)
	}
}

func TestValidateSubmitDataMustBeObject(t *testing.T) {
	bad := v2Envelope(map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5",
		"body":    []interface{}{map[string]interface{}{"type": "TextBlock", "text": "x"}},
		"actions": []interface{}{map[string]interface{}{"type": "Action.Submit", "id": "a", "data": "not-object"}},
	})
	if err := Validate(bad); !errors.Is(err, ErrCardBadShape) {
		t.Errorf("Action.Submit.data 非对象应拒, err=%v", err)
	}
}

func TestSubmitActionExtractsData(t *testing.T) {
	env := v2Envelope(map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5",
		"body": []interface{}{map[string]interface{}{"type": "TextBlock", "text": "x"}},
		"actions": []interface{}{map[string]interface{}{
			"type": "Action.Submit", "id": "approve",
			"data": map[string]interface{}{"action": "approve", "record_id": float64(42)},
		}},
	})
	raw, _ := json.Marshal(env)

	data, found := SubmitAction(raw, "approve")
	if !found {
		t.Fatal("approve 应命中")
	}
	if data["action"] != "approve" || data["record_id"] != float64(42) {
		t.Errorf("data 提取错误: %v", data)
	}

	// 未知 id → 未命中（伪造 / 被重写移除的按钮 fail-closed）。
	if _, found := SubmitAction(raw, "ghost"); found {
		t.Error("未知 action_id 不应命中")
	}

	// 命中但无 data → data=nil, found=true。
	env2 := v2Envelope(map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5",
		"body":    []interface{}{map[string]interface{}{"type": "TextBlock", "text": "x"}},
		"actions": []interface{}{map[string]interface{}{"type": "Action.Submit", "id": "plain"}},
	})
	raw2, _ := json.Marshal(env2)
	if data, found := SubmitAction(raw2, "plain"); !found || data != nil {
		t.Errorf("无 data 应 found=true data=nil, got found=%v data=%v", found, data)
	}
}

func TestValidateInputsTrustBoundary(t *testing.T) {
	env := v2Envelope(map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5",
		"body": []interface{}{
			map[string]interface{}{"type": "Input.Text", "id": "comment"},
			map[string]interface{}{"type": "Input.Toggle", "id": "agree"},
			map[string]interface{}{"type": "Input.ChoiceSet", "id": "pick",
				"choices": []interface{}{
					map[string]interface{}{"title": "A", "value": "a"},
					map[string]interface{}{"title": "B", "value": "b"},
				}},
		},
	})
	raw, _ := json.Marshal(env)

	// 合法：声明过的 id + 合法值。
	if err := ValidateInputs(raw, map[string]interface{}{
		"comment": "LGTM", "agree": "true", "pick": "a",
	}); err != nil {
		t.Errorf("合法 inputs 应通过, err=%v", err)
	}

	// 未声明键 → 拒。
	if err := ValidateInputs(raw, map[string]interface{}{"ghost": "x"}); !errors.Is(err, ErrCardInputInvalid) {
		t.Errorf("未声明 input 应拒, err=%v", err)
	}
	// ChoiceSet 越界 → 拒。
	if err := ValidateInputs(raw, map[string]interface{}{"pick": "z"}); !errors.Is(err, ErrCardInputInvalid) {
		t.Errorf("ChoiceSet 越界应拒, err=%v", err)
	}
	// Toggle 非声明值 → 拒。
	if err := ValidateInputs(raw, map[string]interface{}{"agree": "maybe"}); !errors.Is(err, ErrCardInputInvalid) {
		t.Errorf("Toggle 非法值应拒, err=%v", err)
	}
	// 非字符串值 → 拒。
	if err := ValidateInputs(raw, map[string]interface{}{"comment": 123}); !errors.Is(err, ErrCardInputInvalid) {
		t.Errorf("非字符串值应拒, err=%v", err)
	}
	// Input.Text 超 4KiB → 拒。
	if err := ValidateInputs(raw, map[string]interface{}{"comment": strings.Repeat("x", MaxInputTextBytes+1)}); !errors.Is(err, ErrCardInputInvalid) {
		t.Errorf("Input.Text 超限应拒, err=%v", err)
	}
	// 空 inputs → 通过（no-op）。
	if err := ValidateInputs(raw, nil); err != nil {
		t.Errorf("空 inputs 应 no-op, err=%v", err)
	}
}

func TestValidateInputsMultiSelect(t *testing.T) {
	env := v2Envelope(map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5",
		"body": []interface{}{
			map[string]interface{}{"type": "Input.ChoiceSet", "id": "tags", "isMultiSelect": true,
				"choices": []interface{}{
					map[string]interface{}{"title": "A", "value": "a"},
					map[string]interface{}{"title": "B", "value": "b"},
				}},
		},
	})
	raw, _ := json.Marshal(env)
	// 逗号分隔子集合法。
	if err := ValidateInputs(raw, map[string]interface{}{"tags": "a,b"}); err != nil {
		t.Errorf("multiSelect 合法子集应通过, err=%v", err)
	}
	// 含未声明项 → 拒。
	if err := ValidateInputs(raw, map[string]interface{}{"tags": "a,z"}); !errors.Is(err, ErrCardInputInvalid) {
		t.Errorf("multiSelect 含未声明项应拒, err=%v", err)
	}
	// 空子集合法。
	if err := ValidateInputs(raw, map[string]interface{}{"tags": ""}); err != nil {
		t.Errorf("multiSelect 空子集应通过, err=%v", err)
	}
}

func TestCardSeqReads(t *testing.T) {
	env := v2Envelope(cardWithBody(map[string]interface{}{"type": "TextBlock", "text": "x"}))
	if _, ok := CardSeq(env); ok {
		t.Error("无 card_seq 应 ok=false")
	}
	// json.Number 口径（BindJSON/decodeEnvelope UseNumber 场景，实时唯一入口）；
	// float64 已刻意不再接受（PR#548 review P2-d，防 >2^53 静默坍缩）。
	env["card_seq"] = json.Number("3")
	if seq, ok := CardSeq(env); !ok || seq != 3 {
		t.Errorf("card_seq 应为 3, got %d ok=%v", seq, ok)
	}
	// float64 不再被识别 → 退化为 (0,false)（fail-safe，绝不接受被截断的序号）。
	env["card_seq"] = float64(3)
	if _, ok := CardSeq(env); ok {
		t.Error("float64 card_seq 应不被接受(P2-d)")
	}

	// json.Number 口径（BindJSON UseNumber 场景）。
	edit := `{"type":17,"card":{"type":"AdaptiveCard","version":"1.5","body":[{"type":"TextBlock","text":"x"}]},"card_version":"1.5","profile":"octo/v2","card_seq":5}`
	if seq, ok := CardSeqFromContentEdit(edit); !ok || seq != 5 {
		t.Errorf("CardSeqFromContentEdit 应为 5, got %d ok=%v", seq, ok)
	}
}

func TestCardSeqPrecisionAbove2Pow53(t *testing.T) {
	// PR#548 review：card_seq 必须以精确 int64 解析。普通 json.Unmarshal 经 float64
	// 会把 >2^53 的相邻帧序号坍缩为相等，令 D9 CAS（stored ≥ incoming 即拒）失真 ——
	// 迟到帧被接受或有效推进被误判 stale。纳秒 epoch(~1.75e18)/雪花号皆 > 2^53。
	const a = int64(1750000000000000001) // > 2^53(=9007199254740992)
	const b = int64(1750000000000000002)
	body := `[{"type":"TextBlock","text":"x"}]`
	editOf := func(seq int64) string {
		return `{"type":17,"card":{"type":"AdaptiveCard","version":"1.5","body":` + body +
			`},"card_version":"1.5","profile":"octo/v2","card_seq":` + strconv.FormatInt(seq, 10) + `}`
	}

	seqA, okA := CardSeqFromContentEdit(editOf(a))
	seqB, okB := CardSeqFromContentEdit(editOf(b))
	if !okA || !okB {
		t.Fatalf("card_seq 应解析成功, okA=%v okB=%v", okA, okB)
	}
	if seqA != a || seqB != b {
		t.Errorf("card_seq 精度丢失: seqA=%d(want %d) seqB=%d(want %d)", seqA, a, seqB, b)
	}
	if seqA == seqB {
		t.Errorf("相邻 >2^53 card_seq 不得坍缩为相等: %d == %d", seqA, seqB)
	}

	// normalize 往返也必须保精 —— send 路径读的是 NormalizeContentEdit 之后的 card_seq
	// (send.go: CardSeqFromContentEdit(normalized))，量化点也在这条往返里。
	norm, err := NormalizeContentEdit(editOf(a))
	if err != nil {
		t.Fatalf("normalize 失败: %v", err)
	}
	if seq, ok := CardSeqFromContentEdit(norm); !ok || seq != a {
		t.Errorf("normalize 往返后 card_seq 应仍为 %d, got %d ok=%v", a, seq, ok)
	}

	// 非整数 card_seq → (0,false)：D9 退化为 last-write-wins，而非误取截断值。
	frac := `{"type":17,"card":{"type":"AdaptiveCard","version":"1.5","body":` + body +
		`},"card_version":"1.5","profile":"octo/v2","card_seq":1.5}`
	if seq, ok := CardSeqFromContentEdit(frac); ok {
		t.Errorf("非整数 card_seq 应 ok=false, got %d", seq)
	}
}

func TestNormalizeContentEdit(t *testing.T) {
	// type-17 编辑体：validate + Finalize，plain 被服务端重算。
	edit := `{"type":17,"card":{"type":"AdaptiveCard","version":"1.5","body":[{"type":"TextBlock","text":"审批已通过"}]},"plain":"forged","card_version":"1.5","profile":"octo/v2"}`
	out, err := NormalizeContentEdit(edit)
	if err != nil {
		t.Fatalf("合法 type-17 编辑体应通过, err=%v", err)
	}
	if strings.Contains(out, "forged") {
		t.Error("plain 应被服务端重算覆盖")
	}
	if !strings.Contains(out, "审批已通过") {
		t.Error("权威 plain 应来自卡片内容")
	}

	// 非卡片编辑体：原样返回（richtext 路径不变）。
	rich := `{"type":14,"content":"..."}`
	if out, err := NormalizeContentEdit(rich); err != nil || out != rich {
		t.Errorf("非卡片编辑体应原样返回, out=%q err=%v", out, err)
	}

	// 脏卡片（白名单外元素）→ 拒。
	dirty := `{"type":17,"card":{"type":"AdaptiveCard","version":"1.5","body":[{"type":"Action.Execute","id":"e"}]},"card_version":"1.5","profile":"octo/v2"}`
	if _, err := NormalizeContentEdit(dirty); err == nil {
		t.Error("脏卡片编辑体应拒")
	}
}
