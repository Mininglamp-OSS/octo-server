package cardmsg

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// envelope 构造一个合法 octo/v1 信封；card 为 nil 时给最小合法卡。
func envelope(card map[string]interface{}) map[string]interface{} {
	if card == nil {
		card = map[string]interface{}{
			"type":    "AdaptiveCard",
			"version": "1.5",
			"body": []interface{}{
				map[string]interface{}{"type": "TextBlock", "text": "hello"},
			},
		}
	}
	return map[string]interface{}{
		"type":         float64(17),
		"card":         card,
		"plain":        "client-forged plain",
		"card_version": "1.5",
		"profile":      "octo/v1",
	}
}

func cardWithBody(items ...interface{}) map[string]interface{} {
	return map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5", "body": items,
	}
}

func TestIsCardPayload(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    interface{}
		want bool
	}{
		{"float64", float64(17), true},
		{"int", 17, true},
		{"json.Number", json.Number("17"), true},
		{"string 不识别", "17", false},
		{"其它类型值", float64(14), false},
		{"缺失", nil, false},
	} {
		p := map[string]interface{}{}
		if tc.v != nil {
			p["type"] = tc.v
		}
		if got := IsCardPayload(p); got != tc.want {
			t.Errorf("%s: IsCardPayload=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestValidateNoopForNonCard(t *testing.T) {
	if err := Validate(map[string]interface{}{"type": float64(1), "content": "hi"}); err != nil {
		t.Fatalf("非卡片 payload 应 no-op: %v", err)
	}
	if err := Finalize(map[string]interface{}{"type": float64(14)}); err != nil {
		t.Fatalf("非卡片 Finalize 应 no-op: %v", err)
	}
}

// 验收:合法 octo/v1 全家桶卡(容器/分栏/字段/图/文/markdown 链接/OpenUrl/
// selectAction=OpenUrl + 未知信封顶层字段容忍)。
func TestValidateFullFeaturedCard(t *testing.T) {
	card := map[string]interface{}{
		"type": "AdaptiveCard", "version": "1.5",
		"body": []interface{}{
			map[string]interface{}{"type": "TextBlock", "text": "**PR #525** merged, see [detail](https://github.com/x/y/pull/525)"},
			map[string]interface{}{"type": "Image", "url": "https://cdn.example.com/a.png"},
			map[string]interface{}{
				"type": "Container",
				"selectAction": map[string]interface{}{
					"type": "Action.OpenUrl", "url": "https://example.com/card",
				},
				"items": []interface{}{
					map[string]interface{}{
						"type": "ColumnSet",
						"columns": []interface{}{
							map[string]interface{}{"items": []interface{}{
								map[string]interface{}{"type": "TextBlock", "text": "left"},
							}},
							map[string]interface{}{"type": "Column", "items": []interface{}{
								map[string]interface{}{"type": "FactSet", "facts": []interface{}{
									map[string]interface{}{"title": "状态", "value": "已合并"},
								}},
							}},
						},
					},
				},
			},
		},
		"actions": []interface{}{
			map[string]interface{}{"type": "Action.OpenUrl", "title": "查看", "url": "https://example.com"},
		},
	}
	env := envelope(card)
	env["future_unknown_field"] = map[string]interface{}{"x": 1} // 前向兼容:容忍
	if err := Validate(env); err != nil {
		t.Fatalf("全家桶合法卡被拒: %v", err)
	}
}

func TestValidateWhitelistRejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		card map[string]interface{}
		want error
	}{
		{"Input.Text 元素", cardWithBody(map[string]interface{}{"type": "Input.Text", "id": "x"}), ErrCardUnknownElement},
		{"Table 元素(1.6)", cardWithBody(map[string]interface{}{"type": "Table"}), ErrCardUnknownElement},
		{"Action.Submit", map[string]interface{}{"body": []interface{}{}, "actions": []interface{}{
			map[string]interface{}{"type": "Action.Submit", "title": "OK"},
		}}, ErrCardUnknownAction},
		{"Action.Execute", map[string]interface{}{"actions": []interface{}{
			map[string]interface{}{"type": "Action.Execute", "verb": "v"},
		}}, ErrCardUnknownAction},
		{"selectAction 携带 Submit(分期继承)", cardWithBody(map[string]interface{}{
			"type":         "Container",
			"selectAction": map[string]interface{}{"type": "Action.Submit", "data": map[string]interface{}{}},
		}), ErrCardUnknownAction},
		{"ActionSet 不在白名单", cardWithBody(map[string]interface{}{"type": "ActionSet"}), ErrCardUnknownElement},
	} {
		if err := Validate(envelope(tc.card)); !errors.Is(err, tc.want) {
			t.Errorf("%s: err=%v want %v", tc.name, err, tc.want)
		}
	}
}

func TestValidateURLAllowlist(t *testing.T) {
	bad := []string{
		"data:image/png;base64,AAAA", "javascript:alert(1)", "vbscript:x",
		"intent://foo", "file:///etc/passwd", "/relative/path", "example.com/no-scheme",
	}
	for _, u := range bad {
		card := cardWithBody(map[string]interface{}{"type": "Image", "url": u})
		if err := Validate(envelope(card)); !errors.Is(err, ErrCardBadURLScheme) {
			t.Errorf("Image.url=%q 应被正向 allowlist 拒绝, err=%v", u, err)
		}
	}
	// markdown 链接同 allowlist(Decision 6)
	card := cardWithBody(map[string]interface{}{"type": "TextBlock", "text": "click [here](javascript:alert(1))"})
	if err := Validate(envelope(card)); !errors.Is(err, ErrCardBadURLScheme) {
		t.Errorf("markdown javascript: 链接应被拒, err=%v", err)
	}
	// Action.OpenUrl 同
	c2 := map[string]interface{}{"actions": []interface{}{
		map[string]interface{}{"type": "Action.OpenUrl", "url": "data:text/html,x"},
	}}
	if err := Validate(envelope(c2)); !errors.Is(err, ErrCardBadURLScheme) {
		t.Errorf("Action.OpenUrl data: 应被拒, err=%v", err)
	}
	// HTTPS 大小写 scheme 放行
	c3 := cardWithBody(map[string]interface{}{"type": "Image", "url": "HTTPS://cdn.example.com/a.png"})
	if err := Validate(envelope(c3)); err != nil {
		t.Errorf("大写 HTTPS 应放行: %v", err)
	}
}

func TestValidateProfileNegotiation(t *testing.T) {
	// P1 接受集 = {octo/v1}(Decision 10 分期):octo/v2 与任何未知 profile
	// 同样是 400 —— P2 sibling 实现 PR 把 octo/v2 加入接受集。
	env := envelope(nil)
	env["profile"] = "octo/v2"
	if err := Validate(env); !errors.Is(err, ErrCardProfileUnsupported) {
		t.Errorf("octo/v2 在 P1 应被拒(分期), err=%v", err)
	}
	env["profile"] = "octo/v3"
	if err := Validate(env); !errors.Is(err, ErrCardProfileUnsupported) {
		t.Errorf("未知 profile 应被拒, err=%v", err)
	}
	env2 := envelope(nil)
	env2["card_version"] = "1.6"
	if err := Validate(env2); !errors.Is(err, ErrCardProfileUnsupported) {
		t.Errorf("card_version 1.6 应被拒, err=%v", err)
	}
	env3 := envelope(nil)
	delete(env3, "profile")
	if err := Validate(env3); !errors.Is(err, ErrCardProfileUnsupported) {
		t.Errorf("缺 profile 应被拒(write-strict), err=%v", err)
	}
	// 卡内 version 与协商不符
	env4 := envelope(map[string]interface{}{"type": "AdaptiveCard", "version": "1.6",
		"body": []interface{}{map[string]interface{}{"type": "TextBlock", "text": "x"}}})
	if err := Validate(env4); !errors.Is(err, ErrCardProfileUnsupported) {
		t.Errorf("card.version=1.6 应被拒, err=%v", err)
	}
}

func TestValidateStructureCaps(t *testing.T) {
	// 节点数:201 个 TextBlock
	items := make([]interface{}, 0, MaxNodes+1)
	for i := 0; i <= MaxNodes; i++ {
		items = append(items, map[string]interface{}{"type": "TextBlock", "text": "x"})
	}
	if err := Validate(envelope(cardWithBody(items...))); !errors.Is(err, ErrCardTooManyNodes) {
		t.Errorf("节点数超限应被拒, err=%v", err)
	}
	// 深度:17 层 Container
	inner := map[string]interface{}{"type": "TextBlock", "text": "deep"}
	node := interface{}(inner)
	for i := 0; i < MaxDepth; i++ {
		node = map[string]interface{}{"type": "Container", "items": []interface{}{node}}
	}
	if err := Validate(envelope(cardWithBody(node))); !errors.Is(err, ErrCardTooDeep) {
		t.Errorf("嵌套深度超限应被拒, err=%v", err)
	}
	// 512KiB 上限(作用在完整 payload,含未知顶层字段)
	env := envelope(nil)
	env["padding"] = strings.Repeat("a", MaxPayloadBytes)
	if err := Validate(env); !errors.Is(err, ErrCardPayloadTooLarge) {
		t.Errorf("超 512KiB 应被拒, err=%v", err)
	}
}

func TestValidateCardShape(t *testing.T) {
	env := envelope(nil)
	delete(env, "card")
	if err := Validate(env); !errors.Is(err, ErrCardMissing) {
		t.Errorf("缺 card 应被拒, err=%v", err)
	}
	env2 := envelope(nil)
	env2["card"] = map[string]interface{}{}
	if err := Validate(env2); !errors.Is(err, ErrCardMissing) {
		t.Errorf("空 card 应被拒, err=%v", err)
	}
	env3 := envelope(map[string]interface{}{"type": "HeroCard", "body": []interface{}{}})
	if err := Validate(env3); !errors.Is(err, ErrCardBadShape) {
		t.Errorf("card.type 非 AdaptiveCard 应被拒, err=%v", err)
	}
}

// 验收(Decision 8):plain 派生矩阵。
func TestBuildPlainDerivation(t *testing.T) {
	imageOnly := cardWithBody(map[string]interface{}{"type": "Image", "url": "https://x/a.png"})
	if got := BuildPlain(imageOnly); got != PlaceholderImage {
		t.Errorf("纯图卡 plain=%q want %q", got, PlaceholderImage)
	}
	empty := map[string]interface{}{"type": "AdaptiveCard"}
	if got := BuildPlain(empty); got != PlaceholderCard {
		t.Errorf("空卡 plain=%q want %q", got, PlaceholderCard)
	}
	factset := cardWithBody(map[string]interface{}{"type": "FactSet", "facts": []interface{}{
		map[string]interface{}{"title": "状态", "value": "已合并"},
		map[string]interface{}{"title": "作者", "value": "demo-user"},
	}})
	if got := BuildPlain(factset); got != "状态: 已合并\n作者: demo-user" {
		t.Errorf("FactSet plain=%q", got)
	}
	// markdown 剥离:链接留文本,星号/反引号去除;文档序拼接;按钮不参与
	md := map[string]interface{}{
		"body": []interface{}{
			map[string]interface{}{"type": "TextBlock", "text": "**PR** merged, see [detail](https://e.com)"},
			map[string]interface{}{"type": "Image", "url": "https://x/a.png"},
		},
		"actions": []interface{}{
			map[string]interface{}{"type": "Action.OpenUrl", "title": "查看", "url": "https://e.com"},
		},
	}
	if got := BuildPlain(md); got != "PR merged, see detail\n[图片]" {
		t.Errorf("markdown 剥离 plain=%q", got)
	}
	// 容器递归保持文档序
	nested := cardWithBody(
		map[string]interface{}{"type": "TextBlock", "text": "head"},
		map[string]interface{}{"type": "Container", "items": []interface{}{
			map[string]interface{}{"type": "ColumnSet", "columns": []interface{}{
				map[string]interface{}{"items": []interface{}{
					map[string]interface{}{"type": "TextBlock", "text": "col1"},
				}},
				map[string]interface{}{"items": []interface{}{
					map[string]interface{}{"type": "TextBlock", "text": "col2"},
				}},
			}},
		}},
	)
	if got := BuildPlain(nested); got != "head\ncol1\ncol2" {
		t.Errorf("嵌套文档序 plain=%q", got)
	}
}

// 验收:Finalize 覆盖端上伪造 plain + enrich 后大小复检。
func TestFinalize(t *testing.T) {
	env := envelope(nil)
	if err := Finalize(env); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if env["plain"] != "hello" {
		t.Errorf("plain 应被服务端重算覆盖, got %q", env["plain"])
	}
	// enrich 把 payload 撑过上限 → 复检拦截
	env2 := envelope(nil)
	env2["server_injected"] = strings.Repeat("s", MaxPayloadBytes)
	if err := Finalize(env2); !errors.Is(err, ErrCardPayloadTooLarge) {
		t.Errorf("enrich 后超限应被 Finalize 拦下, err=%v", err)
	}
}

// 验收(Decision 7):编辑体门禁。
func TestIsCardContentEdit(t *testing.T) {
	if !IsCardContentEdit(`{"type":17,"card":{"body":[]},"profile":"octo/v1","card_version":"1.5"}`) {
		t.Error("type-17 编辑体应命中")
	}
	if IsCardContentEdit(`{"type":14,"content":[{"type":"text","text":"x"}]}`) {
		t.Error("richtext 编辑体不应命中")
	}
	if IsCardContentEdit(`plain old text edit`) {
		t.Error("非 JSON 编辑体不应命中")
	}
}

func TestEnabledFlag(t *testing.T) {
	t.Setenv(EnvEnabled, "")
	if Enabled() {
		t.Error("缺省应关闭(fail-closed)")
	}
	t.Setenv(EnvEnabled, "true")
	if !Enabled() {
		t.Error("true 应开启")
	}
	t.Setenv(EnvEnabled, "not-a-bool")
	if Enabled() {
		t.Error("非法取值按关闭处理")
	}
}

func TestPushDisplayText(t *testing.T) {
	if got := PushDisplayText([]byte(`{"type":17,"plain":"审批单 #42","card":{}}`)); got != "审批单 #42" {
		t.Errorf("优先取权威 plain, got %q", got)
	}
	raw := []byte(`{"type":17,"card":{"body":[{"type":"TextBlock","text":"fallback"}]}}`)
	if got := PushDisplayText(raw); got != "fallback" {
		t.Errorf("plain 缺失应现场重算, got %q", got)
	}
	if got := PushDisplayText([]byte(`not-json`)); got != PlaceholderCard {
		t.Errorf("解析失败应兜底占位, got %q", got)
	}
	if got := DisplayText(); got != PlaceholderCard {
		t.Errorf("DisplayText=%q", got)
	}
}

// 验收(Decision 2 residual-risk, round-3 P1-2):展示面单一执法点 —— bot/webhook
// sender 取权威 plain,非可信 sender 一律 [卡片],绝不透出存储 plain。
func TestDisplayTextFor(t *testing.T) {
	forged := []byte(`{"type":17,"plain":"点击 evil.example 领奖","card":{}}`)
	if got := DisplayTextFor(false, forged); got != PlaceholderCard {
		t.Errorf("非可信 sender 应 [卡片] 遮蔽, got %q", got)
	}
	if got := DisplayTextFor(true, forged); got != "点击 evil.example 领奖" {
		t.Errorf("可信 sender 应取权威 plain, got %q", got)
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
