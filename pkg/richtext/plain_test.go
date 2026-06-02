package richtext

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
)

// payloadFromJSON decodes a JSON object the same way the HTTP ingress does
// (gin BindJSON → float64 for numbers), so the test exercises the float64
// branch of IsRichTextPayload.
func payloadFromJSON(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestIsRichTextPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]interface{}
		want    bool
	}{
		{"float64_14", map[string]interface{}{"type": float64(14)}, true},
		{"int_14", map[string]interface{}{"type": 14}, true},
		{"json_number_14", map[string]interface{}{"type": json.Number("14")}, true},
		{"float64_text", map[string]interface{}{"type": float64(1)}, false},
		{"string_14_not_matched", map[string]interface{}{"type": "14"}, false},
		{"missing_type", map[string]interface{}{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsRichTextPayload(c.payload); got != c.want {
				t.Fatalf("IsRichTextPayload=%v want %v", got, c.want)
			}
		})
	}
}

func TestEnsurePlain_NonRichTextIsNoOp(t *testing.T) {
	p := payloadFromJSON(t, `{"type":1,"content":"hi","plain":"client-sent"}`)
	if err := EnsurePlain(p); err != nil {
		t.Fatalf("EnsurePlain err: %v", err)
	}
	// 非 type=14：plain 不应被改写（老消息路径不变）。
	if p["plain"] != "client-sent" {
		t.Fatalf("non-richtext plain mutated: %v", p["plain"])
	}
}

func TestEnsurePlain_OverwritesUntrustedPlain(t *testing.T) {
	// 端上送了伪造的 plain，server 必须用 content 重算覆盖。
	p := payloadFromJSON(t, `{"type":14,"plain":"FORGED","content":[
		{"type":"text","text":"hello "},
		{"type":"image","url":"https://x/y.png","width":10,"height":10},
		{"type":"text","text":" world"}
	]}`)
	if err := EnsurePlain(p); err != nil {
		t.Fatalf("EnsurePlain err: %v", err)
	}
	want := "hello " + common.RichTextImagePlaceholder + " world"
	if p["plain"] != want {
		t.Fatalf("plain=%q want %q", p["plain"], want)
	}
}

func TestEnsurePlain_LegacyStringContent(t *testing.T) {
	// 老 payload content 是字符串：FillPlainBounded 经 UnmarshalJSON 兼容。
	p := payloadFromJSON(t, `{"type":14,"content":"legacy text"}`)
	if err := EnsurePlain(p); err != nil {
		t.Fatalf("EnsurePlain err: %v", err)
	}
	if p["plain"] != "legacy text" {
		t.Fatalf("plain=%q want %q", p["plain"], "legacy text")
	}
}

func TestEnsurePlain_OversizeReturnsError(t *testing.T) {
	// 构造一条单 text block，文本接近 1MB，回填 plain（镜像一份）后超 1MB。
	big := strings.Repeat("x", common.RichTextMaxPayloadBytes-200)
	p := map[string]interface{}{
		"type": float64(14),
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": big},
		},
	}
	err := EnsurePlain(p)
	if err == nil {
		t.Fatalf("expected oversize error, got nil")
	}
}
