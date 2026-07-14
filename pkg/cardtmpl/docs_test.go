package cardtmpl

import (
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func exampleDocsResourceCard() ResourceCard {
	return ResourceCard{
		Title:       "Product roadmap 2026",
		Attribution: "Alice shared a document",
		Excerpt:     "Q3 launch plan is finalized.",
		Variant:     "docs.shared",
		Source:      Source{Label: "Docs"},
	}
}

func TestBuildDocsResourceCardSnapshotAndValidation(t *testing.T) {
	document, err := BuildDocsResourceCard(
		localizedContext("en-US"),
		"https://im.example.com/login?redirect=1",
		"doc/42",
		"space a",
		exampleDocsResourceCard(),
	)
	require.NoError(t, err)

	want := `{
	  "type":"AdaptiveCard",
	  "version":"1.5",
	  "metadata":{
	    "webUrl":"https://im.example.com/d/doc%2F42?sp=space+a",
	    "octo":{
	      "variant":"docs.shared",
	      "source":{"label":"Docs"}
	    }
	  },
	  "body":[
	    {"type":"Container","items":[
	      {"type":"TextBlock","text":"Product roadmap 2026","weight":"Bolder","wrap":true},
	      {"type":"TextBlock","text":"Alice shared a document","isSubtle":true,"spacing":"None","wrap":true}
	    ]},
	    {"type":"TextBlock","text":"Q3 launch plan is finalized.","wrap":true},
	    {"type":"ActionSet","actions":[
	      {"type":"Action.OpenUrl","title":"View details","url":"https://im.example.com/d/doc%2F42?sp=space+a"}
	    ]}
	  ]
	}`
	assert.JSONEq(t, want, string(document))

	envelope := map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"profile":      cardmsg.ProfileV1,
		"card":         mustDecodeCard(t, document),
	}
	require.NoError(t, cardmsg.Validate(envelope))
}

func TestBuildDocsResourceCardUsesOutboundLanguage(t *testing.T) {
	document, err := BuildDocsResourceCard(
		localizedContext("zh-CN"),
		"https://im.example.com/login",
		"doc-1",
		"space-1",
		exampleDocsResourceCard(),
	)
	require.NoError(t, err)
	assert.Contains(t, string(document), `"title":"查看详情"`)
}

func TestBuildDocsResourceCardDeepLinkShape(t *testing.T) {
	document, err := BuildDocsResourceCard(
		localizedContext("en-US"),
		"https://im.example.com/login?redirect=1",
		"doc-abc",
		"space-1",
		exampleDocsResourceCard(),
	)
	require.NoError(t, err)

	var card struct {
		Body []map[string]interface{} `json:"body"`
	}
	require.NoError(t, json.Unmarshal(document, &card))
	// ActionSet is the last body element; first action's url pins the /d/ shape.
	require.NotEmpty(t, card.Body)
	last := card.Body[len(card.Body)-1]
	require.Equal(t, "ActionSet", last["type"])
	actions, _ := last["actions"].([]interface{})
	require.NotEmpty(t, actions)
	first, _ := actions[0].(map[string]interface{})
	require.NotNil(t, first)
	assert.Equal(t, "Action.OpenUrl", first["type"])
	assert.Equal(t, "https://im.example.com/d/doc-abc?sp=space-1", first["url"])
}

func TestBuildDocsResourceCardRejectsUnsafeInputs(t *testing.T) {
	cases := []struct {
		name       string
		webBaseURL string
		docID      string
		spaceID    string
		resource   ResourceCard
	}{
		{name: "non https web base", webBaseURL: "http://im.example.com/login", docID: "doc-1", spaceID: "space-1", resource: exampleDocsResourceCard()},
		{name: "unsafe icon", webBaseURL: "https://im.example.com/login", docID: "doc-1", spaceID: "space-1", resource: func() ResourceCard { r := exampleDocsResourceCard(); r.IconURL = "javascript:alert(1)"; return r }()},
		{name: "missing title", webBaseURL: "https://im.example.com/login", docID: "doc-1", spaceID: "space-1", resource: func() ResourceCard { r := exampleDocsResourceCard(); r.Title = ""; return r }()},
		{name: "empty doc id", webBaseURL: "https://im.example.com/login", docID: "", spaceID: "space-1", resource: exampleDocsResourceCard()},
		{name: "empty space id", webBaseURL: "https://im.example.com/login", docID: "doc-1", spaceID: "", resource: exampleDocsResourceCard()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			document, err := BuildDocsResourceCard(localizedContext("en-US"), tc.webBaseURL, tc.docID, tc.spaceID, tc.resource)
			assert.Nil(t, document)
			assert.Error(t, err)
		})
	}
}
