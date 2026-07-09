package cardmsg

// card-message-p3-rich-inputs（P3-3）：octo/v2 输入白名单扩容
// Input.Number/Date/Time（均 AC 1.0，落在固定 card_version="1.5" 内），
// 发送期继承现有 Input.* 纪律；提交期按「形状可信」信任边界校验值
// （声明过 + 类型对 + 声明区间内），isRequired/regex 仍不服务端强制。

import (
	"encoding/json"
	"errors"
	"testing"
)

// numDateTimeCard 构造仅含单个新输入元素的 octo/v2 卡片。
func richInputCard(el map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5",
		"body": []interface{}{el},
	}
}

// TestValidateV2RichInputWhitelist：Number/Date/Time 在 octo/v2 放行、octo/v1 越级拒。
func TestValidateV2RichInputWhitelist(t *testing.T) {
	for _, typ := range []string{"Input.Number", "Input.Date", "Input.Time"} {
		el := map[string]interface{}{"type": typ, "id": "f"}
		if err := Validate(v2Envelope(richInputCard(el))); err != nil {
			t.Errorf("octo/v2 应放行 %s, err=%v", typ, err)
		}
		if err := Validate(envelope(richInputCard(el))); !errors.Is(err, ErrCardUnknownElement) {
			t.Errorf("octo/v1 携带 %s 应拒(越级), err=%v", typ, err)
		}
	}
}

// TestValidateV2RichInputIDRequired：新类型缺 id → ErrCardBadShape。
func TestValidateV2RichInputIDRequired(t *testing.T) {
	for _, typ := range []string{"Input.Number", "Input.Date", "Input.Time"} {
		el := map[string]interface{}{"type": typ} // 无 id
		if err := Validate(v2Envelope(richInputCard(el))); !errors.Is(err, ErrCardBadShape) {
			t.Errorf("%s 缺 id 应拒, err=%v", typ, err)
		}
	}
}

// TestValidateV2RichInputDuplicateID：新类型 id 帧内重复 → 冲突拒。
func TestValidateV2RichInputDuplicateID(t *testing.T) {
	card := map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5",
		"body": []interface{}{
			map[string]interface{}{"type": "Input.Number", "id": "dup"},
			map[string]interface{}{"type": "Input.Date", "id": "dup"},
		},
	}
	if err := Validate(v2Envelope(card)); err == nil {
		t.Error("帧内重复 id 应拒，实际放行")
	}
}

// TestValidateV2RichInputLabelURL：新类型 label/errorMessage 的 javascript: markdown 链接 → 拒。
func TestValidateV2RichInputLabelURL(t *testing.T) {
	for _, field := range []string{"label", "errorMessage"} {
		el := map[string]interface{}{
			"type": "Input.Number", "id": "n",
			field: "点[这里](javascript:alert(1))",
		}
		if err := Validate(v2Envelope(richInputCard(el))); !errors.Is(err, ErrCardBadURLScheme) {
			t.Errorf("Input.Number.%s 的 javascript: 链接应拒, err=%v", field, err)
		}
	}
}

// TestValidateStyleTolerance：1.5 内 renderer-only 风格属性应被发送期容忍。
func TestValidateStyleTolerance(t *testing.T) {
	cases := []map[string]interface{}{
		{"type": "Input.Text", "id": "p", "style": "password"},
		{"type": "Input.ChoiceSet", "id": "c1", "style": "filtered",
			"choices": []interface{}{map[string]interface{}{"title": "A", "value": "a"}}},
		{"type": "Input.ChoiceSet", "id": "c2", "style": "expanded",
			"choices": []interface{}{map[string]interface{}{"title": "A", "value": "a"}}},
	}
	for _, el := range cases {
		if err := Validate(v2Envelope(richInputCard(el))); err != nil {
			t.Errorf("style=%v 应被容忍, err=%v", el["style"], err)
		}
	}
}

// TestValidateInputsNumber：Input.Number 值校验（格式 + min/max + 空放行）。
func TestValidateInputsNumber(t *testing.T) {
	env := v2Envelope(richInputCard(map[string]interface{}{
		"type": "Input.Number", "id": "qty", "min": float64(1), "max": float64(10),
	}))
	raw, _ := json.Marshal(env)

	ok := []string{"1", "10", "5", "3.5", ""} // 区间内 + 空(未填)放行
	for _, v := range ok {
		if err := ValidateInputs(raw, map[string]interface{}{"qty": v}); err != nil {
			t.Errorf("Input.Number 合法值 %q 应通过, err=%v", v, err)
		}
	}
	bad := []string{"abc", "0", "11", "1e3"} // 非数字 / 越下界 / 越上界
	for _, v := range bad {
		if err := ValidateInputs(raw, map[string]interface{}{"qty": v}); !errors.Is(err, ErrCardInputInvalid) {
			t.Errorf("Input.Number 非法值 %q 应拒, err=%v", v, err)
		}
	}
}

// TestValidateInputsNumberNoBounds：未声明 min/max 时只校验「是数字」。
func TestValidateInputsNumberNoBounds(t *testing.T) {
	env := v2Envelope(richInputCard(map[string]interface{}{"type": "Input.Number", "id": "n"}))
	raw, _ := json.Marshal(env)
	if err := ValidateInputs(raw, map[string]interface{}{"n": "999999"}); err != nil {
		t.Errorf("无界 Input.Number 任意数字应通过, err=%v", err)
	}
	if err := ValidateInputs(raw, map[string]interface{}{"n": "-3.14"}); err != nil {
		t.Errorf("无界 Input.Number 负数应通过, err=%v", err)
	}
	if err := ValidateInputs(raw, map[string]interface{}{"n": "x"}); !errors.Is(err, ErrCardInputInvalid) {
		t.Errorf("Input.Number 非数字应拒, err=%v", err)
	}
}

// TestValidateInputsDate：Input.Date 值校验（YYYY-MM-DD + 区间 + 空放行）。
func TestValidateInputsDate(t *testing.T) {
	env := v2Envelope(richInputCard(map[string]interface{}{
		"type": "Input.Date", "id": "day", "min": "2026-01-01", "max": "2026-12-31",
	}))
	raw, _ := json.Marshal(env)

	ok := []string{"2026-01-01", "2026-12-31", "2026-07-09", ""}
	for _, v := range ok {
		if err := ValidateInputs(raw, map[string]interface{}{"day": v}); err != nil {
			t.Errorf("Input.Date 合法值 %q 应通过, err=%v", v, err)
		}
	}
	bad := []string{"2026/07/09", "07-09-2026", "2025-12-31", "2027-01-01", "2026-13-01", "notadate"}
	for _, v := range bad {
		if err := ValidateInputs(raw, map[string]interface{}{"day": v}); !errors.Is(err, ErrCardInputInvalid) {
			t.Errorf("Input.Date 非法值 %q 应拒, err=%v", v, err)
		}
	}
}

// TestValidateInputsTime：Input.Time 值校验（HH:MM 24h + 区间 + 空放行）。
func TestValidateInputsTime(t *testing.T) {
	env := v2Envelope(richInputCard(map[string]interface{}{
		"type": "Input.Time", "id": "t", "min": "09:00", "max": "18:00",
	}))
	raw, _ := json.Marshal(env)

	ok := []string{"09:00", "18:00", "12:30", ""}
	for _, v := range ok {
		if err := ValidateInputs(raw, map[string]interface{}{"t": v}); err != nil {
			t.Errorf("Input.Time 合法值 %q 应通过, err=%v", v, err)
		}
	}
	bad := []string{"8:00", "08:60", "24:00", "18:01", "08:59", "0900", "notatime"}
	for _, v := range bad {
		if err := ValidateInputs(raw, map[string]interface{}{"t": v}); !errors.Is(err, ErrCardInputInvalid) {
			t.Errorf("Input.Time 非法值 %q 应拒, err=%v", v, err)
		}
	}
}

// TestValidateInputsRichUndeclared：新类型仍受 fail-closed（未声明键拒 / 非字符串拒）。
func TestValidateInputsRichUndeclared(t *testing.T) {
	env := v2Envelope(richInputCard(map[string]interface{}{"type": "Input.Number", "id": "n"}))
	raw, _ := json.Marshal(env)
	if err := ValidateInputs(raw, map[string]interface{}{"ghost": "1"}); !errors.Is(err, ErrCardInputInvalid) {
		t.Errorf("未声明键应拒, err=%v", err)
	}
	if err := ValidateInputs(raw, map[string]interface{}{"n": 123}); !errors.Is(err, ErrCardInputInvalid) {
		t.Errorf("非字符串值应拒, err=%v", err)
	}
}

// TestValidateInputsNumberRejectsNonFinite：Input.Number 必须拒非有限数。strconv.ParseFloat
// 接受 "NaN"/"Inf"/"Infinity"（大小写、正负变体）且**不报 error**，其中 NaN 与 min/max 的
// 所有比较恒 false → 会绕过区间校验。非有限数不是合法数值输入，不得当「形状可信」值透传
// 给 bot（信任边界）。无界与有界 Number 都必须显式拒。
func TestValidateInputsNumberRejectsNonFinite(t *testing.T) {
	nonFinite := []string{"NaN", "nan", "Inf", "inf", "+Inf", "-Inf", "Infinity", "-infinity"}
	unbounded := v2Envelope(richInputCard(map[string]interface{}{"type": "Input.Number", "id": "n"}))
	rawU, _ := json.Marshal(unbounded)
	bounded := v2Envelope(richInputCard(map[string]interface{}{
		"type": "Input.Number", "id": "n", "min": float64(1), "max": float64(10),
	}))
	rawB, _ := json.Marshal(bounded)
	for _, v := range nonFinite {
		if err := ValidateInputs(rawU, map[string]interface{}{"n": v}); !errors.Is(err, ErrCardInputInvalid) {
			t.Errorf("无界 Input.Number 应拒非有限数 %q, err=%v", v, err)
		}
		if err := ValidateInputs(rawB, map[string]interface{}{"n": v}); !errors.Is(err, ErrCardInputInvalid) {
			t.Errorf("有界 Input.Number 应拒非有限数 %q, err=%v", v, err)
		}
	}
}

// TestSubmitActionDispatchRichInputInlineAction：Number/Date/Time 的 inlineAction Submit
// 发送期校验通过后必须派发期可解析 —— 否则「发送通过、点击 invalid」死按钮。发送期已对全部
// isInputElement 校验 inlineAction，派发侧必须对齐（校验面 == 派发面，同
// TestSubmitActionDispatchMatchesValidation，覆盖 P3-3 新增输入类型）。
func TestSubmitActionDispatchRichInputInlineAction(t *testing.T) {
	for _, typ := range []string{"Input.Number", "Input.Date", "Input.Time"} {
		env := v2Envelope(cardWithBody(map[string]interface{}{
			"type": typ, "id": "f",
			"inlineAction": map[string]interface{}{
				"type": "Action.Submit", "id": "go", "data": map[string]interface{}{"k": "v"},
			},
		}))
		if err := Validate(env); err != nil {
			t.Errorf("%s.inlineAction Submit 发送期应通过, err=%v", typ, err)
		}
		raw, _ := json.Marshal(env)
		if d, found := SubmitAction(raw, "go"); !found || d["k"] != "v" {
			t.Errorf("%s.inlineAction Submit 应派发可解析, found=%v d=%v", typ, found, d)
		}
	}
}
