// Package bot_api · YUJ-1166 / Mininglamp-OSS/octo-server#81 — Persona Clone
// (On-Behalf-Of) v0 data layer.
//
// Backing tables: obo_grants, obo_scopes (see SQL migration
// 20260519000001_obo_v0.sql). Public surface is the oboStore interface so
// HTTP handlers, checkOBO, and the fan-out listener can all be unit-tested
// against an in-memory fake without sqlmock plumbing.
//
// Cache strategy (RFC §11 risk row): the two hot-path questions are answered
// by short-TTL Redis keys, populated on read-through, invalidated on write:
//
//   - obo:grantor:{uid}        "1" any active grant exists for grantor;
//     "0" no active grant. Read by
//     findActiveGrantByGrantorBot — negative answer
//     short-circuits the (grantor, bot) MySQL probe
//     that checkOBO would otherwise issue per send.
//   - obo:chan:{ctype}:{cid}   "1" channel has at least one (active grant ×
//     enabled scope) match; "0" no match. Read AND
//     written ONLY by the unfiltered
//     `findActiveGrantsForChannel` — negative answer
//     short-circuits the JOIN that the fan-out
//     listener would otherwise issue per inbound
//     message system-wide. The filtered sibling
//     `findActiveGrantsForChannelByGrantors`
//     deliberately bypasses this key in BOTH
//     directions (PR#114 R4): its result is a
//     UID-scoped subset, so it cannot prove the
//     channel-wide negative answer the cache
//     encodes, and reading a write from the
//     unfiltered path against a same-string DM key
//     would suppress legitimate group fan-outs.
//
// Both keys are negative-cache friendly: a "0" answer returned within the
// 30-second TTL eliminates the MySQL round-trip entirely. Writes that can
// flip either answer (insertGrant / updateGrant / revokeGrant /
// insertScope / deleteScope) invalidate the affected keys inline. Stale
// "1" answers are safe — callers still consult MySQL when the cache says
// "1", so the cache cannot grant authorization it shouldn't. Stale "0"
// answers cap at 30s and are acceptable per RFC §11 (risk explicitly
// accepted for v0). Redis is best-effort throughout: a Redis outage
// silently degrades to the pre-cache path (full MySQL load), never to a
// permissions regression.
package bot_api

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
)

// isGroupLikeChannelType reports whether channelType is a "group-shaped"
// type (Group / CommunityTopic) for which an OBO grant with
// `global_enabled=1` covers EVERY group/topic the grantor participates in
// without requiring a per-channel `obo_scopes` row.
//
// YUJ-1538 rationale: PR#82 / PR#109 v1+v2 modeled scopes as a strict
// white-list — the grantor explicitly enumerated each channel they
// wanted the persona to observe. In practice operators only ever
// installed `channel_type=1` (DM) scopes; for groups the v2 fan-out
// narrowing gate (`mention.uids` must contain the grantor) is the
// effective opt-in signal, not a scope row. The fan-out trigger query
// must therefore not require scope rows for group/topic channels — a
// `global_enabled=1` grant suffices, and the per-grant
// `grantorCanReadChannel` re-check inside fanoutForMessage still
// enforces live membership.
//
// DM (Person) channels keep the strict scope-row contract: a DM is a
// 1:1 conversation that the persona must be explicitly authorized for,
// and the @grantor narrowing gate cannot be applied (DM payloads carry
// no mention).
//
// PR#114 R3 update — `checkOBO` (the third-party reply send path) was
// widened symmetrically in the same PR: for group-like channel types
// it ALSO skips the scope-row requirement when the grant has
// `global_enabled=1`, so a fan-out copy delivered into a group reaches
// the bot AND the bot's OBO reply succeeds. The previous version of
// this comment claimed `checkOBO` still required the scope row "regardless
// of channel type" — true for the v1 ship but stale after the PR#114
// commit `fix(obo): checkOBO skips scope check for group-like channels`.
// DM (Person) `checkOBO` does still require the per-peer scope row.
func isGroupLikeChannelType(channelType uint8) bool {
	return channelType == common.ChannelTypeGroup.Uint8() ||
		channelType == common.ChannelTypeCommunityTopic.Uint8()
}

// ==================== Models ====================

// oboGrantModel mirrors the obo_grants row. JSON tags are reused by HTTP
// handlers, which return rows verbatim (v0 has no nuanced DTOs).
//
// GranteeBotName is NOT a column on obo_grants — it is populated by
// listGrantsByGrantor via a LEFT JOIN against the `user` table (the bot's
// display name lives on user.name, joined on user.uid = grantee_bot_uid).
// Other reads that do `SELECT * FROM obo_grants` leave it empty; only the
// listing endpoint pays the JOIN, since that is the only path the web UI
// reads (PersonaCard renders `grantee_bot_name || grantee_bot_uid`, so a
// missing name fell back to the raw uid — YUJ-1358 / octo-web#60).
type oboGrantModel struct {
	ID            int64  `db:"id" json:"id"`
	GrantorUID    string `db:"grantor_uid" json:"grantor_uid"`
	GranteeBotUID string `db:"grantee_bot_uid" json:"grantee_bot_uid"`
	// GranteeBotName is the bot's human-facing display name (user.name on
	// the row whose uid == grantee_bot_uid). Empty string when the bot
	// has no user row OR when the field was loaded by a query that did
	// not include the JOIN. listGrantsByGrantor guarantees a non-empty
	// value via COALESCE(u.name, g.grantee_bot_uid).
	GranteeBotName string `db:"grantee_bot_name" json:"grantee_bot_name"`
	Mode           string `db:"mode" json:"mode"`
	GlobalEnabled  int    `db:"global_enabled" json:"global_enabled"`
	Active         int    `db:"active" json:"active"`
	// PersonaPrompt — YUJ-1465 / Mininglamp-OSS/octo-server#108 (OBO v2).
	// Free-form, grantor-authored prompt that the fan-out path appends to
	// the synthetic `obo_system_hint` string handed to the grantee bot.
	// Empty string disables the append (the default for legacy grants).
	// Column is TEXT (NULL-able at the schema level — `DEFAULT ''` cannot
	// be expressed on TEXT for MySQL < 8.0.13). The migration backfills
	// any pre-v2 NULL row to '' immediately after the ALTER, and every
	// post-v2 insert / reactivate writes an explicit value, so this
	// field is safe to scan into a non-pointer `string` on every read
	// path. The defensive COALESCE in listGrantsByGrantor remains as
	// belt-and-suspenders for any future non-migrated environment.
	PersonaPrompt string     `db:"persona_prompt" json:"persona_prompt"`
	CreatedAt     time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time  `db:"updated_at" json:"updated_at"`
	RevokedAt     *time.Time `db:"revoked_at" json:"revoked_at,omitempty"`
}

// oboScopeModel mirrors obo_scopes.
type oboScopeModel struct {
	ID          int64     `db:"id" json:"id"`
	GrantID     int64     `db:"grant_id" json:"grant_id"`
	ChannelID   string    `db:"channel_id" json:"channel_id"`
	ChannelType uint8     `db:"channel_type" json:"channel_type"`
	Enabled     int       `db:"enabled" json:"enabled"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
}

// ==================== Store interface (test seam) ====================

// oboStore is the minimal data dependency consumed by checkOBO, the REST
// handlers, and the fan-out listener. Both the production DB-backed impl and
// the test fake satisfy this surface; *botAPIDB satisfies it implicitly.
//
// Method contracts:
//   - findActiveGrantByGrantorBot: returns (nil, nil) if no row matches OR
//     the row is soft-deleted / globally disabled; callers MUST treat that as
//     "not authorized". Returning ErrNotFound was rejected because callers
//     would have to import dbr and branch on it.
//   - scopeEnabled: returns false (no error) when the scope row is missing,
//     enabled=0, or the grant_id doesn't exist. The hot path on sendMessage
//     only needs a boolean.
//   - findActiveGrantsForChannel: feeder for the fan-out listener; returns
//     active+global_enabled grants for the channel. Channel-type-aware
//     (YUJ-1538 / PR#114): for DM (Person) the scope row is still
//     required (strict per-peer white-list); for group-like channel
//     types (Group / CommunityTopic) a `global_enabled=1` grant alone
//     suffices and no `obo_scopes` row is consulted — the per-message
//     v2 narrowing gate (`@grantor` / `mention.all=1`) and the
//     `grantorCanReadChannel` re-check carry the opt-in. Empty slice
//     (not nil) on no match keeps callers branch-free.
//   - findActiveGrantsForChannelByGrantors: PR#114 R3. Same shape as
//     findActiveGrantsForChannel for group-like channels but adds a
//     `grantor_uid IN (...)` filter so the fan-out hot path can
//     restrict the system-wide scan to the explicit @mentioned UIDs
//     decoded from `payload.mention.uids`. Used ONLY for group-like
//     channels with explicit @uids — `mention.all` (@所有人) still goes
//     through the unfiltered method (see fanoutForMessage doc for the
//     trade-off). An empty / nil grantorUIDs slice returns an empty
//     result without touching MySQL — callers should early-return on
//     the no-mention case instead of paying a wasted query.
type oboStore interface {
	findActiveGrantByGrantorBot(grantorUID, granteeBotUID string) (*oboGrantModel, error)
	// findGrantByGrantorBotActiveOnly — YUJ-1428. Same shape as
	// findActiveGrantByGrantorBot but ONLY filters on active=1 (the
	// `global_enabled` master switch is intentionally not consulted).
	//
	// Why a separate method instead of a parameter: the existing
	// findActiveGrantByGrantorBot is the auth gate for third-party OBO
	// sends (checkOBO) and MUST keep requiring global_enabled=1 — the
	// global switch is the user-facing "stop letting this persona fan
	// out my messages" kill switch and silently demoting it on the hot
	// path would re-open exactly the class of bug the switch exists to
	// solve. The grantor-reply bypass is a different concern: a bot
	// must always be able to reply to its OWN grantor in DM as long
	// as the grant is not revoked (active=1), independent of the
	// global fan-out switch. Splitting the methods keeps both call
	// sites locked to the right contract at compile time.
	//
	// Also intentionally does NOT consult the `obo:grantor:{uid}`
	// negative cache: that cache is populated based on
	// (active=1 AND global_enabled=1) and would falsely return
	// "no grant" for a grantor who has an active grant with the
	// global switch off. The bypass call is on the DM reply path,
	// not the system-wide fan-out path, so the per-call MySQL probe
	// is acceptable.
	findGrantByGrantorBotActiveOnly(grantorUID, granteeBotUID string) (*oboGrantModel, error)
	scopeEnabled(grantID int64, channelID string, channelType uint8) (bool, error)
	findActiveGrantsForChannel(channelID string, channelType uint8) ([]*oboGrantModel, error)
	// findActiveGrantsForChannelByGrantors — PR#114 R3 (Jerry-Xin).
	// Group-like-only fan-out lookup that filters at the DB layer by the
	// explicit `mention.uids` set. Returns the subset of grants where
	// `g.grantor_uid IN (grantorUIDs)` AND `g.active=1 AND
	// g.global_enabled=1`. An empty / nil grantorUIDs slice returns an
	// empty result with no DB round-trip — callers should treat the
	// "no mentions" case at the fan-out layer instead of asking the DB.
	// DM (Person) MUST NOT call this method (no mention semantics on DMs).
	findActiveGrantsForChannelByGrantors(channelID string, channelType uint8, grantorUIDs []string) ([]*oboGrantModel, error)

	// CRUD used by the REST layer
	insertGrant(grantorUID, granteeBotUID, mode, personaPrompt string) (int64, error)
	listGrantsByGrantor(grantorUID string) ([]*oboGrantModel, error)
	findGrantByID(id int64) (*oboGrantModel, error)
	// findGrantByGrantorBot returns the row for (grantor, bot) regardless of
	// active state. Added for the reactivation path on oboCreateGrant — when
	// the UNIQUE KEY uk_grantor_grantee fires on insert, the caller looks up
	// the existing row and, if it's a soft-deleted row the caller owns, flips
	// active=1 / global_enabled=0 / revoked_at=NULL rather than returning 409.
	// (PR#82 review #2 P1-1 — without this the (grantor, bot) pair would be
	// permanently bricked after a single DELETE /v1/obo/grants/:id.)
	findGrantByGrantorBot(grantorUID, granteeBotUID string) (*oboGrantModel, error)
	updateGrant(id int64, mode string, globalEnabled *int, personaPrompt *string) error
	// reactivateGrant flips a soft-deleted row back to active=1 /
	// global_enabled=0 / revoked_at=NULL. Used by oboCreateGrant when the
	// duplicate-key conflict resolves to a row the caller already owns.
	// Returns nil on missing row so callers can treat reactivation as
	// idempotent. See findGrantByGrantorBot for the lookup pattern.
	reactivateGrant(id int64) error
	// createOrReactivateGrantAtomic — YUJ-1471 / PR#109 review blocker #2.
	// Atomically creates a fresh grant or reactivates a soft-deleted grant
	// for the (grantor, bot) pair, applies `personaPrompt`, and demotes
	// every OTHER active grant under the same grantor. The entire flow
	// runs inside a single transaction so callers can never observe a
	// partial state — in particular, two concurrent creates for different
	// bots under the same grantor cannot both succeed and then mutually
	// demote each other to active=0.
	//
	// The transaction takes a `SELECT ... FOR UPDATE` row-lock on the
	// grantor's user row before doing any obo_grants work, so concurrent
	// create/reactivate flows for the SAME grantor serialize on that lock.
	// The (grantor_uid, grantee_bot_uid) UNIQUE KEY remains the secondary
	// floor for same-bot duplicates.
	//
	// Reactivation semantics (PR#109 review blocker #3): on reactivation
	// `personaPrompt` is written verbatim — including empty string, which
	// is the explicit "clear the prompt" signal. The previously-revoked
	// row's stale prompt is overwritten regardless of the new value, so
	// a reactivation never inherits the prior persona's instructions.
	//
	// Returns:
	//   - (grant, false, nil) on a fresh insert
	//   - (grant, true,  nil) on a reactivation of a previously-revoked row
	//   - (nil,   false, errOBOGrantAlreadyActive) when the (grantor, bot)
	//     pair already has an active grant (REST translates to 409)
	//
	// Any DB failure (insert, update, demotion, etc.) rolls the entire
	// transaction back so the caller never observes a half-applied state.
	createOrReactivateGrantAtomic(grantorUID, granteeBotUID, mode, personaPrompt string) (*oboGrantModel, bool, error)
	revokeGrant(id int64) error
	insertScope(grantID int64, channelID string, channelType uint8, enabled int) (int64, error)
	deleteScope(id int64) error
	listScopesByGrant(grantID int64) ([]*oboScopeModel, error)
	// findScopeOwner answers "who owns scope X" in one query via the
	// obo_scopes → obo_grants JOIN. Replaces the O(grants × scopes_per_grant)
	// linear scan that scopeOwnedBy previously performed for every scope
	// delete (PR#82 review #2 P1-3; v1 quoted worst case 50×200 = 10k DB
	// queries for a single delete). Returns ("", false, nil) when the scope
	// row is missing.
	findScopeOwner(scopeID int64) (grantorUID string, found bool, err error)
	// queryRobotOwner returns the bot's creator uid and a flag indicating it
	// is registered as a bot (user.robot=1). Used by oboCreateGrant to enforce
	// that callers can only grant OBO power to their OWN bots (PR#82 review #2
	// P2-3 + task spec P1-2). Returns (_, _, false, nil) when no robot row
	// exists for botUID.
	queryRobotOwner(botUID string) (creatorUID string, isBot bool, found bool, err error)
}

// Compile-time guard.
var _ oboStore = (*botAPIDB)(nil)

// ==================== Production impl (botAPIDB) ====================

const (
	// oboGrantorActiveCacheKeyFmt is the Redis key for "does grantor X have
	// at least one active grant". checkOBO consults this scalar before the
	// (grantor, bot) MySQL lookup. Population: written on every
	// findActiveGrantByGrantorBot result (positive or negative). Eviction:
	// any write touching the grantor's rows.
	oboGrantorActiveCacheKeyFmt = "obo:grantor:%s"
	// oboChannelActiveCacheKeyFmt is the Redis key for "does this channel
	// have at least one (active grant × enabled scope) match". The fan-out
	// listener consults this scalar before the JOIN it would otherwise
	// issue per inbound message system-wide. Population: written on every
	// UNFILTERED findActiveGrantsForChannel result (count 0 → "0",
	// count >0 → "1"). The filtered sibling
	// findActiveGrantsForChannelByGrantors MUST NOT write here — see
	// PR#114 R4 and the function doc for the poisoning scenario.
	// Eviction: insertScope / deleteScope (the only operations that can
	// flip the answer for a given channel within the TTL window).
	oboChannelActiveCacheKeyFmt = "obo:chan:%d:%s"
	// oboCacheTTL is 30s per RFC §11. Tradeoff documented in the package
	// comment above.
	oboCacheTTL = 30 * time.Second
)

// findActiveGrantByGrantorBot — see oboStore for the contract.
//
// Read path consults `obo:grantor:{uid}` first; "0" short-circuits to nil
// without a MySQL round-trip. Any other value (including absent) falls
// through to MySQL, and the result is written back to the cache as "1"
// for a hit and "0" for a miss with oboCacheTTL. Cache errors are
// swallowed — Redis is best-effort and the production read remains
// correct regardless of cache state.
func (d *botAPIDB) findActiveGrantByGrantorBot(grantorUID, granteeBotUID string) (*oboGrantModel, error) {
	if grantorUID == "" || granteeBotUID == "" {
		return nil, nil
	}
	// Negative-cache fast path: grantor known to have zero active grants
	// in the last oboCacheTTL window → no need to probe MySQL.
	if d.grantorCacheSaysNone(grantorUID) {
		return nil, nil
	}
	var m *oboGrantModel
	_, err := d.session.Select("*").From("obo_grants").
		Where("grantor_uid=? AND grantee_bot_uid=? AND active=1 AND global_enabled=1",
			grantorUID, granteeBotUID).
		Load(&m)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, err
	}
	if m != nil {
		d.writeGrantorCache(grantorUID, true)
	} else {
		// Refine: confirm the negative answer applies to the grantor as a
		// whole, not just this (grantor, bot) pair. We probe with a cheap
		// COUNT — same index as the row lookup above. Avoids a stale "0"
		// suppressing other valid grant-bot pairs of the same grantor.
		d.maybeCacheGrantorNegative(grantorUID)
	}
	return m, nil
}

// findGrantByGrantorBotActiveOnly — see oboStore. YUJ-1428.
//
// Bypasses the `obo:grantor:{uid}` negative cache because that cache
// answers "any active AND global_enabled grant exists for grantor",
// which would falsely return "no grant" for a grantor whose grant is
// active but has the global switch toggled off — exactly the case the
// grantor-reply bypass is designed to handle. The MySQL probe runs on
// the same `(grantor_uid, grantee_bot_uid)` covering index used by
// findActiveGrantByGrantorBot, so the per-call cost is comparable to
// the cache-miss path of the strict variant.
func (d *botAPIDB) findGrantByGrantorBotActiveOnly(grantorUID, granteeBotUID string) (*oboGrantModel, error) {
	if grantorUID == "" || granteeBotUID == "" {
		return nil, nil
	}
	var m *oboGrantModel
	_, err := d.session.Select("*").From("obo_grants").
		Where("grantor_uid=? AND grantee_bot_uid=? AND active=1",
			grantorUID, granteeBotUID).
		Load(&m)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, err
	}
	return m, nil
}

// scopeEnabled — see oboStore.
func (d *botAPIDB) scopeEnabled(grantID int64, channelID string, channelType uint8) (bool, error) {
	if grantID == 0 || channelID == "" {
		return false, nil
	}
	var count int
	err := d.session.SelectBySql(
		"SELECT COUNT(*) FROM obo_scopes WHERE grant_id=? AND channel_id=? AND channel_type=? AND enabled=1",
		grantID, channelID, channelType,
	).LoadOne(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// findActiveGrantsForChannel — see oboStore. Single index-hit so the
// fan-out hot path doesn't have to issue a per-grant scope lookup.
//
// Read path consults `obo:chan:{type}:{id}` first. A cached "0" answer
// returns an empty slice without touching MySQL — the fan-out listener
// fires for every inbound message system-wide, so the vast majority of
// channels (those with no OBO grants) avoid the lookup entirely. Positive
// hits and MySQL fallback both repopulate the cache with the count-based
// scalar ("1" any matches, "0" none). Cache errors swallowed; production
// behavior is identical whether Redis is healthy or absent.
//
// YUJ-1538 — channel-type-aware lookup:
//
//   - DM (Person, channel_type=1): keeps the strict
//     `obo_grants ⨝ obo_scopes` JOIN. DMs are 1:1 conversations and the
//     persona must be explicitly white-listed per peer. This is the
//     pre-existing contract and is unchanged.
//
//   - Group (channel_type=2) / CommunityTopic (channel_type=5): a grant
//     with `active=1 AND global_enabled=1` covers EVERY group/topic the
//     grantor participates in, WITHOUT requiring an `obo_scopes` row.
//     v2 narrowing (`mention.uids` must contain the grantor, or
//     `mention.all=1`) is the effective per-message opt-in instead of a
//     scope row, and the per-grant `grantorCanReadChannel` re-check in
//     fanoutForMessage still enforces live channel membership. The bug
//     PR#109 left behind: `obo_scopes` only ever held `channel_type=1`
//     rows in production (operators never created group scopes), so the
//     INNER JOIN returned zero matches for every group inbound and the
//     fan-out copy never reached the bot — even though `checkOBO` had
//     already been updated to bypass the scope check for groups.
//
// Cache notes (group/topic): the per-channel cache key still gates the
// hot path. When the FIRST `global_enabled=1` grant is enabled in a
// previously-empty system, group channel cache entries holding a stale
// "0" remain so for at most `oboCacheTTL` (30s); after that the next
// inbound re-queries MySQL and the cache flips. Per-channel
// invalidation of group caches at grant-enable time would require
// enumerating every group the grantor participates in, which is more
// work than the bounded warmup window saves; the same 30s TTL was the
// design's accepted risk in RFC §11.
func (d *botAPIDB) findActiveGrantsForChannel(channelID string, channelType uint8) ([]*oboGrantModel, error) {
	if channelID == "" {
		return []*oboGrantModel{}, nil
	}
	if d.channelCacheSaysNone(channelID, channelType) {
		return []*oboGrantModel{}, nil
	}
	var grants []*oboGrantModel
	var err error
	if isGroupLikeChannelType(channelType) {
		// Group / CommunityTopic — `global_enabled=1` is sufficient,
		// no scope row required. Returns every active+enabled grant
		// system-wide; the per-grant `grantorCanReadChannel` re-check
		// in the fan-out loop filters to grants whose grantor is
		// actually a member of THIS group/topic. With the v2
		// `uk_grantor_grantee` UNIQUE + create-mutex (PR#109), the
		// number of active+enabled grants is bounded — at most one
		// per grantor — so the fan-out cost is O(grantors) per
		// inbound, not O(scopes).
		_, err = d.session.SelectBySql(
			"SELECT g.* FROM obo_grants g " +
				"WHERE g.active=1 AND g.global_enabled=1",
		).Load(&grants)
	} else {
		// DM (Person) and any other unrecognized channel type — keep
		// the original strict JOIN so behavior is unchanged outside
		// the group-like path.
		_, err = d.session.SelectBySql(
			"SELECT g.* FROM obo_grants g INNER JOIN obo_scopes s ON s.grant_id=g.id "+
				"WHERE g.active=1 AND g.global_enabled=1 AND s.enabled=1 "+
				"AND s.channel_id=? AND s.channel_type=?",
			channelID, channelType,
		).Load(&grants)
	}
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, err
	}
	if grants == nil {
		grants = []*oboGrantModel{}
	}
	d.writeChannelCache(channelID, channelType, len(grants) > 0)
	return grants, nil
}

// findActiveGrantsForChannelByGrantors — PR#114 R3 (Jerry-Xin perf
// blocker). Same shape as `findActiveGrantsForChannel` for group-like
// channels but scoped at the DB layer to the explicit `grantorUIDs`
// set decoded from `payload.mention.uids`. The fan-out hot path uses
// this when an inbound group message carries an explicit @grantor
// mention so we never load grants for OTHER grantors who weren't
// mentioned.
//
// Why this matters (Jerry-Xin's perf flag): the v2 group lookup loads
// EVERY active+global_enabled grant system-wide and then post-filters
// in Go, so every inbound group message — even plain text or @AI —
// would otherwise pay an O(grants_total) DB scan. With this method
// the DB returns at most `len(grantorUIDs)` rows (uk_grantor_grantee
// guarantees one row per grantor), so the cost is O(mentioned_grantors)
// per inbound and the unmentioned grantors never leave their index page.
//
// Behavior:
//   - DM / Person (or any non-group-like type) → empty slice + nil
//     error. Callers must NOT use this for DMs (no mention semantics).
//   - Empty / nil grantorUIDs → empty slice + nil error WITHOUT a DB
//     round-trip. The caller's early-return path should mean we never
//     reach this branch, but the guard makes the contract self-evident.
//   - channelID == "" → empty slice + nil error (defensive).
//
// Cache: this method MUST NOT touch the `obo:chan:{type}:{id}`
// channel-wide scalar in either direction (PR#114 R4 — Jerry-Xin /
// lml2468 cache-poisoning blocker, 2026-05-21).
//
// Why no WRITE: the cache key answers "does this channel have ANY
// active grant × enabled scope" — a CHANNEL-WIDE property. This
// method's result is a UID-filtered subset, so a zero result here
// only proves "none of the mentioned grantors have a grant", NOT
// "the channel has no grants". Writing "0" after a filtered miss
// would poison the cache for the NEXT inbound that mentions a
// different grantor who DOES have a grant — the channelCacheSaysNone
// short-circuit at the top of `findActiveGrantsForChannel` (or any
// other consumer) would early-return empty and suppress the fan-out.
//
// Why no READ: the same cache key may legitimately hold "0" written
// by the unfiltered `findActiveGrantsForChannel` for a channelID that
// happens to share its string with this method's group ID (DM peer
// uids and group ids live in the same Redis namespace). Honoring
// that "0" here would incorrectly suppress a group query whose
// mentioned-grantor SET we have not actually probed against MySQL.
//
// The unfiltered `findActiveGrantsForChannel` (used by the DM path
// and the `@所有人` group broadcast path) still manages the channel
// cache correctly because its result IS the channel-wide truth.
func (d *botAPIDB) findActiveGrantsForChannelByGrantors(channelID string, channelType uint8, grantorUIDs []string) ([]*oboGrantModel, error) {
	if channelID == "" || len(grantorUIDs) == 0 {
		return []*oboGrantModel{}, nil
	}
	if !isGroupLikeChannelType(channelType) {
		// Method is group-like-only by contract; defensive return so a
		// caller wiring this on the DM path can't silently widen access.
		return []*oboGrantModel{}, nil
	}
	// Intentionally NO channelCacheSaysNone check here — see the
	// "Why no READ" paragraph in the doc comment above.

	// Build the IN (...) placeholder list. dbr's positional binding
	// expands a `[]interface{}` arg into the right number of `?` for
	// the query, but we need to construct the literal placeholder
	// string ourselves because the dbr SelectBySql shape in this file
	// uses positional `?` markers.
	placeholders := make([]string, len(grantorUIDs))
	args := make([]interface{}, 0, len(grantorUIDs))
	for i, uid := range grantorUIDs {
		placeholders[i] = "?"
		args = append(args, uid)
	}

	var grants []*oboGrantModel
	q := "SELECT g.* FROM obo_grants g " +
		"WHERE g.active=1 AND g.global_enabled=1 " +
		"AND g.grantor_uid IN (" + strings.Join(placeholders, ",") + ")"
	_, err := d.session.SelectBySql(q, args...).Load(&grants)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, err
	}
	if grants == nil {
		grants = []*oboGrantModel{}
	}
	// Intentionally NO writeChannelCache call here — see the
	// "Why no WRITE" paragraph in the doc comment above.
	return grants, nil
}

// insertGrant creates a new grant row. Returns the autoincrement ID. Unique
// constraint violations (grantor+grantee already exists) surface verbatim so
// the REST layer can translate them to 409.
//
// YUJ-1465 — `personaPrompt` is the free-form behavioral prompt the
// fan-out path appends to `obo_system_hint`. Empty string is the
// schema default and the legacy behavior.
func (d *botAPIDB) insertGrant(grantorUID, granteeBotUID, mode, personaPrompt string) (int64, error) {
	if mode == "" {
		mode = "auto"
	}
	res, err := d.session.InsertInto("obo_grants").
		Columns("grantor_uid", "grantee_bot_uid", "mode", "global_enabled", "active",
			"persona_prompt", "created_at", "updated_at").
		Values(grantorUID, granteeBotUID, mode, 0, 1, personaPrompt, time.Now(), time.Now()).
		Exec()
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	// Defensive: brand-new grant starts with global_enabled=0, so it cannot
	// influence the fan-out hot path until a PUT toggles it on. We still bust
	// the cache so a previously-cached "false" for this grantor is dropped.
	d.invalidateGrantorCache(grantorUID)
	return id, nil
}

// listGrantsByGrantor returns ALL rows (active + revoked) so the UI can
// surface history. Callers that only want active rows must filter.
//
// LEFT JOIN `user` enriches each row with the grantee bot's display name
// (user.name on the row whose uid == grantee_bot_uid). The bot's display
// name lives on the `user` table, NOT the `robot` table (the robot table
// has no name column — see modules/robot/sql/20210926000001_robot_legacy01
// and the precedent in modules/user/api.go ~L3612: every other place that
// needs a bot's name does the same JOIN). COALESCE falls back to the raw
// uid when the user row is missing, so callers always get a non-empty
// `grantee_bot_name` — eliminating the PersonaCard fallback that
// surfaced `<uid>_bot` literals to humans (YUJ-1358 / octo-web#60).
//
// LEFT JOIN (not INNER) preserves grants whose bot user row has been
// deleted (e.g. cleanup script ran ahead of the grant revoke). Those
// rows still need to render in the UI so the operator can revoke them.
func (d *botAPIDB) listGrantsByGrantor(grantorUID string) ([]*oboGrantModel, error) {
	var grants []*oboGrantModel
	_, err := d.session.SelectBySql(
		"SELECT g.id, g.grantor_uid, g.grantee_bot_uid, "+
			"COALESCE(u.name, g.grantee_bot_uid) AS grantee_bot_name, "+
			"g.mode, g.global_enabled, g.active, "+
			"COALESCE(g.persona_prompt, '') AS persona_prompt, "+
			"g.created_at, g.updated_at, g.revoked_at "+
			"FROM obo_grants g "+
			"LEFT JOIN `user` u ON u.uid = g.grantee_bot_uid "+
			"WHERE g.grantor_uid=? "+
			"ORDER BY g.created_at DESC",
		grantorUID,
	).Load(&grants)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, err
	}
	if grants == nil {
		grants = []*oboGrantModel{}
	}
	return grants, nil
}

// findGrantByID — used by the per-grant PUT/DELETE/scopes endpoints to
// resolve+authorize the row before mutating.
func (d *botAPIDB) findGrantByID(id int64) (*oboGrantModel, error) {
	var m *oboGrantModel
	_, err := d.session.Select("*").From("obo_grants").Where("id=?", id).Load(&m)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, err
	}
	return m, nil
}

// updateGrant applies optional fields. mode="" leaves mode untouched;
// globalEnabled=nil leaves the toggle untouched. The cache for the row's
// grantor is always invalidated because either change can flip the
// "any active grant" answer. When `global_enabled` is touched, the
// per-channel `obo:chan:*` cache is ALSO invalidated for every scope on
// this grant — otherwise a `PUT global_enabled=1` could leave the
// channel-level negative cache holding "0" for up to oboCacheTTL (30s),
// causing fan-out to drop messages on a freshly-enabled grant for the
// remainder of the TTL window (PR#82 R3 non-blocking finding).
func (d *botAPIDB) updateGrant(id int64, mode string, globalEnabled *int, personaPrompt *string) error {
	updates := map[string]interface{}{}
	if mode != "" {
		updates["mode"] = mode
	}
	if globalEnabled != nil {
		// Normalize to 0/1; anything truthy becomes 1.
		v := 0
		if *globalEnabled != 0 {
			v = 1
		}
		updates["global_enabled"] = v
	}
	if personaPrompt != nil {
		// Empty string is the explicit "clear the prompt" signal; the
		// schema default is also '' so callers can round-trip either
		// shape safely.
		updates["persona_prompt"] = *personaPrompt
	}
	if len(updates) == 0 {
		return nil
	}
	updates["updated_at"] = time.Now()
	_, err := d.session.Update("obo_grants").SetMap(updates).Where("id=?", id).Exec()
	if err != nil {
		return err
	}
	// Cache may be wrong now; force re-read on next access.
	g, _ := d.findGrantByID(id)
	if g != nil {
		d.invalidateGrantorCache(g.GrantorUID)
	}
	// PR#82 R3 non-blocking — when the global toggle flipped, every
	// channel this grant covers may now have a different
	// "any active grant × enabled scope" answer. The per-channel cache
	// otherwise sticks at its prior value (most commonly "0", written
	// when the grant was disabled) until the 30s TTL expires, causing
	// the UI to look broken after an enable. Bust them all.
	//
	// Best-effort: errors are swallowed (caches are correctness-safe
	// to be stale; the only cost is the next message paying the JOIN).
	// Mode-only updates don't change any cached answer, so the work is
	// skipped in that branch.
	if globalEnabled != nil {
		scopes, _ := d.listScopesByGrant(id)
		for _, s := range scopes {
			d.invalidateChannelCache(s.ChannelID, s.ChannelType)
		}
	}
	return nil
}

// revokeGrant soft-deletes (active=0, global_enabled=0, revoked_at=now).
// We intentionally keep the row for audit. The FK on obo_scopes is
// ON DELETE CASCADE, which doesn't fire here — scopes remain so reactivation
// could be implemented in v1 without losing the channel list.
func (d *botAPIDB) revokeGrant(id int64) error {
	now := time.Now()
	g, err := d.findGrantByID(id)
	if err != nil {
		return err
	}
	if g == nil {
		return nil
	}
	_, err = d.session.Update("obo_grants").SetMap(map[string]interface{}{
		"active":         0,
		"global_enabled": 0,
		"revoked_at":     now,
		"updated_at":     now,
	}).Where("id=?", id).Exec()
	if err != nil {
		return err
	}
	d.invalidateGrantorCache(g.GrantorUID)
	return nil
}

// insertScope creates a per-channel toggle row. Duplicate (grant_id,
// channel_id, channel_type) returns the unique-key error verbatim so REST
// can translate to 409.
func (d *botAPIDB) insertScope(grantID int64, channelID string, channelType uint8, enabled int) (int64, error) {
	v := 0
	if enabled != 0 {
		v = 1
	}
	res, err := d.session.InsertInto("obo_scopes").
		Columns("grant_id", "channel_id", "channel_type", "enabled", "created_at").
		Values(grantID, channelID, channelType, v, time.Now()).
		Exec()
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	// Adding a new scope can extend fan-out reach; if grant is enabled the
	// per-channel hot path uses obo_scopes directly, but invalidating cache
	// keeps the contract simple.
	if g, _ := d.findGrantByID(grantID); g != nil {
		d.invalidateGrantorCache(g.GrantorUID)
	}
	d.invalidateChannelCache(channelID, channelType)
	return id, nil
}

// deleteScope removes a per-channel row (hard delete — there's nothing to
// audit about which channels you stopped using).
func (d *botAPIDB) deleteScope(id int64) error {
	// Look up parent grant + (channel_id, channel_type) so we can bust both
	// caches before the delete commits. The grant lookup serves
	// invalidateGrantorCache; the channel coords serve invalidateChannelCache.
	type scopeMeta struct {
		GrantID     int64  `db:"grant_id"`
		ChannelID   string `db:"channel_id"`
		ChannelType uint8  `db:"channel_type"`
	}
	var meta scopeMeta
	_, _ = d.session.SelectBySql(
		"SELECT grant_id, channel_id, channel_type FROM obo_scopes WHERE id=?", id,
	).Load(&meta)
	_, err := d.session.DeleteFrom("obo_scopes").Where("id=?", id).Exec()
	if err != nil {
		return err
	}
	if meta.GrantID != 0 {
		if g, _ := d.findGrantByID(meta.GrantID); g != nil {
			d.invalidateGrantorCache(g.GrantorUID)
		}
	}
	if meta.ChannelID != "" {
		d.invalidateChannelCache(meta.ChannelID, meta.ChannelType)
	}
	return nil
}

// listScopesByGrant — REST `/v1/obo/grants/:id/scopes`.
func (d *botAPIDB) listScopesByGrant(grantID int64) ([]*oboScopeModel, error) {
	var scopes []*oboScopeModel
	_, err := d.session.Select("*").From("obo_scopes").
		Where("grant_id=?", grantID).
		OrderBy("created_at DESC").
		Load(&scopes)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, err
	}
	if scopes == nil {
		scopes = []*oboScopeModel{}
	}
	return scopes, nil
}

// findGrantByGrantorBot — see oboStore. Returns the row regardless of
// active state. Used by oboCreateGrant's reactivation path when a fresh
// insert collides with the soft-deleted row left behind by a prior
// revokeGrant on the same (grantor, bot) pair.
func (d *botAPIDB) findGrantByGrantorBot(grantorUID, granteeBotUID string) (*oboGrantModel, error) {
	if grantorUID == "" || granteeBotUID == "" {
		return nil, nil
	}
	var m *oboGrantModel
	_, err := d.session.Select("*").From("obo_grants").
		Where("grantor_uid=? AND grantee_bot_uid=?", grantorUID, granteeBotUID).
		Load(&m)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, err
	}
	return m, nil
}

// reactivateGrant flips a soft-deleted grant back to the same shape
// `insertGrant` would have produced: active=1, global_enabled=0,
// revoked_at=NULL. Used by oboCreateGrant when the unique-key conflict
// resolves to a soft-deleted row owned by the same grantor — the row is
// reactivated in place instead of returning 409, so the (grantor, bot)
// pair never becomes permanently bricked (PR#82 review #2 P1-1).
func (d *botAPIDB) reactivateGrant(id int64) error {
	if id == 0 {
		return nil
	}
	_, err := d.session.Update("obo_grants").SetMap(map[string]interface{}{
		"active":         1,
		"global_enabled": 0,
		"revoked_at":     nil,
		"updated_at":     time.Now(),
	}).Where("id=?", id).Exec()
	if err != nil {
		return err
	}
	if g, _ := d.findGrantByID(id); g != nil {
		d.invalidateGrantorCache(g.GrantorUID)
	}
	return nil
}

// deactivateOtherActiveGrants — removed in PR#109 R3.
// The non-transactional standalone soft-deleter was superseded by
// createOrReactivateGrantAtomic, which folds the demote step inside the
// same SERIALIZABLE-grade transaction (SELECT ... FOR UPDATE on the
// grantor row + UPDATE on the OTHER active rows + commit). Keeping the
// standalone method around encouraged callers to bypass the mutex
// transaction and re-introduce the two-grant race PR#109 closed. The
// fan-out cache-invalidation logic now lives inline in
// createOrReactivateGrantAtomic.

// errOBOGrantAlreadyActive is the sentinel returned by
// createOrReactivateGrantAtomic when the (grantor, bot) pair already has
// an active grant on file. The REST layer translates this into a 409
// Conflict response so the caller can distinguish "you must revoke the
// existing grant first" from other DB failure modes.
var errOBOGrantAlreadyActive = errors.New("obo: active grant already exists for (grantor, bot) pair")

// createOrReactivateGrantAtomic — see oboStore. YUJ-1471 / PR#109 review
// blocker #2 + #3.
//
// Wraps the entire (insert | reactivate) + deactivate-others sequence in
// a single MySQL transaction. The first statement inside the tx is a
// `SELECT 1 FROM user WHERE uid=? FOR UPDATE` row lock on the grantor's
// user row — concurrent grant create/reactivate flows for the SAME
// grantor block on this lock, eliminating the v2 race where two
// concurrent POSTs (different bots, same grantor) could both succeed
// and then mutually demote each other to active=0. The (grantor_uid,
// grantee_bot_uid) UNIQUE KEY remains the floor for same-bot duplicates.
//
// Reactivation always overwrites the previously-revoked row's
// persona_prompt with the request value — including the empty string,
// which is the explicit "clear the prompt" signal. Otherwise a caller
// who soft-deletes a grant and then recreates it with no PersonaPrompt
// (or PersonaPrompt="") would silently inherit the prior persona's
// instructions (PR#109 review blocker #3).
//
// Demotion of every OTHER active grant for the grantor runs in the SAME
// tx, so if demotion fails the new/reactivated row is also rolled back —
// no half-applied "row inserted but other rows still active" state.
func (d *botAPIDB) createOrReactivateGrantAtomic(grantorUID, granteeBotUID, mode, personaPrompt string) (*oboGrantModel, bool, error) {
	if grantorUID == "" || granteeBotUID == "" {
		return nil, false, errors.New("obo: grantor_uid and grantee_bot_uid are required")
	}
	if mode == "" {
		mode = "auto"
	}

	tx, err := d.session.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.RollbackUnlessCommitted()

	// Serialize concurrent create/reactivate for the same grantor.
	// SELECT ... FOR UPDATE on the grantor's user row gives us a row
	// lock that any sibling tx for the same grantor will block on,
	// regardless of which bot they target. We tolerate `ErrNotFound`
	// here — the grantor user row is normally guaranteed by the auth
	// middleware that gates POST /v1/obo/grants, but a missing user
	// row should not crash the request. The unique key still prevents
	// same-(grantor, bot) duplicates and the in-tx scan below catches
	// any racing demotion attempt.
	var lockHit int
	if lockErr := tx.SelectBySql(
		"SELECT 1 FROM `user` WHERE uid=? FOR UPDATE", grantorUID,
	).LoadOne(&lockHit); lockErr != nil && !errors.Is(lockErr, dbr.ErrNotFound) {
		return nil, false, lockErr
	}

	now := time.Now()
	var (
		grantID     int64
		reactivated bool
	)

	res, insErr := tx.InsertInto("obo_grants").
		Columns("grantor_uid", "grantee_bot_uid", "mode", "global_enabled", "active",
			"persona_prompt", "created_at", "updated_at").
		Values(grantorUID, granteeBotUID, mode, 0, 1, personaPrompt, now, now).
		Exec()
	switch {
	case insErr == nil:
		grantID, err = res.LastInsertId()
		if err != nil {
			return nil, false, err
		}
	case isDuplicateKeyErr(insErr):
		// Reactivation candidate. Re-read the existing row under FOR
		// UPDATE so the demote-others step that follows operates on a
		// locked snapshot.
		var existing *oboGrantModel
		if _, lookupErr := tx.Select("*").From("obo_grants").
			Where("grantor_uid=? AND grantee_bot_uid=?", grantorUID, granteeBotUID).
			Suffix("FOR UPDATE").
			Load(&existing); lookupErr != nil && !errors.Is(lookupErr, dbr.ErrNotFound) {
			return nil, false, lookupErr
		}
		if existing == nil {
			// Should not happen — the duplicate key fired but the row
			// vanished before our locked SELECT. Defensive: treat as
			// 409 so the caller can retry.
			return nil, false, errOBOGrantAlreadyActive
		}
		if existing.GrantorUID != grantorUID {
			// Belt-and-suspenders: UNIQUE on (grantor, grantee) means
			// the duplicate must be ours. If somehow it isn't, refuse.
			return nil, false, errOBOGrantAlreadyActive
		}
		if existing.Active == 1 {
			// Live row → genuine duplicate, not a reactivation case.
			return nil, false, errOBOGrantAlreadyActive
		}
		if _, updErr := tx.Update("obo_grants").SetMap(map[string]interface{}{
			"active":         1,
			"global_enabled": 0,
			"revoked_at":     nil,
			"persona_prompt": personaPrompt,
			"updated_at":     now,
		}).Where("id=?", existing.ID).Exec(); updErr != nil {
			return nil, false, updErr
		}
		grantID = existing.ID
		reactivated = true
	default:
		return nil, false, insErr
	}

	// Snapshot the IDs we are about to demote so the post-commit cache
	// bust knows which channel-scope caches to drop.
	type row struct {
		ID int64 `db:"id"`
	}
	var demoted []*row
	if _, scanErr := tx.SelectBySql(
		"SELECT id FROM obo_grants WHERE grantor_uid=? AND active=1 AND id<>? FOR UPDATE",
		grantorUID, grantID,
	).Load(&demoted); scanErr != nil && !errors.Is(scanErr, dbr.ErrNotFound) {
		return nil, false, scanErr
	}

	if len(demoted) > 0 {
		if _, demErr := tx.Update("obo_grants").SetMap(map[string]interface{}{
			"active":         0,
			"global_enabled": 0,
			"revoked_at":     now,
			"updated_at":     now,
		}).Where("grantor_uid=? AND active=1 AND id<>?", grantorUID, grantID).Exec(); demErr != nil {
			return nil, false, demErr
		}
	}

	// Read the canonical post-write row inside the tx so the caller
	// gets the same view we just committed.
	var grant *oboGrantModel
	if _, readErr := tx.Select("*").From("obo_grants").
		Where("id=?", grantID).
		Load(&grant); readErr != nil {
		return nil, false, readErr
	}
	if grant == nil {
		return nil, false, errors.New("obo: row vanished between write and read inside tx")
	}

	if commitErr := tx.Commit(); commitErr != nil {
		return nil, false, commitErr
	}

	// Post-commit, best-effort cache invalidation. Cache layer is
	// correctness-safe to be stale (callers always re-query MySQL on a
	// positive cache) so we don't fail the request on Redis errors.
	d.invalidateGrantorCache(grantorUID)
	for _, r := range demoted {
		if r == nil || r.ID == 0 {
			continue
		}
		scopes, _ := d.listScopesByGrant(r.ID)
		for _, s := range scopes {
			if s == nil {
				continue
			}
			d.invalidateChannelCache(s.ChannelID, s.ChannelType)
		}
	}

	return grant, reactivated, nil
}

// O(grants × scopes_per_grant) scan that scopeOwnedBy used to perform
// on every `DELETE /v1/obo/scopes/:id` (PR#82 review #2 P1-3).
func (d *botAPIDB) findScopeOwner(scopeID int64) (string, bool, error) {
	if scopeID == 0 {
		return "", false, nil
	}
	var grantorUID string
	err := d.session.SelectBySql(
		"SELECT g.grantor_uid FROM obo_scopes s INNER JOIN obo_grants g ON g.id = s.grant_id WHERE s.id=?",
		scopeID,
	).LoadOne(&grantorUID)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	if grantorUID == "" {
		return "", false, nil
	}
	return grantorUID, true, nil
}

// queryRobotOwner — see oboStore. Reads the `user` table and returns the
// creator uid plus an IsBot flag derived from `robot=1`. Returns
// ("", false, false, nil) when no row exists for the given uid. The
// creator uid for User Bots lives on the `robot` table, NOT the `user`
// table — we read both: `robot.creator_uid` for ownership and `user.robot`
// for the bot flag, joined on uid==robot_id.
func (d *botAPIDB) queryRobotOwner(botUID string) (string, bool, bool, error) {
	if botUID == "" {
		return "", false, false, nil
	}
	type row struct {
		CreatorUID string `db:"creator_uid"`
		IsBot      int    `db:"is_bot"`
	}
	var r row
	// LEFT JOIN so a robot row without a matching user row (or vice versa)
	// still surfaces — we treat the IsBot flag as authoritative when present.
	// COALESCE keeps NULLs from corrupting the typed read.
	err := d.session.SelectBySql(
		"SELECT COALESCE(r.creator_uid, '') AS creator_uid, "+
			"COALESCE(u.robot, 0) AS is_bot "+
			"FROM robot r LEFT JOIN user u ON u.uid = r.robot_id "+
			"WHERE r.robot_id=?",
		botUID,
	).LoadOne(&r)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			return "", false, false, nil
		}
		return "", false, false, err
	}
	return r.CreatorUID, r.IsBot == 1, true, nil
}

// ==================== Cache helpers ====================

// oboGrantorCacheKey returns the Redis key for the "any active grant for
// grantor" scalar. Exposed as a function (not a const) so tests can derive
// the same key without re-implementing the format string.
func oboGrantorCacheKey(grantorUID string) string {
	return fmt.Sprintf(oboGrantorActiveCacheKeyFmt, grantorUID)
}

// oboChannelCacheKey returns the Redis key for the "any active grant ×
// enabled scope for this channel" scalar.
func oboChannelCacheKey(channelID string, channelType uint8) string {
	return fmt.Sprintf(oboChannelActiveCacheKeyFmt, channelType, channelID)
}

// invalidateGrantorCache best-effort drops the cache key. Cache misses are
// safe (the hot path falls back to DB), so the cache layer cannot be a
// correctness regression and we swallow Redis errors. nil ctx is also
// tolerated for unit tests that wire *botAPIDB without Redis.
func (d *botAPIDB) invalidateGrantorCache(grantorUID string) {
	if d.ctx == nil || grantorUID == "" {
		return
	}
	redis := d.ctx.GetRedisConn()
	if redis == nil {
		return
	}
	_ = redis.Del(oboGrantorCacheKey(grantorUID))
}

// invalidateChannelCache mirrors invalidateGrantorCache for the per-channel
// fan-out cache. Called from insertScope and deleteScope.
func (d *botAPIDB) invalidateChannelCache(channelID string, channelType uint8) {
	if d.ctx == nil || channelID == "" {
		return
	}
	redis := d.ctx.GetRedisConn()
	if redis == nil {
		return
	}
	_ = redis.Del(oboChannelCacheKey(channelID, channelType))
}

// grantorCacheSaysNone returns true iff the cache currently holds a
// definitive "no active grants for this grantor" answer. Any other state
// (cached "1", absent key, Redis outage, decode error) returns false so
// the caller falls through to MySQL.
func (d *botAPIDB) grantorCacheSaysNone(grantorUID string) bool {
	if d.ctx == nil || grantorUID == "" {
		return false
	}
	redis := d.ctx.GetRedisConn()
	if redis == nil {
		return false
	}
	v, err := redis.GetString(oboGrantorCacheKey(grantorUID))
	if err != nil || v == "" {
		return false
	}
	return v == "0"
}

// channelCacheSaysNone — same semantics as grantorCacheSaysNone but for
// the per-channel fan-out cache. False on any non-"0" state.
func (d *botAPIDB) channelCacheSaysNone(channelID string, channelType uint8) bool {
	if d.ctx == nil || channelID == "" {
		return false
	}
	redis := d.ctx.GetRedisConn()
	if redis == nil {
		return false
	}
	v, err := redis.GetString(oboChannelCacheKey(channelID, channelType))
	if err != nil || v == "" {
		return false
	}
	return v == "0"
}

// writeGrantorCache populates `obo:grantor:{uid}` with "1" (any active
// grant exists) or "0" (none), oboCacheTTL. Errors swallowed.
func (d *botAPIDB) writeGrantorCache(grantorUID string, anyActive bool) {
	if d.ctx == nil || grantorUID == "" {
		return
	}
	redis := d.ctx.GetRedisConn()
	if redis == nil {
		return
	}
	v := "0"
	if anyActive {
		v = "1"
	}
	_ = redis.SetAndExpire(oboGrantorCacheKey(grantorUID), v, oboCacheTTL)
}

// writeChannelCache populates `obo:chan:{type}:{id}` with "1"/"0".
func (d *botAPIDB) writeChannelCache(channelID string, channelType uint8, any bool) {
	if d.ctx == nil || channelID == "" {
		return
	}
	redis := d.ctx.GetRedisConn()
	if redis == nil {
		return
	}
	v := "0"
	if any {
		v = "1"
	}
	_ = redis.SetAndExpire(oboChannelCacheKey(channelID, channelType), v, oboCacheTTL)
}

// maybeCacheGrantorNegative writes "0" to `obo:grantor:{uid}` iff the
// grantor truly has zero active grants. Called after a miss on
// findActiveGrantByGrantorBot to confirm the negative answer applies
// broadly (the row miss could just mean THIS bot has no grant, not that
// the grantor has zero grants total). The COUNT(*) is cheap and runs on
// the (grantor_uid, active) covering index.
func (d *botAPIDB) maybeCacheGrantorNegative(grantorUID string) {
	if grantorUID == "" {
		return
	}
	var count int
	err := d.session.SelectBySql(
		"SELECT COUNT(*) FROM obo_grants WHERE grantor_uid=? AND active=1 AND global_enabled=1",
		grantorUID,
	).LoadOne(&count)
	if err != nil {
		// DB error → don't poison the cache; let the next call re-query.
		return
	}
	d.writeGrantorCache(grantorUID, count > 0)
}

// ==================== Helpers ====================

// isDuplicateKeyErr reports whether the given DB error came from a UNIQUE
// constraint violation. Used by REST handlers to translate insert errors
// into 409 Conflict without leaking driver text into the response.
//
// Prefers the typed MySQL error path (`*mysql.MySQLError.Number == 1062`)
// — driver-stable and the convention used elsewhere in the codebase
// (see modules/app_bot/db.go, modules/oidc/api.go). Falls back to a
// substring match against the wrapped error text so the in-memory test
// fake (which surfaces `errors.New("Error 1062: ...")`) continues to
// satisfy the contract without depending on the real driver type.
// (PR#82 review #2 P2-2.)
func isDuplicateKeyErr(err error) bool {
	if err == nil {
		return false
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "Duplicate entry") ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "Error 1062")
}
