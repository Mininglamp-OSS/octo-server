// Package aireasoningprocess wires the AI reasoning-process progress card
// (ai.reasoning-process@0.1.0 and its bounded 0.2.0 successor) into the cardtmpl
// base. Unlike the Go-authored L2a cards, this card ships as a pure JSON handoff
// (roadmap E1): it has no
// hand-written Template — the base compiles its `.template.json` views via
// Registry.RegisterJSON. This package only embeds the handoff assets and
// exposes their identity for the composition root.
//
// Registration (in the composition root):
//
//	registry.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV1)
//	registry.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV2)
//	registry.SetDefault(aireasoningprocess.TemplateID, aireasoningprocess.TemplateVersionV2)
//
// Layering: L1 JSON artifacts live under handoff/. The card carries the
// producer's own action buttons (reasoning_stop / reasoning_retry Submit +
// reasoning_toggle) under a fixed platform owner "ai" / action_type
// "reasoning.control"; the active/error views are octo/v2, the display-only
// result view is octo/v1. The server-side handler + RouteSpec that make the
// buttons act (stop / retry a reasoning run) and the bot streaming delivery are
// deliberately downstream — see the task brief cardtmpl-reasoning-progress-card.
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

	// HandoffRoot and TemplateVersion retain the package's current-version
	// aliases for callers that do not need an explicit historical version.
	HandoffRoot     = HandoffRootV2
	TemplateVersion = TemplateVersionV2
)
