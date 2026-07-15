package cardtmpl

import (
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildDocsAccessRequestCardProducesReviewedV2Actions(t *testing.T) {
	document, err := BuildDocsAccessRequestCard(
		localizedContext("zh-CN"),
		"https://im.example.com/login",
		"doc-1",
		"request-1",
		"space-1",
		exampleDocsResourceCard(),
		ApprovalActions{ApproveTitle: "允许", DenyTitle: "拒绝"},
	)
	require.NoError(t, err)

	var card map[string]interface{}
	require.NoError(t, json.Unmarshal(document, &card))
	actions, ok := card["actions"].([]interface{})
	require.True(t, ok)
	require.Len(t, actions, 2)

	want := []struct {
		id       string
		title    string
		decision string
	}{
		{id: DocsApproveActionID, title: "允许", decision: "approve"},
		{id: DocsDenyActionID, title: "拒绝", decision: "deny"},
	}
	for i, expected := range want {
		action, ok := actions[i].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, "Action.Submit", action["type"])
		assert.Equal(t, expected.id, action["id"])
		assert.Equal(t, expected.title, action["title"])
		data, ok := action["data"].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, "docs", data["owner"])
		assert.Equal(t, "access_request.decision", data["action_type"])
		assert.Equal(t, expected.decision, data["decision"])
		assert.Equal(t, "doc-1", data["doc_id"])
		assert.Equal(t, "request-1", data["request_id"])
	}

	envelope := map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"profile":      cardmsg.ProfileV2,
		"card":         card,
	}
	require.NoError(t, cardmsg.Validate(envelope), "reviewed docs template must pass octo/v2 validation")
}

func TestBuildDocsAccessRequestCardRejectsMissingRequestID(t *testing.T) {
	_, err := BuildDocsAccessRequestCard(
		localizedContext("en-US"),
		"https://im.example.com/login",
		"doc-1",
		"",
		"space-1",
		exampleDocsResourceCard(),
		ApprovalActions{ApproveTitle: "Allow", DenyTitle: "Deny"},
	)
	require.Error(t, err)
}
