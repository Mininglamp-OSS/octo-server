package group

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncAITeamGroupSubscribersAddsAndRemovesParentAndSubarea(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	require.NoError(t, testutil.CleanAllTables(ctx))
	ensureAITeamSubscriberTestTables(t, ctx)

	const (
		groupNo = "ai-team-subscriber-group"
		spaceID = "ai-team-subscriber-space"
		shortID = "2097400000000000001"
		owner   = "ai-team-owner"
		added   = "ai-team-added-bot"
		removed = "ai-team-removed-bot"
	)
	_, err := ctx.DB().InsertBySql(`INSERT INTO thread
		(short_id,group_no,name,creator_uid,status,version) VALUES (?,?,?,?,1,1)`,
		shortID, groupNo, "existing subarea", owner).Exec()
	require.NoError(t, err)
	var threadID int64
	require.NoError(t, ctx.DB().Select("id").From("thread").Where("short_id=?", shortID).LoadOne(&threadID))
	_, err = ctx.DB().InsertBySql("INSERT INTO thread_member (thread_id,uid,role,version) VALUES (?,?,1,1)", threadID, removed).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql("INSERT INTO thread_setting (group_no,short_id,uid,mute,version) VALUES (?,?,?,1,1)", groupNo, shortID, removed).Exec()
	require.NoError(t, err)

	type call struct {
		Path        string
		ChannelID   string   `json:"channel_id"`
		ChannelType uint8    `json:"channel_type"`
		Subscribers []string `json:"subscribers"`
	}
	var mu sync.Mutex
	var calls []call
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var got call
		_ = json.Unmarshal(body, &got)
		got.Path = r.URL.Path
		mu.Lock()
		calls = append(calls, got)
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer stub.Close()
	previousURL := ctx.GetConfig().WuKongIM.APIURL
	ctx.GetConfig().WuKongIM.APIURL = stub.URL
	defer func() { ctx.GetConfig().WuKongIM.APIURL = previousURL }()

	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	shortIDs, err := PrepareAITeamGroupSubscriberProjectionTx(tx, groupNo, []string{removed})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.NoError(t, SyncAITeamGroupSubscribers(ctx, groupNo, spaceID,
		shortIDs, []string{owner, added}, []string{removed}))

	channelID := groupNo + "____" + shortID
	assert.Contains(t, calls, call{Path: "/channel", ChannelID: groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(), Subscribers: []string{added, owner}})
	assert.Contains(t, calls, call{Path: "/channel/subscriber_remove", ChannelID: groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(), Subscribers: []string{removed}})
	assert.Contains(t, calls, call{Path: "/channel/subscriber_add", ChannelID: channelID,
		ChannelType: common.ChannelTypeCommunityTopic.Uint8(), Subscribers: []string{added, owner}})
	assert.Contains(t, calls, call{Path: "/channel/subscriber_remove", ChannelID: channelID,
		ChannelType: common.ChannelTypeCommunityTopic.Uint8(), Subscribers: []string{removed}})

	var memberCount, settingCount int
	require.NoError(t, ctx.DB().Select("COUNT(*)").From("thread_member").Where("thread_id=? AND uid=?", threadID, removed).LoadOne(&memberCount))
	require.NoError(t, ctx.DB().Select("COUNT(*)").From("thread_setting").Where("group_no=? AND short_id=? AND uid=?", groupNo, shortID, removed).LoadOne(&settingCount))
	assert.Zero(t, memberCount)
	assert.Zero(t, settingCount)
}

func ensureAITeamSubscriberTestTables(t *testing.T, ctx *config.Context) {
	t.Helper()
	statements := []string{
		`CREATE TABLE IF NOT EXISTS thread (
			id BIGINT AUTO_INCREMENT PRIMARY KEY,
			short_id VARCHAR(32) NOT NULL,
			group_no VARCHAR(40) NOT NULL,
			name VARCHAR(100) NOT NULL,
			creator_uid VARCHAR(40) NOT NULL,
			status TINYINT NOT NULL DEFAULT 1,
			version BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS thread_member (
			thread_id BIGINT UNSIGNED NOT NULL,
			uid VARCHAR(40) NOT NULL,
			role TINYINT NOT NULL DEFAULT 0,
			version BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS thread_setting (
			group_no VARCHAR(40) NOT NULL,
			short_id VARCHAR(32) NOT NULL,
			uid VARCHAR(40) NOT NULL,
			mute TINYINT NOT NULL DEFAULT 0,
			version BIGINT NOT NULL DEFAULT 0
		)`,
	}
	for _, statement := range statements {
		_, err := ctx.DB().InsertBySql(statement).Exec()
		require.NoError(t, err)
	}
}
