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
// Three layered checks (any failure → ErrOBONotAuthorized):
//  1. Grant row exists with active=1 AND global_enabled=1 for
//     (grantor, botUID). This rejects revoked grants and grants whose
//     master switch is off.
//  2. Scope row exists with enabled=1 for (grant_id, channel_id,
//     channel_type). White-list semantics per RFC §2 — opening a channel
//     to a persona is always explicit.
//  3. (No self-grant check at this layer; the REST POST /v1/obo/grants
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
	if !ok {
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
