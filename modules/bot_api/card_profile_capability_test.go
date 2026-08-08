package bot_api

// Review round 3 on #720, P1-C (yujiawei) and P1-A (Jerry-Xin).
//
// `CapabilityFor` had zero test call sites while being the live source for
// `/v1/bot/card/profile`, and `cf574fa0` then changed its failure contract
// without pinning either the old or the new behaviour. Three properties are
// covered here, all of which a careful reader had previously "traced by hand" —
// which is exactly what the round before last relied on, and what it missed was
// a method with no callers at all.
//
//  1. Gate off, the manifest is byte-identical to the boot-time `Capability()`.
//     That is the additive-only wire promise `card_profile.go`'s header makes to
//     SDKs, and wiring a per-request resolver behind it is the change most
//     likely to break it silently.
//  2. Gate on, one unreadable template drops out and the rest of the manifest
//     survives — the property `cf574fa0` exists to establish.
//  3. Gate on, what the manifest advertises is what the send prefilter accepts.
//     This is P1-A: the prefilter used to compare against the *static*
//     `AdvertisedRef`, so a dynamically shadowed version was advertised by the
//     profile and refused by the send path in the same breath.

import (
	"context"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

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

	// Compare the whole advertised set, not just its length: this endpoint's
	// contract is additive-only and SDKs read the versions out of it, so a
	// resolver that returned the right *number* of different templates would
	// still be a wire break.
	if len(got.Templates) != len(want.Templates) ||
		got.Supported != want.Supported || got.Wire != want.Wire {
		t.Fatalf("gate-off manifest = %+v, want the boot-time manifest %+v", got, want)
	}
	for i := range want.Templates {
		if got.Templates[i].ID != want.Templates[i].ID ||
			got.Templates[i].Version != want.Templates[i].Version {
			t.Fatalf("gate-off template[%d] = %s@%s, want %s@%s", i,
				got.Templates[i].ID, got.Templates[i].Version,
				want.Templates[i].ID, want.Templates[i].Version)
		}
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
