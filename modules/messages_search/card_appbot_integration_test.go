//go:build integration

package messages_search

import (
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/cardtrust"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSingleMessageHitPublishedAppBotProjection(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	_, err := ctx.DB().InsertBySql(`
		INSERT INTO app_bot(id,uid,display_name,scope,status,token,created_by)
		VALUES('search_app','app_search_1','Search App','platform',1,'app_search_token_1','owner')`).Exec()
	require.NoError(t, err)

	cardType := payloadTypeCard
	doc := Doc{
		MessageID: 9017,
		From:      "app_search_1",
		Payload:   &Payload{Type: &cardType},
		PayloadRaw: json.RawMessage(
			`{"type":17,"card":{"body":[{"type":"TextBlock","text":"内部字段"}]},"plain":"App Bot 审批单","card_version":"1.5","profile":"octo/v1"}`,
		),
	}

	h := &Handler{cardTrust: cardtrust.New(ctx)}
	mh := h.singleMessageHit(doc, "g_1", 0, nil)
	assert.Equal(t, "App Bot 审批单", mh.Snippet)
}
