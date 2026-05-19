// Package bot_api · YUJ-1166 — PR#82 R6 P0 friend-gate OBO bypass.
//
// Background: the bot send / typing / readReceipt / messages-sync paths
// require the bot to be a friend of the target user (BotKindUser DM
// branch). That rule is correct for "bot user is talking directly to
// you" — the user must opt in.
//
// It is **wrong** for the managed-persona / OBO path. When admin
// grants the persona-clone bot james OBO authority over the DM pair
// admin↔bob, bob has chosen to DM admin (not james); admin's clone is
// expected to reply as admin. Requiring a `bot↔bob` friend row would
// either:
//
//  1. force the server to silently fabricate the friendship behind the
//     real user's back — leaking the bot's existence to bob (R6 P0 Bug A
//     in reverse: bob sees bot as a contact), OR
//  2. block every managed-persona reply with `bot is not a friend of
//     this user` — which is what im-test 2026-05-19 surfaced for james.
//
// The fix is a server-side **implicit** bypass: when the friend gate
// would reject a DM send, we check whether an active OBO grant
// authorises the bot to operate as some grantor in that channel, and
// the grantor still has a valid relation with the target. If so, the
// friendship requirement is satisfied transitively by the grantor's
// relation — the bot is acting as the grantor, the grantor is friends
// with the target, so the send is legitimate.
//
// Why implicit (instead of an `on_behalf_of` header on typing /
// readReceipt): the bot adapter is the only caller and it already
// signals OBO context for the actual message via `on_behalf_of`. typing
// and readReceipt are side-channel signals tied to the same OBO
// session; routing them through the same explicit field would require
// matched changes in every adapter (octo-server fix should be
// self-contained per the issue out-of-scope list). The implicit bypass
// only fires when an active OBO grant + scope already exists, so it
// cannot bypass anything a legitimate OBO send couldn't already do.
//
// Hot-path cost: one cached `findActiveGrantsForChannel` lookup (the
// same one the fan-out listener consults — already negative-cached via
// `obo:chan:*`) PLUS a per-matching-grant `grantorCanReadChannel`
// re-check. For the common case (no OBO grant covers the target
// channel), the negative cache makes this a single Redis GET.
package bot_api

import (
	"go.uber.org/zap"
)

// hasOBOAccessToChannel reports whether bot `botUID` has any active OBO
// grant covering (channelID, channelType) where the grantor still has
// live read access to that channel. Used by the friend gate
// (checkSendPermission / syncMessages) to bypass the bot↔user
// friendship requirement on the managed-persona OBO send path.
//
// Returns:
//
//   - (true,  nil) → bypass applies; caller should treat the friend
//     gate as satisfied for the (botUID, channelID, channelType) tuple.
//   - (false, nil) → no bypass; the friend gate continues to apply.
//   - (false, err) → DB / cache failure on the lookup itself. Callers
//     fail-closed (treat as no bypass) so a transient blip can never
//     widen access.
//
// Contract notes:
//   - For DMs the OBO scope row is keyed by the PEER uid in the
//     grantor's frame of reference (see grantorCanReadChannel +
//     fanoutLookupChannelID). When the bot sends a DM to bob,
//     channelID == bob, which is the same key the scope row uses, so
//     `findActiveGrantsForChannel(bob, Person)` returns every grant
//     whose grantor scoped peer=bob.
//   - The per-grant `grantorCanReadChannel` re-check closes the same
//     TOCTOU window that checkOBO closes on the send hot path
//     (PR#82 round-2 P1-A) — a stale scope row whose grantor was
//     un-friended must NOT silently extend bot reach.
func (ba *BotAPI) hasOBOAccessToChannel(botUID, channelID string, channelType uint8) (bool, error) {
	if botUID == "" || channelID == "" {
		return false, nil
	}
	store := ba.oboStoreOrDefault()
	grants, err := store.findActiveGrantsForChannel(channelID, channelType)
	if err != nil {
		ba.Error("OBO friend-gate bypass lookup failed",
			zap.String("bot", botUID),
			zap.String("channel_id", channelID),
			zap.Uint8("channel_type", channelType),
			zap.Error(err))
		return false, err
	}
	if len(grants) == 0 {
		return false, nil
	}
	for _, g := range grants {
		if g.GranteeBotUID != botUID {
			// Other grantors' grants for the same channel don't
			// authorize *this* bot. (A grant is scoped to one specific
			// grantee bot per uk_grantor_grantee.)
			continue
		}
		ok, err := ba.grantorCanReadChannel(g.GrantorUID, channelID, channelType)
		if err != nil {
			// Single grant's check errored — log and try the next.
			// Don't propagate so that a transient blip on one
			// grantor's row doesn't block a bypass that another
			// grantor's row would satisfy.
			ba.Warn("OBO friend-gate grantor access re-check failed; trying next grant",
				zap.String("bot", botUID),
				zap.String("grantor", g.GrantorUID),
				zap.String("channel_id", channelID),
				zap.Uint8("channel_type", channelType),
				zap.Error(err))
			continue
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// isFriendOrOBOBypass is the friend-gate decision used by the bot send
// path's BotKindUser/ChannelTypePerson branch. It first asks the user
// service whether bot↔target are friends (the original v0 rule); if
// not, it falls back to the OBO implicit bypass.
//
// The two-step shape is intentional: the OBO bypass is the EXCEPTION,
// not the rule. Most bots interact with users they ARE friends with
// (DM apply flow), so we keep the hot path on the existing IsFriend
// query and only consult the OBO lookup when the friend answer is no.
//
// Errors from IsFriend are surfaced verbatim — the caller treats them
// as "permission verification failed". Errors from hasOBOAccessToChannel
// are logged and converted to a `false` bypass answer (fail-closed) so
// the original "not a friend" error is returned, preserving the
// pre-fix behaviour when no OBO bypass would have applied anyway.
//
// Test seam: see friendCheckOverride on BotAPI.
func (ba *BotAPI) isFriendOrOBOBypass(botUID, targetUID string, channelType uint8) (bool, error) {
	isFriend, err := ba.isFriend(botUID, targetUID)
	if err != nil {
		return false, err
	}
	if isFriend {
		return true, nil
	}
	// Not friends → try the OBO managed-persona implicit bypass.
	bypass, _ := ba.hasOBOAccessToChannel(botUID, targetUID, channelType)
	return bypass, nil
}

// isFriend wraps userService.IsFriend behind a test seam. Production
// path delegates to ba.userService.IsFriend; unit tests inject
// friendCheckOverride to assert on the call without standing up a
// real user service. nil userService + nil override → (false, nil)
// (fail-closed; the OBO bypass can still apply).
func (ba *BotAPI) isFriend(uid, toUID string) (bool, error) {
	if ba.friendCheckOverride != nil {
		return ba.friendCheckOverride(uid, toUID)
	}
	if ba.userService == nil {
		return false, nil
	}
	return ba.userService.IsFriend(uid, toUID)
}
