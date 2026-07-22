package docsaccessrequest_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
)

// TestRegisterAndRender_Smoke 端到端跑一遍:
//
//	NewRegistry → Register(pilot) → SetDefault → Freeze → Render(pending sample)
//
// 断言产物含 metadata.octo.{protocol,template} + webUrl,且 body/actions 非空。
func TestRegisterAndRender_Smoke(t *testing.T) {
	r := cardtmpl.NewRegistry()
	tmpl := docsaccessrequest.New()
	r.Register(tmpl, docsaccessrequest.Assets, docsaccessrequest.HandoffRoot)
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersion)
	r.Freeze()

	// 加载 pending sample 直接作 fields
	sample, err := docsaccessrequest.Assets.ReadFile(
		docsaccessrequest.HandoffRoot + "/samples/pending.json",
	)
	if err != nil {
		t.Fatalf("read pending sample: %v", err)
	}

	env := cardtmpl.BuildEnv{
		WebLoginURL: "https://web.example.com",
		Lang:        "zh-CN",
		SpaceID:     "space-1",
	}
	payload, err := r.Render(context.Background(),
		docsaccessrequest.TemplateID, "",
		docsaccessrequest.StatePending, sample, env)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// envelope
	if got, ok := payload["type"].(int); !ok || got != 17 {
		t.Fatalf("type != 17: %v (%T)", payload["type"], payload["type"])
	}
	if got, _ := payload["profile"].(string); got != "octo/v2" {
		t.Fatalf("profile != octo/v2: %v", payload["profile"])
	}

	// card + metadata
	card, ok := payload["card"].(map[string]any)
	if !ok {
		t.Fatalf("card not a map: %T", payload["card"])
	}
	metadata, _ := card["metadata"].(map[string]any)
	if metadata == nil {
		t.Fatalf("card.metadata missing")
	}
	webURL, _ := metadata["webUrl"].(string)
	if !strings.HasPrefix(webURL, "https://web.example.com/d/") {
		t.Fatalf("webUrl not on WebLoginURL host: %q", webURL)
	}
	octo, _ := metadata["octo"].(map[string]any)
	if octo == nil {
		t.Fatalf("metadata.octo missing")
	}
	if got, _ := octo["protocol"].(string); got != cardtmpl.Protocol {
		t.Fatalf("metadata.octo.protocol != %q: %v", cardtmpl.Protocol, got)
	}
	tplMeta, _ := octo["template"].(map[string]any)
	if tplMeta == nil {
		t.Fatalf("metadata.octo.template missing")
	}
	if got, _ := tplMeta["id"].(string); got != string(docsaccessrequest.TemplateID) {
		t.Fatalf("template.id: %v", got)
	}
	if got, _ := tplMeta["version"].(string); got != docsaccessrequest.TemplateVersion {
		t.Fatalf("template.version: %v", got)
	}

	// body & actions non-empty
	body, _ := card["body"].([]any)
	if len(body) == 0 {
		t.Fatalf("body empty")
	}
	actions, _ := card["actions"].([]any)
	if len(actions) == 0 {
		t.Fatalf("actions empty")
	}
	// re-marshal 保序 (调试用)
	raw, _ := json.MarshalIndent(payload, "", "  ")
	t.Logf("payload size = %d bytes", len(raw))
}

// TestFallbackText_ParityAndSanitize 验证:
//   - R4-1 (#633 blocking):FallbackText 保留 excerpt(requestReason)与时间
//     (requestedAtDisplay),不在降级路径丢字段(与 legacy 4 行信息量对齐);
//   - R2-3:每个字段各自 sanitizeLine,内部 \n\r\t 被替空格,只有字段间是合法换行
//     —— 调用方无法通过 "Ann\nEvil" 在字段内伪造多余行。
func TestFallbackText_ParityAndSanitize(t *testing.T) {
	tmpl := docsaccessrequest.New() // FallbackText 不依赖 Meta,无需 Register
	fields := json.RawMessage(
		`{"requestId":"r1","state":"pending",` +
			`"document":{"title":"Title\nInjected"},` +
			`"requester":{"name":"Ann\r\nEvil\tspoof"},` +
			`"requestReason":"why\nplease","requestedAtDisplay":"2026-01-01\t12:00"}`,
	)
	for _, lang := range []string{"zh-CN", "en"} {
		text, err := tmpl.FallbackText(docsaccessrequest.StatePending, fields, lang)
		if err != nil {
			t.Fatalf("FallbackText(lang=%q): %v", lang, err)
		}
		// 字段内 \r/\t 必须已被 sanitize 成空格,全文不得残留。
		if strings.ContainsAny(text, "\r\t") {
			t.Errorf("lang=%q fallback contains raw CR/TAB: %q", lang, text)
		}
		// 恰好 3 个字段行(actor+title / reason / time);证明注入的 "\n" 未新增行。
		lines := strings.Split(text, "\n")
		if len(lines) != 3 {
			t.Errorf("lang=%q want 3 field lines, got %d: %q", lang, len(lines), text)
		}
		// R4-1 parity:excerpt 与 time 内容必须出现(sanitize 后)。
		if !strings.Contains(text, "why please") {
			t.Errorf("lang=%q fallback dropped excerpt: %q", lang, text)
		}
		if !strings.Contains(text, "2026-01-01 12:00") {
			t.Errorf("lang=%q fallback dropped time: %q", lang, text)
		}
	}

	// 无 reason/time 时退回单行(actor+title),不产生空字段行。
	minimal := json.RawMessage(`{"requestId":"r1","state":"pending","document":{"title":"T"},"requester":{"name":"Ann"}}`)
	text, err := tmpl.FallbackText(docsaccessrequest.StatePending, minimal, "zh-CN")
	if err != nil {
		t.Fatalf("FallbackText(minimal): %v", err)
	}
	if strings.ContainsAny(text, "\n\r\t") {
		t.Errorf("minimal fallback should be single line, got %q", text)
	}
}

// Registry 从 manifest.sourceLabel 载入的中文默认值必须被 pilot Build 按
// env.Lang 覆盖,避免英文卡片带中文来源角标。
func TestBuild_SourceLocalization(t *testing.T) {
	r := cardtmpl.NewRegistry()
	tmpl := docsaccessrequest.New()
	r.Register(tmpl, docsaccessrequest.Assets, docsaccessrequest.HandoffRoot)
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersion)
	r.Freeze()

	sample, err := docsaccessrequest.Assets.ReadFile(
		docsaccessrequest.HandoffRoot + "/samples/pending.json",
	)
	if err != nil {
		t.Fatalf("read pending sample: %v", err)
	}

	cases := []struct {
		lang    string
		wantSrc string
	}{
		{"zh-CN", "文档"},
		{"en", "Docs"},
		{"en-US", "Docs"},
		{"", "Docs"}, // 未知 lang 默认走英文分支
	}
	for _, tc := range cases {
		t.Run(tc.lang, func(t *testing.T) {
			env := cardtmpl.BuildEnv{
				WebLoginURL: "https://web.example.com",
				Lang:        tc.lang,
				SpaceID:     "space-1",
			}
			payload, err := r.Render(context.Background(),
				docsaccessrequest.TemplateID, "",
				docsaccessrequest.StatePending, sample, env)
			if err != nil {
				t.Fatalf("Render(lang=%q): %v", tc.lang, err)
			}
			card, _ := payload["card"].(map[string]any)
			metadata, _ := card["metadata"].(map[string]any)
			octo, _ := metadata["octo"].(map[string]any)
			source, _ := octo["source"].(map[string]any)
			got, _ := source["label"].(string)
			if got != tc.wantSrc {
				t.Errorf("lang=%q metadata.octo.source.label = %q, want %q",
					tc.lang, got, tc.wantSrc)
			}
		})
	}
}
