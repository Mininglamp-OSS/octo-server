package cardmsg

// card-message-p3-rich-inputs（P3-3）：卡片元素白名单「单一权威」守卫。
// DisplayElements()/InputElements() 是校验器（validate.go）、inputs 采集（inputs.go）、
// D12 能力清单（GET /v1/bot/card/profile）三处共用的权威列表 —— 这些测试锁死列表成员
// 与校验器实际接受集不漂移（清单据此下发，漂移=对外谎报能力）。

import (
	"errors"
	"slices"
	"testing"
)

// TestInputElementsAuthority：InputElements() 与文档/校验器一致 —— 每个成员 octo/v2
// 放行、octo/v1 越级拒；非成员 Input.*（Input.Rating）即便 octo/v2 也拒。
func TestInputElementsAuthority(t *testing.T) {
	want := []string{
		"Input.Text", "Input.Toggle", "Input.ChoiceSet",
		"Input.Number", "Input.Date", "Input.Time",
	}
	got := InputElements()
	if !slices.Equal(got, want) {
		t.Fatalf("InputElements()=%v, want %v", got, want)
	}
	for _, typ := range got {
		el := map[string]interface{}{"type": typ, "id": "f"}
		if err := Validate(v2Envelope(richInputCard(el))); err != nil {
			t.Errorf("octo/v2 应放行权威成员 %s, err=%v", typ, err)
		}
		if err := Validate(envelope(richInputCard(el))); !errors.Is(err, ErrCardUnknownElement) {
			t.Errorf("octo/v1 应越级拒 %s, err=%v", typ, err)
		}
	}
	// 非白名单 Input.*：校验器必须拒（列表是唯一放行来源）。
	bogus := map[string]interface{}{"type": "Input.Rating", "id": "r"}
	if err := Validate(v2Envelope(richInputCard(bogus))); !errors.Is(err, ErrCardUnknownElement) {
		t.Errorf("非白名单 Input.Rating 应拒, err=%v", err)
	}
}

// TestDisplayElementsAuthority：DisplayElements() 与文档 §2 展示元素集一致，且**逐个从
// 列表派生**校验（PR#556 review）——不像 inputElements 那样有结构性单一权威（校验器对展示
// 元素是逐类型手写 case），displayElements↔校验器一致性只能靠这个测试守卫。故遍历
// DisplayElements()，每个元素放在 body **顶层**跑 Validate；缺 fixture 即 fail —— 逼「新增展示
// 元素」必须同时给校验器加顶层 case 并补 fixture 证明它真被接受，而非只往清单加一行（否则 D12
// 清单会广播一个校验器拒绝的元素 = 谎报能力）。
func TestDisplayElementsAuthority(t *testing.T) {
	want := []string{
		"TextBlock", "RichTextBlock", "Image", "ImageSet",
		"Container", "ColumnSet", "FactSet",
		"Table", "ActionSet",
	}
	got := DisplayElements()
	if !slices.Equal(got, want) {
		t.Fatalf("DisplayElements()=%v, want %v", got, want)
	}
	// 每个展示元素 → 一份把它放在 body **顶层**的最小卡片（诚实校验：清单广告的是顶层可放置
	// 元素，故 fixture 必须按顶层位置校验、不得包裹遮盖。Column 已不在清单——它只作 ColumnSet
	// 子列、无顶层 case，顶层 Column 会被校验器拒；若误加回 displayElements，这里缺顶层 fixture
	// 即 fail，挡住谎报）。
	fixtures := map[string]map[string]interface{}{
		"TextBlock":     cardWithBody(map[string]interface{}{"type": "TextBlock", "text": "x"}),
		"RichTextBlock": cardWithBody(map[string]interface{}{"type": "RichTextBlock", "inlines": []interface{}{"x"}}),
		"Image":         cardWithBody(map[string]interface{}{"type": "Image", "url": "https://example.com/i.png"}),
		"ImageSet": cardWithBody(map[string]interface{}{"type": "ImageSet", "images": []interface{}{
			map[string]interface{}{"type": "Image", "url": "https://example.com/i.png"},
		}}),
		"Container": cardWithBody(map[string]interface{}{"type": "Container", "items": []interface{}{}}),
		"ColumnSet": cardWithBody(map[string]interface{}{"type": "ColumnSet", "columns": []interface{}{}}),
		"FactSet":   cardWithBody(map[string]interface{}{"type": "FactSet", "facts": []interface{}{}}),
		"Table": cardWithBody(map[string]interface{}{"type": "Table", "rows": []interface{}{
			map[string]interface{}{"type": "TableRow", "cells": []interface{}{
				map[string]interface{}{"type": "TableCell", "items": []interface{}{}},
			}},
		}}),
		"ActionSet": cardWithBody(map[string]interface{}{"type": "ActionSet", "actions": []interface{}{
			map[string]interface{}{"type": "Action.OpenUrl", "url": "https://example.com"},
		}}),
	}
	for _, typ := range got {
		card, ok := fixtures[typ]
		if !ok {
			t.Errorf("展示元素 %s 缺校验 fixture —— 新增展示元素必须补 fixture 证明校验器接受它"+
				"（否则 displayElements↔校验器漂移、D12 清单谎报能力）", typ)
			continue
		}
		if err := Validate(v2Envelope(card)); err != nil {
			t.Errorf("展示元素 %s 应通过校验, err=%v", typ, err)
		}
	}
	// 非白名单元素被拒。
	if err := Validate(v2Envelope(cardWithBody(map[string]interface{}{"type": "Bogus"}))); !errors.Is(err, ErrCardUnknownElement) {
		t.Errorf("非白名单元素 Bogus 应拒, err=%v", err)
	}
}
