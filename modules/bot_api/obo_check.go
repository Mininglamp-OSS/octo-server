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
// Four layered checks (any failure → ErrOBONotAuthorized):
//  1. Grant row exists with active=1 AND global_enabled=1 for
//     (grantor, botUID). This rejects revoked grants and grants whose
//     master switch is off.
//  2. Scope row exists with enabled=1 for (grant_id, channel_id,
//     channel_type). White-list semantics per RFC §2 — opening a channel
//     to a persona is always explicit.
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

	ok, err := store.scopeEnabled(grant.ID, channelID, channelType)
	if err != nil {
		ba.Error("OBO scope lookup failed",
			zap.Int64("grant_id", grant.ID),
			zap.String("channel_id", channelID),
			zap.Uint8("channel_type", channelType),
			zap.Error(err))
		return err
	}
	// Implicit scope: when global_enabled=1 and NO explicit scope row exists
	// for this channel, check if the grantor is a member. If so, allow the OBO
	// send. BUT if a scope row exists (even with enabled=0), respect it —
	// an explicitly disabled scope means the admin intentionally excluded
	// this channel.
	//
	// GH#122 — scopeRowExists is fail-closed: a DB error here is propagated,
	// not swallowed. Treating an error as "no explicit scope" would silently
	// fall through to the implicit-scope branch and could approve a send the
	// admin explicitly disabled, because the disabled scope row would be
	// invisible to the check. Bubble up so the handler can 500 and the
	// operator notices the outage.
	//
	// PR#121 R7 (YUJ-1671) — only consult scopeRowExists when we actually
	// need it, i.e. the explicit-scope check was negative (`!ok`) AND the
	// channel type is one where implicit scope is possible
	// (isGroupLikeChannelType). For DM (Person) and any future
	// non-group-like channel, scopeRowExists adds nothing — the
	// implicit-scope branch below would short-circuit on the
	// isGroupLikeChannelType guard regardless. Skipping the redundant
	// SELECT trims one DB round-trip off every successful OBO send (where
	// `ok=true`), which is the dominant case.
	if !ok && isGroupLikeChannelType(channelType) {
		hasExplicitScope, scopeExistErr := store.scopeRowExists(grant.ID, channelID, channelType)
		if scopeExistErr != nil {
			ba.Error("OBO scopeRowExists check failed",
				zap.Int64("grant_id", grant.ID),
				zap.String("channel_id", channelID),
				zap.Uint8("channel_type", channelType),
				zap.Error(scopeExistErr))
			return scopeExistErr
		}
		if !hasExplicitScope && grant.GlobalEnabled == 1 {
			isMember, mErr := ba.grantorCanReadChannel(grantor, channelID, channelType)
			if mErr != nil {
				ba.Error("OBO implicit-scope membership check failed",
					zap.String("grantor", grantor),
					zap.String("channel_id", channelID),
					zap.Error(mErr))
				return mErr
			}
			if isMember {
				ba.Info("OBO checkOBO: implicit-scope approved (grantor is group member)",
					zap.String("grantor", grantor),
					zap.String("bot", botUID),
					zap.String("channel_id", channelID))
				ok = true
			}
		}
	}
	if !ok {
		return ErrOBONotAuthorized
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
// as a grantee by `grantorUID` — i.e. there is an `active=1` row in
// obo_grants for (grantor=grantorUID, grantee=botUID), REGARDLESS of the
// `global_enabled` flag. It is a thin boolean wrapper over the
// `findGrantByGrantorBotActiveOnly` store call (YUJ-1428 / PR#121 R5 / B3),
// which deliberately bypasses the global_enabled predicate so the grantor-
// reply bypass keeps working even when the persona is globally paused
// (see the inline rationale below).
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
	// YUJ-1428 / PR#121 R5 / B3: must NOT consult the
	// global_enabled-aware lookup. The grantor-reply bypass is the
	// "bot may always talk to its OWN grantor in DM as long as the
	// grant is active" gate; the global switch only governs whether
	// the persona intercepts THIRD-PARTY messages for fan-out. Using
	// findActiveGrantByGrantorBot (active=1 AND global_enabled=1)
	// here would falsely return "no grant" the moment a user paused
	// the persona, fall through to the strict OBO scope check, and
	// reject the reply with "obo not authorized" — breaking direct
	// grantor→bot DM conversation. checkOBO (the strict third-party
	// send path) still uses findActiveGrantByGrantorBot.
	grant, err := store.findGrantByGrantorBotActiveOnly(grantorUID, botUID)
	if err != nil {
		return false, err
	}
	return grant != nil, nil
}
