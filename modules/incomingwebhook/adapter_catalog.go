package incomingwebhook

import (
	"embed"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/base/common/msgtmpl"
	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"go.uber.org/zap"
)

//go:embed templates
var adapterTemplatesFS embed.FS

var adapterMessages = msgtmpl.MustNew(adapterTemplatesFS, "templates")

type adapterAuthResp struct {
	Type        string `json:"type"`
	Header      string `json:"header,omitempty"`
	ValueSource string `json:"value_source,omitempty"`
}

type adapterExampleResp struct {
	Key         string          `json:"key"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	URL         string          `json:"url"`
	ContentType string          `json:"content_type"`
	Auth        adapterAuthResp `json:"auth"`
	Steps       []string        `json:"steps"`
}

type publicAdapterDef struct {
	Key         string
	Suffix      string
	ContentType string
	Auth        adapterAuthResp
	ShowExample bool
}

var publicAdapterCatalog = []publicAdapterDef{
	{Key: adapterNative, ContentType: "application/json"},
	{Key: adapterGitHub, Suffix: adapterGitHub, ContentType: "application/json", Auth: adapterAuthResp{Type: "url_token"}, ShowExample: true},
	{Key: adapterGitLab, Suffix: adapterGitLab, ContentType: "application/json", Auth: adapterAuthResp{Type: "url_token_and_header", Header: "X-Gitlab-Token", ValueSource: "token"}, ShowExample: true},
	{Key: adapterFeishu, Suffix: adapterFeishu, ContentType: "application/json", Auth: adapterAuthResp{Type: "url_token"}, ShowExample: true},
	{Key: adapterMultica, Suffix: adapterMultica, ContentType: "application/json", Auth: adapterAuthResp{Type: "url_token"}, ShowExample: true},
	{Key: adapterWeCom, Suffix: adapterWeCom, ContentType: "application/json", Auth: adapterAuthResp{Type: "url_token"}, ShowExample: true},
}

func publicAdapterExamples(webhookID, token, lang string) ([]adapterExampleResp, error) {
	urls := publicURLs(webhookID, token)
	examples := make([]adapterExampleResp, 0, len(publicAdapterCatalog)-1)
	for _, def := range publicAdapterCatalog {
		if !def.ShowExample {
			continue
		}
		data := map[string]any{
			"URL":         urls[def.Key],
			"ContentType": def.ContentType,
		}
		title, err := adapterMessages.Render(adapterTemplateName(def.Key, "title"), lang, data)
		if err != nil {
			return nil, err
		}
		description, err := adapterMessages.Render(adapterTemplateName(def.Key, "description"), lang, data)
		if err != nil {
			return nil, err
		}
		rawSteps, err := adapterMessages.Render(adapterTemplateName(def.Key, "steps"), lang, data)
		if err != nil {
			return nil, err
		}
		examples = append(examples, adapterExampleResp{
			Key:         def.Key,
			Title:       strings.TrimSpace(title),
			Description: strings.TrimSpace(description),
			URL:         urls[def.Key],
			ContentType: def.ContentType,
			Auth:        def.Auth,
			Steps:       splitAdapterSteps(rawSteps),
		})
	}
	return examples, nil
}

func renderPublicAdapterExamples(w *IncomingWebhook, c *wkhttp.Context, webhookID, token string) []adapterExampleResp {
	examples, err := publicAdapterExamples(webhookID, token, incomingWebhookResponseLanguage(c))
	if err != nil {
		w.Error("render incoming webhook adapter examples failed", zap.Error(err))
		return []adapterExampleResp{}
	}
	return examples
}

func adapterTemplateName(key, part string) string {
	return fmt.Sprintf("adapter.%s.%s", key, part)
}

func splitAdapterSteps(s string) []string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func incomingWebhookResponseLanguage(c *wkhttp.Context) string {
	def, err := octoi18n.DefaultLanguageFromEnv()
	if err != nil {
		def = octoi18n.DefaultLanguage
	}
	if c == nil || c.Request == nil {
		return def
	}
	if decision, ok := octoi18n.LanguageFromContext(c.Request.Context()); ok {
		return decision.Language
	}
	return octoi18n.NegotiateLanguage(c.Request, octoi18n.LanguageNegotiationOptions{
		DefaultLanguage: def,
	}).Language
}
