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
//   - A stored identity naming some other template. That is a routing mistake,
//     not something to paper over by rendering the docs card anyway.
//   - A stored identity naming 0.2.0. That version's manifest declares only the
//     `pending` view, so it has no `result` to render into. Nothing sends 0.2.0
//     today — new sends resolve through the registry default — so a *marked*
//     0.2.0 frame is an impossible state rather than a case to upgrade. Say so
//     here instead of failing several layers down inside ViewFor, where the
//     error names a missing view and not the reason it is missing.
func docsResultVersion(storedID, storedVersion string) (string, error) {
	storedID, storedVersion = strings.TrimSpace(storedID), strings.TrimSpace(storedVersion)
	if storedID == "" && storedVersion == "" {
		return docsaccessrequest.TemplateVersionV3, nil
	}
	if storedID != string(docsaccessrequest.TemplateID) || storedVersion == "" {
		return "", fmt.Errorf("%w: stored card context identifies %q@%q, not docs.access-request",
			errDocsResultVersion, storedID, storedVersion)
	}
	if storedVersion == docsaccessrequest.TemplateVersion {
		return "", fmt.Errorf("%w: %s@%s declares no result view; a marked frame at this version "+
			"should not exist because new sends resolve through the registry default",
			errDocsResultVersion, storedID, storedVersion)
	}
	return storedVersion, nil
}

// docsResultVersionFromFrame reads the stored identity off a persisted envelope
// and resolves the result version from it. An envelope with no template marker
// is a pre-PR-C frame and takes the legacy branch above.
func docsResultVersionFromFrame(envelope []byte) (string, error) {
	markers, err := cardmsg.CatalogFrameMarkers(envelope)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errDocsResultVersion, err)
	}
	if !markers.HasRef {
		return docsResultVersion("", "")
	}
	return docsResultVersion(markers.Ref.ID, markers.Ref.Version)
}
