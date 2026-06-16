package messages_search

import (
	"encoding/json"
	"strings"
	"testing"
)

// This file pins the YUJ-4667 (阶段 8) verification-gate matrix (V1–V10) to
// concrete, machine-checkable Go assertions so a future change that silently
// regresses a gate fails CI. Each test names the gate it backs. Gates whose
// truth depends on the octo-search-indexer writing new doc fields (spaceId,
// visibles) are written but SKIPPED with an explicit reason, per the plan's
// "write-but-red" instruction — they flip to live once 分叉 B (索引契约收敛)
// lands and the indexer emits those fields.

// --- V1(a): cross-space p2p returns nothing (mechanism already covered by
// space_filter_test.go::TestApplySpaceIDScope_P2PEmitsTermFilter — the DSL
// carries the spaceId term so a space-A caller cannot match a space-B doc). ---

// --- V1(b): same-space p2p DOES return hits. This is the false-positive guard
// (codex P1): without it, "0 hits" could mean "isolation works" OR "indexer
// never wrote spaceId so nothing matches". It cannot be asserted end-to-end
// until the indexer writes spaceId on p2p docs (分叉 B). ---
func TestVGate_V1b_SameSpaceReturnsHits(t *testing.T) {
	t.Skip("V1(b) blocked on 分叉 B: octo-search-indexer must write doc.spaceId " +
		"on p2p messages before same-space recall can be asserted end-to-end. " +
		"Until then the spaceId term filter matches nothing on legacy docs and " +
		"this gate would be red for the wrong reason. See YUJ-4662 §1.4 / STOP #6.")
}

// --- V3b: visibles whitelist. The unit-level gate already passes
// (visibility_test.go::TestFilterVisible_VisiblesWhitelist_NotInListDropped),
// but the END-TO-END gate (group system message visible only to admins is not
// searchable by a normal member) is fail-OPEN until the indexer writes
// doc.visibles — an empty visibles array means "no gate" (CONSTRAINTS D24). ---
func TestVGate_V3b_VisiblesWhitelistEndToEnd(t *testing.T) {
	t.Skip("V3b blocked on 分叉 B: octo-search-indexer must write doc.visibles " +
		"on group system messages. Until then visibles is empty => fail-OPEN " +
		"(a normal member could search out admin-only system messages). The " +
		"post-filter gate itself is unit-tested in visibility_test.go; this is " +
		"the end-to-end assertion that depends on the indexer. See YUJ-4662 §1.2 / STOP #6.")
}

// --- V10: response projection does not leak invisible content. A MessageHit
// built from a Doc carrying revoked=true / visibles / raw payload internals
// must not serialise any of those sensitive fields onto the wire. The hit set
// is already visibility-filtered upstream; this guards the *projection* so a
// future field addition can't accidentally ship revoked/visibles/deleted
// metadata to the client. ---
func TestVGate_V10_MessageHitProjectionNoLeak(t *testing.T) {
	h := &Handler{cfg: SearchConfig{}}
	doc := Doc{
		MessageID:   42,
		MessageSeq:  7,
		From:        "u1",
		ChannelID:   "C1",
		ChannelType: 2,
		Timestamp:   1717000000,
		Revoked:     true,
		SpaceID:     "secret-space",
		Visibles:    []string{"admin-only"},
		Payload:     &Payload{Text: &TextPayload{Content: "hidden body"}},
	}
	hit := h.singleMessageHit(doc, "C1", nil)
	b, err := json.Marshal(hit)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire := string(b)
	for _, leaked := range []string{
		"revoked", "visibles", "admin-only", "spaceId", "secret-space",
		"is_deleted", "isDeleted",
	} {
		if strings.Contains(wire, leaked) {
			t.Fatalf("V10 leak: MessageHit wire form must not contain %q; got %s", leaked, wire)
		}
	}
}

// V10 sibling for _search_all: the result envelope must not leak the same
// internal fields through its nested Message/File projection.
func TestVGate_V10_SearchAllHitProjectionNoLeak(t *testing.T) {
	h := &Handler{cfg: SearchConfig{}}
	doc := Doc{
		MessageID: 99,
		From:      "u1",
		ChannelID: "C1",
		Timestamp: 1717000000,
		Revoked:   true,
		SpaceID:   "secret-space",
		Visibles:  []string{"admin-only"},
		Payload:   &Payload{Text: &TextPayload{Content: "hidden body"}},
	}
	entry := h.singleSearchAllHit(doc, SearchAllReq{ChannelID: "C1"}, nil)
	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire := string(b)
	for _, leaked := range []string{"revoked", "visibles", "admin-only", "spaceId", "secret-space"} {
		if strings.Contains(wire, leaked) {
			t.Fatalf("V10 leak: SearchAllHit wire form must not contain %q; got %s", leaked, wire)
		}
	}
}
