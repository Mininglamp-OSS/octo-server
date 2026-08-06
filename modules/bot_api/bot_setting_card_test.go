package bot_api

// task bot-setting-store —— /v1/bot/card/profile 的 config 对象与 sendMessage
// 的 per-Bot 策略门。这里只覆盖不需要 DB 的部分（清单渲染的两条不变量），
// 端到端行为在 card_profile_test.go / card_template_send_test.go 里。

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/gin-gonic/gin"
)

// stubCardConfig is the fake botCardConfigResolver focused tests inject in
// place of the real robot service.
type stubCardConfig struct {
	config robot.BotCardConfig
	err    error
}

func (s stubCardConfig) BotCardConfig(string) (robot.BotCardConfig, error) {
	return s.config, s.err
}

// allCardCapabilitiesOn is the resolver a focused send test wants when the
// per-Bot policy is not what it is exercising.
func allCardCapabilitiesOn() stubCardConfig {
	return stubCardConfig{config: robot.BotCardConfig{
		CardEnabled: true, DisplayEnabled: true,
		InteractionEnabled: true, ReasoningEnabled: true,
	}}
}

// TestBotCardConfigResponse_ReasoningRefMatchesAdvertisedCatalog pins the two
// invariants the manifest owes a producer: a ref never accompanies a false
// switch, and a ref that IS present is one the send path will accept.
//
// The second half is the one worth a test: the profile response and the send
// allowlist are built in different functions, and a hand-written version string
// in the manifest would drift silently the first time the advertised template
// version moves.
func TestBotCardConfigResponse_ReasoningRefMatchesAdvertisedCatalog(t *testing.T) {
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	ba := &BotAPI{Log: log.NewTLog("BotAPI-card-config"), cardTemplates: catalog}

	t.Run("enabled advertises a ref the send path accepts", func(t *testing.T) {
		out := ba.botCardConfigResponse(robot.BotCardConfig{
			CardEnabled: true, DisplayEnabled: true,
			InteractionEnabled: true, ReasoningEnabled: true,
		})
		if out["reasoning_enabled"] != true {
			t.Fatalf("reasoning_enabled = %v, want true", out["reasoning_enabled"])
		}
		ref, ok := out["reasoning_template_ref"].(map[string]interface{})
		if !ok {
			t.Fatalf("reasoning_template_ref = %#v, want an object", out["reasoning_template_ref"])
		}
		// The advertised ref must be present in templating.templates of the very
		// same response — that is what makes "manifest says on" imply "send works".
		advertised := false
		for _, template := range catalog.Capability().Templates {
			if template.ID == ref["id"] && template.Version == ref["version"] {
				advertised = true
			}
		}
		if !advertised {
			t.Fatalf("ref %#v is not in the advertised catalog", ref)
		}
		// And it must survive the send-path allowlist check verbatim.
		if _, err := catalog.requireRef(
			map[string]any{"id": ref["id"], "version": ref["version"]},
			catalog.sendAllowed, "not Bot-callable for new send",
		); err != nil {
			t.Fatalf("advertised ref rejected by send allowlist: %v", err)
		}
	})

	t.Run("disabled carries no ref", func(t *testing.T) {
		out := ba.botCardConfigResponse(robot.BotCardConfig{
			CardEnabled: true, DisplayEnabled: true,
			InteractionEnabled: true, ReasoningEnabled: false,
		})
		if out["reasoning_enabled"] != false {
			t.Fatalf("reasoning_enabled = %v, want false", out["reasoning_enabled"])
		}
		if out["reasoning_template_ref"] != nil {
			t.Fatalf("reasoning_template_ref = %#v, want nil", out["reasoning_template_ref"])
		}
	})
}

// TestBotCardConfigResponse_NoCatalogForcesReasoningOff covers the deployment
// whose catalog does not advertise the reasoning template: the switch alone is
// not enough, so the manifest must report the capability as off rather than
// advertise a template nothing can render.
func TestBotCardConfigResponse_NoCatalogForcesReasoningOff(t *testing.T) {
	ba := &BotAPI{Log: log.NewTLog("BotAPI-card-config-nocatalog")} // cardTemplates nil

	out := ba.botCardConfigResponse(robot.BotCardConfig{
		CardEnabled: true, DisplayEnabled: true,
		InteractionEnabled: true, ReasoningEnabled: true,
	})
	if out["reasoning_enabled"] != false {
		t.Fatalf("reasoning_enabled = %v, want false when nothing is advertised", out["reasoning_enabled"])
	}
	if out["reasoning_template_ref"] != nil {
		t.Fatalf("reasoning_template_ref = %#v, want nil", out["reasoning_template_ref"])
	}
}

// TestAdvertisedRef_UnknownTemplateIsNotAdvertised guards the lookup itself:
// asking for a template this deployment does not advertise must report "no",
// never fall back to some other entry.
func TestAdvertisedRef_UnknownTemplateIsNotAdvertised(t *testing.T) {
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := catalog.AdvertisedRef("docs.access-request"); ok {
		t.Fatal("AdvertisedRef returned a ref for a template that is not advertised to bots")
	}
	if ref, ok := catalog.AdvertisedRef(aireasoningprocess.TemplateID); !ok ||
		ref.Version != aireasoningprocess.TemplateVersionV3 {
		t.Fatalf("AdvertisedRef(%s) = %#v ok=%v", aireasoningprocess.TemplateID, ref, ok)
	}
}

// TestResolveBotCardConfig_FailsClosedWithoutResolver pins the wiring-error
// posture: no resolver must not read as "every capability is on".
func TestResolveBotCardConfig_FailsClosedWithoutResolver(t *testing.T) {
	ba := &BotAPI{Log: log.NewTLog("BotAPI-card-config-unwired")}
	if _, err := ba.resolveBotCardConfig("bot-1"); err == nil {
		t.Fatal("resolveBotCardConfig succeeded without a resolver; must fail closed")
	}
}

// TestBotCardConfigResponse_MasterSwitchOffZeroesEverything pins the manifest
// side of the master-switch invariant: with the deployment gate off, no
// sub-capability may be advertised and no ref may be handed out. Advertising
// either would point a producer at a send that BotEnabled() refuses outright.
func TestBotCardConfigResponse_MasterSwitchOffZeroesEverything(t *testing.T) {
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	ba := &BotAPI{Log: log.NewTLog("BotAPI-card-config-master-off"), cardTemplates: catalog}

	// This is what robot.BotCardConfig returns once applyCardMasterSwitch has
	// run with the deployment gate off.
	out := ba.botCardConfigResponse(robot.BotCardConfig{})
	for _, key := range []string{
		"card_enabled", "display_enabled", "interaction_enabled", "reasoning_enabled",
	} {
		if out[key] != false {
			t.Errorf("%s = %v, want false when the master switch is off", key, out[key])
		}
	}
	if out["reasoning_template_ref"] != nil {
		t.Errorf("reasoning_template_ref = %#v, want nil", out["reasoning_template_ref"])
	}
}

// TestSendMessage_RejectedByBotCardPolicy covers the enforcement half: the
// manifest is advisory (a producer can hold a stale copy or ignore it), so each
// switch has to be refused where the message is actually accepted.
//
// Every case must come back as the same generic card_invalid — a distinct code
// per reason would let a caller probe another bot's configuration by diffing
// error codes.
func TestSendMessage_RejectedByBotCardPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// Open the deployment master switch, otherwise the pre-existing
	// BotEnabled() gate refuses everything with card_disabled and these cases
	// would pass on the right status for the wrong reason.
	t.Setenv(cardmsg.EnvEnabled, "true")

	rawCard := func(profile string) map[string]any {
		body := []any{map[string]any{"type": "TextBlock", "text": "policy gate"}}
		card := map[string]any{"type": "AdaptiveCard", "version": cardmsg.CardVersion, "body": body}
		if profile == cardmsg.ProfileV2 {
			// octo/v2 needs an interactive affordance to be a meaningful v2 card.
			card["actions"] = []any{map[string]any{
				"type": "Action.Submit", "id": "ack", "title": "ack",
			}}
		}
		return map[string]any{
			"channel_id":   "user_creator",
			"channel_type": common.ChannelTypePerson.Uint8(),
			"payload": map[string]any{
				"type": cardmsg.InteractiveCard.Int(), "profile": profile,
				"card_version": cardmsg.CardVersion, "card": card,
			},
		}
	}

	cases := []struct {
		name   string
		config robot.BotCardConfig
		body   func(t *testing.T) map[string]any
	}{
		{
			name: "display off rejects a raw octo/v1 card",
			config: robot.BotCardConfig{
				CardEnabled: true, DisplayEnabled: false,
				InteractionEnabled: true, ReasoningEnabled: true,
			},
			body: func(*testing.T) map[string]any { return rawCard(cardmsg.ProfileV1) },
		},
		{
			name: "interaction off rejects a raw octo/v2 card",
			config: robot.BotCardConfig{
				CardEnabled: true, DisplayEnabled: true,
				InteractionEnabled: false, ReasoningEnabled: true,
			},
			body: func(*testing.T) map[string]any { return rawCard(cardmsg.ProfileV2) },
		},
		{
			name: "reasoning off rejects the template ref",
			config: robot.BotCardConfig{
				CardEnabled: true, DisplayEnabled: true,
				InteractionEnabled: true, ReasoningEnabled: false,
			},
			body: func(t *testing.T) map[string]any {
				return registrySendBody(t, "reasoning", testReasoningData(t, "reasoning"))
			},
		},
		{
			name: "a version this deployment does not advertise is refused even with reasoning on",
			config: robot.BotCardConfig{
				CardEnabled: true, DisplayEnabled: true,
				InteractionEnabled: true, ReasoningEnabled: true,
			},
			body: func(t *testing.T) map[string]any {
				// V1 stays edit-compatible but is not advertised for new sends.
				return registrySendBodyVersion(t, aireasoningprocess.TemplateVersionV1,
					"reasoning", testReasoningData(t, "reasoning"))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
			if err != nil {
				t.Fatal(err)
			}
			dispatch := &dispatchCapture{}
			ba := &BotAPI{
				Log:              log.NewTLog("BotAPI-card-policy-gate"),
				cardTemplates:    catalog,
				cardConfig:       stubCardConfig{config: tc.config},
				spaceQuerier:     &fakeSpaceQuerier{defaultSpace: "space-authoritative"},
				dispatchOverride: dispatch.hook,
			}

			recorder := invokeTemplateSend(t, ba, tc.body(t), "")
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400; body=%s", recorder.Code, recorder.Body.String())
			}
			// Compared against the code's own DefaultMessage rather than a
			// literal, so this asserts *which gate* refused without pinning
			// copy. Distinguishing card_invalid from card_disabled matters:
			// the latter would mean the deployment switch fired and the
			// per-bot gate was never reached.
			if msg := errorMessageOf(t, recorder.Body.Bytes()); msg != errcode.ErrBotAPICardInvalid.DefaultMessage {
				t.Fatalf("msg=%q, want the generic card_invalid (anti-enumeration)", msg)
			}
			dispatch.mu.Lock()
			captured := dispatch.captured
			dispatch.mu.Unlock()
			if captured != nil {
				t.Fatal("a refused card must not reach dispatch")
			}
		})
	}
}

// TestSendMessage_ConfigReadFailureFailsClosed pins that an unreadable config
// refuses the send rather than falling back to permissive defaults, and that it
// is reported as an internal error rather than as a caller mistake.
func TestSendMessage_ConfigReadFailureFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(cardmsg.EnvEnabled, "true")

	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	dispatch := &dispatchCapture{}
	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-card-policy-unreadable"),
		cardTemplates:    catalog,
		cardConfig:       stubCardConfig{err: errors.New("db is down")},
		spaceQuerier:     &fakeSpaceQuerier{defaultSpace: "space-authoritative"},
		dispatchOverride: dispatch.hook,
	}

	body := registrySendBody(t, "reasoning", testReasoningData(t, "reasoning"))
	recorder := invokeTemplateSend(t, ba, body, "")
	if recorder.Code == http.StatusOK {
		t.Fatal("send succeeded while the config was unreadable; must fail closed")
	}
	if msg := errorMessageOf(t, recorder.Body.Bytes()); msg != errcode.ErrSharedInternal.DefaultMessage {
		t.Fatalf("msg=%q, want the internal-error message", msg)
	}
	dispatch.mu.Lock()
	captured := dispatch.captured
	dispatch.mu.Unlock()
	if captured != nil {
		t.Fatal("dispatch happened despite an unreadable config")
	}
}

// TestBotMessageEdit_StillReachesTerminalStateWhenReasoningOff is the other
// half of the switch's contract, and the reason the gate lives in sendMessage
// only.
//
// Turning reasoning off must stop *new* cards, not freeze the ones already on
// screen: a card whose progress frames are in flight would otherwise be stranded
// showing "thinking" forever, with no way for the producer to move it to
// completed. So the edit path is deliberately ungated, and this test would fail
// the moment someone "fixes" that asymmetry.
func TestBotMessageEdit_StillReachesTerminalStateWhenReasoningOff(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(cardmsg.EnvEnabled, "true")

	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	mutator := &fakeBotCardMutator{
		snapshot:     carddispatch.CardMutationSnapshot{Envelope: initialRegistryEnvelope(t, catalog), CardSeq: 1},
		mutateResult: carddispatch.CardMutationResult{Applied: true},
	}
	ba := &BotAPI{
		Log:           log.NewTLog("BotAPI-edit-reasoning-off"),
		cardTemplates: catalog,
		cardMutator:   mutator,
		// Every card capability is off for this bot — including the one that
		// governs creating the card being edited.
		cardConfig: stubCardConfig{config: robot.BotCardConfig{CardEnabled: true}},
	}

	body := registryEditBody(t, "completed", testReasoningData(t, "completed"), 2, true)
	recorder := invokeTemplateEdit(t, ba, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 — an in-flight card must still reach its terminal state; body=%s",
			recorder.Code, recorder.Body.String())
	}
	if len(mutator.mutateRequests) != 1 {
		t.Fatalf("mutate calls = %d, want 1", len(mutator.mutateRequests))
	}
}

// errorMessageOf pulls the message out of an error response.
//
// These focused tests drive the handler with a synthetic gin context, so no
// wkhttp router — and therefore no i18n ErrorRenderer — is attached and the
// body carries only the legacy {msg,status} shape. `msg` still falls back to
// the code's DefaultMessage, which is enough to tell the gates apart; the full
// error.code envelope is covered by the server-backed tests.
//
// The wire status is pinned to 400 by ResponseErrorL, so status alone cannot
// distinguish a caller mistake from an internal failure — hence asserting on
// the message.
func errorMessageOf(t *testing.T, body []byte) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode error envelope: %v (body=%s)", err, body)
	}
	if envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	return envelope.Msg
}
