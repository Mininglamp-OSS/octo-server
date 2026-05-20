// modules/message/mention_rewrite.go
//
// Thin re-export of pkg/mentionrewrite.RewriteMention into the
// `message` package so the existing message-module dispatch sites
// (Message.sendMessage in api.go) can keep using an unqualified symbol
// — same pattern as sanitize_user_ingress.go wrapping
// pkg/obopayload.StripReservedKeys.
//
// Why a separate package owns the helper
// ======================================
// The three-state rewrite has to be invoked from THREE client-controlled
// message ingresses:
//
//   - modules/message/api.go      (Message.sendMessage)
//   - modules/bot_api/send.go     (BotAPI.sendMessage)
//   - modules/robot/api.go        (Robot.sendMessage)
//
// `modules/message` already imports `modules/robot` (revoke flow), so
// placing the helper in `modules/message` and importing it from
// `modules/robot` would create a `robot → message → robot` cycle. The
// helper therefore lives in `pkg/mentionrewrite` (leaf package, no
// module deps) and this file re-exports it for the message-package
// callers. See pkg/mentionrewrite/rewrite.go for the contract and the
// PR#70 audit doc (docs/2026-05-mention-all-chokepoint-audit.md §5)
// for the design rationale.
//
// Spec: Mininglamp-OSS/octo-server#94, Multica YUJ-1343.
package message

import "github.com/Mininglamp-OSS/octo-server/pkg/mentionrewrite"

// RewriteMention normalizes the payload's `mention` sub-map per the
// three-state contract. Delegates to pkg/mentionrewrite so the shared
// behavior cannot drift between the three ingress packages. See
// pkg/mentionrewrite.RewriteMention for the full contract.
func RewriteMention(payload map[string]interface{}) map[string]interface{} {
	return mentionrewrite.RewriteMention(payload)
}
