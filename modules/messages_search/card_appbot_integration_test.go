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
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	_, err := ctx.DB().Exec(`CREATE TABLE IF NOT EXISTS app_bot (
		id VARCHAR(40) PRIMARY KEY,
		uid VARCHAR(40) UNIQUE NOT NULL,
		display_name VARCHAR(100) NOT NULL,
		description VARCHAR(500) DEFAULT '',
		avatar VARCHAR(200) DEFAULT '',
		scope VARCHAR(20) NOT NULL DEFAULT 'platform',
		space_id VARCHAR(40) DEFAULT NULL,
		status TINYINT NOT NULL DEFAULT 0,
		token VARCHAR(100) UNIQUE NOT NULL,
		created_by VARCHAR(40) NOT NULL,
		created_at DATETIME NOT NULL DEFAULT NOW(),
		updated_at DATETIME NOT NULL DEFAULT NOW() ON UPDATE NOW()
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`)
	require.NoError(t, err)

	_, err = ctx.DB().InsertBySql(`
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
