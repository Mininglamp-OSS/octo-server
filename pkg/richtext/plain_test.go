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

// --- PR#232 Critical#1: user-send write-strict 校验（与 robot 路径对称）---

// TestValidate_NonRichTextIsNoOp 非 type=14 的 payload 不进 RichText gate，
// 任意 shape 都通过（老消息路径不变）。
func TestValidate_NonRichTextIsNoOp(t *testing.T) {
	p := payloadFromJSON(t, `{"type":1,"content":""}`)
	if err := Validate(p); err != nil {
		t.Fatalf("non-richtext Validate err: %v", err)
	}
}

// TestValidate_RejectsDirtyPayloads 覆盖 Jerry-Xin Critical#1 列出的脏 payload：
// 缺/空 content、空 text 块、data: 图片 URL、缺图片宽高、未知 block type
// 全部必须被拒（旧 EnsurePlain 经 FillPlainBounded 会放过这些）。
func TestValidate_RejectsDirtyPayloads(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"missing_content", `{"type":14,"plain":"x"}`},
		{"null_content", `{"type":14,"content":null}`},
		{"empty_content_array", `{"type":14,"content":[]}`},
		{"empty_text_block", `{"type":14,"content":[{"type":"text","text":""}]}`},
		{"blank_text_block", `{"type":14,"content":[{"type":"text","text":"   "}]}`},
		{"data_uri_image", `{"type":14,"content":[{"type":"image","url":"data:image/png;base64,AAAA","width":10,"height":10}]}`},
		{"javascript_uri_image", `{"type":14,"content":[{"type":"image","url":"javascript:alert(1)","width":10,"height":10}]}`},
		{"image_missing_size", `{"type":14,"content":[{"type":"image","url":"https://x/y.png"}]}`},
		{"image_zero_size", `{"type":14,"content":[{"type":"image","url":"https://x/y.png","width":0,"height":0}]}`},
		{"image_no_url", `{"type":14,"content":[{"type":"image","width":10,"height":10}]}`},
		{"unknown_block_type", `{"type":14,"content":[{"type":"video","text":"hi"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := payloadFromJSON(t, c.payload)
			if err := Validate(p); err == nil {
				t.Fatalf("Validate accepted dirty payload %q, want reject", c.payload)
			}
		})
	}
}

// TestValidate_AcceptsCleanPayload 合法 payload 通过，且 Validate 不改写 payload
// （plain 的权威生成留给 enrich 之后的 Finalize）。
func TestValidate_AcceptsCleanPayload(t *testing.T) {
	p := payloadFromJSON(t, `{"type":14,"plain":"FORGED","content":[
		{"type":"text","text":"hi"},
		{"type":"image","url":"https://x/y.png","width":10,"height":10}
	]}`)
	if err := Validate(p); err != nil {
		t.Fatalf("Validate rejected clean payload: %v", err)
	}
	// Validate 是只读 gate：不得在此处覆盖端上的 plain。
	if p["plain"] != "FORGED" {
		t.Fatalf("Validate mutated plain: %v", p["plain"])
	}
}

// TestValidate_OversizeWithExtraTopLevelField 校验大小计算用的是「完整」payload
// 字节（含未知顶层字段），而不是 content+plain 子集（Jerry-Xin Critical#2 的
// size 计算分支）。一个未知顶层巨字段必须把 payload 顶过 1MB 并被拒。
func TestValidate_OversizeWithExtraTopLevelField(t *testing.T) {
	bigExtra := strings.Repeat("x", common.RichTextMaxPayloadBytes)
	p := map[string]interface{}{
		"type": float64(14),
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "hi"},
		},
		// 未知顶层字段：octo-lib RichTextPayload 解析时会丢弃它，
		// 若 size check 跑在裁剪后子集上就会漏检——这里必须算进总大小。
		"junk": bigExtra,
	}
	if err := Validate(p); err == nil {
		t.Fatalf("Validate accepted payload oversized by unknown top-level field")
	}
}

// --- PR#232 Critical#2: 1MB 复检作用在真实最终 payload（enrich 之后）---

// TestFinalize_OverwritesUntrustedPlain Finalize 用 content 重算权威 plain，
// 覆盖端上不可信 plain（与旧 EnsurePlain 行为一致）。
func TestFinalize_OverwritesUntrustedPlain(t *testing.T) {
	p := payloadFromJSON(t, `{"type":14,"plain":"FORGED","content":[
		{"type":"text","text":"hello "},
		{"type":"image","url":"https://x/y.png","width":10,"height":10},
		{"type":"text","text":" world"}
	]}`)
	if err := Finalize(p); err != nil {
		t.Fatalf("Finalize err: %v", err)
	}
	want := "hello " + common.RichTextImagePlaceholder + " world"
	if p["plain"] != want {
		t.Fatalf("plain=%q want %q", p["plain"], want)
	}
}

// TestFinalize_NonRichTextIsNoOp 非 type=14 payload Finalize 不改写。
func TestFinalize_NonRichTextIsNoOp(t *testing.T) {
	p := payloadFromJSON(t, `{"type":1,"content":"hi","plain":"client-sent"}`)
	if err := Finalize(p); err != nil {
		t.Fatalf("Finalize err: %v", err)
	}
	if p["plain"] != "client-sent" {
		t.Fatalf("non-richtext plain mutated: %v", p["plain"])
	}
}

// TestFinalize_OversizeAfterServerEnrich 模拟 server enrich 后超限：入站时
// payload 在 1MB 内（能过 Validate），但 server 注入一个顶层字段把它顶过 1MB，
// Finalize 必须对真实最终 payload 复检并拒发（Jerry-Xin Critical#2 主场景）。
func TestFinalize_OversizeAfterServerEnrich(t *testing.T) {
	// 入站 payload：text 接近上限但仍合法、可过 Validate。
	big := strings.Repeat("x", common.RichTextMaxPayloadBytes-400)
	p := map[string]interface{}{
		"type": float64(14),
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": big},
		},
	}
	if err := Validate(p); err != nil {
		t.Fatalf("入站 payload 应能过 Validate，却被拒: %v", err)
	}
	// server enrich：注入权威 space_id（enrichPayloadWithSpaceID 等）后再注入
	// plain 镜像，整体顶过 1MB。
	p["space_id"] = strings.Repeat("s", 800)
	if err := Finalize(p); err == nil {
		t.Fatalf("Finalize 未拦下 enrich 后超限的 payload")
	}
}

// TestFinalize_SizeCountsFullPayload 复检大小落在完整 payload 上：一个 content 合法、
// 但带未知顶层巨字段的 payload，Finalize 必须按完整字节判定超限。
func TestFinalize_SizeCountsFullPayload(t *testing.T) {
	bigExtra := strings.Repeat("x", common.RichTextMaxPayloadBytes)
	p := map[string]interface{}{
		"type": float64(14),
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "hi"},
		},
		"junk": bigExtra,
	}
	if err := Finalize(p); err == nil {
		t.Fatalf("Finalize 未按完整 payload 字节判定超限")
	}
}

// TestFinalize_PreservesExtraTopLevelFields Finalize 只写 plain，不得丢弃
// payload 上的其它顶层字段（server enrich 注入的 space_id 等必须保留出站）。
func TestFinalize_PreservesExtraTopLevelFields(t *testing.T) {
	p := payloadFromJSON(t, `{"type":14,"space_id":"sp-1","content":[
		{"type":"text","text":"hi"}
	]}`)
	if err := Finalize(p); err != nil {
		t.Fatalf("Finalize err: %v", err)
	}
	if p["space_id"] != "sp-1" {
		t.Fatalf("Finalize dropped top-level space_id: %v", p["space_id"])
	}
	if p["plain"] != "hi" {
		t.Fatalf("plain=%q want %q", p["plain"], "hi")
	}
}

// ---- NormalizeContentEdit：四个消息编辑写入口共用的 content_edit 收敛 gate ----

// TestNormalizeContentEdit_OverwritesUntrustedPlain 编辑 14 体时，server 必须用
// content 重算权威 plain 覆盖端上送的伪造 plain，并返回 canonical JSON。
func TestNormalizeContentEdit_OverwritesUntrustedPlain(t *testing.T) {
	in := `{"type":14,"plain":"FORGED","content":[
		{"type":"text","text":"hello "},
		{"type":"image","url":"https://x/y.png","width":10,"height":10},
		{"type":"text","text":" world"}
	]}`
	out, err := NormalizeContentEdit(in)
	if err != nil {
		t.Fatalf("NormalizeContentEdit err: %v", err)
	}
	got := payloadFromJSON(t, out)
	want := "hello " + common.RichTextImagePlaceholder + " world"
	if got["plain"] != want {
		t.Fatalf("plain=%q want %q", got["plain"], want)
	}
}

// TestNormalizeContentEdit_RejectsDirtyPayload 脏 14 content_edit 必须被拒，
// 不允许写脏 content / 丢 plain 进库（与 send 路径 Validate 对称）。
func TestNormalizeContentEdit_RejectsDirtyPayload(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty_content_array", `{"type":14,"content":[]}`},
		{"empty_text_block", `{"type":14,"content":[{"type":"text","text":""}]}`},
		{"data_uri_image", `{"type":14,"content":[{"type":"image","url":"data:image/png;base64,AAAA","width":10,"height":10}]}`},
		{"image_missing_size", `{"type":14,"content":[{"type":"image","url":"https://x/y.png"}]}`},
		{"unknown_block_type", `{"type":14,"content":[{"type":"video","text":"hi"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NormalizeContentEdit(c.in); err == nil {
				t.Fatalf("NormalizeContentEdit accepted dirty payload %s", c.in)
			}
		})
	}
}

// TestNormalizeContentEdit_NonRichTextUnchanged 非 14 编辑体原样返回，老编辑路径
// 行为不变（不解析、不改 plain、不重排字段）。
func TestNormalizeContentEdit_NonRichTextUnchanged(t *testing.T) {
	cases := []string{
		`{"type":1,"content":"hi","plain":"client-sent"}`,
		`{"type":1,"content":""}`,
	}
	for _, in := range cases {
		out, err := NormalizeContentEdit(in)
		if err != nil {
			t.Fatalf("NormalizeContentEdit err: %v", err)
		}
		if out != in {
			t.Fatalf("non-14 content_edit mutated: in=%q out=%q", in, out)
		}
	}
}

// TestNormalizeContentEdit_NonJSONUnchanged content_edit 不是 JSON 对象（老的/
// 自由文本编辑体）时原样返回，不报错、不改老行为。
func TestNormalizeContentEdit_NonJSONUnchanged(t *testing.T) {
	cases := []string{
		"",
		"just plain text",
		`["a","b"]`,
		`"a string"`,
	}
	for _, in := range cases {
		out, err := NormalizeContentEdit(in)
		if err != nil {
			t.Fatalf("NormalizeContentEdit(%q) err: %v", in, err)
		}
		if out != in {
			t.Fatalf("non-JSON content_edit mutated: in=%q out=%q", in, out)
		}
	}
}

// TestNormalizeContentEdit_OversizeRejected 编辑后超 1MB 的 14 体必须被拒。
func TestNormalizeContentEdit_OversizeRejected(t *testing.T) {
	big := strings.Repeat("x", common.RichTextMaxPayloadBytes-200)
	in := `{"type":14,"content":[{"type":"text","text":"` + big + `"}]}`
	if _, err := NormalizeContentEdit(in); err == nil {
		t.Fatalf("NormalizeContentEdit accepted oversize 14 content_edit")
	}
}
