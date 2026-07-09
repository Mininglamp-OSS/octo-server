package cardmsg

// card-message-interaction P2 D11（round-3 P1-3，spec: .octospec/tasks/
// card-message-interaction/brief.md）：card/action 上行 inputs 的信任边界。
//
// inputs 只能携带「生效帧」声明过的 Input.* id；值一律是字符串（AC submit 线上
// 语义），逐类型校验 + 尺寸上限；未声明键 fail-closed。校验通过后
// event_data.inputs 才允许原样透传给 bot —— bot 拿到的是「形状可信」的值
// （声明过、类型对、有上限），内容仍是不可信用户文本，bot 侧照常转义。
// isRequired 不在服务端强制：表单完整性是端上 UX + bot 业务校验的事。

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// MaxInputTextBytes 单个 Input.Text 值上限（UTF-8 字节，D11）。
	MaxInputTextBytes = 4 << 10
	// MaxInputsBytes inputs 序列化总量上限（D11）。
	MaxInputsBytes = 16 << 10
)

// jsonNumberPattern 是 RFC 8259 JSON 数字文法（= JS Number / bot 端 JSON 解析口径）：
// 可选前导负号、整数部分无前导零、可选小数、可选指数。刻意**不**用 strconv.ParseFloat
// 判定「是不是数字」：ParseFloat 接受 Go 专有文法——下划线分隔（"1_000"）、十六进制浮点
// （"0x1p4"）、前导 "+"、前导零、裸 "NaN"/"Inf"——是 JSON/JS Number 的**超集**，会放行
// bot 端 JSON 解析器拒绝或解读不同的串，造成「服务端判合法、bot 拿到的却是另一个数 / 解析
// 失败」的静默数值错位。用严格文法先把服务端的「合法数字」钉到与 bot 收到的 JSON 一致的
// 口径，再用 ParseFloat 求值兜溢出（PR#556 review）。
var jsonNumberPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][-+]?[0-9]+)?$`)

// inputSpec 是生效帧里一个已声明 Input.* 元素的校验视图。
type inputSpec struct {
	typ         string
	choices     map[string]struct{} // Input.ChoiceSet 声明的合法 value 集
	multiSelect bool                // isMultiSelect：值为逗号分隔子集（AC 线上格式）
	valueOn     string              // Input.Toggle；缺省 "true"/"false"
	valueOff    string
	// Number/Date/Time 只校验格式，不采集 min/max —— 区间不服务端强制（PR#556 review：
	// AC 规范把 min/max 定义为「可被客户端忽略的 hint」，合规客户端可提交越界值，而
	// card/action 把错误折叠成单一 invalid，越界拒绝会让用户收到无从更正的笼统错；区间
	// 校验下放 bot 业务逻辑，与 isRequired/regex 同）。
}

// ValidateInputs 按 D11 校验 card/action 请求的 inputs（fail-closed）：
//   - 每个键必须命中生效帧声明的 Input.* id；
//   - 值必须是字符串；Input.Text ≤ 4KiB；Input.Toggle 必须等于 valueOn/valueOff；
//     Input.ChoiceSet 必须命中声明的 choice value（multiSelect 为逗号分隔子集，
//     单选允许 "" 表示未选择）；
//   - P3-3：Input.Number 必须是合法 JSON 数字且有限；Input.Date 必须是 YYYY-MM-DD；
//     Input.Time 必须是 HH:MM(24h)；三者 "" 均视为未填放行。min/max 区间**不**服务端
//     强制（AC 规范定义为可忽略 hint，区间校验下放 bot，与 isRequired/regex 同）；
//   - 序列化总量 ≤ 16KiB。
//
// envelopeRaw 是「生效卡片」信封字节（content_edit 优先，与 SubmitAction 的取帧
// 口径一致 —— 校验对象必须是用户实际看到的那一帧）。
func ValidateInputs(envelopeRaw []byte, inputs map[string]interface{}) error {
	if len(inputs) == 0 {
		return nil
	}
	raw, err := json.Marshal(inputs)
	if err != nil {
		return fmt.Errorf("%w: inputs 无法序列化", ErrCardInputInvalid)
	}
	if len(raw) > MaxInputsBytes {
		return fmt.Errorf("%w: inputs 总量超过 %d 字节", ErrCardInputInvalid, MaxInputsBytes)
	}
	specs := collectInputSpecs(envelopeRaw)
	for key, v := range inputs {
		spec, declared := specs[key]
		if !declared {
			return fmt.Errorf("%w: 未声明的 input %q", ErrCardInputInvalid, key)
		}
		s, isStr := v.(string)
		if !isStr {
			return fmt.Errorf("%w: input %q 的值必须是字符串", ErrCardInputInvalid, key)
		}
		switch spec.typ {
		case "Input.Text":
			if len(s) > MaxInputTextBytes {
				return fmt.Errorf("%w: input %q 超过 %d 字节", ErrCardInputInvalid, key, MaxInputTextBytes)
			}
		case "Input.Toggle":
			if s != spec.valueOn && s != spec.valueOff {
				return fmt.Errorf("%w: input %q 不是声明的 valueOn/valueOff", ErrCardInputInvalid, key)
			}
		case "Input.ChoiceSet":
			if spec.multiSelect {
				for _, part := range strings.Split(s, ",") {
					if part == "" {
						continue // 空子集/尾逗号：无选择是合法形状
					}
					if _, ok := spec.choices[part]; !ok {
						return fmt.Errorf("%w: input %q 含未声明选项 %q", ErrCardInputInvalid, key, part)
					}
				}
			} else if s != "" {
				if _, ok := spec.choices[s]; !ok {
					return fmt.Errorf("%w: input %q 不是声明的选项", ErrCardInputInvalid, key)
				}
			}
		case "Input.Number":
			// 空串=未填（isRequired 不服务端强制）；否则必须匹配严格 JSON 数字文法。min/max
			// 区间不服务端强制（下放 bot，见 inputSpec 注释）。
			if s == "" {
				break
			}
			// 先过严格 JSON 数字文法（见 jsonNumberPattern 注释：ParseFloat 的文法是 JSON/JS
			// Number 的超集，会静默放行 bot 端解析不同的串）。
			if !jsonNumberPattern.MatchString(s) {
				return fmt.Errorf("%w: input %q 不是合法数字", ErrCardInputInvalid, key)
			}
			// 文法已排除 NaN/Inf 字面量与 Go 专有形态；ParseFloat 在此仅用于求值兜数值溢出
			// （如 "1e999"→±Inf，返回 ErrRange）。非有限数不是合法数值输入，不得当「形状可信」
			// 值透传给 bot（信任边界：格式/类型校验，与已下放 bot 的 range 无关）。
			n, err := strconv.ParseFloat(s, 64)
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
				return fmt.Errorf("%w: input %q 不是有限数", ErrCardInputInvalid, key)
			}
		case "Input.Date":
			// 空串=未填；否则必须是 YYYY-MM-DD。声明区间不服务端强制（同 Number）。
			if s == "" {
				break
			}
			if !isValidDate(s) {
				return fmt.Errorf("%w: input %q 不是合法日期(YYYY-MM-DD)", ErrCardInputInvalid, key)
			}
		case "Input.Time":
			// 空串=未填；否则必须是 HH:MM(24h)。声明区间不服务端强制（同 Number）。
			if s == "" {
				break
			}
			if !isValidTime(s) {
				return fmt.Errorf("%w: input %q 不是合法时间(HH:MM)", ErrCardInputInvalid, key)
			}
		default:
			// 声明帧里出现「已白名单收集、但校验器无对应 case」的输入类型：fail-closed 拒。
			// 当前不可达——collectInputSpecs 只从 isInputElement（inputElements）收集，六类都
			// 有 case——但显式兜底防止未来往 inputElements 加类型却漏加 case 时退化成 fail-open
			// （「已声明即放行任意值」的天窗）。与信任边界「未覆盖即拒」一致（PR#556 review）。
			return fmt.Errorf("%w: input %q 类型 %q 无值校验", ErrCardInputInvalid, key, spec.typ)
		}
	}
	return nil
}

// collectInputSpecs 从生效帧信封字节提取声明的 Input.* 元素，遍历口径与
// SubmitAction 相同（body 内嵌套 Container/ColumnSet 均收集）。解析失败返回空集
// —— 查无声明即拒绝（fail-closed）。
func collectInputSpecs(envelopeRaw []byte) map[string]inputSpec {
	specs := make(map[string]inputSpec)
	var payload struct {
		Card map[string]interface{} `json:"card"`
	}
	if err := json.Unmarshal(envelopeRaw, &payload); err != nil || payload.Card == nil {
		return specs
	}
	if body, ok := payload.Card["body"].([]interface{}); ok {
		collectInputSpecsFromElements(body, specs)
	}
	return specs
}

func collectInputSpecsFromElements(items []interface{}, specs map[string]inputSpec) {
	for _, it := range items {
		el, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		t, _ := el["type"].(string)
		if isInputElement(t) {
			if id, _ := el["id"].(string); id != "" {
				spec := inputSpec{typ: t}
				// 仅 Toggle/ChoiceSet 需采集额外声明（valueOn/off、choices、multiSelect）；
				// Text/Number/Date/Time 只需 typ（提交期值校验只看格式，不采集 min/max —— 区间
				// 不服务端强制，见 inputSpec 注释），故无 case。
				switch t {
				case "Input.Toggle":
					spec.valueOn, spec.valueOff = "true", "false"
					if s, _ := el["valueOn"].(string); s != "" {
						spec.valueOn = s
					}
					if s, _ := el["valueOff"].(string); s != "" {
						spec.valueOff = s
					}
				case "Input.ChoiceSet":
					spec.choices = make(map[string]struct{})
					if choices, ok := el["choices"].([]interface{}); ok {
						for _, c := range choices {
							if ch, ok := c.(map[string]interface{}); ok {
								if v, _ := ch["value"].(string); v != "" {
									spec.choices[v] = struct{}{}
								}
							}
						}
					}
					spec.multiSelect, _ = el["isMultiSelect"].(bool)
				}
				specs[id] = spec
			}
		}
		// items/columns/cells 仅对容器类递归，与 Validate / findSubmitInElements 完全一致
		// （叶子元素的 items 发送期不被遍历，采集侧也不得把其中的 input id 当作「已声明」，
		// 否则声明面 > 校验面）。Table.rows→cells→items 同样承载标准元素（含 Input.*），必须
		// 递归 —— 否则 Table 单元格内的输入发送/派发都通过、提交却被当「未声明」拒（PR#556）。
		switch t {
		case "Container":
			if sub, ok := el["items"].([]interface{}); ok {
				collectInputSpecsFromElements(sub, specs)
			}
		case "ColumnSet":
			if cols, ok := el["columns"].([]interface{}); ok {
				for _, c := range cols {
					if col, ok := c.(map[string]interface{}); ok {
						if sub, ok := col["items"].([]interface{}); ok {
							collectInputSpecsFromElements(sub, specs)
						}
					}
				}
			}
		case "Table":
			if rows, ok := el["rows"].([]interface{}); ok {
				for _, r := range rows {
					row, ok := r.(map[string]interface{})
					if !ok {
						continue
					}
					cells, ok := row["cells"].([]interface{})
					if !ok {
						continue
					}
					for _, c := range cells {
						if cell, ok := c.(map[string]interface{}); ok {
							if sub, ok := cell["items"].([]interface{}); ok {
								collectInputSpecsFromElements(sub, specs)
							}
						}
					}
				}
			}
		}
	}
}

// isValidDate 校验 AC Input.Date 线上值格式（严格 YYYY-MM-DD + 真实日历日）。
// 先卡定宽/分隔位（拒非零填充如 "2026-7-9"），再用 time.Parse 拒非法日历日（如 13 月）。
func isValidDate(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// isValidTime 校验 AC Input.Time 线上值格式（严格 HH:MM 24h）。
func isValidTime(s string) bool {
	if len(s) != 5 || s[2] != ':' {
		return false
	}
	_, err := time.Parse("15:04", s)
	return err == nil
}
