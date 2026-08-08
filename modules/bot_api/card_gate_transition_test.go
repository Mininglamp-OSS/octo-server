package bot_api

// Review round 2 on #720, P1-0 (yujiawei): the gate transition itself.
//
// Every test around this code so far has run with the gate in one position for
// the whole test. The defect lived in the *change* of position: a card sent
// while OCTO_CARD_RUNTIME_CATALOG_NEW_SEND_ENABLED was off, then edited after
// it was turned on. With the gate off, `botSendCatalogPrincipal` short-circuits
// and `send.go` populates `env.SpaceID` for DMs only, so a group send stamps a
// provenance marker whose `space_id` is empty. With the gate on, the edit could
// resolve the group's real Space — and because `storedProvenanceSpaceID`
// returned the same empty string for "no marker" and "marker recording no
// Space", the edit recomposed instead of preserving, stamped the real Space,
// and `CatalogMarkersPreserved` refused the whole-struct comparison. Forever,
// on every retry, because the stored frame can never acquire a Space.
//
// `ai.reasoning-process` is that population: streaming progress cards whose
// entire purpose is to be edited repeatedly.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
)

func TestAGroupCardSentBeforeTheGateStaysEditableAfterIt(t *testing.T) {
	// Phase 1 — the gate is off. This is what every deployment looks like today.
	off := newTestBotAPIWithCatalog(t, &fakeSpaceQuerier{
		multiRows:   map[string][]string{"bot-42": {"space-bot"}},
		groupSpaces: map[string]string{"group-1": "space-group"},
	}, &stubAuthorizationSource{newSend: false})
	ctx := newGrantSpaceContext(t, "")

	sendPrincipal := off.botSendCatalogPrincipal(ctx, "bot-42", "group-1", common.ChannelTypeGroup.Uint8())
	// group sends leave env.SpaceID empty; with the gate off the principal
	// carries no Space either, so there is nothing to stamp.
	env := cardtmpl.BuildEnv{Lang: "zh-CN"}
	sent, err := off.cardTemplates.RenderPayloadForPrincipal(context.Background(), sendPrincipal,
		registrySendBody(t, "reasoning", testReasoningData(t, "reasoning"))["payload"].(map[string]any), env)
	if err != nil {
		t.Fatalf("gate-off group send: %v", err)
	}
	envelope, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := cardmsg.CatalogFrameMarkers(envelope)
	if err != nil {
		t.Fatalf("stored markers: %v", err)
	}
	if !stored.HasProvenance {
		t.Fatal("fixture precondition broken: the gate-off send stamped no provenance at all")
	}
	if stored.Provenance.SpaceID != "" {
		t.Fatalf("fixture precondition broken: gate-off group send stamped Space %q; "+
			"this test is about the empty-Space population and no longer reaches it",
			stored.Provenance.SpaceID)
	}

	// Phase 2 — the gate is turned on. Same bot, same group, same card.
	on := newTestBotAPIWithCatalog(t, &fakeSpaceQuerier{
		multiRows:   map[string][]string{"bot-42": {"space-bot"}},
		groupSpaces: map[string]string{"group-1": "space-group"},
	}, &stubAuthorizationSource{newSend: true})
	editPrincipal := on.botSendCatalogPrincipal(ctx, "bot-42", "group-1", common.ChannelTypeGroup.Uint8())
	if editPrincipal.SpaceID != "space-group" {
		t.Fatalf("edit principal = %+v, want the group's Space now that the gate is on", editPrincipal)
	}

	ref := botTemplateRef{ID: aireasoningprocess.TemplateID, Version: testReasoningVersionCurrent}
	if err := requireEffectiveCardTemplate(
		envelope, ref, "bot-42", editProvenanceSpaceCheck(true, editPrincipal)); err != nil {
		t.Fatalf("the edit guard refused a card this bot sent itself: %v", err)
	}

	storedSpace, storedMarked := storedProvenanceSpaceID(envelope)
	edited, err := on.cardTemplates.RenderEditPayloadForPrincipal(context.Background(), editPrincipal,
		registrySendBody(t, "reasoning", testReasoningData(t, "reasoning"))["payload"].(map[string]any),
		env, storedSpace, storedMarked)
	if err != nil {
		t.Fatalf("post-gate edit of a pre-gate card: %v", err)
	}
	editedEnvelope, err := json.Marshal(edited)
	if err != nil {
		t.Fatal(err)
	}
	next, err := cardmsg.CatalogFrameMarkers(editedEnvelope)
	if err != nil {
		t.Fatalf("replacement markers: %v", err)
	}

	// The assertion that matters: the replacement must preserve the empty Space
	// rather than compose the group's real one. Composing it is not a cosmetic
	// difference — it is what CatalogMarkersPreserved refuses.
	if next.Provenance.SpaceID != "" {
		t.Fatalf("the edit re-stamped Space %q onto a card whose stored marker records none; "+
			"CatalogMarkersPreserved will refuse this replacement and every retry of it",
			next.Provenance.SpaceID)
	}

	// And prove that directly against the real rule, rather than trusting the
	// field comparison above to imply it. This is the function the mutation
	// boundary calls, so a pass here is the card staying editable.
	if err := cardmsg.CatalogMarkersPreserved(stored, next); err != nil {
		t.Fatalf("the mutation boundary would refuse this edit: %v", err)
	}
}
