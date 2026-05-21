// Package bot_api · YUJ-1166 / Mininglamp-OSS/octo-server#81 — Persona Clone
// authorization check used by sendMessage / stream endpoints.
//
// checkOBO is the single boolean question on the dispatch hot path:
// "is bot B allowed to act as grantor G in (channel_id, channel_type)?".
// It is intentionally a thin wrapper over oboStore so:
//   - the HTTP handler stays tiny (build req → check → dispatch);
//   - unit tests can swap a fake oboStore without standing up MySQL;
//   - future cache-aware variants (e.g. negative cache) can land here
//     without touching the handler.
package bot_api

import (
	"errors"

	"go.uber.org/zap"
)

// Sentinel errors returned by checkOBO. Handlers map them to user-visible
// strings (and HTTP status); production logs include the underlying detail.
var (
	// ErrOBONotAuthorized — no active+globally-enabled grant exists OR the
	// scope row for the channel is missing/disabled. Returned for both
	// "grant never existed" and "grant revoked" so callers can't probe.
	ErrOBONotAuthorized = errors.New("obo not authorized")
)

// checkOBO validates that grantee bot `botUID` may send a message in
// (channelID, channelType) as `grantor`. Returns nil on success and
// ErrOBONotAuthorized when any check fails. Unexpected DB errors are
// returned wrapped so the handler can 500.
//
// Layered checks (any failure → ErrOBONotAuthorized):
//  1. Grant row exists with active=1 AND global_enabled=1 for
//     (grantor, botUID). This rejects revoked grants and grants whose
//     master switch is off.
//  2. Scope row exists with enabled=1 for (grant_id, channel_id,
//     channel_type) — DM / Person ONLY. White-list semantics per RFC §2
//     for 1:1 conversations: the persona must be explicitly authorized
//     per peer because there is no in-message narrowing signal
//     (mentions don't apply to DMs).
//
//     YUJ-1538 / PR#114 review fix — for group-like channel types
//     (Group / CommunityTopic), the scope-row requirement is SKIPPED
//     entirely. A grant with `active=1 AND global_enabled=1` covers
//     every group/topic the grantor participates in; the per-message
//     v2 narrowing gate (`@grantor` mention or `mention.all=1`) is the
//     effective opt-in instead of a scope row, and the
//     `grantorCanReadChannel` re-check below still enforces live
//     membership. Without this skip, the fan-out copy delivered into a
//     group reaches the bot but the bot's OBO reply hits scopeEnabled,
//     returns false (operators never installed group scopes), and the
//     reply 403s — defeating the whole PR#109 group fan-out path.
//     `findActiveGrantsForChannel` (modules/bot_api/obo_db.go) was
//     already widened symmetrically in PR#114.
//  3. PR#82 round-2 P1-A — the grantor STILL has read access to the
//     channel right now (`grantorCanReadChannel`). The scope-create-time
//     check is not load-bearing for live membership: a grantor who
//     authored a scope while a member of group_42 and was later kicked
//     out must NOT be able to keep sending into group_42 as themselves
//     through the bot, otherwise the kick is bypassable. Same logic for
//     un-friended DM peers and parent-group leaves for community topics.
//     DB cost: one covering-index lookup per OBO send.
//  4. (No self-grant check at this layer; the REST POST /v1/obo/grants
//     handler is the right place to reject `grantor == grantee` and we
//     don't want to second-guess existing rows.)
func (ba *BotAPI) checkOBO(botUID, grantor, channelID string, channelType uint8) error {
	if botUID == "" || grantor == "" || channelID == "" {
		return ErrOBONotAuthorized
	}
	if botUID == grantor {
		// A bot cannot represent itself — this would be a no-op and a sign
		// the caller is confused about which field to set. Fail closed.
		return ErrOBONotAuthorized
	}

	store := ba.oboStoreOrDefault()
	grant, err := store.findActiveGrantByGrantorBot(grantor, botUID)
	if err != nil {
		ba.Error("OBO grant lookup failed",
			zap.String("grantor", grantor),
			zap.String("bot", botUID),
			zap.Error(err))
		return err
	}
	if grant == nil {
		return ErrOBONotAuthorized
	}

	// YUJ-1538 / PR#114 review fix (Jerry-Xin, lml2468) — skip the
	// scope-row check for group-like channel types when the grant is
	// `global_enabled=1`. See the function-level doc comment for the
	// full rationale; without this branch, a group fan-out copy reaches
	// the bot but the bot's OBO reply hits scopeEnabled, returns false
	// (operators never install group scopes in production), and the
	// reply 403s. DM (Person) and any unrecognized channel type keep
	// the strict scope-row contract — the test
	// TestCheckOBO_DMNoScope_StillUnauthorized pins that regression.
	if !isGroupLikeChannelType(channelType) {
		ok, err := store.scopeEnabled(grant.ID, channelID, channelType)
		if err != nil {
			ba.Error("OBO scope lookup failed",
				zap.Int64("grant_id", grant.ID),
				zap.String("channel_id", channelID),
				zap.Uint8("channel_type", channelType),
				zap.Error(err))
			return err
		}
		if !ok {
			return ErrOBONotAuthorized
		}
	}

	// PR#82 round-2 P1-A — TOCTOU close-out. Re-check the grantor's live
	// channel access on the hot path; revoking group/friend/thread access
	// MUST stop the OBO send even when the scope row is still on file.
	// Unexpected DB error → bubble up so the handler can 500 (matches the
	// scopeEnabled error contract above); a clean "no access" answer
	// degrades to ErrOBONotAuthorized.
	canRead, err := ba.grantorCanReadChannel(grantor, channelID, channelType)
	if err != nil {
		ba.Error("OBO grantor channel-access re-check failed",
			zap.String("grantor", grantor),
			zap.String("channel_id", channelID),
			zap.Uint8("channel_type", channelType),
			zap.Error(err))
		return err
	}
	if !canRead {
		ba.Warn("OBO denied: grantor no longer has read access to channel",
			zap.String("grantor", grantor),
			zap.String("bot", botUID),
			zap.String("channel_id", channelID),
			zap.Uint8("channel_type", channelType))
		return ErrOBONotAuthorized
	}
	return nil
}

// oboStoreOrDefault returns the test-injected oboStore if set, else the
// production DB-backed one. Mirrors spaceQuerierOrDefault so the test seam
// is consistent across the module.
func (ba *BotAPI) oboStoreOrDefault() oboStore {
	if ba.oboStoreOverride != nil {
		return ba.oboStoreOverride
	}
	return ba.db
}

// botHasActiveGrantFrom reports whether bot `botUID` is currently authorised
// as a grantee by `grantorUID` — i.e. there is an active row in obo_grants
// for (grantor=grantorUID, grantee=botUID), regardless of the
// `global_enabled` master switch.
//
// YUJ-1428: this helper deliberately uses
// `findGrantByGrantorBotActiveOnly` rather than the strict
// `findActiveGrantByGrantorBot` that checkOBO consults. The two checks
// answer different questions:
//
//   - checkOBO (third-party send path) — "may this bot fan out a message
//     while impersonating grantor X to peer Y?". Must respect
//     global_enabled because that switch is the user-facing kill for
//     persona fan-out and silently demoting it on the hot path re-opens
//     the bug the switch exists to solve.
//   - botHasActiveGrantFrom (grantor-reply bypass) — "is this bot
//     legitimately authorised to talk to its OWN grantor in DM?". The
//     relationship is established by the grant existing and not being
//     revoked; whether the grantor has temporarily silenced fan-out is
//     orthogonal. Pre-YUJ-1428 this consulted the global_enabled-aware
//     query and broke the bypass whenever a user flipped the persona
//     off — the bot could no longer reply to the grantor in DM even
//     though the grant was still active.
//
// Used by sendMessage to power the YUJ-1418 grantor-reply bypass: when a
// persona-clone bot is asked to reply (on behalf of the grantor) to the
// grantor themselves in DM, the OBO scope check would otherwise reject
// (no scope row covers a grantor-to-self DM, and creating one would be
// semantic noise). The bypass treats the dispatch as a normal bot reply
// — fromUID stays as the bot, no OBO substitution, no OBO markers — and
// this helper is the auth gate that distinguishes "bot has a legitimate
// relationship with the recipient" from "bot is forging a relationship".
//
// Empty bot or grantor → (false, nil); DB errors are surfaced verbatim so
// the caller can 500 rather than silently widening access.
func (ba *BotAPI) botHasActiveGrantFrom(botUID, grantorUID string) (bool, error) {
	if botUID == "" || grantorUID == "" {
		return false, nil
	}
	if botUID == grantorUID {
		// Defensive: a bot cannot grant OBO to itself (the REST create-grant
		// handler rejects this and checkOBO short-circuits too). Treat as
		// no grant so the bypass cannot fire on a malformed pair.
		return false, nil
	}
	store := ba.oboStoreOrDefault()
	grant, err := store.findGrantByGrantorBotActiveOnly(grantorUID, botUID)
	if err != nil {
		return false, err
	}
	return grant != nil, nil
}
