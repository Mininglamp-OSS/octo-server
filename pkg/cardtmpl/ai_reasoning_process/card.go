// Package aireasoningprocess wires the AI reasoning-process progress card and
// its immutable successors into the cardtmpl base. Unlike the Go-authored L2a
// cards, this card ships as a pure JSON handoff (roadmap E1): it has no
// hand-written Template — the base compiles its `.template.json` views via
// Registry.RegisterJSON. This package only embeds the handoff assets and
// exposes their identity for the composition root.
//
// Registration (in the composition root):
//
//	registry.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV1)
//	registry.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV2)
//	registry.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV3)
//	registry.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV4)
//	registry.SetDefault(aireasoningprocess.TemplateID, aireasoningprocess.TemplateVersionV4)
//
// Layering: L1 JSON artifacts live under handoff/. Frozen V1/V2 retain their
// historical reasoning_stop/reasoning_retry Submit contracts. V3 removes those
// unsupported server callbacks and keeps only the client-local reasoning_toggle.
// V4 keeps V3's action surface and adds the simplified presentation: a slimmed
// header with chevron toggles and per-phase collapsible tool panels. Across all
// versions active/error remain octo/v2 and result remains octo/v1.
package aireasoningprocess

import (
	"embed"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

// Assets embeds the handoff contract resources, loaded by Registry.RegisterJSON.
//
//go:embed all:handoff
var Assets embed.FS

const (
	// TemplateID is the stable card identifier.
	TemplateID cardtmpl.ID = "ai.reasoning-process"

	// HandoffRootV1 and TemplateVersionV1 identify the frozen first release.
	HandoffRootV1     = "handoff/ai.reasoning-process@0.1.0"
	TemplateVersionV1 = "0.1.0"
	// HandoffRootV2 and TemplateVersionV2 identify the bounded successor.
	HandoffRootV2     = "handoff/ai.reasoning-process@0.2.0"
	TemplateVersionV2 = "0.2.0"
	// HandoffRootV3 and TemplateVersionV3 identify the successor that hides
	// unsupported stop/retry controls while retaining the bounded V2 schema.
	HandoffRootV3     = "handoff/ai.reasoning-process@0.3.0"
	TemplateVersionV3 = "0.3.0"
	// HandoffRootV4 and TemplateVersionV4 identify the simplified-presentation
	// successor: same five states, same wire mapping and empty Submit surface as
	// V3, but a slimmed header (no timerText line) and per-phase collapsible tool
	// panels. It keeps V3's bounds verbatim — the richer per-phase markup spends
	// noticeably more of the cardmsg node budget, so the aggregate action cap of
	// 13 is load-bearing rather than incidental.
	HandoffRootV4     = "handoff/ai.reasoning-process@0.4.0"
	TemplateVersionV4 = "0.4.0"

	// HandoffRoot and TemplateVersion retain the package's current-version
	// aliases for callers that do not need an explicit historical version.
	HandoffRoot     = HandoffRootV4
	TemplateVersion = TemplateVersionV4
)
