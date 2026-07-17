package cardtmpl

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func exampleDocsApprovalContent() DocsApprovalContent {
	return DocsApprovalContent{
		Title:        "2026 Q3 产品路线图",
		Actor:        "李四",
		Timestamp:    "2026-07-16 11:15",
		Reason:       "申请查看权限，用于对齐 Q3 交付节奏。",
		Variant:      "docs.access_requested",
		Source:       Source{Label: "文档"},
		HeaderLabel:  "文档申请",
		StatusLabel:  "待你处理",
		BannerSuffix: "申请成为此文档的查看者。",
		RoleLabel:    "申请人",
		ReasonLabel:  "申请原因",
	}
}

// flattenCardNodes recursively collects every element/action object (any map
// carrying a "type") reachable through the known AC child collections.
func flattenCardNodes(v interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	switch node := v.(type) {
	case map[string]interface{}:
		if _, ok := node["type"]; ok {
			out = append(out, node)
		}
		for _, key := range []string{"body", "items", "columns", "inlines", "actions", "facts"} {
			if child, ok := node[key]; ok {
				out = append(out, flattenCardNodes(child)...)
			}
		}
	case []interface{}:
		for _, item := range node {
			out = append(out, flattenCardNodes(item)...)
		}
	}
	return out
}

func nodesOfType(nodes []map[string]interface{}, t string) []map[string]interface{} {
	var out []map[string]interface{}
	for _, n := range nodes {
		if s, _ := n["type"].(string); s == t {
			out = append(out, n)
		}
	}
	return out
}

func TestBuildDocsAccessRequestCardEnrichedLayoutAndActions(t *testing.T) {
	document, err := BuildDocsAccessRequestCard(
		localizedContext("zh-CN"),
		"https://im.example.com/login",
		"doc-1",
		"request-1",
		"space-1",
		exampleDocsApprovalContent(),
		ApprovalActions{ApproveTitle: "允许", DenyTitle: "拒绝"},
	)
	require.NoError(t, err)

	var card map[string]interface{}
	require.NoError(t, json.Unmarshal(document, &card))

	// Actions: view-details (OpenUrl) then deny (destructive) then approve
	// (positive) — approve is the primary/rightmost decision.
	actions, ok := card["actions"].([]interface{})
	require.True(t, ok)
	require.Len(t, actions, 3)
	view := actions[0].(map[string]interface{})
	assert.Equal(t, "Action.OpenUrl", view["type"])
	assert.Equal(t, "查看详情", view["title"])
	deny := actions[1].(map[string]interface{})
	assert.Equal(t, "Action.Submit", deny["type"])
	assert.Equal(t, DocsDenyActionID, deny["id"])
	assert.Equal(t, "destructive", deny["style"])
	approve := actions[2].(map[string]interface{})
	assert.Equal(t, "Action.Submit", approve["type"])
	assert.Equal(t, DocsApproveActionID, approve["id"])
	assert.Equal(t, "positive", approve["style"])
	for _, a := range []map[string]interface{}{deny, approve} {
		data := a["data"].(map[string]interface{})
		assert.Equal(t, "docs", data["owner"])
		assert.Equal(t, "access_request.decision", data["action_type"])
		assert.Equal(t, "doc-1", data["doc_id"])
		assert.Equal(t, "request-1", data["request_id"])
	}
	assert.Equal(t, "deny", deny["data"].(map[string]interface{})["decision"])
	assert.Equal(t, "approve", approve["data"].(map[string]interface{})["decision"])

	nodes := flattenCardNodes(card["body"])
	// The hidden deny-reason input must be declared (id channel for the reason).
	inputs := nodesOfType(nodes, "Input.Text")
	require.Len(t, inputs, 1)
	assert.Equal(t, DocsDenyReasonInputID, inputs[0]["id"])
	assert.Equal(t, false, inputs[0]["isVisible"])

	// Enriched sections are present: header label, status, big title, reason box.
	require.NotEmpty(t, nodesOfType(nodes, "ColumnSet"))
	require.NotEmpty(t, nodesOfType(nodes, "Container"))
	assert.True(t, cardHasText(nodes, "文档申请"))
	assert.True(t, cardHasText(nodes, "待你处理"))
	assert.True(t, cardHasText(nodes, "2026 Q3 产品路线图"))
	assert.True(t, cardHasText(nodes, "申请原因"))

	envelope := map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"profile":      cardmsg.ProfileV2,
		"card":         card,
	}
	require.NoError(t, cardmsg.Validate(envelope), "enriched docs template must pass octo/v2 validation")
}

// cardHasText reports whether any TextBlock/TextRun's text equals want.
func cardHasText(nodes []map[string]interface{}, want string) bool {
	for _, n := range nodes {
		if s, _ := n["text"].(string); s == want {
			return true
		}
	}
	return false
}

// TestDocsAccessRequestCardDenyReasonSubmitContract closes the load-bearing seam
// of the reason channel without infra: the enriched card declares the deny_reason
// input, so the real card/action inputs validator (cardmsg.ValidateInputs)
// accepts a submitted inputs[deny_reason] and fail-closes on undeclared keys.
func TestDocsAccessRequestCardDenyReasonSubmitContract(t *testing.T) {
	document, err := BuildDocsAccessRequestCard(
		localizedContext("zh-CN"), "https://im.example.com/login", "doc-1", "request-1", "space-1",
		exampleDocsApprovalContent(), ApprovalActions{ApproveTitle: "允许", DenyTitle: "拒绝"},
	)
	require.NoError(t, err)
	var card map[string]interface{}
	require.NoError(t, json.Unmarshal(document, &card))
	envelope, err := json.Marshal(map[string]interface{}{"card": card})
	require.NoError(t, err)

	// The reviewer reason submits under the declared deny_reason input → accepted.
	require.NoError(t, cardmsg.ValidateInputs(envelope, map[string]interface{}{
		DocsDenyReasonInputID: "范围不符，请对齐后再申请",
	}))
	// Empty reason is a valid shape (the server does not enforce isRequired; the
	// dialog does). Approve submits deny_reason:"" harmlessly.
	require.NoError(t, cardmsg.ValidateInputs(envelope, map[string]interface{}{
		DocsDenyReasonInputID: "",
	}))
	// An undeclared input is rejected fail-closed — a forged key can't ride along.
	require.Error(t, cardmsg.ValidateInputs(envelope, map[string]interface{}{
		"totally_undeclared": "x",
	}))
}

func TestBuildDocsAccessRequestCardRejectsMissingRequestID(t *testing.T) {
	_, err := BuildDocsAccessRequestCard(
		localizedContext("en-US"),
		"https://im.example.com/login",
		"doc-1",
		"",
		"space-1",
		exampleDocsApprovalContent(),
		ApprovalActions{ApproveTitle: "Allow", DenyTitle: "Deny"},
	)
	require.Error(t, err)
}

func TestBuildDocsAccessRequestCardAvatarHTTPSOnly(t *testing.T) {
	base := "https://im.example.com/login"
	actions := ApprovalActions{ApproveTitle: "允许", DenyTitle: "拒绝"}

	// Empty avatar => no Image element.
	noAvatar := exampleDocsApprovalContent()
	doc, err := BuildDocsAccessRequestCard(localizedContext("zh-CN"), base, "d", "r", "s", noAvatar, actions)
	require.NoError(t, err)
	var card map[string]interface{}
	require.NoError(t, json.Unmarshal(doc, &card))
	assert.Empty(t, nodesOfType(flattenCardNodes(card["body"]), "Image"))

	// https avatar => one Person Image.
	withAvatar := exampleDocsApprovalContent()
	withAvatar.ActorAvatar = "https://cdn.example.com/a/lisi.png"
	doc, err = BuildDocsAccessRequestCard(localizedContext("zh-CN"), base, "d", "r", "s", withAvatar, actions)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(doc, &card))
	images := nodesOfType(flattenCardNodes(card["body"]), "Image")
	require.Len(t, images, 1)
	assert.Equal(t, "Person", images[0]["style"])
	assert.Equal(t, "https://cdn.example.com/a/lisi.png", images[0]["url"])

	// non-https avatar => build error (positive https allowlist).
	for _, bad := range []string{"http://cdn.example.com/a.png", "data:image/png;base64,AAAA", "//cdn/a.png"} {
		c := exampleDocsApprovalContent()
		c.ActorAvatar = bad
		_, err := BuildDocsAccessRequestCard(localizedContext("zh-CN"), base, "d", "r", "s", c, actions)
		require.Error(t, err, "avatar %q must be rejected", bad)
	}
}

func TestBuildDocsAccessRequestCardEscapesCallerText(t *testing.T) {
	c := exampleDocsApprovalContent()
	c.Actor = "李四*x*"
	c.Reason = "see [here](http://evil)"
	c.Title = "Road_map_"
	doc, err := BuildDocsAccessRequestCard(
		localizedContext("zh-CN"),
		"https://im.example.com/login", "d", "r", "s", c,
		ApprovalActions{ApproveTitle: "允许", DenyTitle: "拒绝"},
	)
	require.NoError(t, err)
	// Raw markdown control chars must be backslash-escaped in the card bytes so a
	// crafted actor/title/reason cannot forge formatting or links.
	nodes := flattenCardNodes(mustCardMap(t, doc)["body"])
	assert.True(t, anyTextContains(nodes, `李四\*x\*`), "actor markdown must be escaped")
	assert.True(t, anyTextContains(nodes, `\[here\]`), "reason link markdown must be escaped")
	assert.True(t, anyTextContains(nodes, `Road\_map\_`), "title markdown must be escaped")
}

func mustCardMap(t *testing.T, doc json.RawMessage) map[string]interface{} {
	t.Helper()
	var card map[string]interface{}
	require.NoError(t, json.Unmarshal(doc, &card))
	return card
}

func anyTextContains(nodes []map[string]interface{}, want string) bool {
	for _, n := range nodes {
		if s, _ := n["text"].(string); strings.Contains(s, want) {
			return true
		}
	}
	return false
}

func TestBuildDocsAccessRequestCardBannerTextRunNotEscaped(t *testing.T) {
	// TextRun renders literal text (no markdown surface), so the bold-actor banner
	// must NOT be backslash-escaped — otherwise "Wang (FE)" shows as "Wang \(FE\)".
	c := exampleDocsApprovalContent()
	c.Actor = "Wang (FE)_lead"
	c.BannerSuffix = "requested access."
	doc, err := BuildDocsAccessRequestCard(
		localizedContext("en-US"), "https://im.example.com/login", "d", "r", "s", c,
		ApprovalActions{ApproveTitle: "Allow", DenyTitle: "Deny"},
	)
	require.NoError(t, err)
	nodes := flattenCardNodes(mustCardMap(t, doc)["body"])
	runs := nodesOfType(nodes, "TextRun")
	require.NotEmpty(t, runs, "enriched banner should use RichTextBlock TextRun inlines")
	rawActor := false
	for _, r := range runs {
		s, _ := r["text"].(string)
		if strings.Contains(s, `\(`) || strings.Contains(s, `\_`) {
			t.Fatalf("banner TextRun must not be markdown-escaped: %q", s)
		}
		if strings.Contains(s, "Wang (FE)_lead") {
			rawActor = true
		}
	}
	assert.True(t, rawActor, "banner TextRun should carry the raw (unescaped) actor")
	// The requester-row name is a TextBlock (markdown-rendered) and MUST stay escaped.
	assert.True(t, anyTextContains(nodes, `Wang \(FE\)\_lead`), "requester-row TextBlock actor stays escaped")
}

func TestBuildDocsAccessRequestCardAnonymous(t *testing.T) {
	c := exampleDocsApprovalContent()
	c.Actor = ""
	c.ActorAvatar = ""
	c.BannerSuffix = "Someone requested access to this document."
	doc, err := BuildDocsAccessRequestCard(
		localizedContext("en-US"), "https://im.example.com/login", "d", "r", "s", c,
		ApprovalActions{ApproveTitle: "Allow", DenyTitle: "Deny"},
	)
	require.NoError(t, err)
	card := mustCardMap(t, doc)
	nodes := flattenCardNodes(card["body"])
	// Anonymous: subject-less banner is a plain TextBlock (no TextRun inlines) and
	// there is no requester row (no avatar Image).
	assert.Empty(t, nodesOfType(nodes, "TextRun"), "anonymous banner must not use TextRun inlines")
	assert.Empty(t, nodesOfType(nodes, "Image"), "anonymous request has no avatar row")
	assert.True(t, cardHasText(nodes, "Someone requested access to this document."))
	require.NoError(t, cardmsg.Validate(map[string]interface{}{
		"type": cardmsg.InteractiveCard.Int(), "card_version": cardmsg.CardVersion,
		"profile": cardmsg.ProfileV2, "card": card,
	}))
}

func TestBuildDocsAccessRequestCardBoundsActorInData(t *testing.T) {
	c := exampleDocsApprovalContent()
	c.Actor = strings.Repeat("名", maxActorRunes+50)
	doc, err := BuildDocsAccessRequestCard(
		localizedContext("zh-CN"), "https://im.example.com/login", "d", "r", "s", c,
		ApprovalActions{ApproveTitle: "允许", DenyTitle: "拒绝"},
	)
	require.NoError(t, err)
	actions := mustCardMap(t, doc)["actions"].([]interface{})
	for _, a := range actions {
		m := a.(map[string]interface{})
		data, ok := m["data"].(map[string]interface{})
		if !ok {
			continue // Action.OpenUrl has no data
		}
		actorInData, _ := data["actor"].(string)
		assert.LessOrEqual(t, len([]rune(actorInData)), maxActorRunes,
			"data.actor must be bounded to maxActorRunes")
	}
}

func TestBuildDocsApprovalOutcomeCard(t *testing.T) {
	base := "https://im.example.com/login"

	// Approved: good box, no reason, validates.
	approved, err := BuildDocsApprovalOutcomeCard(localizedContext("zh-CN"), base, "d", "s", DocsOutcomeContent{
		Title: "2026 Q3 产品路线图", Variant: "docs.access_approved", Source: Source{Label: "文档"},
		Denied: false, HeaderLabel: "文档申请", StatusLabel: "已允许",
		ResultText: "申请人已获得所申请的文档权限。",
	})
	require.NoError(t, err)
	card := mustCardMap(t, approved)
	nodes := flattenCardNodes(card["body"])
	assert.True(t, cardHasText(nodes, "已允许"))
	assert.True(t, cardHasText(nodes, "申请人已获得所申请的文档权限。"))
	requireOutcomeValidates(t, card)

	// Denied: attention box surfaces the reviewer reason (escaped), validates.
	denied, err := BuildDocsApprovalOutcomeCard(localizedContext("zh-CN"), base, "d", "s", DocsOutcomeContent{
		Title: "2026 Q3 产品路线图", Variant: "docs.access_denied", Source: Source{Label: "文档"},
		Denied: true, HeaderLabel: "文档申请", StatusLabel: "已拒绝",
		ResultText: "申请已被拒绝。", ReasonLabel: "拒绝原因", Reason: "权限范围*不符*",
	})
	require.NoError(t, err)
	card = mustCardMap(t, denied)
	nodes = flattenCardNodes(card["body"])
	assert.True(t, cardHasText(nodes, "已拒绝"))
	assert.True(t, cardHasText(nodes, "拒绝原因"))
	assert.True(t, anyTextContains(nodes, `权限范围\*不符\*`), "deny reason must be escaped")
	requireOutcomeValidates(t, card)
}

func requireOutcomeValidates(t *testing.T, card map[string]interface{}) {
	t.Helper()
	envelope := map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"profile":      cardmsg.ProfileV2,
		"card":         card,
	}
	require.NoError(t, cardmsg.Validate(envelope), "outcome card must pass octo/v2 validation")
}
