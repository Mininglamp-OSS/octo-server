package robot

// task bot-setting-store —— 三层解析、总闸支配、写入校验的纯函数单测。
// 这些用例刻意不碰 DB：解析优先级与来源标注是本任务的 load-bearing 语义，
// 值得能在没有 MySQL 的环境里跑。端点级行为由集成测试覆盖。

import (
	"os"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
)

func boolPtr(v bool) *bool { return &v }

// TestResolveBotSettingBool_PriorityAndSource 钉住三层解析的优先级**和来源标注**。
//
// 来源标注和有效值一样重要：owner UI 靠 source 区分「我显式设成了 false」与
// 「我没设、上层默认就是 false」，两者有效值相同但「恢复默认」的行为完全不同。
// 只断言 effective 的测试会漏掉把 source 标错层的 bug。
func TestResolveBotSettingBool_PriorityAndSource(t *testing.T) {
	def := botSettingDef{
		Key: "bot.test_switch", Type: botSettingTypeBool, Editable: true, CodeDefault: true,
	}

	cases := []struct {
		name       string
		override   *bool
		globalVal  bool
		globalOK   bool
		wantEff    bool
		wantSource string
		wantValue  *bool
	}{
		{
			name: "bot override wins over global", override: boolPtr(false),
			globalVal: true, globalOK: true,
			wantEff: false, wantSource: botSettingSourceBot, wantValue: boolPtr(false),
		},
		{
			name: "bot override wins even when it agrees with global", override: boolPtr(true),
			globalVal: true, globalOK: true,
			wantEff: true, wantSource: botSettingSourceBot, wantValue: boolPtr(true),
		},
		{
			name:      "global default applies when the bot has no override",
			globalVal: false, globalOK: true,
			wantEff: false, wantSource: botSettingSourceGlobal, wantValue: nil,
		},
		{
			name:    "code default applies when neither layer has an opinion",
			wantEff: true, wantSource: botSettingSourceDefault, wantValue: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveBotSettingBool(def, tc.override, tc.globalVal, tc.globalOK)
			if got.Eff != tc.wantEff {
				t.Errorf("effective = %v, want %v", got.Eff, tc.wantEff)
			}
			if got.Source != tc.wantSource {
				t.Errorf("source = %q, want %q", got.Source, tc.wantSource)
			}
			switch {
			case tc.wantValue == nil && got.Override != nil:
				t.Errorf("value = %v, want null (no override)", *got.Override)
			case tc.wantValue != nil && got.Override == nil:
				t.Errorf("value = null, want %v", *tc.wantValue)
			case tc.wantValue != nil && *got.Override != *tc.wantValue:
				t.Errorf("value = %v, want %v", *got.Override, *tc.wantValue)
			}
		})
	}
}

// TestResolveBotSettingBool_DerivedKeyIgnoresStoredLayers pins that a derived
// key answers from the environment even if a row somehow exists for it. A
// stored value that could outvote the env is exactly the "DB says cards are on
// while the env has them off" state the read-only design exists to prevent.
func TestResolveBotSettingBool_DerivedKeyIgnoresStoredLayers(t *testing.T) {
	def := botSettingDef{
		Key: BotSettingKeyCardEnabled, Type: botSettingTypeBool, Editable: false,
		CodeDefault: true,
		Derived:     func() bool { return false },
	}
	got := resolveBotSettingBool(def, boolPtr(true), true, true)
	if got.Eff {
		t.Error("derived key was overridden by a stored value")
	}
	if got.Source != botSettingSourceEnv {
		t.Errorf("source = %q, want %q", got.Source, botSettingSourceEnv)
	}
	if got.Override != nil {
		t.Error("derived key reported a stored override; it has no editable layer")
	}
}

// TestApplyCardMasterSwitch_DominatesSubSwitches pins the invariant the manifest
// depends on: with the deployment switch off, no sub-capability may report true.
// Advertising one would break the "manifest agrees with the send gate" contract
// pkg/cardmsg established — the send path refuses everything when BotEnabled()
// is false.
func TestApplyCardMasterSwitch_DominatesSubSwitches(t *testing.T) {
	off := applyCardMasterSwitch(BotCardConfig{
		CardEnabled: false, DisplayEnabled: true,
		InteractionEnabled: true, ReasoningEnabled: true,
	})
	if off.DisplayEnabled || off.InteractionEnabled || off.ReasoningEnabled || off.CardEnabled {
		t.Fatalf("master switch off must zero every sub-switch, got %#v", off)
	}

	on := BotCardConfig{
		CardEnabled: true, DisplayEnabled: true,
		InteractionEnabled: false, ReasoningEnabled: true,
	}
	if got := applyCardMasterSwitch(on); got != on {
		t.Fatalf("master switch on must pass sub-switches through, got %#v want %#v", got, on)
	}
}

// TestBotCardConfigFrom_ProjectsRegisteredKeys covers the projection from the
// generic resolution list to the typed card config, including the master-switch
// application that rides along with it.
func TestBotCardConfigFrom_ProjectsRegisteredKeys(t *testing.T) {
	resolutions := []botSettingResolution{
		{Key: BotSettingKeyCardEnabled, Eff: true},
		{Key: BotSettingKeyDisplayEnabled, Eff: false},
		{Key: BotSettingKeyInteractionEnabled, Eff: true},
		{Key: BotSettingKeyReasoningEnabled, Eff: true},
		{Key: "bot.unrelated_future_key", Eff: true},
	}
	got := botCardConfigFrom(resolutions)
	want := BotCardConfig{
		CardEnabled: true, DisplayEnabled: false,
		InteractionEnabled: true, ReasoningEnabled: true,
	}
	if got != want {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

// TestFindBotSettingDef_RegistryShape guards the two properties the write path
// relies on: an unregistered key is unknown (no wild keys), and card_enabled is
// not editable (it is derived from the environment).
func TestFindBotSettingDef_RegistryShape(t *testing.T) {
	if findBotSettingDef("bot.not_a_real_key") != nil {
		t.Error("an unregistered key resolved to a definition")
	}
	cardEnabled := findBotSettingDef(BotSettingKeyCardEnabled)
	if cardEnabled == nil {
		t.Fatal("bot.card_enabled is not registered")
	}
	if cardEnabled.Editable {
		t.Error("bot.card_enabled must not be editable; it is derived from the deployment env")
	}
	for _, key := range []string{
		BotSettingKeyDisplayEnabled, BotSettingKeyInteractionEnabled, BotSettingKeyReasoningEnabled,
	} {
		def := findBotSettingDef(key)
		if def == nil {
			t.Fatalf("%s is not registered", key)
		}
		if !def.Editable {
			t.Errorf("%s must be owner-editable", key)
		}
		if !def.CodeDefault {
			t.Errorf("%s code default = false, want true (the master switch carries the safe default)", key)
		}
	}
}

// TestBotSettingBoolLiterals covers both directions of the bool encoding.
//
// The read side deliberately treats garbage as "not configured" rather than
// false: a hand-edited row must fall through to the next layer, not silently
// switch a capability off.
func TestBotSettingBoolLiterals(t *testing.T) {
	for _, raw := range []string{"1", "true", "TRUE", "True"} {
		if got, ok := normalizeBotSettingBool(raw); !ok || got != botSettingTrue {
			t.Errorf("normalize(%q) = %q,%v want %q,true", raw, got, ok, botSettingTrue)
		}
	}
	for _, raw := range []string{"0", "false", "FALSE", "False", " false "} {
		if got, ok := normalizeBotSettingBool(raw); !ok || got != botSettingFalse {
			t.Errorf("normalize(%q) = %q,%v want %q,true", raw, got, ok, botSettingFalse)
		}
	}
	for _, raw := range []string{"", "yes", "on", "2", "null"} {
		if _, ok := normalizeBotSettingBool(raw); ok {
			t.Errorf("normalize(%q) accepted an illegal literal", raw)
		}
	}
	if _, ok := parseBotSettingBool("garbage"); ok {
		t.Error("parse accepted a garbage stored value; it must read as not-configured")
	}
}

// TestBotSettingCardEnabledTracksDeploymentSwitch ties the registered derived
// key to the real deployment gate rather than a copy of its logic — the whole
// point of making it derived.
func TestBotSettingCardEnabledTracksDeploymentSwitch(t *testing.T) {
	def := findBotSettingDef(BotSettingKeyCardEnabled)
	if def == nil || def.Derived == nil {
		t.Fatal("bot.card_enabled must be a derived key")
	}

	t.Setenv(cardmsg.EnvEnabled, "true")
	t.Setenv(cardmsg.EnvBotEnabled, "true")
	if !def.Derived() {
		t.Error("card_enabled = false with both deployment switches on")
	}

	t.Setenv(cardmsg.EnvBotEnabled, "false")
	if def.Derived() {
		t.Error("card_enabled = true while the bot sub-switch is off")
	}

	os.Unsetenv(cardmsg.EnvEnabled)
	os.Unsetenv(cardmsg.EnvBotEnabled)
	if def.Derived() {
		t.Error("card_enabled = true with no deployment switch set; it must fail closed")
	}
}

// TestBuildBotSettingChangedEventData pins that the invalidation event carries
// no values. If it ever did, an adapter could apply a stale shape from the event
// on top of the authoritative profile response.
func TestBuildBotSettingChangedEventData(t *testing.T) {
	data := buildBotSettingChangedEventData()
	if data["scope"] != "bot_setting" {
		t.Errorf("scope = %v, want bot_setting", data["scope"])
	}
	for _, key := range []string{
		BotSettingKeyCardEnabled, BotSettingKeyDisplayEnabled,
		BotSettingKeyInteractionEnabled, BotSettingKeyReasoningEnabled,
		"value", "enabled",
	} {
		if _, present := data[key]; present {
			t.Errorf("event data carries %q; it must be a signal to re-fetch, not a value channel", key)
		}
	}
}
