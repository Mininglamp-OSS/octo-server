package notify

// PR-C review follow-up — one place decides which version a docs
// access-request result edit renders at.
//
// There are two callers: the click-driven finalizer, which learns the version
// from the durable event's stored card context, and the sibling mutate
// endpoint, which learns it from the stored frame it is about to replace. They
// used to disagree — the finalizer followed the stored version while the mutate
// endpoint hardcoded V3 — which is fine only for as long as the registry
// default happens to be V3. The moment it moves (as it already moved off
// 0.2.0), one path renders the stored version, the other renders a constant,
// and `CardUpdater.ReplaceView`'s stored-identity guard rejects whichever one
// guessed wrong. A card half-finalized by whichever path the user happened to
// trigger is not a failure mode worth keeping.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
)

// errDocsResultVersion marks a result edit whose target version cannot be
// determined, or which was routed to the wrong template entirely.
var errDocsResultVersion = errors.New("notify: docs result edit has no usable template version")

// docsResultVersion resolves the exact version for a result-view edit from the
// template identity stored on the card.
//
// A card is always edited at the version it was sent as: the result view has to
// exist in *that* contract, and following the activation pointer instead would
// rewrite an existing card across versions. The three cases:
//
//   - No stored identity at all. The frame predates PR-C's provenance markers,
//     so there is nothing to honour and the historical behaviour — render V3 —
//     is preserved exactly.
//
//   - A stored identity naming some other template. That is a routing mistake,
//     not something to paper over by rendering the docs card anyway.
//
//   - A stored identity naming 0.2.0. That version's manifest declares only the
//     `pending` view, so there is no `result` to render into and the card is
//     upgraded to the V3 result view — which is what main.go's registration
//     comment promises ("旧 0.2.0 pending 消息仍可由 finalizer 升级成
//     0.3.0/result") and what the pre-PR-C finalizer did by hardcoding V3.
//
//     An earlier revision refused this instead, on the reasoning that a
//     *marked* 0.2.0 frame is an impossible state. That much is true — markers
//     postdate 0.2.0 — but it is not what reaches here. The identity comes from
//     `card.metadata.octo.template`, which unmarked pre-PR-C frames carry too,
//     so the refusal landed on exactly the legacy population it argued was out
//     of reach: 0.2.0 was the shipped registry default between #633 and #641,
//     so those cards were delivered to production, and a reviewer clicking one
//     got a failed finalization instead of the documented upgrade, leaving the
//     card pending forever.
//
//     The upgrade needs no extra machinery. An unmarked frame has
//     markers.HasRef false, so ReplaceView's stored-identity pin does not fire
//     and rendering V3 over a stored 0.2.0 frame works exactly as it did before
//     this package existed.
func docsResultVersion(storedID, storedVersion string) (string, error) {
	storedID, storedVersion = strings.TrimSpace(storedID), strings.TrimSpace(storedVersion)
	if storedID == "" && storedVersion == "" {
		return docsaccessrequest.TemplateVersionV3, nil
	}
	if storedID != string(docsaccessrequest.TemplateID) || storedVersion == "" {
		return "", fmt.Errorf("%w: stored card context identifies %q@%q, not docs.access-request",
			errDocsResultVersion, storedID, storedVersion)
	}
	// TemplateVersionV2, not TemplateVersion: this branch names the specific
	// version whose manifest has no `result` view, not whatever the registry
	// default happens to be. Keyed off the moving pointer, a version bump turns
	// this into a no-op and strands in-flight 0.2.0 cards pending forever —
	// which is the exact failure this function was written to fix.
	if storedVersion == docsaccessrequest.TemplateVersionV2 {
		return docsaccessrequest.TemplateVersionV3, nil
	}
	return storedVersion, nil
}

// docsResultVersionFromFrame reads the stored identity off a persisted envelope
// and resolves the result version from it.
//
// The identity comes from `card.metadata.octo.template`, which is where the
// click path reads it too (`cardmsg.CardTemplateContext`, via
// resolveRegistryCardContext). Keying off the top-level `template_ref` instead
// would look equivalent but is not: that marker only exists on frames sent
// since PR-C Slice 1, so every card already delivered carries the metadata and
// no marker — and the two callers would answer differently for exactly the
// population this helper was written to keep consistent.
//
// The marker check still runs first. It does not supply the identity; it
// rejects a frame whose two identities disagree, which is a tampering signal
// rather than a version question.
func docsResultVersionFromFrame(envelope []byte) (string, error) {
	if _, err := cardmsg.CatalogFrameMarkers(envelope); err != nil {
		return "", fmt.Errorf("%w: %v", errDocsResultVersion, err)
	}
	stored, ok := cardmsg.CardTemplateContext(envelope)
	if !ok {
		return docsResultVersion("", "")
	}
	return docsResultVersion(stored.ID, stored.Version)
}
