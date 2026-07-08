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
	"strings"
)

const (
	// MaxInputTextBytes 单个 Input.Text 值上限（UTF-8 字节，D11）。
	MaxInputTextBytes = 4 << 10
	// MaxInputsBytes inputs 序列化总量上限（D11）。
	MaxInputsBytes = 16 << 10
)

// inputSpec 是生效帧里一个已声明 Input.* 元素的校验视图。
type inputSpec struct {
	typ         string
	choices     map[string]struct{} // Input.ChoiceSet 声明的合法 value 集
	multiSelect bool                // isMultiSelect：值为逗号分隔子集（AC 线上格式）
	valueOn     string              // Input.Toggle；缺省 "true"/"false"
	valueOff    string
}

// ValidateInputs 按 D11 校验 card/action 请求的 inputs（fail-closed）：
//   - 每个键必须命中生效帧声明的 Input.* id；
//   - 值必须是字符串；Input.Text ≤ 4KiB；Input.Toggle 必须等于 valueOn/valueOff；
//     Input.ChoiceSet 必须命中声明的 choice value（multiSelect 为逗号分隔子集，
//     单选允许 "" 表示未选择）；
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
		if t == "Input.Text" || t == "Input.Toggle" || t == "Input.ChoiceSet" {
			if id, _ := el["id"].(string); id != "" {
				spec := inputSpec{typ: t}
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
		// items/columns 仅对容器类递归，与 Validate 一致（叶子元素的 items/columns
		// 发送期不被遍历，派发侧也不得把其中的 input id 当作「已声明」，否则 D11 声明面
		// > 校验面 —— 与 findSubmitInElements 同口径）。
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
		}
	}
}
