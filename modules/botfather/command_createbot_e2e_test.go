package botfather

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/require"
)

func TestBotFatherNewBotSpaceBindingE2E(t *testing.T) {
	ctx := newCreateBotTestContext(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()

	tests := []struct {
		name       string
		creatorUID string
		spaceID    string
		seed       func(t *testing.T, ctx *config.Context, creatorUID, spaceID string)
		wantBot    bool
	}{
		{
			name:       "forged payload Space is rejected",
			creatorUID: "user_e2e_rejected",
			spaceID:    "space_synthetic_e2e_foreign",
			seed: func(t *testing.T, ctx *config.Context, creatorUID, spaceID string) {
				seedCreateBotSpace(t, ctx, "space_synthetic_e2e_owned", 1)
				seedCreateBotMembership(t, ctx, "space_synthetic_e2e_owned", creatorUID, 1)
				seedCreateBotSpace(t, ctx, spaceID, 1)
			},
			wantBot: false,
		},
		{
			name:       "authorized payload Space creates bound Bot",
			creatorUID: "user_e2e_authorized",
			spaceID:    "space_synthetic_e2e_target",
			seed: func(t *testing.T, ctx *config.Context, creatorUID, spaceID string) {
				seedCreateBotSpace(t, ctx, spaceID, 1)
				seedCreateBotMembership(t, ctx, spaceID, creatorUID, 1)
				seedCreateBotSpace(t, ctx, "space_synthetic_e2e_other", 1)
				seedCreateBotMembership(t, ctx, "space_synthetic_e2e_other", creatorUID, 1)
			},
			wantBot: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, testutil.CleanAllTables(ctx))
			seedCreateBotUser(t, ctx, tt.creatorUID, 1)
			tt.seed(t, ctx, tt.creatorUID, tt.spaceID)

			bf := New(ctx)
			require.NoError(t, bf.cmdHandler.sm.Clear(tt.creatorUID, tt.spaceID))
			bf.messagesListen([]*config.MessageResp{
				newCreateBotMessage(tt.creatorUID, tt.spaceID, CmdNewBot),
			})
			require.Eventually(t, func() bool {
				state, err := bf.cmdHandler.sm.GetState(tt.creatorUID, tt.spaceID)
				return err == nil && state == StateWaitingBotName
			}, 5*time.Second, 20*time.Millisecond)

			bf.messagesListen([]*config.MessageResp{
				newCreateBotMessage(tt.creatorUID, tt.spaceID, "Synthetic E2E Bot"),
			})
			require.Eventually(t, func() bool {
				return len(bf.msgSem) == 0
			}, 10*time.Second, 20*time.Millisecond)

			bots, err := bf.db.queryRobotsByCreatorUID(tt.creatorUID)
			require.NoError(t, err)
			if !tt.wantBot {
				require.Empty(t, bots)
				return
			}

			require.Len(t, bots, 1)
			assertCreateBotMembershipCount(t, ctx, bots[0].RobotID, tt.spaceID, 1)

			cleanupTarget := setSpaceIDFromPayload(tt.creatorUID, tt.spaceID)
			visibleInTarget, queryErr := bf.cmdHandler.queryBotsForUser(tt.creatorUID)
			cleanupTarget()
			require.NoError(t, queryErr)
			require.Len(t, visibleInTarget, 1)

			cleanupOther := setSpaceIDFromPayload(tt.creatorUID, "space_synthetic_e2e_other")
			visibleInOther, queryErr := bf.cmdHandler.queryBotsForUser(tt.creatorUID)
			cleanupOther()
			require.NoError(t, queryErr)
			require.Empty(t, visibleInOther)
		})
	}
}

func newCreateBotMessage(fromUID, spaceID, content string) *config.MessageResp {
	return &config.MessageResp{
		FromUID:     fromUID,
		ChannelID:   common.GetFakeChannelIDWith(fromUID, BotFatherUID),
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload: []byte(util.ToJson(map[string]interface{}{
			"type":     common.Text,
			"content":  content,
			"space_id": spaceID,
		})),
	}
}
