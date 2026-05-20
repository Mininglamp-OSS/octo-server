// modules/robot/sanitize_robot_ingress.go
//
// PR#82 review #2 R1 (Jerry-Xin 2026-05-19 follow-up): strip any
// reserved `__obo_*` top-level key from robot-supplied payloads at the
// legacy `/v1/robots/:robot_id/:app_key/sendMessage` ingress.
//
// Why this exists
// ===============
// The OBO persona-clone fan-out listener
// (modules/bot_api/obo_fanout.go) breaks its dispatch → listener →
// fan-out loop by dropping any inbound message whose payload carries
// `__obo_processed__: true` (gate 3). The bot API
// (`/v1/bot/sendMessage`, modules/bot_api/send.go) rejects any
// `__obo_*` top-level key on bot client input, and the user message
// API (`/v1/message/send`, modules/message/api.go) silently strips it
// (see modules/message/sanitize_user_ingress.go). The legacy robot
// `sendMessage` endpoint (this file's target) was the third ingress
// and was the only one still letting `__obo_processed__: true` through
// unmodified — a misbehaving / malicious robot script could forge the
// marker and suppress its own persona-clone fan-out copy, silently
// breaking the OBO delivery guarantee.
//
// Strip (vs. reject) at the robot ingress
// =======================================
// We follow the user-API "silent strip" precedent rather than the
// bot-API "reject with 4xx" precedent for two reasons:
//
//  1. The legacy robot endpoint predates the OBO reserved namespace
//     and there is no expectation in its public contract that
//     `__obo_*` is meaningful. A silent strip avoids breaking any
//     real legacy caller that may have accidentally chosen a colliding
//     payload key.
//  2. The new bot API is the documented surface for new integrations;
//     authors who target the modern endpoint get the loud 4xx so
//     mistakes are caught early. The legacy endpoint is in
//     maintenance mode — quiet correctness is preferable to forcing
//     callers to update their schema for a server-only namespace.
//
// Shared contract
// ===============
// Both ingresses (user + robot) share pkg/obopayload's prefix definition
// with the bot-API reject + the fan-out listener's gate-3 check so the
// three sites cannot drift apart. A future refactor renaming the
// marker key only has to touch pkg/obopayload.
package robot

import (
	"github.com/Mininglamp-OSS/octo-server/pkg/obopayload"
	"go.uber.org/zap"
)

// logWarnFn matches the signature of (*log.TLog).Warn used by Robot,
// extracted so this helper can be exercised in unit tests without
// instantiating a real logger. Mirrors modules/message's logWarnFn so
// the two ingress helpers stay shape-identical.
type logWarnFn func(msg string, fields ...zap.Field)

// sanitizeRobotIngressPayload strips every top-level `__obo_*` key
// from `payload` (mutates in place) and logs a single warn line when
// any keys were stripped. Safe on nil / empty payloads (no-op).
//
// channelID / channelType / fromUID are passed through to the log
// line so abuse attempts surface with enough context to investigate.
// logWarn is dependency-injected so unit tests can capture log calls
// without a real logger.
//
// Called from rb.sendMessage (modules/robot/api.go) at the top of the
// legacy robot sendMessage dispatch, BEFORE any payload validation,
// space_id enrichment, or mention rewrite — the strip must precede
// every chokepoint so a forged `__obo_processed__` cannot leak into
// any downstream step that would observe it.
func sanitizeRobotIngressPayload(payload map[string]interface{}, channelID string, channelType uint8, fromUID string, logWarn logWarnFn) int {
	stripped := obopayload.StripReservedKeys(payload)
	if stripped > 0 && logWarn != nil {
		logWarn("stripped reserved OBO keys from robot-message payload (PR#82 review #2 R1 guard)",
			zap.String("channel_id", channelID),
			zap.Uint8("channel_type", channelType),
			zap.String("from_uid", fromUID),
			zap.Int("stripped_count", stripped))
	}
	return stripped
}
