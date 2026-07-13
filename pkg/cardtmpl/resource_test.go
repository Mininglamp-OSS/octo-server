package cardtmpl

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func localizedContext(lang string) context.Context {
	return i18n.WithLanguage(context.Background(), i18n.LanguageDecision{
		Language: lang,
		Source:   i18n.LanguageSourceUser,
	})
}

func exampleResourceCard() ResourceCard {
	return ResourceCard{
		IconURL:     "https://static.example.com/summary.png",
		Title:       "Quarterly summary",
		Attribution: "Alice shared a smart summary",
		Excerpt:     "Revenue increased by **18%**.",
		Facts: []Fact{
			{Title: "Status", Value: "Completed"},
			{Title: "Messages", Value: "128"},
		},
		CopyText: "summary-42",
	}
}

func TestBuildSummaryResourceCardSnapshotAndValidation(t *testing.T) {
	document, err := BuildSummaryResourceCard(
		localizedContext("en-US"),
		"https://im.example.com/login?redirect=1",
		"task/42",
		"space a",
		exampleResourceCard(),
	)
	require.NoError(t, err)

	want := `{
	  "type":"AdaptiveCard",
	  "version":"1.5",
	  "body":[
	    {"type":"ColumnSet","columns":[
	      {"type":"Column","width":"auto","items":[{"type":"Image","url":"https://static.example.com/summary.png","size":"Small"}]},
	      {"type":"Column","width":"stretch","items":[
	        {"type":"TextBlock","text":"Quarterly summary","weight":"Bolder","wrap":true},
	        {"type":"TextBlock","text":"Alice shared a smart summary","isSubtle":true,"spacing":"None","wrap":true}
	      ]}
	    ]},
	    {"type":"TextBlock","text":"Revenue increased by \\*\\*18%\\*\\*.","wrap":true},
	    {"type":"FactSet","facts":[{"title":"Status","value":"Completed"},{"title":"Messages","value":"128"}]},
	    {"type":"ActionSet","actions":[
	      {"type":"Action.OpenUrl","title":"View details","url":"https://im.example.com/s/task%2F42?sp=space+a"},
	      {"type":"Action.CopyToClipboard","title":"Copy","text":"summary-42"}
	    ]}
	  ]
	}`
	assert.JSONEq(t, want, string(document))

	var card map[string]interface{}
	require.NoError(t, json.Unmarshal(document, &card))
	envelope := map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"profile":      cardmsg.ProfileV1,
		"card":         card,
	}
	require.NoError(t, cardmsg.Validate(envelope))
}

func TestBuildSummaryResourceCardIconIsOptional(t *testing.T) {
	resource := exampleResourceCard()
	resource.IconURL = ""
	document, err := BuildSummaryResourceCard(
		localizedContext("en-US"),
		"https://im.example.com/login",
		"task-1",
		"space-1",
		resource,
	)
	require.NoError(t, err)

	var card struct {
		Body []map[string]interface{} `json:"body"`
	}
	require.NoError(t, json.Unmarshal(document, &card))
	require.NotEmpty(t, card.Body)
	assert.Equal(t, "Container", card.Body[0]["type"], "no-icon header should be a plain Container, not a ColumnSet")
	assert.NotContains(t, string(document), `"type":"Image"`)

	envelope := map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"profile":      cardmsg.ProfileV1,
		"card":         mustDecodeCard(t, document),
	}
	require.NoError(t, cardmsg.Validate(envelope))
}

func mustDecodeCard(t *testing.T, document []byte) map[string]interface{} {
	t.Helper()
	var card map[string]interface{}
	require.NoError(t, json.Unmarshal(document, &card))
	return card
}

func TestBuildSummaryResourceCardUsesOutboundLanguage(t *testing.T) {
	document, err := BuildSummaryResourceCard(
		localizedContext("zh-CN"),
		"https://im.example.com/login",
		"task-1",
		"space-1",
		exampleResourceCard(),
	)
	require.NoError(t, err)
	assert.Contains(t, string(document), `"title":"查看详情"`)
	assert.Contains(t, string(document), `"title":"复制"`)
}

func TestBuildSummaryResourceCardEscapesExternalMarkdownLeaves(t *testing.T) {
	resource := exampleResourceCard()
	resource.Title = `[forged](javascript:alert(1)) **admin**`
	resource.Attribution = `<https://evil.example>`
	resource.Excerpt = `![image](https://evil.example/x.png)`
	resource.Facts = []Fact{{Title: "[role]", Value: "**owner**"}}
	document, err := BuildSummaryResourceCard(
		localizedContext("en-US"),
		"https://im.example.com/login",
		"task-1",
		"space-1",
		resource,
	)
	require.NoError(t, err)
	assert.NotContains(t, string(document), `"text":"[forged](`)
	assert.Contains(t, string(document), `\\[forged\\]\\(javascript:alert\\(1\\)\\)`)
	assert.Contains(t, string(document), `\\*\\*owner\\*\\*`)
}

func TestBuildSummaryResourceCardBoundsExcerptByRunes(t *testing.T) {
	resource := exampleResourceCard()
	resource.Excerpt = strings.Repeat("摘", MaxExcerptRunes+50)
	document, err := BuildSummaryResourceCard(
		localizedContext("zh-CN"),
		"https://im.example.com/login",
		"task-1",
		"space-1",
		resource,
	)
	require.NoError(t, err)

	var card struct {
		Body []map[string]interface{} `json:"body"`
	}
	require.NoError(t, json.Unmarshal(document, &card))
	require.GreaterOrEqual(t, len(card.Body), 2)
	excerpt, _ := card.Body[1]["text"].(string)
	assert.LessOrEqual(t, utf8.RuneCountInString(excerpt), MaxExcerptRunes)
	assert.True(t, strings.HasSuffix(excerpt, "…"))
}

func TestBuildSummaryResourceCardRejectsUnsafeInputs(t *testing.T) {
	cases := []struct {
		name       string
		webBaseURL string
		resource   ResourceCard
	}{
		{name: "non https web base", webBaseURL: "http://im.example.com/login", resource: exampleResourceCard()},
		{name: "unsafe icon", webBaseURL: "https://im.example.com/login", resource: func() ResourceCard { r := exampleResourceCard(); r.IconURL = "javascript:alert(1)"; return r }()},
		{name: "missing title", webBaseURL: "https://im.example.com/login", resource: func() ResourceCard { r := exampleResourceCard(); r.Title = ""; return r }()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			document, err := BuildSummaryResourceCard(localizedContext("en-US"), tc.webBaseURL, "task-1", "space-1", tc.resource)
			assert.Nil(t, document)
			assert.Error(t, err)
		})
	}
}
