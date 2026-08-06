package bot_api

// task bot-setting-store —— /v1/bot/card/profile 的 config 对象与 sendMessage
// 的 per-Bot 策略门。这里只覆盖不需要 DB 的部分（清单渲染的两条不变量），
// 端到端行为在 card_profile_test.go / card_template_send_test.go 里。

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
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
