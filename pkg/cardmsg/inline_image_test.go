package cardmsg

import (
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"testing"
)

func inlineSVG(svg string) string {
	return "data:image/svg+xml," + url.QueryEscape(svg)
}

// 默认档位（raw card / webhook adapter / message edit 走的那条）必须继续拒 data:，
// 否则 TestValidateURLAllowlist 的保证就名存实亡了。
func TestInlineImageRejectedWithoutOption(t *testing.T) {
	good := inlineSVG(`<svg xmlns="http://www.w3.org/2000/svg"><path d="m6 9l6 6l6-6"/></svg>`)
	card := cardWithBody(map[string]interface{}{"type": "Image", "url": good})
	if err := Validate(envelope(card)); !errors.Is(err, ErrCardBadURLScheme) {
		t.Fatalf("默认档位应拒内联图, err=%v", err)
	}
	// 开了 option 同一张卡通过 —— 证明差异只来自调用位置。
	if err := Validate(envelope(card), AllowInlineImageData()); err != nil {
		t.Fatalf("可信档位应放行合法内联 SVG: %v", err)
	}
}

// 放开的是「受限的矢量图标」，不是「任意 data:」。逐条钉住被拒的形状。
func TestInlineImageAllowlistIsNarrow(t *testing.T) {
	const okSVG = `<svg xmlns="http://www.w3.org/2000/svg"><path d="m6 9l6 6l6-6"/></svg>`
	for _, tc := range []struct{ name, raw string }{
		{"其他 MIME: text/html", "data:text/html,<h1>x</h1>"},
		{"其他 MIME: image/png", "data:image/png;base64,AAAA"},
		{"script 元素", inlineSVG(`<svg><script>alert(1)</script></svg>`)},
		{"事件属性 onload", inlineSVG(`<svg onload="alert(1)"><path d="m0 0"/></svg>`)},
		{"事件属性带引号前缀", inlineSVG(`<svg><path onclick="x()" d="m0 0"/></svg>`)},
		{"foreignObject", inlineSVG(`<svg><foreignObject><body/></foreignObject></svg>`)},
		{"use 外部引用", inlineSVG(`<svg><use xlink:href="http://evil/x#y"/></svg>`)},
		{"内嵌 image", inlineSVG(`<svg><image href="http://evil/x.png"/></svg>`)},
		{"CSS url()", inlineSVG(`<svg><style>path{fill:url(http://evil/x)}</style></svg>`)},
		{"@import", inlineSVG(`<svg><style>@import "http://evil/x.css"</style></svg>`)},
		{"XXE doctype/entity", inlineSVG(`<!DOCTYPE svg [<!ENTITY x SYSTEM "file:///etc/passwd">]><svg/>`)},
		{"javascript: 在内容里", inlineSVG(`<svg><a href="javascript:alert(1)"/></svg>`)},
		{"animate", inlineSVG(`<svg><animate attributeName="x"/></svg>`)},
		{"不是 SVG 文档", inlineSVG(`just text`)},
		{"超过尺寸上限", inlineSVG(`<svg>` + strings.Repeat("<path d='m0 0'/>", 400) + `</svg>`)},
		{"base64 解码失败", "data:image/svg+xml;base64,!!!not-base64!!!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			card := cardWithBody(map[string]interface{}{"type": "Image", "url": tc.raw})
			if err := Validate(envelope(card), AllowInlineImageData()); !errors.Is(err, ErrCardBadURLScheme) {
				t.Fatalf("即使在可信档位也应拒 %s, err=%v", tc.name, err)
			}
		})
	}
	// base64 形式的合法图标必须放行（两种编码都支持）。
	b64 := "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(okSVG))
	card := cardWithBody(map[string]interface{}{"type": "Image", "url": b64})
	if err := Validate(envelope(card), AllowInlineImageData()); err != nil {
		t.Fatalf("合法 base64 内联 SVG 应放行: %v", err)
	}
}

// 放开只覆盖图片字段；动作/背景等 URL 面不受影响。
func TestInlineImageDoesNotLeakToOtherURLFields(t *testing.T) {
	good := inlineSVG(`<svg xmlns="http://www.w3.org/2000/svg"><path d="m6 9l6 6l6-6"/></svg>`)
	for _, tc := range []struct {
		name string
		card map[string]interface{}
	}{
		{"Action.OpenUrl.url", cardWithActions(map[string]interface{}{"type": "Action.OpenUrl", "url": good})},
		{"Action.OpenUrl.iconUrl", cardWithActions(map[string]interface{}{
			"type": "Action.OpenUrl", "url": "https://ok.example.com", "iconUrl": good})},
		{"AdaptiveCard.backgroundImage", func() map[string]interface{} {
			c := cardWithBody(map[string]interface{}{"type": "TextBlock", "text": "x"})
			c["backgroundImage"] = good
			return c
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(envelope(tc.card), AllowInlineImageData()); !errors.Is(err, ErrCardBadURLScheme) {
				t.Fatalf("%s 不应因图片档位而放行 data:, err=%v", tc.name, err)
			}
		})
	}
}
