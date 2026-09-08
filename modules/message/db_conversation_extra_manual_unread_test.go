package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueryManualUnread_NoSidebarCandidatesSkipsDatabase(t *testing.T) {
	rows, err := (&conversationExtraDB{}).queryManualUnread("uid", nil, nil)

	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestQueryManualUnread_ScopesToSidebarKeysAndLoadsOnlyIdentity(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	defer func() {
		require.NoError(t, testutil.CleanAllTables(ctx))
	}()

	extraDB := newConversationExtraDB(ctx)
	groupType := common.ChannelTypeGroup.Uint8()
	topicType := common.ChannelTypeCommunityTopic.Uint8()
	personType := common.ChannelTypePerson.Uint8()
	const (
		sharedChannelID = "manual-unread-shared-id"
		topicTargetID   = "manual-unread-group____topic-target"
		unrelatedID     = "manual-unread-unrelated-group"
		otherUID        = "manual-unread-other-user"
	)

	seed := func(uid, channelID string, channelType uint8, version int64) {
		t.Helper()
		changed, err := extraDB.insertOrUpdate(&conversationExtraModel{
			UID:         uid,
			ChannelID:   channelID,
			ChannelType: channelType,
			BrowseTo:    42,
			Draft:       "must-not-be-loaded",
			Version:     version,
		})
		require.NoError(t, err)
		require.True(t, changed)

		changed, err = extraDB.setManualUnread(uid, channelID, channelType, version+1)
		require.NoError(t, err)
		require.True(t, changed)
	}

	seed(testutil.UID, sharedChannelID, groupType, 100)
	seed(testutil.UID, sharedChannelID, topicType, 200)
	seed(testutil.UID, sharedChannelID, personType, 300)
	seed(testutil.UID, topicTargetID, topicType, 400)
	seed(testutil.UID, unrelatedID, groupType, 500)
	seed(otherUID, sharedChannelID, groupType, 600)

	rows, err := extraDB.queryManualUnread(
		testutil.UID,
		[]string{sharedChannelID},
		[]string{topicTargetID},
	)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	byKey := make(map[string]*conversationExtraModel, len(rows))
	for _, row := range rows {
		byKey[channelKey(row.ChannelID, row.ChannelType)] = row
		assert.Zero(t, row.BrowseTo)
		assert.Empty(t, row.Draft)
		assert.Zero(t, row.Version)
		assert.False(t, row.ManualUnread)
	}
	assert.Contains(t, byKey, channelKey(sharedChannelID, groupType))
	assert.Contains(t, byKey, channelKey(topicTargetID, topicType))
	assert.NotContains(t, byKey, channelKey(sharedChannelID, topicType))
	assert.NotContains(t, byKey, channelKey(sharedChannelID, personType))
	assert.NotContains(t, byKey, channelKey(unrelatedID, groupType))
}
