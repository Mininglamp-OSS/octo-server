package robot

// task bot-setting-store round-2 P1-1 —— legacy robot ingress 的 per-Bot 卡片门。
//
// 这条 ingress 与 bot_api 的 send gate 共用同一个 Bot 身份（authRobot 按
// robot.robot_id 解析，bot_setting.robot_id 是同一列），payloadIsVail 的注释也
// 明确写着「三个卡片生产者入口对称」。bot_setting 首版只给 bot_api 那道门加了
// per-Bot 判定，于是 owner 关掉能力后换这条路仍能发卡。

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
)

func cardPayloadWithProfile(profile string) map[string]interface{} {
	return map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"profile":      profile,
		"card_version": cardmsg.CardVersion,
		"card": map[string]interface{}{
			"type": "AdaptiveCard", "version": cardmsg.CardVersion,
			"body": []interface{}{
				map[string]interface{}{"type": "TextBlock", "text": "legacy ingress"},
			},
		},
	}
}

// TestRejectRobotCardBySetting_OnlyCardsConsultTheConfig keeps the cost claim
// honest: an ordinary robot message must not trigger a config read.
func TestRejectRobotCardBySetting_OnlyCardsConsultTheConfig(t *testing.T) {
	if rejectRobotCardBySetting(map[string]interface{}{"type": 1, "content": "hi"}) {
		t.Error("a plain text payload asked for a card policy decision")
	}
	if !rejectRobotCardBySetting(cardPayloadWithProfile(cardmsg.ProfileV1)) {
		t.Error("a type-17 payload did not ask for a card policy decision")
	}
}

// TestRobotCardPolicyAllows_MatchesBotAPIGate pins that the legacy ingress
// reaches the same verdict as the bot_api gate for the same card and the same
// config — the symmetry payloadIsVail's own comment requires. Both call
// AllowsRawDisplayCard / AllowsRawInteractiveCard, so a divergence here would
// mean somebody re-derived the booleans locally.
func TestRobotCardPolicyAllows_MatchesBotAPIGate(t *testing.T) {
	cases := []struct {
		name    string
		profile string
		cfg     BotCardConfig
		want    bool
	}{
		{
			name: "display off refuses octo/v1", profile: cardmsg.ProfileV1,
			cfg:  BotCardConfig{CardEnabled: true, InteractionEnabled: true},
			want: false,
		},
		{
			name: "display off refuses octo/v2 too (display is a floor)", profile: cardmsg.ProfileV2,
			cfg:  BotCardConfig{CardEnabled: true, InteractionEnabled: true},
			want: false,
		},
		{
			name: "interaction off refuses octo/v2", profile: cardmsg.ProfileV2,
			cfg:  BotCardConfig{CardEnabled: true, DisplayEnabled: true},
			want: false,
		},
		{
			name: "interaction off still allows octo/v1", profile: cardmsg.ProfileV1,
			cfg:  BotCardConfig{CardEnabled: true, DisplayEnabled: true},
			want: true,
		},
		{
			name: "everything on allows octo/v2", profile: cardmsg.ProfileV2,
			cfg: BotCardConfig{
				CardEnabled: true, DisplayEnabled: true, InteractionEnabled: true,
			},
			want: true,
		},
		{
			name: "master switch off refuses everything", profile: cardmsg.ProfileV1,
			// What applyCardMasterSwitch produces when the deployment gate is off.
			cfg:  BotCardConfig{},
			want: false,
		},
		{
			name: "an unknown profile fails closed", profile: "octo/v9",
			cfg: BotCardConfig{
				CardEnabled: true, DisplayEnabled: true, InteractionEnabled: true,
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := robotCardPolicyAllows(cardPayloadWithProfile(tc.profile), tc.cfg)
			if got != tc.want {
				t.Fatalf("allow=%v, want %v", got, tc.want)
			}
		})
	}
}
