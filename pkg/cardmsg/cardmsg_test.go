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
		{"Media 元素(未支持,AC1.1)", cardWithBody(map[string]interface{}{"type": "Media"}), ErrCardUnknownElement},
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
		{"Action.ToggleVisibility(未支持,AC1.2)", cardWithBody(map[string]interface{}{
			"type":         "Container",
			"selectAction": map[string]interface{}{"type": "Action.ToggleVisibility"},
		}), ErrCardUnknownAction},
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
	// destination 前导空白不得绕过 allowlist —— CommonMark 剥离前后空白后端上
	// 就是 javascript: 链接;校验面必须覆盖(回归:曾因 `(` 后要求非空白而漏检)。
	for _, evil := range []string{
		"click [x]( javascript:alert(1))",
		"click [x](\tjavascript:alert(1))",
		"click [x](  data:text/html,x)",
	} {
		c := cardWithBody(map[string]interface{}{"type": "TextBlock", "text": evil})
		if err := Validate(envelope(c)); !errors.Is(err, ErrCardBadURLScheme) {
			t.Errorf("前导空白 markdown 危险链接应被拒 %q, err=%v", evil, err)
		}
	}
	// 合法 http 链接带前导空白应放行(空白只是格式,target 仍是 https)
	ok := cardWithBody(map[string]interface{}{"type": "TextBlock", "text": "see [x]( https://example.com)"})
	if err := Validate(envelope(ok)); err != nil {
		t.Errorf("前导空白 + https 应放行: %v", err)
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

// 验收(PR#543 review P1):URL 正向 allowlist 必须覆盖整个渲染面 —— 除 url 外的
// backgroundImage / iconUrl,以及 markdown 的 autolink / 引用式链接,都不得绕过。
//
// 本测试 + TestValidateMarkdownRenderSurfaceParity 一起,是 octo/v1 **URL 承载面
// 的权威枚举**:每类元素/字段(Image.url、Action.OpenUrl.url+iconUrl、各容器
// backgroundImage、TextBlock/FactSet markdown 链接/图片/autolink)各一条危险用例。
// 新增会渲染 URL 的元素或字段时,必须在此补一行——保持"校验面 ≥ 渲染面"可复核。
func TestValidateURLAllowlistFullSurface(t *testing.T) {
	// backgroundImage —— 字符串简写形(卡片根)
	rootBg := envelope(nil)
	rootBg["card"].(map[string]interface{})["backgroundImage"] = "javascript:alert(1)"
	if err := Validate(rootBg); !errors.Is(err, ErrCardBadURLScheme) {
		t.Errorf("card 根 backgroundImage(字符串)危险 scheme 应被拒, err=%v", err)
	}
	// backgroundImage —— 对象全写形 {url:...}(Container 上)
	contBg := cardWithBody(map[string]interface{}{
		"type":            "Container",
		"backgroundImage": map[string]interface{}{"url": "vbscript:x", "fillMode": "cover"},
		"items":           []interface{}{map[string]interface{}{"type": "TextBlock", "text": "x"}},
	})
	if err := Validate(envelope(contBg)); !errors.Is(err, ErrCardBadURLScheme) {
		t.Errorf("Container backgroundImage(对象)危险 scheme 应被拒, err=%v", err)
	}
	// iconUrl —— Action.OpenUrl 上
	iconAct := map[string]interface{}{"actions": []interface{}{
		map[string]interface{}{"type": "Action.OpenUrl", "url": "https://ok.example.com", "iconUrl": "data:image/png;base64,AAAA"},
	}}
	if err := Validate(envelope(iconAct)); !errors.Is(err, ErrCardBadURLScheme) {
		t.Errorf("Action.OpenUrl.iconUrl 危险 scheme 应被拒, err=%v", err)
	}
	// autolink <javascript:...> —— 通用 CommonMark 渲成活链接
	al := cardWithBody(map[string]interface{}{"type": "TextBlock", "text": "see <javascript:alert(1)>"})
	if err := Validate(envelope(al)); !errors.Is(err, ErrCardBadURLScheme) {
		t.Errorf("autolink javascript: 应被拒, err=%v", err)
	}
	// 引用式定义 [r]: javascript:... —— 带 scheme,应被拒
	ref := cardWithBody(map[string]interface{}{"type": "TextBlock", "text": "tap [go][r]\n\n[r]: javascript:alert(1)"})
	if err := Validate(envelope(ref)); !errors.Is(err, ErrCardBadURLScheme) {
		t.Errorf("引用式定义 javascript: 应被拒, err=%v", err)
	}
	// FactSet.value 里的 markdown 危险链接 —— 与 TextBlock 对等的渲染面,应被拒
	fs := cardWithBody(map[string]interface{}{
		"type": "FactSet",
		"facts": []interface{}{
			map[string]interface{}{"title": "操作", "value": "[点击](javascript:alert(1))"},
		},
	})
	if err := Validate(envelope(fs)); !errors.Is(err, ErrCardBadURLScheme) {
		t.Errorf("FactSet.value markdown javascript: 应被拒, err=%v", err)
	}
	// FactSet.title 同理
	fsT := cardWithBody(map[string]interface{}{
		"type": "FactSet",
		"facts": []interface{}{
			map[string]interface{}{"title": "[t](data:text/html,x)", "value": "v"},
		},
	})
	if err := Validate(envelope(fsT)); !errors.Is(err, ErrCardBadURLScheme) {
		t.Errorf("FactSet.title markdown data: 应被拒, err=%v", err)
	}

	// —— 放行/不误伤 ——
	// 合法 https backgroundImage(对象形)放行
	okBg := cardWithBody(map[string]interface{}{
		"type":            "Container",
		"backgroundImage": map[string]interface{}{"url": "https://cdn.example.com/bg.png"},
		"items":           []interface{}{map[string]interface{}{"type": "TextBlock", "text": "x"}},
	})
	if err := Validate(envelope(okBg)); err != nil {
		t.Errorf("https backgroundImage 应放行: %v", err)
	}
	// 正文里形如 "[Note]: do this" 的非链接行不得被误当引用定义拒绝(裸词无 scheme)
	notRef := cardWithBody(map[string]interface{}{"type": "TextBlock", "text": "[Note]: do this first, then that"})
	if err := Validate(envelope(notRef)); err != nil {
		t.Errorf("裸词引用定义式正文不应被误拒: %v", err)
	}
	// 合法 https autolink 放行
	okAl := cardWithBody(map[string]interface{}{"type": "TextBlock", "text": "docs <https://example.com/x>"})
	if err := Validate(envelope(okAl)); err != nil {
		t.Errorf("https autolink 应放行: %v", err)
	}
}

// 验收(PR#543 round-4 review P1):markdown URL 提取必须 = CommonMark 渲染面。
// 旧正则提取器漏抽两类 CommonMark 会渲成活链接的形态,危险 scheme 因此绕过正向
// allowlist。改用 goldmark 后必须全部被拒;合法链接不误伤。
func TestValidateMarkdownRenderSurfaceParity(t *testing.T) {
	// —— 危险:必须被拒(此前正则绕过) ——
	danger := []string{
		// 嵌套/转义方括号 label:markdownLinkRe 的 [^\]]* 抽不到,goldmark 能。
		"click [a [b]](javascript:alert(1))",
		`click [x\]](javascript:alert(1))`,
		// 转义 scheme 引用定义 + 引用:refDefRe 的 scheme 预判被 `\:` 破坏而漏检;
		// goldmark 解析出链接节点,destination 非 http(s) → allowlist 拒。
		"tap [go][l]\n\n[l]: javascript\\:alert(1)",
		// markdown 图片 destination 也是渲染面。
		"![x](vbscript:x)",
	}
	for _, txt := range danger {
		c := cardWithBody(map[string]interface{}{"type": "TextBlock", "text": txt})
		if err := Validate(envelope(c)); !errors.Is(err, ErrCardBadURLScheme) {
			t.Errorf("危险 markdown 链接面应被拒 %q, err=%v", txt, err)
		}
	}
	// FactSet 承载同款嵌套 label 危险链接(与 TextBlock 对等)。
	fs := cardWithBody(map[string]interface{}{
		"type": "FactSet",
		"facts": []interface{}{
			map[string]interface{}{"title": "t", "value": "[a [b]](javascript:alert(1))"},
		},
	})
	if err := Validate(envelope(fs)); !errors.Is(err, ErrCardBadURLScheme) {
		t.Errorf("FactSet 嵌套 label 危险链接应被拒, err=%v", err)
	}

	// —— 合法/不误伤:必须放行 ——
	ok := []string{
		"see [a [b]](https://example.com)",          // 嵌套 label + https
		"ref [go][l]\n\n[l]: https://example.com/x", // 引用式 + https
		"docs <https://example.com>",                // autolink https
		"[Note]: do this, then that",                // 孤立引用定义(未被引用)不产链接
		"plain text with a colon: value here",       // 冒号不构成链接
		"placeholder [t]() link",                    // 空 href 不承载 scheme,不误拒
	}
	for _, txt := range ok {
		c := cardWithBody(map[string]interface{}{"type": "TextBlock", "text": txt})
		if err := Validate(envelope(c)); err != nil {
			t.Errorf("合法/非链接 markdown 不应被拒 %q: %v", txt, err)
		}
	}
	// mailto/email 面与正向 allowlist 一致:只放行 http(s),故内联 mailto 与 email
	// autolink 都应被拒(与 Image.url 的 no-scheme 拒绝同口径)。
	for _, txt := range []string{
		"mail [x](mailto:a@b.com)",
		"contact <a@b.com> please",
	} {
		c := cardWithBody(map[string]interface{}{"type": "TextBlock", "text": txt})
		if err := Validate(envelope(c)); !errors.Is(err, ErrCardBadURLScheme) {
			t.Errorf("非 http(s) 链接面应被 allowlist 拒 %q, err=%v", txt, err)
		}
	}
}

// 验收(PR#543 round-4 review P1):出站 payload 在 Finalize 之后仍会被 mention.ais
// 展开等 mutation 增大;RecheckPayloadSize 必须能在最后一次 mutation 后拦下超限,
// 且序列化口径与 util.ToJson(=json.Marshal) 一致。
func TestRecheckPayloadSize(t *testing.T) {
	// 非卡片 no-op
	if err := RecheckPayloadSize(map[string]interface{}{"type": float64(1), "content": "hi"}); err != nil {
		t.Errorf("非卡片应 no-op: %v", err)
	}
	// 卡片正常大小放行
	env := envelope(nil)
	if err := RecheckPayloadSize(env); err != nil {
		t.Errorf("正常卡片应放行: %v", err)
	}
	// 模拟展开后追加的 mention 子表把出站 payload 撑过上限 → 必须拦下
	env["mention"] = map[string]interface{}{"uids": strings.Repeat("u", MaxPayloadBytes)}
	if err := RecheckPayloadSize(env); !errors.Is(err, ErrCardPayloadTooLarge) {
		t.Errorf("出站超限应被 RecheckPayloadSize 拦下, err=%v", err)
	}
}

func TestValidateProfileNegotiation(t *testing.T) {
	// P2 接受集 = {octo/v1, octo/v2}(D2)：octo/v2 现被接受（展示型 body 无交互
	// 元素时也合法）；octo/v3 与任何未知 profile 仍是 400。
	env := envelope(nil)
	env["profile"] = "octo/v2"
	if err := Validate(env); err != nil {
		t.Errorf("octo/v2 应被接受(P2 D2), err=%v", err)
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
	// FactSet.title/value 的 markdown 也须剥离(Decision 8；PR#543 review：曾拼接
	// raw title/value 泄漏 markdown 到权威 plain)。链接降为文本、星号去除。
	factsetMD := cardWithBody(map[string]interface{}{"type": "FactSet", "facts": []interface{}{
		map[string]interface{}{"title": "**动作**", "value": "[详情](https://e.com/x)"},
	}})
	if got := BuildPlain(factsetMD); got != "动作: 详情" {
		t.Errorf("FactSet markdown 剥离 plain=%q want %q", got, "动作: 详情")
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

// 验收(PR#543 round-5 🟡):stripMarkdown 走 goldmark 后,旧正则漏剥的形态
// (引用式链接 / autolink / 图片)不得把原始 markdown 语法泄进权威 plain。
func TestStripMarkdownRenderForms(t *testing.T) {
	cases := []struct{ in, want string }{
		{"see [detail](https://e.com)", "see detail"},            // 内联链接 → 文本
		{"tap [go][l]\n\n[l]: https://e.com", "tap go"},          // 引用式链接 → 文本(定义行不泄漏)
		{"docs <https://e.com/x>", "docs https://e.com/x"},       // autolink → 可见 URL 文本
		{"pic ![alt text](https://e.com/a.png)", "pic alt text"}, // 图片 → alt 文本(不泄 ![]() 语法)
		{"**bold** and `code` and *em*", "bold and code and em"}, // 强调/行内代码去标记
		{"line1\nline2", "line1\nline2"},                         // 软换行保留
	}
	for _, c := range cases {
		card := cardWithBody(map[string]interface{}{"type": "TextBlock", "text": c.in})
		if got := BuildPlain(card); got != c.want {
			t.Errorf("stripMarkdown(%q) plain=%q want %q", c.in, got, c.want)
		}
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

// 验收(PR#543 review B1):编辑守卫单点谓词 —— 目标是卡片 OR 编辑体是卡片 都拒。
// bot_api 与 robot 两个编辑入口共用本谓词，防止两条路各自拼守卫而漂移
// （原 robot 路径漏了「目标是卡片」这半边，可把已存在卡片改成非卡片正文）。
func TestRejectsCardEdit(t *testing.T) {
	cardRaw := []byte(`{"type":17,"card":{}}`)
	textRaw := []byte(`{"type":1,"content":"hi"}`)
	cardEdit := `{"type":17,"card":{"body":[]},"profile":"octo/v1","card_version":"1.5"}`
	textEdit := `{"type":14,"content":[{"type":"text","text":"x"}]}`

	// 目标是卡片、编辑体是非卡片 —— 这是 B1 漏掉的一路：把已存在卡片改成非卡片。
	if !RejectsCardEdit(cardRaw, textEdit) {
		t.Error("目标为卡片时应拒绝任何编辑（B1 回归）")
	}
	// 目标非卡片、编辑体是卡片 —— 把普通消息改写成卡片。
	if !RejectsCardEdit(textRaw, cardEdit) {
		t.Error("编辑体为卡片时应拒绝（绕过 Validate 的 ingress）")
	}
	// 两者皆卡片 —— 当然拒。
	if !RejectsCardEdit(cardRaw, cardEdit) {
		t.Error("目标与编辑体都为卡片时应拒绝")
	}
	// 两者皆非卡片 —— 普通 richtext 编辑放行（老路径不变）。
	if RejectsCardEdit(textRaw, textEdit) {
		t.Error("非卡片目标 + 非卡片编辑体不应拒绝（老编辑路径不变）")
	}
}
