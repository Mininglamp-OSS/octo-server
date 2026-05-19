// modules/message/sanitize_user_ingress.go
//
// PR#82 R8 (Jerry-Xin 2026-05-19 review on head 244fe9fa) — strip any
// reserved `__obo_*` top-level key from user-supplied payloads before
// the message module persists or dispatches them.
//
// Why this exists
// ===============
// The OBO persona-clone fan-out listener
// (modules/bot_api/obo_fanout.go) breaks its dispatch→listener loop by
// dropping inbound messages whose payload carries
// `__obo_processed__: true` (gate 3). The bot API
// (/v1/bot/sendMessage) already rejects any `__obo_*` top-level key on
// client input so a bot can't forge the marker. But the user-message
// ingress (/v1/message/send → m.sendMessage) was accepting and
// dispatching arbitrary user payload keys, so a normal user in a DM or
// group could send `{"__obo_processed__": true, ...}` and suppress
// fan-out to every grantor's persona-clone bot — silently breaking
// the persona-clone delivery guarantee.
//
// Strip (vs. reject) at the user ingress
// ======================================
// Bot clients are expected to know the reserved namespace and the bot
// API rejects with a 4xx so authors notice the mistake. Real users
// never knowingly send `__obo_*` — the keys come from a malicious
// client trying to exploit gate 3. A silent strip is the right UX
// (legitimate clients see no behavior change) and the simplest fix
// (no error surface to model in mobile / web clients).
//
// Shared contract
// ===============
// Both ingresses share pkg/obopayload's prefix definition so the strip
// (here) and the reject (modules/bot_api/send.go) cannot drift apart.
// The fan-out listener's gate-3 check also pulls its marker key from
// pkg/obopayload so a future refactor renaming the marker can't leave
// one ingress filtering a stale name.
package message

import (
	"github.com/Mininglamp-OSS/octo-server/pkg/obopayload"
	"go.uber.org/zap"
)

// logWarnFn matches the signature of (*log.TLog).Warn used by Message,
// extracted so this helper can be exercised in unit tests without
// instantiating a real logger.
type logWarnFn func(msg string, fields ...zap.Field)

// sanitizeUserIngressPayload strips every top-level `__obo_*` key from
// `payload` (mutates in place) and logs a single warn line when any
// keys were stripped. Safe on nil / empty payloads (no-op).
//
// channelID / channelType / fromUID are passed through to the log line
// so abuse attempts surface with enough context to investigate.
// logWarn is dependency-injected so unit tests can capture log calls.
//
// Called from m.sendMessage at the top of every user-message dispatch.
// The list of additional user ingresses to wire this through (e.g. any
// future user-payload relay) is enumerated in the package-level
// comment of pkg/obopayload — keep them in sync.
func sanitizeUserIngressPayload(payload map[string]interface{}, channelID string, channelType uint8, fromUID string, logWarn logWarnFn) int {
	stripped := obopayload.StripReservedKeys(payload)
	if stripped > 0 && logWarn != nil {
		logWarn("stripped reserved OBO keys from user-message payload (PR#82 R8 guard)",
			zap.String("channel_id", channelID),
			zap.Uint8("channel_type", channelType),
			zap.String("from_uid", fromUID),
			zap.Int("stripped_count", stripped))
	}
	return stripped
}
