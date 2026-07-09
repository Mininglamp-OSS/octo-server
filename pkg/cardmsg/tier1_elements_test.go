package cardmsg

// card-message P3-3 Tier 1：AC 1.5 展示元素补全 —— ImageSet(1.0)/RichTextBlock(1.2)/
// Table(1.5)/ActionSet(1.2)。四者都是展示类（octo/v1+v2 均放行；ActionSet 内的
// Action.Submit 仍受 octo/v2 门控）。每个元素覆盖四个面：发送期校验、URL allowlist、
// 派发对称（可载 Submit 处）、plain 派生。

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// ---- 发送期放行（v2 + v1，纯展示两档均可）----

func TestTier1ElementsAccepted(t *testing.T) {
	cards := map[string]map[string]interface{}{
		"ImageSet": cardWithBody(map[string]interface{}{
			"type": "ImageSet", "images": []interface{}{
				map[string]interface{}{"type": "Image", "url": "https://example.com/a.png"},
			}}),
		"RichTextBlock": cardWithBody(map[string]interface{}{
			"type": "RichTextBlock", "inlines": []interface{}{
				"plain run ", map[string]interface{}{"type": "TextRun", "text": "styled"},
			}}),
		"Table": cardWithBody(map[string]interface{}{
			"type": "Table", "rows": []interface{}{
				map[string]interface{}{"type": "TableRow", "cells": []interface{}{
					map[string]interface{}{"type": "TableCell", "items": []interface{}{
						map[string]interface{}{"type": "TextBlock", "text": "cell"},
					}},
				}},
			}}),
		"ActionSet": cardWithBody(map[string]interface{}{
			"type": "ActionSet", "actions": []interface{}{
				map[string]interface{}{"type": "Action.OpenUrl", "url": "https://example.com"},
			}}),
	}
	for name, card := range cards {
		if err := Validate(v2Envelope(card)); err != nil {
			t.Errorf("octo/v2 应放行 %s, err=%v", name, err)
		}
		if err := Validate(envelope(card)); err != nil {
			t.Errorf("octo/v1 应放行展示元素 %s, err=%v", name, err)
		}
	}
}

// ---- URL allowlist：javascript: 必须被拒（校验面 ≥ 渲染/派发面）----

func TestTier1ElementsURLAllowlist(t *testing.T) {
	bad := map[string]map[string]interface{}{
		"ImageSet.image.url": cardWithBody(map[string]interface{}{
			"type": "ImageSet", "images": []interface{}{
				map[string]interface{}{"type": "Image", "url": "javascript:alert(1)"},
			}}),
		"ImageSet.image.selectAction": cardWithBody(map[string]interface{}{
			"type": "ImageSet", "images": []interface{}{
				map[string]interface{}{"type": "Image", "url": "https://example.com/a.png",
					"selectAction": map[string]interface{}{"type": "Action.OpenUrl", "url": "javascript:x"}},
			}}),
		"RichTextBlock.textrun.selectAction": cardWithBody(map[string]interface{}{
			"type": "RichTextBlock", "inlines": []interface{}{
				map[string]interface{}{"type": "TextRun", "text": "t",
					"selectAction": map[string]interface{}{"type": "Action.OpenUrl", "url": "javascript:x"}},
			}}),
		"Table.cell.nested.image": cardWithBody(map[string]interface{}{
			"type": "Table", "rows": []interface{}{
				map[string]interface{}{"type": "TableRow", "cells": []interface{}{
					map[string]interface{}{"type": "TableCell", "items": []interface{}{
						map[string]interface{}{"type": "Image", "url": "javascript:x"},
					}},
				}},
			}}),
		"Table.cell.selectAction": cardWithBody(map[string]interface{}{
			"type": "Table", "rows": []interface{}{
				map[string]interface{}{"type": "TableRow", "cells": []interface{}{
					map[string]interface{}{"type": "TableCell",
						"selectAction": map[string]interface{}{"type": "Action.OpenUrl", "url": "javascript:x"},
						"items":        []interface{}{}},
				}},
			}}),
		"ActionSet.action.openurl": cardWithBody(map[string]interface{}{
			"type": "ActionSet", "actions": []interface{}{
				map[string]interface{}{"type": "Action.OpenUrl", "url": "javascript:x"},
			}}),
	}
	for name, card := range bad {
		if err := Validate(v2Envelope(card)); !errors.Is(err, ErrCardBadURLScheme) {
			t.Errorf("%s 的 javascript: 应被拒(ErrCardBadURLScheme), err=%v", name, err)
		}
	}
}

// ---- 结构错误 ----

func TestTier1ElementsBadShape(t *testing.T) {
	bad := map[string]map[string]interface{}{
		"ImageSet.images 非数组":       cardWithBody(map[string]interface{}{"type": "ImageSet", "images": "x"}),
		"RichTextBlock.inlines 非数组": cardWithBody(map[string]interface{}{"type": "RichTextBlock", "inlines": "x"}),
		"Table.rows 非数组":            cardWithBody(map[string]interface{}{"type": "Table", "rows": "x"}),
		"ActionSet.actions 非数组":     cardWithBody(map[string]interface{}{"type": "ActionSet", "actions": "x"}),
	}
	for name, card := range bad {
		if err := Validate(v2Envelope(card)); !errors.Is(err, ErrCardBadShape) {
			t.Errorf("%s 应拒(ErrCardBadShape), err=%v", name, err)
		}
	}
}

// ---- 派发对称：Submit 藏在这些位置必须可派发（否则死按钮）----

func TestTier1SubmitDispatchSymmetry(t *testing.T) {
	sub := func(id string) map[string]interface{} {
		return map[string]interface{}{"type": "Action.Submit", "id": id, "data": map[string]interface{}{"k": "v"}}
	}
	cases := map[string]map[string]interface{}{
		"ActionSet.actions": cardWithBody(map[string]interface{}{
			"type": "ActionSet", "actions": []interface{}{sub("go")},
		}),
		"ImageSet.image.selectAction": cardWithBody(map[string]interface{}{
			"type": "ImageSet", "images": []interface{}{
				map[string]interface{}{"type": "Image", "url": "https://example.com/a.png", "selectAction": sub("go")},
			}}),
		"RichTextBlock.textrun.selectAction": cardWithBody(map[string]interface{}{
			"type": "RichTextBlock", "inlines": []interface{}{
				map[string]interface{}{"type": "TextRun", "text": "t", "selectAction": sub("go")},
			}}),
		"Table.cell.selectAction": cardWithBody(map[string]interface{}{
			"type": "Table", "rows": []interface{}{
				map[string]interface{}{"type": "TableRow", "cells": []interface{}{
					map[string]interface{}{"type": "TableCell", "selectAction": sub("go"), "items": []interface{}{}},
				}},
			}}),
		"Table.cell.nested.selectAction": cardWithBody(map[string]interface{}{
			"type": "Table", "rows": []interface{}{
				map[string]interface{}{"type": "TableRow", "cells": []interface{}{
					map[string]interface{}{"type": "TableCell", "items": []interface{}{
						map[string]interface{}{"type": "Container", "items": []interface{}{}, "selectAction": sub("go")},
					}},
				}},
			}}),
	}
	for name, card := range cases {
		env := v2Envelope(card)
		if err := Validate(env); err != nil {
			t.Errorf("%s 发送期应通过, err=%v", name, err)
		}
		raw, _ := json.Marshal(env)
		if d, found := SubmitAction(raw, "go"); !found || d["k"] != "v" {
			t.Errorf("%s 的 Submit 应派发可解析, found=%v d=%v", name, found, d)
		}
	}
}

// ---- plain 派生 ----

func TestTier1PlainDerivation(t *testing.T) {
	card := cardWithBody(
		map[string]interface{}{"type": "RichTextBlock", "inlines": []interface{}{
			"Hello ", map[string]interface{}{"type": "TextRun", "text": "world"},
		}},
		map[string]interface{}{"type": "ImageSet", "images": []interface{}{
			map[string]interface{}{"type": "Image", "url": "https://example.com/a.png"},
		}},
		map[string]interface{}{"type": "Table", "rows": []interface{}{
			map[string]interface{}{"type": "TableRow", "cells": []interface{}{
				map[string]interface{}{"type": "TableCell", "items": []interface{}{
					map[string]interface{}{"type": "TextBlock", "text": "cellfact"},
				}},
			}},
		}},
		map[string]interface{}{"type": "ActionSet", "actions": []interface{}{
			map[string]interface{}{"type": "Action.OpenUrl", "url": "https://example.com", "title": "btn"},
		}},
	)
	plain := BuildPlain(card)
	for _, want := range []string{"Hello world", PlaceholderImage, "cellfact"} {
		if !strings.Contains(plain, want) {
			t.Errorf("plain 应含 %q, got=%q", want, plain)
		}
	}
	// ActionSet 的按钮标题不入 plain（动作是操作面）。
	if strings.Contains(plain, "btn") {
		t.Errorf("ActionSet 按钮标题不应入 plain, got=%q", plain)
	}
}

// TestTier1TableCellInputCollected：Table cell 内的 Input.* 提交期必须被采集为「已声明」。
// 发送期（w.elements 递归 cell.items）与派发期（findSubmitInElements 递归 Table）都进 cell，
// 采集期 collectInputSpecsFromElements 也必须递归 Table cell items —— 否则合法提交会被当
// 「未声明 input」拒（三个镜像面必须同步，PR#556 review）。
func TestTier1TableCellInputCollected(t *testing.T) {
	env := v2Envelope(cardWithBody(map[string]interface{}{
		"type": "Table", "rows": []interface{}{
			map[string]interface{}{"type": "TableRow", "cells": []interface{}{
				map[string]interface{}{"type": "TableCell", "items": []interface{}{
					map[string]interface{}{"type": "Input.Text", "id": "table_field"},
				}},
			}},
		}}))
	if err := Validate(env); err != nil {
		t.Fatalf("Table 内 Input.Text 发送期应通过, err=%v", err)
	}
	raw, _ := json.Marshal(env)
	// 已声明的 table 内 input 提交必须放行（不能被当未声明拒）。
	if err := ValidateInputs(raw, map[string]interface{}{"table_field": "hi"}); err != nil {
		t.Errorf("Table cell 内已声明 input 的提交应通过, err=%v", err)
	}
	// 反向：未声明 id 仍 fail-closed 拒（确认没顺手把声明面开太大）。
	if err := ValidateInputs(raw, map[string]interface{}{"ghost": "x"}); !errors.Is(err, ErrCardInputInvalid) {
		t.Errorf("未声明 input 仍应拒, err=%v", err)
	}
}
