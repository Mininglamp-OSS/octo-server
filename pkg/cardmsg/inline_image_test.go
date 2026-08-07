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

// 白名单是精确全串匹配。这条测试钉住「只有审过的那几串字节能过」——它是本机制的
// 全部安全论证：任意 SVG（无论多无害）都不匹配，所以不存在需要过滤的内容面。
func TestInlineImageAllowlistIsExactBytes(t *testing.T) {
	for uri := range vettedInlineImages {
		card := cardWithBody(map[string]interface{}{"type": "Image", "url": uri})
		if err := Validate(envelope(card)); err != nil {
			t.Fatalf("审过的内联图应放行: %v", err)
		}
	}

	// 每一条都是「合法 SVG」或「审过图标的近似变体」。全部必须被拒 —— 白名单不解码、
	// 不解析、不规范化，只认字节。
	for _, tc := range []struct{ name, raw string }{
		{"无害但未审过的 SVG", inlineSVG(`<svg xmlns="http://www.w3.org/2000/svg"><path d="m6 9l6 6l6-6"/></svg>`)},
		{"审过图标改一个字节", strings.Replace(chevronDownIcon, "%23a1a6ab", "%23a1a6ac", 1)},
		{"审过图标末尾加空格", chevronDownIcon + " "},
		{"审过图标前置空格", " " + chevronDownIcon},
		{"审过图标换 base64 编码", "data:image/svg+xml;base64," +
			base64.StdEncoding.EncodeToString([]byte(`<svg viewBox="0 0 24 24"/>`))},
		{"审过图标去掉 MIME", strings.Replace(chevronDownIcon, "image/svg+xml", "", 1)},
		{"审过图标截断", chevronDownIcon[:len(chevronDownIcon)-10]},
		{"审过图标后追加内容", chevronDownIcon + "%3Cscript%3E"},

		// 以下是子串黑名单版本挡不住的五个绕过（PR#712 review）。精确匹配下它们与
		// 任何其他未审字节串同等被拒，不再依赖过滤器认得出它们。
		{"命名空间前缀 script", inlineSVG(`<svg xmlns:s="http://www.w3.org/2000/svg"><s:script>alert(1)</s:script></svg>`)},
		{"命名空间前缀 use", inlineSVG(`<svg xmlns:s="http://www.w3.org/2000/svg"><s:use/></svg>`)},
		{"CSS 标识符转义 url(", inlineSVG(`<svg><style>path{fill:\75 rl(http://evil/x)}</style></svg>`)},
		{"CSS 标识符转义 @import", inlineSVG(`<svg><style>@\69 mport "http://evil/x.css";</style></svg>`)},
		{"SVG 1.2 Tiny handler 元素", inlineSVG(`<svg><handler type="text/javascript">alert(1)</handler></svg>`)},

		// 经典载荷，一并留作回归。
		{"script 元素", inlineSVG(`<svg><script>alert(1)</script></svg>`)},
		{"事件属性 onload", inlineSVG(`<svg onload="alert(1)"><path d="m0 0"/></svg>`)},
		{"foreignObject", inlineSVG(`<svg><foreignObject><body/></foreignObject></svg>`)},
		{"XXE doctype/entity", inlineSVG(`<!DOCTYPE svg [<!ENTITY x SYSTEM "file:///etc/passwd">]><svg/>`)},
		{"其他 MIME: text/html", "data:text/html,<h1>x</h1>"},
		{"其他 MIME: image/png", "data:image/png;base64,AAAA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			card := cardWithBody(map[string]interface{}{"type": "Image", "url": tc.raw})
			if err := Validate(envelope(card)); !errors.Is(err, ErrCardBadURLScheme) {
				t.Fatalf("未审过的字节应被拒 (%s), err=%v", tc.name, err)
			}
		})
	}
}

// 白名单只开在图片字段上；动作/背景等 URL 面即便填审过的图标也不放行 —— 那些字段的
// 语义是「跳转/拉取」，一个 data: 图标出现在那里本身就是配置错误。
func TestInlineImageDoesNotLeakToOtherURLFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		card map[string]interface{}
	}{
		{"Action.OpenUrl.url", cardWithActions(map[string]interface{}{
			"type": "Action.OpenUrl", "url": chevronDownIcon})},
		{"Action.OpenUrl.iconUrl", cardWithActions(map[string]interface{}{
			"type": "Action.OpenUrl", "url": "https://ok.example.com", "iconUrl": chevronDownIcon})},
		{"AdaptiveCard.backgroundImage", func() map[string]interface{} {
			c := cardWithBody(map[string]interface{}{"type": "TextBlock", "text": "x"})
			c["backgroundImage"] = chevronDownIcon
			return c
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(envelope(tc.card)); !errors.Is(err, ErrCardBadURLScheme) {
				t.Fatalf("%s 不应放行 data:, err=%v", tc.name, err)
			}
		})
	}
}

// 内联图随每一帧持久化并下发给每个客户端，所以按「图标」而非「图片」定尺。这条测试
// 不是安全边界（精确匹配已经是），而是防止有人把一张照片 base64 塞进白名单。
func TestVettedInlineImagesStayIconSized(t *testing.T) {
	const maxIconBytes = 1 << 10
	if len(vettedInlineImages) == 0 {
		t.Fatal("白名单为空：checkImageURL 的内联分支成了死代码")
	}
	for uri := range vettedInlineImages {
		if len(uri) > maxIconBytes {
			t.Errorf("内联图 %d 字节超过 %d —— 图标不该这么大；若确需更大，改这个上限并说明理由\n%.80s…",
				len(uri), maxIconBytes, uri)
		}
		if !strings.HasPrefix(uri, "data:image/svg+xml,") {
			t.Errorf("内联图必须是 data:image/svg+xml, 前缀的矢量图：%.80s…", uri)
		}
	}
}
