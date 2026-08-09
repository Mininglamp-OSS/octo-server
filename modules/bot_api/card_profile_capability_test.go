package bot_api

// Review rounds 3 and 4 on #720 — the profile manifest and the send path must
// answer from the same source (D6), and until this file existed nothing pinned
// that they did.
//
// `CapabilityFor` had zero test call sites while being the live source for
// `/v1/bot/card/profile`, and `cf574fa0` then changed its failure contract
// without pinning either the old or the new behaviour.
//
// What is covered here, and nothing else — the previous version of this header
// promised a third property that no test in the file delivered, which is the
// fourth time in this PR family that prose named code that was not there:
//
//  1. Gate off, the manifest is deep-equal to the boot-time `Capability()`.
//     That is the additive-only wire promise `card_profile.go`'s header makes to
//     SDKs, and wiring a per-request resolver behind it is the change most
//     likely to break it silently.
//  2. Gate on, one unreadable template drops out and the rest of the manifest
//     survives — the property `cf574fa0` exists to establish.
//  3. Gate on, every ref the advertised set offers is one the send prefilter
//     accepts. This is P1-A: the prefilter used to compare against the *static*
//     `AdvertisedRef`, so a dynamically shadowed version was advertised by the
//     profile and refused by the send path in the same breath. Restoring that
//     comparison turns `TestEveryAdvertisedRefIsOneThePrefilterAccepts` red.
//  4. Handler level, `reasoning_template_ref` is read out of the resolved
//     manifest rather than the static policy — the invariant
//     `botCardConfigResponse`'s doc comment claims and `6637b8a4` rewired.
//  5. Handler level, an unreadable runtime answers D6's typed unavailable code
//     rather than a generic internal error, so a producer backing off at
//     startup can tell "retry shortly" from "this deployment is broken".
//
// Still NOT covered, deliberately, and it is the open half of P1-1: a Bot
// granted send on a dynamic template whose ID is not `ai.reasoning-process` is
// advertised by `advertisedSendRefs` and refused by `rejectByBotCardPolicy`'s
// `!mapped` branch. Adding such a grant to property 3's fixture below turns it
// red — correctly, because the divergence is real. Which side gives way is a
// contract decision (does a Bot grant reach IDs beyond the static policy in
// this milestone?), so the test lands with the fix rather than ahead of it.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/gin-gonic/gin"
)

// stubBotCardConfig is the per-Bot switch resolver these tests need. It never
// errors: the fail-closed path already has its own coverage
// (TestResolveBotCardConfig_FailsClosedWithoutResolver), and what is under test
// here is the agreement between two paths that both read a *successful* config.
type stubBotCardConfig struct{ cfg robot.BotCardConfig }

func (s stubBotCardConfig) BotCardConfig(string) (robot.BotCardConfig, error) {
	return s.cfg, nil
}

func allBotCardSwitchesOn() robot.BotCardConfig {
	return robot.BotCardConfig{
		CardEnabled: true, DisplayEnabled: true,
		InteractionEnabled: true, ReasoningEnabled: true,
	}
}

func TestCapabilityForIsTheStaticManifestWhileTheGateIsOff(t *testing.T) {
	ba := newTestBotAPIWithCatalog(t, &fakeSpaceQuerier{
		multiRows: map[string][]string{"bot-42": {"space-bot"}},
	}, &stubAuthorizationSource{newSend: false})

	got, err := ba.cardTemplates.CapabilityFor(context.Background(),
		botCatalogPrincipal{BotID: "bot-42"})
	if err != nil {
		t.Fatalf("CapabilityFor with the gate off: %v", err)
	}
	want := ba.cardTemplates.Capability()

	// Whole-object equality, not a field list. This endpoint's contract is
	// additive-only and an SDK reads Views/States/WireProfile/SubmitActions out
	// of it, so a resolver that got ID and Version right and dropped a view
	// would still be a wire break — and a field-by-field comparison would have
	// to be extended by hand every time the capability struct grows, which is
	// precisely when it would be forgotten (review P2-2, yujiawei).
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gate-off manifest = %+v, want the boot-time manifest %+v", got, want)
	}
}

func TestCapabilityForDropsAnUnreadableTemplateInsteadOfFailingTheManifest(t *testing.T) {
	// A granted dynamic template the catalog cannot resolve — the shape a bundle
	// that decodes but will not compile, or one whose compile times out under
	// load, presents to CapabilityFor. advertisedSendRefs returns it (proven by
	// TestBotTemplateManifestIncludesGrantedDynamicTemplates), so it reaches the
	// per-ref MetaExact that used to abort the whole manifest.
	//
	// Before cf574fa0 this returned errBotTemplateRuntimeUnavailable, which
	// card_profile.go turns into a 500 — one bad artifact among a Bot's grants
	// taking feature detection down for that Bot entirely, on the endpoint whose
	// job is to answer when things are partly unavailable.
	const unresolvable = cardtmpl.ID("pilot.not-in-this-catalog")
	ba := newTestBotAPIWithCatalog(t, &fakeSpaceQuerier{
		multiRows: map[string][]string{"bot-42": {"space-bot"}},
	}, &stubAuthorizationSource{
		newSend: true,
		granted: []cardtmpl.RuntimeAdvertisedTemplate{
			{ID: unresolvable, Authorization: cardtmpl.RuntimeAuthorization{
				Version: "0.4.0-pilot.20260805",
				Artifact: cardtmpl.RuntimeArtifactMeta{
					ID: unresolvable, Version: "0.4.0-pilot.20260805",
					Source: cardtmpl.RuntimeSourceDynamic, Owner: "docs",
				},
				Grant: sendGrant(),
			}},
		},
	})
	principal := botCatalogPrincipal{BotID: "bot-42", SpaceID: "space-bot", Space: botSpaceScoped}

	// The precondition that makes this test meaningful: the unresolvable ref is
	// genuinely in the advertised set, so the manifest really does have to
	// handle it rather than never seeing it.
	refs, err := ba.cardTemplates.advertisedSendRefs(context.Background(), principal)
	if err != nil {
		t.Fatalf("advertisedSendRefs: %v", err)
	}
	var offered bool
	for _, ref := range refs {
		if ref.ID == unresolvable {
			offered = true
		}
	}
	if !offered {
		t.Fatalf("fixture precondition broken: the unresolvable template is not in the advertised set %+v", refs)
	}

	got, err := ba.cardTemplates.CapabilityFor(context.Background(), principal)
	if err != nil {
		t.Fatalf("one unreadable template failed the whole manifest: %v", err)
	}
	for _, template := range got.Templates {
		if cardtmpl.ID(template.ID) == unresolvable {
			t.Fatalf("the unreadable template %s was advertised anyway", template.ID)
		}
	}
	// And the rest of the manifest survived, which is the half that makes this
	// a drop rather than a failure.
	if len(got.Templates) == 0 {
		t.Fatal("the manifest was blanked by a single unreadable template")
	}
}

// TestEveryAdvertisedRefIsOneThePrefilterAccepts is property 3, driven through
// the prefilter rather than the resolver.
//
// `6286c233` deleted the static `AdvertisedRef` comparison from
// `rejectByBotCardPolicy` and shipped with no test that fails when it is put
// back — the review's words, and correct. The whole point of that deletion is
// that a *dynamically shadowed* version is sendable, and the deleted comparison
// refused exactly those, so a fixture shadowing ai.reasoning-process at 0.3.0
// is the mutation that proves it: restore the comparison and 0.3.0 != the
// static 0.4.0, so the prefilter rejects and this test goes red.
//
// It is written as a loop over the advertised set rather than one hard-coded
// ref so that it keeps meaning what its name says as the set grows.
func TestEveryAdvertisedRefIsOneThePrefilterAccepts(t *testing.T) {
	const shadowed = aireasoningprocess.TemplateVersionV3
	ba := newTestBotAPIWithCatalog(t, &fakeSpaceQuerier{
		multiRows: map[string][]string{"bot-42": {"space-bot"}},
	}, &stubAuthorizationSource{
		newSend: true,
		auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
			aireasoningprocess.TemplateID: dynamicAuthorization(shadowed, false, sendGrant()),
		},
	})
	ba.cardConfig = stubBotCardConfig{cfg: allBotCardSwitchesOn()}
	principal := botCatalogPrincipal{BotID: "bot-42", SpaceID: "space-bot", Space: botSpaceScoped}

	refs, err := ba.cardTemplates.advertisedSendRefs(context.Background(), principal)
	if err != nil {
		t.Fatalf("advertisedSendRefs: %v", err)
	}
	// Precondition: the set actually contains the shadowed version. Without
	// this the loop below would pass over the static ref alone, which the
	// deleted comparison also accepted — a green test proving nothing.
	var shadowOffered bool
	for _, ref := range refs {
		if ref.ID == aireasoningprocess.TemplateID && ref.Version == shadowed {
			shadowOffered = true
		}
	}
	if !shadowOffered {
		t.Fatalf("fixture precondition broken: %s@%s is not advertised, so the deleted "+
			"AdvertisedRef comparison would not have refused anything here; refs=%+v",
			aireasoningprocess.TemplateID, shadowed, refs)
	}

	for _, ref := range refs {
		recorder := httptest.NewRecorder()
		ginContext, _ := gin.CreateTestContext(recorder)
		ginContext.Request = httptest.NewRequest(http.MethodPost, "/v1/bot/sendMessage", nil)
		ginContext.Set(CtxKeyRobotID, "bot-42")
		c := &wkhttp.Context{Context: ginContext}

		payload := map[string]interface{}{
			"template_ref": map[string]interface{}{
				"id": string(ref.ID), "version": ref.Version,
			},
		}
		if ba.rejectByBotCardPolicy(c, payload, true) {
			t.Fatalf("the profile advertises %s@%s and the send prefilter refuses it "+
				"(HTTP %d, %s) — that is the divergence /v1/bot/card/profile exists to prevent",
				ref.ID, ref.Version, recorder.Code, recorder.Body.String())
		}
	}
}

// TestBotCardProfileReasoningRefTracksTheResolvedManifest is the handler-level
// half: everything else in this file asserts below `botCardProfile`.
//
// `botCardConfigResponse` documents the invariant "reasoning_enabled=true ⟹ the
// ref is one this deployment also advertises under templating.templates", and
// `6637b8a4` made that true by construction by reading the ref out of the same
// resolved manifest the response carries. Nothing pinned it. Swapping
// `advertisedRefIn(templating, …)` back to the static `AdvertisedRef` — the
// shape this used to have — leaves the gate-off case passing and turns the
// gate-on case red, which is the mutation worth catching.
func TestBotCardProfileReasoningRefTracksTheResolvedManifest(t *testing.T) {
	for _, test := range []struct {
		name        string
		source      *stubAuthorizationSource
		wantVersion string
	}{
		{
			name:        "gate off answers from the static policy",
			source:      &stubAuthorizationSource{newSend: false},
			wantVersion: testReasoningVersionCurrent,
		},
		{
			// A granted dynamic activation shadowing the static default. The
			// static AdvertisedRef would still name 0.4.0 here while
			// templating.templates lists 0.3.0 — one response, two versions, and
			// the producer that trusts the ref (which is what the field is for)
			// sends the one the gate now refuses.
			name: "gate on follows the dynamic shadow",
			source: &stubAuthorizationSource{
				newSend: true,
				auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
					aireasoningprocess.TemplateID: dynamicAuthorization(
						aireasoningprocess.TemplateVersionV3, false, sendGrant()),
				},
			},
			wantVersion: aireasoningprocess.TemplateVersionV3,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ba := newTestBotAPIWithCatalog(t, &fakeSpaceQuerier{
				multiRows: map[string][]string{"bot-42": {"space-bot"}},
			}, test.source)
			ba.cardConfig = stubBotCardConfig{cfg: allBotCardSwitchesOn()}

			recorder := httptest.NewRecorder()
			ginContext, _ := gin.CreateTestContext(recorder)
			ginContext.Request = httptest.NewRequest(http.MethodGet, "/v1/bot/card/profile", nil)
			ginContext.Set(CtxKeyRobotID, "bot-42")
			ba.botCardProfile(&wkhttp.Context{Context: ginContext})

			if recorder.Code != http.StatusOK {
				t.Fatalf("profile = HTTP %d, %s", recorder.Code, recorder.Body.String())
			}
			var body struct {
				Config struct {
					ReasoningEnabled bool `json:"reasoning_enabled"`
					ReasoningRef     *struct {
						ID      string `json:"id"`
						Version string `json:"version"`
					} `json:"reasoning_template_ref"`
				} `json:"config"`
				Templating botTemplatingCapability `json:"templating"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode profile: %v — %s", err, recorder.Body.String())
			}
			if !body.Config.ReasoningEnabled {
				t.Fatalf("reasoning_enabled = false with the switch on and the template advertised: %s",
					recorder.Body.String())
			}
			if body.Config.ReasoningRef == nil {
				t.Fatalf("reasoning_template_ref = null: %s", recorder.Body.String())
			}
			if body.Config.ReasoningRef.Version != test.wantVersion {
				t.Fatalf("reasoning_template_ref = %s@%s, want version %s",
					body.Config.ReasoningRef.ID, body.Config.ReasoningRef.Version, test.wantVersion)
			}
			// The invariant itself, not just the version: the ref must name a
			// template the same response advertises. Asserting the version alone
			// would still pass if the two fields drifted apart in some other way.
			var advertised bool
			for _, template := range body.Templating.Templates {
				if template.ID == body.Config.ReasoningRef.ID &&
					template.Version == body.Config.ReasoningRef.Version {
					advertised = true
				}
			}
			if !advertised {
				t.Fatalf("reasoning_template_ref %s@%s is not in templating.templates %+v",
					body.Config.ReasoningRef.ID, body.Config.ReasoningRef.Version,
					body.Templating.Templates)
			}
		})
	}
}

// TestBotCardProfileReportsTheTypedUnavailableWhenTheRuntimeIsUnreadable is S7
// (yujiawei): D6 specifies "runtime DB unavailable → typed localized
// unavailable", and this handler answered the generic internal code.
//
// The distinction is not cosmetic on this endpoint in particular. Feature
// detection runs at producer startup, and this is the one failure here that is
// expected to clear on its own — a producer that reads "internal error" has no
// reason to retry, while one that reads the catalog-unavailable code does.
// Fail-closed is unchanged either way: both refuse to answer with the static
// manifest, which is the part that would actually be unsafe.
func TestBotCardProfileReportsTheTypedUnavailableWhenTheRuntimeIsUnreadable(t *testing.T) {
	ba := newTestBotAPIWithCatalog(t, &fakeSpaceQuerier{
		multiRows: map[string][]string{"bot-42": {"space-bot"}},
	}, &stubAuthorizationSource{newSend: true, err: errors.New("catalog is down")})
	ba.cardConfig = stubBotCardConfig{cfg: allBotCardSwitchesOn()}

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodGet, "/v1/bot/card/profile", nil)
	ginContext.Set(CtxKeyRobotID, "bot-42")
	ba.botCardProfile(&wkhttp.Context{Context: ginContext})

	// Asserted through the message because a bare gin test context has no i18n
	// middleware, so ResponseErrorL falls back to the legacy {msg,status} shape
	// and error.code/error.http_status are not on the wire here. The two codes
	// have distinct DefaultMessages, so this still pins which code was chosen —
	// which is the whole of what S7 changes. The full envelope, including
	// http_status: 503, is the i18n renderer's own contract and is tested there.
	got := errorMessageOf(t, recorder.Body.Bytes())
	if got == errcode.ErrSharedInternal.DefaultMessage {
		t.Fatal("an unreadable runtime still answers the generic internal code; " +
			"a producer cannot tell a transient catalog outage from a broken deployment")
	}
	if got != errcode.ErrCardTemplateCatalogUnavailable.DefaultMessage {
		t.Fatalf("error = %q, want the typed catalog-unavailable code %q",
			got, errcode.ErrCardTemplateCatalogUnavailable.DefaultMessage)
	}
	// And the code really does carry 503, which is the half a client acts on
	// once the envelope is rendered in full.
	if errcode.ErrCardTemplateCatalogUnavailable.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("the typed code is registered as HTTP %d, want 503",
			errcode.ErrCardTemplateCatalogUnavailable.HTTPStatus)
	}
}
