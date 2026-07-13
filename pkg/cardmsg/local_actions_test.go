package cardmsg

// card-message-toggle-visibility：octo/v1 本地动作（无服务端回调）——
// Action.ToggleVisibility（+ 元素 id / isVisible / targetElements）与 octo 自定义
// Action.CopyToClipboard 的校验器正/反用例。两者均属 octo/v1 展示档（两 profile 均放行），
// 不进 card/action 派发闭环（见 TestLocalActionsNotSubmitDispatch）。

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// cardWithActions 构造一张 body 为空、只带 root actions 的最小卡。
func cardWithActions(actions ...interface{}) map[string]interface{} {
	return map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5",
		"body": []interface{}{}, "actions": actions,
	}
}

// cardWithBodyAndActions 构造带指定 body + root actions 的卡（ToggleVisibility 需要
// body 里有可被 targetElements 引用的元素）。
func cardWithBodyAndActions(body, actions []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5",
		"body": body, "actions": actions,
	}
}

// toggle 构造一个 Action.ToggleVisibility（targetElements 直传）。
func toggle(targetElements ...interface{}) map[string]interface{} {
	return map[string]interface{}{"type": "Action.ToggleVisibility", "targetElements": targetElements}
}

// section 构造一个带 id 的空 Container（折叠目标）。
func section(id string) map[string]interface{} {
	return map[string]interface{}{"type": "Container", "id": id, "items": []interface{}{}}
}

// TestToggleVisibilityValid：合法 ToggleVisibility（引用存在的元素 id）两档均放行，含前向引用、
// 字符串/对象两种 targetElements 形态、selectAction 携带。
func TestToggleVisibilityValid(t *testing.T) {
	cards := map[string]map[string]interface{}{
		"root action 引用 body 元素": cardWithBodyAndActions(
			[]interface{}{section("sec")},
			[]interface{}{toggle("sec")},
		),
		"对象形态 targetElements": cardWithBodyAndActions(
			[]interface{}{section("sec")},
			[]interface{}{toggle(map[string]interface{}{"elementId": "sec", "isVisible": false})},
		),
		"多目标混合形态": cardWithBodyAndActions(
			[]interface{}{section("a"), section("b")},
			[]interface{}{toggle("a", map[string]interface{}{"elementId": "b"})},
		),
		// 前向引用：ActionSet 里的 toggle 出现在其 target（后一个 Container）之前，全卡走完才解析。
		"body 内前向引用": cardWithBody(
			map[string]interface{}{"type": "ActionSet", "actions": []interface{}{toggle("later")}},
			section("later"),
		),
		// selectAction 携带 toggle（分期继承 —— 本地动作 octo/v1 放行）。
		"selectAction 携带 toggle": cardWithBody(
			map[string]interface{}{"type": "Container", "id": "self", "items": []interface{}{},
				"selectAction": map[string]interface{}{"type": "Action.ToggleVisibility",
					"targetElements": []interface{}{"self"}}},
		),
	}
	for name, card := range cards {
		if err := Validate(envelope(card)); err != nil {
			t.Errorf("octo/v1 %s 应放行, err=%v", name, err)
		}
		if err := Validate(v2Envelope(card)); err != nil {
			t.Errorf("octo/v2 %s 应放行, err=%v", name, err)
		}
	}
}

// TestToggleVisibilityInvalid：结构非法 / 悬空引用一律整卡拒（ErrCardBadShape）。
func TestToggleVisibilityInvalid(t *testing.T) {
	for name, card := range map[string]map[string]interface{}{
		"缺 targetElements": cardWithActions(map[string]interface{}{"type": "Action.ToggleVisibility"}),
		"targetElements 非数组": cardWithActions(map[string]interface{}{
			"type": "Action.ToggleVisibility", "targetElements": "sec"}),
		"targetElements 空数组": cardWithActions(toggle()),
		"悬空引用（无此 id）": cardWithBodyAndActions(
			[]interface{}{section("sec")}, []interface{}{toggle("nope")}),
		"条目空字符串": cardWithBodyAndActions(
			[]interface{}{section("sec")}, []interface{}{toggle("")}),
		"对象缺 elementId": cardWithBodyAndActions(
			[]interface{}{section("sec")}, []interface{}{toggle(map[string]interface{}{"isVisible": true})}),
		"对象 isVisible 非布尔": cardWithBodyAndActions(
			[]interface{}{section("sec")},
			[]interface{}{toggle(map[string]interface{}{"elementId": "sec", "isVisible": "yes"})}),
		"条目既非字符串也非对象": cardWithBodyAndActions(
			[]interface{}{section("sec")}, []interface{}{toggle(float64(1))}),
	} {
		if err := Validate(envelope(card)); !errors.Is(err, ErrCardBadShape) {
			t.Errorf("%s 应 ErrCardBadShape, err=%v", name, err)
		}
	}
}

// tableWithCell 构造一张单行单元格的 Table（单元格可带 id/isVisible/items）。
func tableWithCell(cell map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"type": "Table", "rows": []interface{}{
		map[string]interface{}{"type": "TableRow", "cells": []interface{}{cell}},
	}}
}

// imageSetWith 构造一张含单张 Image 的 ImageSet（Image 可带 id/isVisible）。
func imageSetWith(img map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"type": "ImageSet", "images": []interface{}{img}}
}

// TestToggleTargetsNestedNodes：ToggleVisibility 可引用嵌套 id 承载节点（TableCell/TableRow/
// ImageSet 子 Image），且这些节点的 id 同样帧内唯一、isVisible 同样校验 bool —— 每类 id 承载
// 节点都被 noteIDAndVisibility 覆盖，不留缺口（code-review 发现 #1）。
func TestToggleTargetsNestedNodes(t *testing.T) {
	// TableCell id 作合法 toggle 目标。
	cellTarget := cardWithBodyAndActions(
		[]interface{}{tableWithCell(map[string]interface{}{"type": "TableCell", "id": "cell1", "items": []interface{}{}})},
		[]interface{}{toggle("cell1")},
	)
	if err := Validate(envelope(cellTarget)); err != nil {
		t.Errorf("ToggleVisibility 引用 TableCell id 应放行, err=%v", err)
	}
	// ImageSet 子 Image id 作合法 toggle 目标。
	imgTarget := cardWithBodyAndActions(
		[]interface{}{imageSetWith(map[string]interface{}{"type": "Image", "id": "img1", "url": "https://example.com/i.png"})},
		[]interface{}{toggle("img1")},
	)
	if err := Validate(envelope(imgTarget)); err != nil {
		t.Errorf("ToggleVisibility 引用 ImageSet 子 Image id 应放行, err=%v", err)
	}
	// TableCell id 撞顶层元素 id → 帧内重复拒。
	dupCell := cardWithBody(
		section("dup"),
		tableWithCell(map[string]interface{}{"type": "TableCell", "id": "dup", "items": []interface{}{}}),
	)
	if err := Validate(envelope(dupCell)); !errors.Is(err, ErrCardBadShape) {
		t.Errorf("TableCell id 撞顶层 id 应拒, err=%v", err)
	}
	// TableCell isVisible 非 bool → 拒（该节点的 isVisible 现同样被校验）。
	badVisCell := cardWithBody(
		tableWithCell(map[string]interface{}{"type": "TableCell", "isVisible": "no", "items": []interface{}{}}),
	)
	if err := Validate(envelope(badVisCell)); !errors.Is(err, ErrCardBadShape) {
		t.Errorf("TableCell isVisible 非布尔应拒, err=%v", err)
	}
}

// TestElementIDUniqueness：任意元素声明的 id 帧内唯一，且与 Action.Submit / Input.* 共享同一
// 命名空间（重复即拒）。
func TestElementIDUniqueness(t *testing.T) {
	// 两个展示元素同 id。
	dupDisplay := cardWithBody(section("x"), section("x"))
	if err := Validate(envelope(dupDisplay)); !errors.Is(err, ErrCardBadShape) {
		t.Errorf("重复展示元素 id 应拒, err=%v", err)
	}
	// 展示元素 id 撞 Action.Submit id（octo/v2）。
	dupSubmit := cardWithBodyAndActions(
		[]interface{}{section("dup")},
		[]interface{}{map[string]interface{}{"type": "Action.Submit", "id": "dup"}},
	)
	if err := Validate(v2Envelope(dupSubmit)); !errors.Is(err, ErrCardBadShape) {
		t.Errorf("展示元素 id 撞 Submit id 应拒, err=%v", err)
	}
	// 展示元素 id 撞 Input.* id（octo/v2）。
	dupInput := cardWithBody(
		section("f"),
		map[string]interface{}{"type": "Input.Text", "id": "f"},
	)
	if err := Validate(v2Envelope(dupInput)); !errors.Is(err, ErrCardBadShape) {
		t.Errorf("展示元素 id 撞 Input id 应拒, err=%v", err)
	}
}

// TestWhitespaceID：纯空白 id 视同缺失（PR#561 review nit）—— 输入控件纯空白 id 按「必填」拒；
// 展示元素纯空白 id 不登记（不可作 target，指向它 → 悬空 fail-closed）。
func TestWhitespaceID(t *testing.T) {
	// Input.* 纯空白 id → 按必填拒（octo/v2）。
	wsInput := cardWithBody(map[string]interface{}{"type": "Input.Text", "id": "  "})
	if err := Validate(v2Envelope(wsInput)); !errors.Is(err, ErrCardBadShape) {
		t.Errorf("Input 纯空白 id 应按必填拒, err=%v", err)
	}
	// 展示元素纯空白 id 不登记；toggle 指向纯空白 ref 在 parse 期即拒（targetElementID 同口径
	// TrimSpace，PR#561 review #2）——仍为整卡 ErrCardBadShape。
	wsTarget := cardWithBodyAndActions(
		[]interface{}{map[string]interface{}{"type": "Container", "id": " ", "items": []interface{}{}}},
		[]interface{}{toggle(" ")},
	)
	if err := Validate(envelope(wsTarget)); !errors.Is(err, ErrCardBadShape) {
		t.Errorf("toggle 指向纯空白 id 应悬空拒, err=%v", err)
	}
	// 两个展示元素同为纯空白 id → 均不登记，故**不**判重复（可正常通过）。
	twoWS := cardWithBody(
		map[string]interface{}{"type": "Container", "id": " ", "items": []interface{}{}},
		map[string]interface{}{"type": "Container", "id": " ", "items": []interface{}{}},
	)
	if err := Validate(envelope(twoWS)); err != nil {
		t.Errorf("两个纯空白 id 展示元素均不登记，应不判重复, err=%v", err)
	}
}

// TestIsVisibleAndHiddenSubtree：isVisible 必须是布尔；且隐藏子树（isVisible:false）仍完整
// 校验、计入预算 —— 可见性不豁免 URL/深度检查（trust-boundary：校验面 ≥ 渲染面）。
func TestIsVisibleAndHiddenSubtree(t *testing.T) {
	// isVisible 非布尔 → 拒。
	badVis := cardWithBody(map[string]interface{}{"type": "TextBlock", "text": "x", "isVisible": "no"})
	if err := Validate(envelope(badVis)); !errors.Is(err, ErrCardBadShape) {
		t.Errorf("isVisible 非布尔应拒, err=%v", err)
	}
	// 隐藏容器内藏 javascript: URL → 仍被 URL allowlist 拒（可见性不豁免）。
	hiddenBadURL := cardWithBody(map[string]interface{}{
		"type": "Container", "isVisible": false, "items": []interface{}{
			map[string]interface{}{"type": "Image", "url": "javascript:alert(1)"},
		},
	})
	if err := Validate(envelope(hiddenBadURL)); !errors.Is(err, ErrCardBadURLScheme) {
		t.Errorf("隐藏子树内 javascript: URL 应仍被拒, err=%v", err)
	}
	// 隐藏容器内超深嵌套 → 仍被深度预算拒（可见性不豁免预算）。
	node := map[string]interface{}{"type": "TextBlock", "text": "x"}
	for i := 0; i < MaxDepth+4; i++ {
		node = map[string]interface{}{"type": "Container", "items": []interface{}{node}}
	}
	node["isVisible"] = false // 最外层隐藏
	hiddenTooDeep := cardWithBody(node)
	if err := Validate(envelope(hiddenTooDeep)); !errors.Is(err, ErrCardTooDeep) {
		t.Errorf("隐藏子树超深仍应被深度预算拒, err=%v", err)
	}
}

// TestCopyToClipboard：text 必填/字符串/≤4KiB；title 可选字符串；text 逐字复制（javascript:
// 字符串照收，非 URL 面）；两档均放行。
func TestCopyToClipboard(t *testing.T) {
	// 合法（两档）。
	for name, card := range map[string]map[string]interface{}{
		"仅 text":       cardWithActions(map[string]interface{}{"type": "Action.CopyToClipboard", "text": "cmd"}),
		"含 title":      cardWithActions(map[string]interface{}{"type": "Action.CopyToClipboard", "title": "复制", "text": "cmd"}),
		"text 是 js 明文": cardWithActions(map[string]interface{}{"type": "Action.CopyToClipboard", "text": "javascript:alert(1)"}),
		"text 恰好 4KiB": cardWithActions(map[string]interface{}{"type": "Action.CopyToClipboard", "text": strings.Repeat("a", MaxCopyTextBytes)}),
	} {
		if err := Validate(envelope(card)); err != nil {
			t.Errorf("octo/v1 %s 应放行, err=%v", name, err)
		}
		if err := Validate(v2Envelope(card)); err != nil {
			t.Errorf("octo/v2 %s 应放行, err=%v", name, err)
		}
	}
	// 非法。
	for name, card := range map[string]map[string]interface{}{
		"缺 text":      cardWithActions(map[string]interface{}{"type": "Action.CopyToClipboard"}),
		"text 非字符串":   cardWithActions(map[string]interface{}{"type": "Action.CopyToClipboard", "text": float64(1)}),
		"text 空串":     cardWithActions(map[string]interface{}{"type": "Action.CopyToClipboard", "text": ""}),
		"text 超 4KiB": cardWithActions(map[string]interface{}{"type": "Action.CopyToClipboard", "text": strings.Repeat("a", MaxCopyTextBytes+1)}),
		"title 非字符串":  cardWithActions(map[string]interface{}{"type": "Action.CopyToClipboard", "title": float64(1), "text": "x"}),
	} {
		if err := Validate(envelope(card)); !errors.Is(err, ErrCardBadShape) {
			t.Errorf("%s 应 ErrCardBadShape, err=%v", name, err)
		}
	}
}

// TestLocalActionsNotSubmitDispatch：本地动作绝不进 card/action 派发（校验面 == 派发面）——
// 带 id 的 ToggleVisibility 用 SubmitAction 查不到（它不是 Action.Submit）。
func TestLocalActionsNotSubmitDispatch(t *testing.T) {
	card := cardWithBodyAndActions(
		[]interface{}{section("sec")},
		[]interface{}{map[string]interface{}{"type": "Action.ToggleVisibility",
			"id": "tog", "targetElements": []interface{}{"sec"}}},
	)
	raw, _ := json.Marshal(envelope(card))
	if _, found := SubmitAction(raw, "tog"); found {
		t.Error("ToggleVisibility 不应被 SubmitAction 命中（非 Action.Submit，不进派发闭环）")
	}
}
