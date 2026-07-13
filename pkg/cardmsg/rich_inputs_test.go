package cardmsg

// card-message-p3-rich-inputs（P3-3）：octo/v2 输入白名单扩容
// Input.Number/Date/Time（均 AC 1.0，落在固定 card_version="1.5" 内），
// 发送期继承现有 Input.* 纪律；提交期按「形状可信」信任边界只校验值的**格式/类型**
// （声明过 + 类型对），min/max 区间不服务端强制（下放 bot，PR#556）；isRequired/regex
// 同样不服务端强制。

import (
	"encoding/json"
	"errors"
	"testing"
)

// richInputCard 构造仅含单个新输入元素的 octo/v2 卡片。
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

// TestValidateInputsNumber：Input.Number 只校验「是合法有限数」——声明了 min/max 也**不**
// 服务端强制（区间外的合法数字照样放行，range 下放 bot，PR#556）。空串=未填放行。
func TestValidateInputsNumber(t *testing.T) {
	// 刻意声明 min/max，用来验证越界值仍放行（服务端不强制区间）。
	env := v2Envelope(richInputCard(map[string]interface{}{
		"type": "Input.Number", "id": "qty", "min": float64(1), "max": float64(10),
	}))
	raw, _ := json.Marshal(env)

	ok := []string{"1", "10", "5", "3.5", "", "0", "11", "1e3", "-7", "999999"} // 含越界值：区间不强制
	for _, v := range ok {
		if err := ValidateInputs(raw, map[string]interface{}{"qty": v}); err != nil {
			t.Errorf("Input.Number 合法有限数 %q 应通过（min/max 不强制）, err=%v", v, err)
		}
	}
	bad := []string{"abc", "1,000", "12/34", "x"} // 只有非数字才拒
	for _, v := range bad {
		if err := ValidateInputs(raw, map[string]interface{}{"qty": v}); !errors.Is(err, ErrCardInputInvalid) {
			t.Errorf("Input.Number 非数字 %q 应拒, err=%v", v, err)
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

// TestValidateInputsDate：Input.Date 只校验 YYYY-MM-DD 格式 + 空放行；声明 min/max 不
// 服务端强制（区间外的合法日期照样放行，range 下放 bot，PR#556）。
func TestValidateInputsDate(t *testing.T) {
	env := v2Envelope(richInputCard(map[string]interface{}{
		"type": "Input.Date", "id": "day", "min": "2026-01-01", "max": "2026-12-31",
	}))
	raw, _ := json.Marshal(env)

	// 合法格式（含声明区间外的 2025-12-31 / 2027-01-01）+ 空 → 放行。
	ok := []string{"2026-01-01", "2026-12-31", "2026-07-09", "2025-12-31", "2027-01-01", ""}
	for _, v := range ok {
		if err := ValidateInputs(raw, map[string]interface{}{"day": v}); err != nil {
			t.Errorf("Input.Date 合法格式 %q 应通过（区间不强制）, err=%v", v, err)
		}
	}
	// 非法格式仍拒（错分隔 / 错顺序 / 非法日历日 / 非零填充 / 乱码）。
	bad := []string{"2026/07/09", "07-09-2026", "2026-13-01", "2026-7-9", "notadate"}
	for _, v := range bad {
		if err := ValidateInputs(raw, map[string]interface{}{"day": v}); !errors.Is(err, ErrCardInputInvalid) {
			t.Errorf("Input.Date 非法格式 %q 应拒, err=%v", v, err)
		}
	}
}

// TestValidateInputsTime：Input.Time 只校验 HH:MM(24h) 格式 + 空放行；声明 min/max 不
// 服务端强制（区间外的合法时间照样放行，range 下放 bot，PR#556）。
func TestValidateInputsTime(t *testing.T) {
	env := v2Envelope(richInputCard(map[string]interface{}{
		"type": "Input.Time", "id": "t", "min": "09:00", "max": "18:00",
	}))
	raw, _ := json.Marshal(env)

	// 合法格式（含声明区间外的 08:59 / 18:01）+ 空 → 放行。
	ok := []string{"09:00", "18:00", "12:30", "08:59", "18:01", "00:00", "23:59", ""}
	for _, v := range ok {
		if err := ValidateInputs(raw, map[string]interface{}{"t": v}); err != nil {
			t.Errorf("Input.Time 合法格式 %q 应通过（区间不强制）, err=%v", v, err)
		}
	}
	// 非法格式仍拒。
	bad := []string{"8:00", "08:60", "24:00", "0900", "notatime"}
	for _, v := range bad {
		if err := ValidateInputs(raw, map[string]interface{}{"t": v}); !errors.Is(err, ErrCardInputInvalid) {
			t.Errorf("Input.Time 非法格式 %q 应拒, err=%v", v, err)
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

// TestValidateInputsNumberRejectsNonFinite：Input.Number 必须拒非有限数——两层防线。
// (1) 字面 "NaN"/"Inf"/"Infinity"（strconv.ParseFloat 接受且**不报 error**）被严格 JSON 数字
// 文法门先挡下（非 JSON 数字形态）；(2) 文法合法但数值溢出的 "1e999"→±Inf 由 ParseFloat 的
// ErrRange + math.IsInf 挡下。非有限数不是合法数值输入，不得当「形状可信」值透传给 bot
// （信任边界；格式/类型校验，与已下放 bot 的 min/max range 无关）。声明与未声明 min/max 都拒。
func TestValidateInputsNumberRejectsNonFinite(t *testing.T) {
	nonFinite := []string{
		"NaN", "nan", "Inf", "inf", "+Inf", "-Inf", "Infinity", "-infinity", // 文法门挡下
		"1e999", "-1e999", "1E1000", // 文法合法但溢出 → 有限检查挡下
	}
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

// TestValidateInputsNumberRejectsParseFloatSuperset：strconv.ParseFloat 的文法是 JSON/JS
// Number 的**超集**（十六进制浮点 "0x1p4"、前导 "+"、前导零、裸小数点、下划线分隔等它都能
// 解析成有限数），会放行 bot 端 JSON 解析器拒绝或解读不同的串，造成静默数值错位。严格 JSON
// 数字文法必须把这些非 JSON 形态一并拒——仅靠 ParseFloat+有限检查会漏（PR#556 review #2）。
func TestValidateInputsNumberRejectsParseFloatSuperset(t *testing.T) {
	env := v2Envelope(richInputCard(map[string]interface{}{"type": "Input.Number", "id": "n"}))
	raw, _ := json.Marshal(env)
	// ParseFloat 宽容 / 非 JSON 数字文法的形态——全部必须拒。
	nonJSON := []string{
		"0x1p4", // 十六进制浮点（ParseFloat=16）
		"+5",    // 前导 +
		"05",    // 前导零
		".5",    // 缺整数部分
		"5.",    // 缺小数部分
		"1_000", // 下划线分隔（Go 数字字面量）
		" 5",    // 前导空白
		"5 ",    // 尾随空白
		"1e",    // 残缺指数
		"٥",     // 非 ASCII 数字（Unicode 数字必须拒，[0-9] 只认 ASCII）
	}
	for _, v := range nonJSON {
		if err := ValidateInputs(raw, map[string]interface{}{"n": v}); !errors.Is(err, ErrCardInputInvalid) {
			t.Errorf("Input.Number 应拒非 JSON 数字文法 %q, err=%v", v, err)
		}
	}
	// 反向锚点：合法 JSON 数字（含指数、负号、小数、-0）必须仍放行。
	okJSON := []string{"0", "-0", "42", "-7", "3.14", "1e3", "1E3", "1.5e-3", "-2.5E+2"}
	for _, v := range okJSON {
		if err := ValidateInputs(raw, map[string]interface{}{"n": v}); err != nil {
			t.Errorf("Input.Number 合法 JSON 数字 %q 应放行, err=%v", v, err)
		}
	}
}

// TestValidateInputsRichInputNested：新输入类型声明在嵌套 Container / ColumnSet>Column 内时，
// 提交期 collectInputSpecs 仍能递归采集到并按类型校验（采集面覆盖新类型，与发送期遍历同口径）。
func TestValidateInputsRichInputNested(t *testing.T) {
	card := map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5",
		"body": []interface{}{
			map[string]interface{}{"type": "Container", "items": []interface{}{
				map[string]interface{}{"type": "Input.Number", "id": "qty"},
			}},
			map[string]interface{}{"type": "ColumnSet", "columns": []interface{}{
				map[string]interface{}{"type": "Column", "items": []interface{}{
					map[string]interface{}{"type": "Input.Date", "id": "day"},
				}},
			}},
		},
	}
	raw, _ := json.Marshal(v2Envelope(card))
	if err := ValidateInputs(raw, map[string]interface{}{"qty": "5", "day": "2026-07-09"}); err != nil {
		t.Errorf("嵌套声明的合法输入应放行, err=%v", err)
	}
	// 非法值按类型拒 —— 证明递归确实采集到了 spec 并施加了类型/格式校验（而非未声明放行）。
	if err := ValidateInputs(raw, map[string]interface{}{"qty": "0x1p4"}); !errors.Is(err, ErrCardInputInvalid) {
		t.Errorf("嵌套 Input.Number 非法值应拒, err=%v", err)
	}
	if err := ValidateInputs(raw, map[string]interface{}{"day": "2026/07/09"}); !errors.Is(err, ErrCardInputInvalid) {
		t.Errorf("嵌套 Input.Date 非法格式应拒, err=%v", err)
	}
}
