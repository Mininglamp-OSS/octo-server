package cardmsg

// card-message-p3-rich-inputs（P3-3）：卡片元素白名单「单一权威」守卫。
// DisplayElements()/InputElements() 是校验器（validate.go）、inputs 采集（inputs.go）、
// D12 能力清单（GET /v1/bot/card/profile）三处共用的权威列表 —— 这些测试锁死列表成员
// 与校验器实际接受集不漂移（清单据此下发，漂移=对外谎报能力）。

import (
	"errors"
	"testing"
)

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestInputElementsAuthority：InputElements() 与文档/校验器一致 —— 每个成员 octo/v2
// 放行、octo/v1 越级拒；非成员 Input.*（Input.Rating）即便 octo/v2 也拒。
func TestInputElementsAuthority(t *testing.T) {
	want := []string{
		"Input.Text", "Input.Toggle", "Input.ChoiceSet",
		"Input.Number", "Input.Date", "Input.Time",
	}
	got := InputElements()
	if !equalStrs(got, want) {
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

// TestDisplayElementsAuthority：DisplayElements() 与文档 §2 展示元素集一致，且每个成员
// 放在合法位置都不被校验器判为未知元素；非成员被拒。
func TestDisplayElementsAuthority(t *testing.T) {
	want := []string{"TextBlock", "Image", "Container", "ColumnSet", "Column", "FactSet"}
	got := DisplayElements()
	if !equalStrs(got, want) {
		t.Fatalf("DisplayElements()=%v, want %v", got, want)
	}
	// 所有展示元素放在合法位置（Column 只存在于 ColumnSet 内）应整卡通过。
	card := cardWithBody(
		map[string]interface{}{"type": "TextBlock", "text": "x"},
		map[string]interface{}{"type": "Image", "url": "https://example.com/i.png"},
		map[string]interface{}{"type": "Container", "items": []interface{}{}},
		map[string]interface{}{"type": "ColumnSet", "columns": []interface{}{
			map[string]interface{}{"type": "Column", "items": []interface{}{}},
		}},
		map[string]interface{}{"type": "FactSet", "facts": []interface{}{}},
	)
	if err := Validate(v2Envelope(card)); err != nil {
		t.Errorf("全部展示元素应通过校验, err=%v", err)
	}
	// 非白名单元素被拒。
	if err := Validate(v2Envelope(cardWithBody(map[string]interface{}{"type": "Bogus"}))); !errors.Is(err, ErrCardUnknownElement) {
		t.Errorf("非白名单元素 Bogus 应拒, err=%v", err)
	}
}
