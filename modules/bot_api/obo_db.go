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
//     enabled scope) match; "0" no match. Read by
//     findActiveGrantsForChannel — negative answer
//     short-circuits the JOIN that the fan-out
//     listener would otherwise issue per inbound
//     message system-wide.
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

// ==================== Models ====================

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
func isGroupLikeChannelType(channelType uint8) bool {
	return channelType == common.ChannelTypeGroup.Uint8() ||
		channelType == common.ChannelTypeCommunityTopic.Uint8()
}

// oboGrantModel mirrors the obo_grants row. JSON tags are reused by HTTP
// handlers, which return rows verbatim (v0 has no nuanced DTOs).
//
// GranteeBotName is NOT a column on obo_grants — it is populated by
// listGrantsByGrantor via a LEFT JOIN against the `user` table (the bot's
// display name lives on user.name, joined on user.uid = grantee_bot_uid).
// Other reads that select via `oboGrantColumns` (the explicit column list
// — see below) leave it empty; only the listing endpoint pays the JOIN,
// since that is the only path the web UI reads (PersonaCard renders
// `grantee_bot_name || grantee_bot_uid`, so a missing name fell back to
// the raw uid — YUJ-1358 / octo-web#60).
type oboGrantModel struct {
	ID            int64  `db:"id" json:"id"`
	GrantorUID    string `db:"grantor_uid" json:"grantor_uid"`
	GranteeBotUID string `db:"grantee_bot_uid" json:"grantee_bot_uid"`
	// GranteeBotName is the bot's human-facing display name (user.name on
	// the row whose uid == grantee_bot_uid). Empty string when the bot
	// has no user row OR when the field was loaded by a query that did
	// not include the JOIN. listGrantsByGrantor guarantees a non-empty
	// value via COALESCE(u.name, g.grantee_bot_uid).
	GranteeBotName string     `db:"grantee_bot_name" json:"grantee_bot_name"`
	Mode           string     `db:"mode" json:"mode"`
	GlobalEnabled  int        `db:"global_enabled" json:"global_enabled"`
	Active         int        `db:"active" json:"active"`
	CreatedAt      time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at" json:"updated_at"`
	RevokedAt      *time.Time `db:"revoked_at" json:"revoked_at,omitempty"`
	PersonaPrompt  string     `db:"persona_prompt" json:"persona_prompt,omitempty"`
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
//     active+global_enabled grants whose scope row matches the channel and
//     enabled=1. Empty slice (not nil) on no match keeps callers branch-free.
type oboStore interface {
	findActiveGrantByGrantorBot(grantorUID, granteeBotUID string) (*oboGrantModel, error)
	// findGrantByGrantorBotActiveOnly — YUJ-1428 / restored after PR#121
	// R5 / B3 rebase regression. Same shape as
	// findActiveGrantByGrantorBot but ONLY filters on active=1 (the
	// `global_enabled` master switch is intentionally NOT consulted).
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
	scopeRowExists(grantID int64, channelID string, channelType uint8) (bool, error)
	findActiveGrantsForChannel(channelID string, channelType uint8) ([]*oboGrantModel, error)
	// findActiveGrantsForChannelByGrantors — PR#114 R3 (Jerry-Xin).
	// Group-like-only fan-out lookup that filters at the DB layer by the
	// explicit `mention.uids` set. Returns the subset of grants where
	// `g.grantor_uid IN (grantorUIDs)` AND `g.active=1 AND
	// g.global_enabled=1`. An empty / nil grantorUIDs slice returns an
	// empty result with no DB round-trip — callers should treat the
	// "no mentions" case at the fan-out layer instead of asking the DB.
	// DM (Person) MUST NOT call this method (no mention semantics on DMs).
	//
	// PR#114 R4 — this method MUST NOT read or write the channel-wide
	// `obo:chan:{type}:{id}` cache: the result is a UID-scoped subset
	// and cannot prove the channel-wide negative answer the cache
	// encodes. Writing it would suppress legitimate fan-out for OTHER
	// grantors; reading a cross-namespace DM write would suppress
	// legitimate group fan-outs.
	findActiveGrantsForChannelByGrantors(channelID string, channelType uint8, grantorUIDs []string) ([]*oboGrantModel, error)
	// PR#121 R9 (YUJ-1676 / Jerry-Xin + lml2468 blocking) — the implicit-
	// scope feeder now serves both Group and CommunityTopic channels.
	// `membershipGroupID` is the group whose membership rows gate the
	// implicit-scope predicate (the channel itself for Group; the PARENT
	// group for CommunityTopic). `channelID` + `channelType` continue to
	// identify the row used for the obo_scopes anti-join, so a topic's
	// own `(parent____short_id, ChannelTypeCommunityTopic)` scope row is
	// the one that gets honoured for the "explicit scope wins" invariant
	// — never the parent-group's. See findGlobalGrantsWithoutScope for
	// the full per-argument contract.
	findGlobalGrantsWithoutScope(membershipGroupID, channelID string, channelType uint8) ([]*oboGrantModel, error)

	// CRUD used by the REST layer.
	//
	// insertGrant persists `persona_prompt` alongside the row. The column was
	// added nullable in migration 20260521000001_obo_v2_persona_prompt.sql and
	// existing call sites pass "" — the explicit parameter exists so the row
	// is written with an empty string instead of NULL, which prevents
	// downstream `SELECT *` reads from blowing up scanning a *string into a
	// non-pointer string field. (GH#122)
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
	// setGrantActive — YUJ-1728 / octo-server#129. Toggle the
	// per-grant `active` selector exposed by PUT /v1/obo/grants/:id.
	// `active=1` runs inside a transaction that takes the
	// grantor-scoped `SELECT 1 FROM user WHERE uid=? FOR UPDATE`
	// row-lock (same pattern as createOrReactivateGrantAtomic), flips
	// the target row to active=1 / revoked_at=NULL, then demotes
	// every OTHER active grant under the same grantor to
	// active=0 / global_enabled=0 / revoked_at=now — preserving the
	// "at most one active grant per grantor" invariant the create
	// path also enforces. `active=0` is the cheap pause path: a
	// single UPDATE on the target row, no demotion. Cache
	// invalidation for the grantor key and every demoted grant's
	// channel scopes runs post-commit (best-effort, errors swallowed
	// — cache is correctness-safe to be stale).
	//
	// Missing/zero id is a no-op (matches updateGrant). The caller
	// — currently only oboUpdateGrant — is responsible for the
	// active-gate check that rejects PUTs on already-revoked rows;
	// setGrantActive itself does NOT re-enforce that policy because
	// the create / reactivate path also legitimately resets `active`
	// on rows whose Active==0.
	setGrantActive(id int64, active int) error
	// reactivateGrant flips a soft-deleted row back to active=1 /
	// global_enabled=0 / revoked_at=NULL. Used by oboCreateGrant when the
	// duplicate-key conflict resolves to a row the caller already owns.
	// Returns nil on missing row so callers can treat reactivation as
	// idempotent. See findGrantByGrantorBot for the lookup pattern.
	reactivateGrant(id int64) error
	// createOrReactivateGrantAtomic — YUJ-1471 / PR#109 review blocker #2
	// (restored after PR#121 R5 / B2 rebase regression).
	//
	// Atomically creates a fresh grant or reactivates a soft-deleted grant
	// for the (grantor, bot) pair, applies `personaPrompt`, and demotes
	// every OTHER active grant under the same grantor. The entire flow
	// runs inside a single MySQL transaction so callers can never observe
	// a partial state — in particular, two concurrent creates for
	// different bots under the same grantor cannot both succeed and then
	// mutually demote each other to active=0, leaving the grantor with
	// zero active personas and a 200 OK on the wire.
	//
	// The transaction takes a `SELECT 1 FROM user WHERE uid=? FOR UPDATE`
	// row-lock on the grantor's user row before doing any obo_grants
	// work, so concurrent create/reactivate flows for the SAME grantor
	// serialize on that lock regardless of which bot they target. The
	// (grantor_uid, grantee_bot_uid) UNIQUE KEY remains the secondary
	// floor for same-bot duplicates.
	//
	// Reactivation semantics (PR#109 review blocker #3): on reactivation
	// `personaPrompt` is written verbatim — including the empty string,
	// which is the explicit "clear the prompt" signal. The
	// previously-revoked row's stale prompt is overwritten regardless of
	// the new value, so a reactivation never inherits the prior
	// persona's instructions.
	//
	// Returns:
	//   - (grant, false, nil) on a fresh insert
	//   - (grant, true,  nil) on a reactivation of a previously-revoked row
	//   - (nil,   false, errOBOGrantAlreadyActive) when the (grantor,
	//     bot) pair already has an active grant (REST translates to 409)
	//
	// Any DB failure (insert, update, demotion, etc.) rolls the entire
	// transaction back so the caller never observes a half-applied
	// state.
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
	// findActiveGrantsForChannel result (count 0 → "0", count >0 → "1").
	// Eviction: insertScope / deleteScope (the only operations that can
	// flip the answer for a given channel within the TTL window).
	oboChannelActiveCacheKeyFmt = "obo:chan:%d:%s"
	// oboCacheTTL is 30s per RFC §11. Tradeoff documented in the package
	// comment above.
	oboCacheTTL = 30 * time.Second
)

// oboGrantColumns is the explicit column list used by every `obo_grants`
// SELECT path that decodes into oboGrantModel. We avoid `SELECT *` because
// `persona_prompt` is a nullable TEXT column (migration
// 20260521000001_obo_v2_persona_prompt.sql) and dbr panics when a NULL is
// scanned into the non-pointer `PersonaPrompt string` field. COALESCE
// guarantees a string at the driver boundary so legacy rows written before
// the column existed (or by call paths that pre-date insertGrant carrying
// persona_prompt) still load cleanly. (GH#122)
const oboGrantColumns = "id, grantor_uid, grantee_bot_uid, mode, global_enabled, active, " +
	"created_at, updated_at, revoked_at, " +
	"COALESCE(persona_prompt, '') AS persona_prompt"

// oboGrantColumnsAliased mirrors oboGrantColumns for queries that JOIN the
// `obo_grants` table aliased as `g` (the fan-out feeders).
const oboGrantColumnsAliased = "g.id, g.grantor_uid, g.grantee_bot_uid, g.mode, g.global_enabled, g.active, " +
	"g.created_at, g.updated_at, g.revoked_at, " +
	"COALESCE(g.persona_prompt, '') AS persona_prompt"

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
	_, err := d.session.SelectBySql(
		"SELECT "+oboGrantColumns+" FROM obo_grants "+
			"WHERE grantor_uid=? AND grantee_bot_uid=? AND active=1 AND global_enabled=1",
		grantorUID, granteeBotUID,
	).Load(&m)
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

// findGrantByGrantorBotActiveOnly — see oboStore. YUJ-1428 / restored
// after PR#121 R5 / B3 rebase regression.
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
	_, err := d.session.SelectBySql(
		"SELECT "+oboGrantColumns+" FROM obo_grants "+
			"WHERE grantor_uid=? AND grantee_bot_uid=? AND active=1",
		grantorUID, granteeBotUID,
	).Load(&m)
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

// scopeRowExists checks if any scope row exists for this (grant, channel)
// regardless of enabled state. Used to distinguish "no scope configured"
// (implicit scope candidate) from "scope explicitly disabled" (admin
// intentionally excluded this channel).
func (d *botAPIDB) scopeRowExists(grantID int64, channelID string, channelType uint8) (bool, error) {
	if grantID == 0 || channelID == "" {
		return false, nil
	}
	var count int
	err := d.session.SelectBySql(
		"SELECT COUNT(*) FROM obo_scopes WHERE grant_id=? AND channel_id=? AND channel_type=?",
		grantID, channelID, channelType,
	).LoadOne(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// findActiveGrantsForChannel — see oboStore. Single JOIN so the fan-out
// hot path doesn't have to issue a per-grant scope lookup.
//
// Read path consults `obo:chan:{type}:{id}` first. A cached "0" answer
// returns an empty slice without touching MySQL — the fan-out listener
// fires for every inbound message system-wide, so the vast majority of
// channels (those with no OBO grants) avoid the JOIN entirely. Positive
// hits and MySQL fallback both repopulate the cache with the count-based
// scalar ("1" any matches, "0" none). Cache errors swallowed; production
// behavior is identical whether Redis is healthy or absent.
func (d *botAPIDB) findActiveGrantsForChannel(channelID string, channelType uint8) ([]*oboGrantModel, error) {
	if channelID == "" {
		return []*oboGrantModel{}, nil
	}
	if d.channelCacheSaysNone(channelID, channelType) {
		return []*oboGrantModel{}, nil
	}
	var grants []*oboGrantModel
	_, err := d.session.SelectBySql(
		"SELECT "+oboGrantColumnsAliased+" "+
			"FROM obo_grants g INNER JOIN obo_scopes s ON s.grant_id=g.id "+
			"WHERE g.active=1 AND g.global_enabled=1 AND s.enabled=1 "+
			"AND s.channel_id=? AND s.channel_type=?",
		channelID, channelType,
	).Load(&grants)
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
// blocker). UID-FILTERED variant of findActiveGrantsForChannel for the
// group-like fan-out path: when a message in a group / community-topic
// explicitly @-mentions one or more grantor UIDs, the fan-out hot path
// passes that set as `grantorUIDs` and the DB filters to just those
// rows instead of returning every system-wide grant.
//
// PR#114 R4 invariant: this method MUST NOT consult or update the
// channel-wide `obo:chan:{type}:{id}` cache in either direction. The
// result is a UID-scoped subset and cannot prove the channel-wide
// negative answer the cache encodes. See the function doc on
// channelCacheSaysNone / writeChannelCache for the poisoning scenario.
//
// PR#121 R5 / B1 (Jerry-Xin + lml2468 blocker): an explicit `obo_scopes`
// row with `enabled=0` MUST suppress fan-out for this channel even when
// the inbound message @-mentions the grantor. The "explicit scope row
// takes precedence" invariant is enforced for the implicit-scope feeder
// (findGlobalGrantsWithoutScope) and for the unfiltered explicit path
// (findActiveGrantsForChannel via INNER JOIN obo_scopes ... enabled=1).
// Pre-fix this UID-filtered query had no scope predicate at all, so a
// channel admin could explicitly disable a channel via POST
// /v1/obo/scopes (enabled=0) and a malicious peer could still trigger
// a fan-out by @-mentioning the grantor — completely defeating the
// admin's disable. The LEFT JOIN below acts as an anti-join: a row with
// `enabled=0` for (grant_id, channel_id, channel_type) makes
// `s.enabled=0` non-NULL and filters the grant out. Scope rows with
// `enabled=1` (explicit allow) and the no-row case (implicit-scope
// candidate) both pass through unaffected.
func (d *botAPIDB) findActiveGrantsForChannelByGrantors(channelID string, channelType uint8, grantorUIDs []string) ([]*oboGrantModel, error) {
	if channelID == "" || len(grantorUIDs) == 0 {
		return []*oboGrantModel{}, nil
	}
	if !isGroupLikeChannelType(channelType) {
		return []*oboGrantModel{}, nil
	}
	// Build the `IN (?,?,?...)` placeholder list. dbr handles the bind
	// expansion when we pass a []string, but we go through the explicit
	// placeholder shape so the SQL string is debuggable in slow-query
	// logs.
	placeholders := make([]string, 0, len(grantorUIDs))
	args := make([]interface{}, 0, len(grantorUIDs)+2)
	// LEFT JOIN bind args first (channel_id, channel_type) so they
	// line up with the `?` slots in the JOIN clause; the IN-list args
	// come after to match the trailing `WHERE ... IN (?, ?, ...)`.
	args = append(args, channelID, channelType)
	for _, u := range grantorUIDs {
		placeholders = append(placeholders, "?")
		args = append(args, u)
	}
	// LEFT JOIN obo_scopes for the *disabled* scope row only — when no
	// disabled row exists, `s.id IS NULL` lets the grant through; when a
	// disabled row exists, the predicate filters the grant out. Allow
	// rows (enabled=1) and absence-of-row both keep the grant eligible,
	// preserving v2 implicit-scope semantics for @-mentioned grantors.
	sqlStr := "SELECT " + oboGrantColumnsAliased + " FROM obo_grants g " +
		"LEFT JOIN obo_scopes s " +
		"  ON s.grant_id = g.id " +
		"  AND s.channel_id = ? " +
		"  AND s.channel_type = ? " +
		"  AND s.enabled = 0 " +
		"WHERE g.active=1 AND g.global_enabled=1 " +
		"  AND s.id IS NULL " +
		"  AND g.grantor_uid IN (" + strings.Join(placeholders, ",") + ")"
	var grants []*oboGrantModel
	_, err := d.session.SelectBySql(sqlStr, args...).Load(&grants)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, err
	}
	if grants == nil {
		grants = []*oboGrantModel{}
	}
	// Intentionally NO cache write: PR#114 R4.
	return grants, nil
}

// findGlobalGrantsWithoutScope returns active grants with global_enabled=1
// that satisfy ALL three implicit-scope conditions for fan-out in (channelID,
// channelType):
//
//  1. no explicit scope row exists for this channel (anything explicit, even
//     `enabled=0`, takes precedence — admins disable a channel intentionally).
//  2. the grantor IS currently a member of `membershipGroupID` (otherwise the
//     grantor has no read access and the bot must not harvest the channel).
//  3. the grantee bot is NOT currently a member of `membershipGroupID` (Gate 4
//     — when the bot is already in the group it receives messages directly via
//     the WuKongIM subscriber pipeline, so a fan-out copy would double-process).
//
// Implicit-scope applies to group-like channels — Group and CommunityTopic
// (PR#121 R9 / YUJ-1676 Jerry-Xin + lml2468 blocker). For non-group-like
// channels (DM, Customer Service, …) the function returns an empty slice;
// the caller already gates on the channel type, the extra check here is
// belt-and-braces so a future caller cannot accidentally trigger the JOIN
// with a non-group `channel_id`.
//
// `membershipGroupID` vs (`channelID`, `channelType`) — pre-R9 the function
// took a single channel id and used it for BOTH the `group_member` join
// (membership predicates 2 & 3) AND the `obo_scopes` anti-join (predicate 1).
// That worked for Group only, because Group's `channel_id` IS the membership
// group_no. For CommunityTopic the `channel_id` is `"<parent>____<short_id>"`
// and the membership rows live on `<parent>` — so we now accept the two
// keys independently:
//
//   - Group           → caller passes (channelID, channelID, ChannelTypeGroup)
//   - CommunityTopic  → caller passes (parentGroupID, topicChannelID,
//                       ChannelTypeCommunityTopic)
//
// Scope anti-join still uses the channel's own (channel_id, channel_type) so
// a topic's `enabled=0` row suppresses fan-out for that topic only — never
// for sibling topics or the parent group itself. The "explicit scope wins"
// invariant from PR#121 R5 / B1 is preserved per-channel.
//
// Perf rationale (PR#121 review — Jerry-Xin 15:21 blocking): the previous
// implementation returned every active+global_enabled grant in the system
// regardless of membership, and the caller looped in Go doing one
// `userIsGroupMember(grantor)` query + one `userIsGroupMember(bot)` query
// per grant. For every inbound group message that meant O(total global
// grants) per-message DB round-trips. Pushing both membership predicates
// into a single JOIN here collapses the entire scan into one SQL statement
// — the planner uses the (group_no, uid) covering index on `group_member`
// once for each join leg and the `(grant_id, channel_id, channel_type)`
// index on `obo_scopes` once for the anti-join.
//
// Returned grants are READY FOR DISPATCH with respect to scope+membership:
// the caller does NOT need to re-run `grantorCanReadChannel` or Gate 4 for
// these rows. (Gates 1 / 2 / TOCTOU-after-DB-query still apply, of course;
// see obo_fanout.go for the residual checks.)
func (d *botAPIDB) findGlobalGrantsWithoutScope(membershipGroupID, channelID string, channelType uint8) ([]*oboGrantModel, error) {
	if membershipGroupID == "" || channelID == "" {
		return []*oboGrantModel{}, nil
	}
	// Implicit-scope semantics are group-like only — the membership joins
	// below have no meaning for DMs or customer-service channels. Caller
	// already gates on this; we re-assert defensively so a future caller
	// cannot regress.
	if !isGroupLikeChannelType(channelType) {
		return []*oboGrantModel{}, nil
	}
	var grants []*oboGrantModel
	_, err := d.session.SelectBySql(
		"SELECT "+oboGrantColumnsAliased+" FROM obo_grants g "+
			"INNER JOIN group_member gm_grantor "+
			"  ON gm_grantor.uid = g.grantor_uid "+
			"  AND gm_grantor.group_no = ? "+
			"  AND gm_grantor.is_deleted = 0 "+
			"LEFT JOIN group_member gm_bot "+
			"  ON gm_bot.uid = g.grantee_bot_uid "+
			"  AND gm_bot.group_no = ? "+
			"  AND gm_bot.is_deleted = 0 "+
			"LEFT JOIN obo_scopes s "+
			"  ON s.grant_id = g.id "+
			"  AND s.channel_id = ? "+
			"  AND s.channel_type = ? "+
			"WHERE g.active = 1 "+
			"  AND g.global_enabled = 1 "+
			"  AND gm_bot.uid IS NULL "+
			"  AND s.id IS NULL",
		membershipGroupID, membershipGroupID, channelID, channelType,
	).Load(&grants)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, err
	}
	if grants == nil {
		grants = []*oboGrantModel{}
	}
	return grants, nil
}

// insertGrant creates a new grant row. Returns the autoincrement ID. Unique
// constraint violations (grantor+grantee already exists) surface verbatim so
// the REST layer can translate them to 409.
//
// `personaPrompt` is persisted on insert (defaulting to "" when the caller
// passes the zero value). Writing an explicit empty string — instead of
// letting the column default to NULL — is what GH#122 required: legacy code
// paths that re-read the row via dbr `Load(*oboGrantModel)` would otherwise
// hit a "scan NULL into string" panic, because the column is nullable but
// the struct field isn't.
func (d *botAPIDB) insertGrant(grantorUID, granteeBotUID, mode, personaPrompt string) (int64, error) {
	if mode == "" {
		mode = "auto"
	}
	res, err := d.session.InsertInto("obo_grants").
		Columns("grantor_uid", "grantee_bot_uid", "mode", "persona_prompt",
			"global_enabled", "active", "created_at", "updated_at").
		Values(grantorUID, granteeBotUID, mode, personaPrompt, 0, 1, time.Now(), time.Now()).
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
	_, err := d.session.SelectBySql(
		"SELECT "+oboGrantColumns+" FROM obo_grants WHERE id=?", id,
	).Load(&m)
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

// setGrantActive — see oboStore. YUJ-1728 / octo-server#129.
//
// Two paths:
//
//   - active=0 (pause). Single UPDATE on the target row's `active`
//     column, scoped to `revoked_at IS NULL` (YUJ-1738 / PR#131 R2
//     B2 race guard). We intentionally do NOT touch `revoked_at` —
//     a paused row is semantically distinct from a revoked row
//     (the latter went through DELETE /v1/obo/grants/:id and carries
//     `revoked_at != NULL` for audit). We also leave `global_enabled`
//     in place so that a re-activation via the create/reactivate path
//     could restore the user's last-known-good state if desired. The
//     `AND revoked_at IS NULL` guard makes the pause UPDATE a no-op
//     against rows a concurrent DELETE has already tombstoned —
//     without it the `updated_at` bump on the revoked row would
//     muddy audit, and a future change that swept other columns
//     could silently reactivate a tombstoned grant.
//
//   - active=1 (activate). Wrapped in a transaction that mirrors
//     createOrReactivateGrantAtomic's mutex semantics AND its lock
//     order (YUJ-1752 / PR#131 R7 — see the LOCK ORDER INVARIANT
//     comment in the function body). The order is:
//       1. Unlocked `SELECT grantor_uid FROM obo_grants WHERE id=?`
//          so we know which user row to lock first. We deliberately
//          do NOT take FOR UPDATE on the grant row here — that would
//          invert the lock order vs createOrReactivateGrantAtomic
//          and re-introduce the AB-BA deadlock YUJ-1752 was filed for.
//       2. `SELECT 1 FROM user WHERE uid=? FOR UPDATE` serializes
//          concurrent activate/create/reactivate flows for the SAME
//          grantor across bots — without it, two PUTs racing on
//          different grants under the same grantor could leave the
//          grantor with TWO active rows (UNIQUE only covers
//          (grantor, bot)). This is the FIRST lock acquired on any
//          row in this tx, matching createOrReactivateGrantAtomic.
//       3. Re-read the target grant row FOR UPDATE inside the tx so
//          the grantor_uid we demote against is the locked snapshot
//          (not the step-1 read that may have moved under us).
//          YUJ-1738 / PR#131 R2 B2: if the re-read shows
//          `revoked_at != NULL` the activate flow is aborted with a
//          clean rollback. The handler-level gate already rejects
//          revoked grants 404 before reaching this method, but a
//          DELETE that COMMITS between the handler's grant load and
//          our tx start would slip past that gate; the in-tx re-read
//          closes the race.
//       4. Flip target row to active=1, clear revoked_at — UPDATE
//          itself also carries `AND revoked_at IS NULL` as a
//          belt-and-braces guard so a tombstoned row is never
//          resurrected even if step 3 misses (e.g. a future
//          refactor moves the check).
//       5. Demote every OTHER active row for the grantor to
//          active=0 / global_enabled=0. YUJ-1744 / PR#131 R4: the
//          demote MUST NOT touch `revoked_at`. Siblings demoted by
//          a persona switch are *paused*, not *revoked* — leaving
//          `revoked_at=NULL` keeps them eligible for re-activation
//          via a later PUT {active:1}, which is exactly the toggle
//          contract this endpoint advertises. If we stamped
//          `revoked_at=now` here, the next switch back would hit
//          oboUpdateGrant's `RevokedAt != nil` gate and 404, turning
//          the selector into a one-way trip. The demote WHERE is
//          also scoped to `revoked_at IS NULL` so a row a concurrent
//          DELETE has tombstoned (revoked_at != NULL, active still
//          racing) keeps its audit-bearing timestamp intact.
//     The entire sequence commits or rolls back together — no
//     half-applied "target active but siblings still active" state.
//
// Post-commit cache invalidation is best-effort: grantor cache always
// busted (both paths change "any active grant exists" answer in
// principle); each demoted grant's channel scopes busted too, mirroring
// updateGrant + createOrReactivateGrantAtomic's pattern. Cache layer
// is correctness-safe to be stale, so Redis errors are swallowed.
func (d *botAPIDB) setGrantActive(id int64, active int) error {
	if id == 0 {
		return nil
	}
	v := 0
	if active != 0 {
		v = 1
	}

	if v == 0 {
		// Pause path — no transaction needed, no sibling demotion.
		g, err := d.findGrantByID(id)
		if err != nil {
			return err
		}
		if g == nil {
			return nil
		}
		// YUJ-1738 / PR#131 R2 B2 — race guard. If a concurrent
		// DELETE has tombstoned the row between handler load and now,
		// treat as a logical no-op: the row is already active=0 and
		// the audit-bearing revoked_at must NOT be disturbed.
		if g.RevokedAt != nil {
			return nil
		}
		if _, err := d.session.Update("obo_grants").SetMap(map[string]interface{}{
			"active":     0,
			"updated_at": time.Now(),
		}).Where("id=? AND revoked_at IS NULL", id).Exec(); err != nil {
			return err
		}
		// "Any active grant exists for grantor" answer may have flipped.
		d.invalidateGrantorCache(g.GrantorUID)
		// Per-channel cache for the paused grant's scopes also flips —
		// before pause the channel may have answered "1" (this grant
		// covered it); now potentially "0".
		scopes, _ := d.listScopesByGrant(id)
		for _, s := range scopes {
			if s == nil {
				continue
			}
			d.invalidateChannelCache(s.ChannelID, s.ChannelType)
		}
		return nil
	}

	// Activate path — tx + grantor row-lock + demote-others.
	tx, err := d.session.Begin()
	if err != nil {
		return err
	}
	defer tx.RollbackUnlessCommitted()

	// 🔒 LOCK ORDER INVARIANT — YUJ-1752 / PR#131 R7.
	//
	// Within any single tx, locks MUST be acquired in the order:
	//
	//     `user` row (grantor) → `obo_grants` row(s) (grant + siblings)
	//
	// createOrReactivateGrantAtomic establishes this order; setGrantActive
	// MUST match it. If the two paths invert the order (e.g. setGrantActive
	// locks the grant row first and createOrReactivateGrantAtomic locks
	// the user row first), a concurrent PUT {active:1} and POST /v1/obo/grants
	// on the SAME grantor will deadlock (AB-BA): the PUT holds the grant
	// row and waits for the user row; the POST holds the user row and
	// waits for the grant row (via the demote-others FOR UPDATE scan).
	// MySQL eventually breaks the cycle with a deadlock error but the
	// loser's request fails — exactly the symptom YUJ-1752 was filed for.
	//
	// Therefore step 1 below is the unlocked grantor_uid lookup, step 2
	// is the user-row lock, and step 3 is the grant-row FOR UPDATE.
	// DO NOT REORDER without updating createOrReactivateGrantAtomic in
	// lock-step, and DO NOT introduce any FOR UPDATE on `obo_grants`
	// above the `user` lock below. The TestOBO_YUJ1752_* tests pin this
	// invariant at the source level.

	// Step 1 — Resolve grantor_uid without taking any row lock.
	// We need grantor_uid to take the user lock FIRST (per the invariant
	// above), but we don't yet hold any lock on the grant row, so a
	// concurrent writer COULD mutate the row between this read and our
	// FOR UPDATE re-read in step 3. That's fine: grantor_uid is
	// immutable for an existing grant row (UNIQUE KEY on
	// (grantor_uid, grantee_bot_uid), never updated), and step 3's
	// FOR UPDATE re-read is the authoritative snapshot we operate on.
	// If the row vanishes between step 1 and step 3 we treat it as an
	// idempotent no-op (same as if the row was missing on entry).
	var preGrantorRow struct {
		GrantorUID string `db:"grantor_uid"`
	}
	if grantorLookupErr := tx.SelectBySql(
		"SELECT grantor_uid FROM obo_grants WHERE id=?", id,
	).LoadOne(&preGrantorRow); grantorLookupErr != nil {
		if errors.Is(grantorLookupErr, dbr.ErrNotFound) {
			return nil
		}
		return grantorLookupErr
	}
	if preGrantorRow.GrantorUID == "" {
		return nil
	}

	// Step 2 — Grantor-scoped user row lock. MUST run BEFORE any
	// FOR UPDATE on `obo_grants` (see LOCK ORDER INVARIANT comment
	// above). Missing user row is tolerated: the UNIQUE on
	// (grantor, bot) + the grant FOR UPDATE in step 3 are sufficient
	// to keep the demote set consistent for THIS request. Same posture
	// as createOrReactivateGrantAtomic.
	var lockHit int
	if lockErr := tx.SelectBySql(
		"SELECT 1 FROM `user` WHERE uid=? FOR UPDATE", preGrantorRow.GrantorUID,
	).LoadOne(&lockHit); lockErr != nil && !errors.Is(lockErr, dbr.ErrNotFound) {
		return lockErr
	}

	// Step 3 — Re-read target inside the tx under FOR UPDATE — the
	// grantor_uid we use to scope the demote query MUST be the locked
	// snapshot, not the step-1 read that may have moved under us.
	// MUST use oboGrantColumns (not `SELECT *`) — `persona_prompt` is
	// a nullable TEXT column and `oboGrantModel.PersonaPrompt` is a
	// non-pointer `string`. Legacy rows created before migration
	// 20260521000001_obo_v2_persona_prompt.sql carry NULL, and dbr
	// will hit a scan error trying to land that into a `string`.
	// `oboGrantColumns` wraps the column in COALESCE(..., '') so the
	// NULL case lands cleanly. PR#131 R3 / YUJ-1740.
	var grant *oboGrantModel
	if _, lookupErr := tx.Select(oboGrantColumns).From("obo_grants").
		Where("id=?", id).
		Suffix("FOR UPDATE").
		Load(&grant); lookupErr != nil && !errors.Is(lookupErr, dbr.ErrNotFound) {
		return lookupErr
	}
	if grant == nil {
		// Row vanished between step 1 and the locked re-read. Treat as
		// idempotent no-op so the caller's earlier 200 OK contract
		// (no row → nothing to do) is preserved.
		return nil
	}

	// YUJ-1738 / PR#131 R2 B2 — DELETE-vs-PUT race guard.
	//
	// Scenario: the handler loads the grant (active=1, revoked_at=NULL),
	// passes the gate, and calls into setGrantActive. A concurrent
	// DELETE /v1/obo/grants/:id commits BEFORE our tx's `SELECT
	// FOR UPDATE` acquires its lock. We now hold the lock on a row
	// whose committed snapshot has revoked_at != NULL. Without this
	// check the activate-path UPDATE below would clear revoked_at
	// and flip active=1, effectively un-deleting a grant the user
	// just asked us to delete.
	//
	// Bail out with a clean rollback. The caller already returned
	// 200 OK to the PUT (handler doesn't surface this race as an
	// error — the user can retry the activate via POST
	// /v1/obo/grants), and the DELETE's audit trail (revoked_at
	// timestamp) is preserved.
	if grant.RevokedAt != nil {
		return nil
	}

	// Defensive: if grantor_uid changed between the unlocked step-1
	// read and the locked step-3 re-read, abort. grantor_uid is
	// immutable in practice (no code path UPDATEs it), but if a
	// future refactor ever did, locking the WRONG user row would
	// break the per-grantor serialization the invariant promises.
	// This check turns that subtle correctness issue into a clean
	// no-op rather than a silent partial commit.
	if grant.GrantorUID != preGrantorRow.GrantorUID {
		return nil
	}

	now := time.Now()
	// Belt-and-braces: the in-tx `revoked_at != nil` check above is
	// the primary guard; this WHERE clause is a defensive second
	// layer so a future refactor that drops the explicit check
	// can't silently resurrect a tombstoned row.
	if _, updErr := tx.Update("obo_grants").SetMap(map[string]interface{}{
		"active":     1,
		"revoked_at": nil,
		"updated_at": now,
	}).Where("id=? AND revoked_at IS NULL", id).Exec(); updErr != nil {
		return updErr
	}

	// Snapshot demote-set IDs for the post-commit channel-cache bust.
	// Same struct shape used by createOrReactivateGrantAtomic.
	type row struct {
		ID int64 `db:"id"`
	}
	var demoted []*row
	if _, scanErr := tx.SelectBySql(
		"SELECT id FROM obo_grants WHERE grantor_uid=? AND active=1 AND id<>? FOR UPDATE",
		grant.GrantorUID, id,
	).Load(&demoted); scanErr != nil && !errors.Is(scanErr, dbr.ErrNotFound) {
		return scanErr
	}

	if len(demoted) > 0 {
		// YUJ-1744 / PR#131 R4 — siblings are PAUSED, not REVOKED.
		// Do NOT set revoked_at; that timestamp is reserved for the
		// explicit DELETE /v1/obo/grants/:id path. Stamping it here
		// would make the next persona switch back to this row 404
		// via oboUpdateGrant's RevokedAt-gate, breaking the toggle
		// contract. The `AND revoked_at IS NULL` predicate also
		// shields any row a concurrent DELETE just tombstoned so
		// its audit timestamp is preserved exactly.
		if _, demErr := tx.Update("obo_grants").SetMap(map[string]interface{}{
			"active":         0,
			"global_enabled": 0,
			"updated_at":     now,
		}).Where("grantor_uid=? AND active=1 AND id<>? AND revoked_at IS NULL", grant.GrantorUID, id).Exec(); demErr != nil {
			return demErr
		}
	}

	if commitErr := tx.Commit(); commitErr != nil {
		return commitErr
	}

	// Post-commit, best-effort cache invalidation. See
	// createOrReactivateGrantAtomic for the same pattern + rationale.
	d.invalidateGrantorCache(grant.GrantorUID)
	// YUJ-1747 / PR#131 R5: bust per-channel cache for the activated
	// grant's own scopes. While the grant was paused, the cache may
	// have been written as "0" (no active grant covers this channel)
	// on a fan-out miss; if we only invalidate sibling scopes, the
	// just-reactivated channel keeps answering "0" until TTL and
	// findActiveGrantsForChannel short-circuits to empty, suppressing
	// fan-out entirely. Mirror the demote loop below.
	scopes, _ := d.listScopesByGrant(id)
	for _, s := range scopes {
		if s == nil {
			continue
		}
		d.invalidateChannelCache(s.ChannelID, s.ChannelType)
	}
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
	_, err := d.session.SelectBySql(
		"SELECT "+oboGrantColumns+" FROM obo_grants WHERE grantor_uid=? AND grantee_bot_uid=?",
		grantorUID, granteeBotUID,
	).Load(&m)
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

// errOBOGrantAlreadyActive is the sentinel returned by
// createOrReactivateGrantAtomic when the (grantor, bot) pair already has
// an active grant on file. The REST layer translates this into a 409
// Conflict response so the caller can distinguish "you must revoke the
// existing grant first" from other DB failure modes.
var errOBOGrantAlreadyActive = errors.New("obo: active grant already exists for (grantor, bot) pair")

// createOrReactivateGrantAtomic — see oboStore. YUJ-1471 / PR#109 review
// blocker #2 + #3, restored after the PR#121 R5 / B2 rebase regression.
//
// Wraps the entire (insert | reactivate) + demote-others sequence in a
// single MySQL transaction. The first statement inside the tx is a
// `SELECT 1 FROM user WHERE uid=? FOR UPDATE` row lock on the grantor's
// user row — concurrent grant create/reactivate flows for the SAME
// grantor block on this lock, eliminating the v2 race where two
// concurrent POSTs (different bots, same grantor) could both succeed and
// then mutually demote each other to active=0. The (grantor_uid,
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

	// 🔒 LOCK ORDER INVARIANT — YUJ-1752 / PR#131 R7.
	//
	// `user` row (grantor) → `obo_grants` row(s). setGrantActive's
	// activate path MUST mirror this order. The TestOBO_YUJ1752_*
	// tests pin both sides. DO NOT reorder without updating
	// setGrantActive in lock-step.
	//
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
		// locked snapshot. Use oboGrantColumns (not `SELECT *`) so a
		// NULL `persona_prompt` on a legacy row decodes cleanly into
		// `oboGrantModel.PersonaPrompt string`. PR#131 R3 / YUJ-1740.
		var existing *oboGrantModel
		if _, lookupErr := tx.Select(oboGrantColumns).From("obo_grants").
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
		// YUJ-1744 / PR#131 R4 — siblings demoted by a create / reactivate
		// are PAUSED, not REVOKED. `revoked_at` is the audit timestamp
		// owned exclusively by revokeGrant (DELETE /v1/obo/grants/:id);
		// stamping it here would make the demoted row 404 on any future
		// PUT {active:1} via oboUpdateGrant's RevokedAt-gate, turning
		// the persona selector into a one-way trip. The `AND revoked_at
		// IS NULL` predicate also shields rows a concurrent DELETE just
		// tombstoned so the audit timestamp is preserved exactly.
		if _, demErr := tx.Update("obo_grants").SetMap(map[string]interface{}{
			"active":         0,
			"global_enabled": 0,
			"updated_at":     now,
		}).Where("grantor_uid=? AND active=1 AND id<>? AND revoked_at IS NULL", grantorUID, grantID).Exec(); demErr != nil {
			return nil, false, demErr
		}
	}

	// Read the canonical post-write row inside the tx so the caller
	// gets the same view we just committed. Use oboGrantColumns (not
	// `SELECT *`) so a NULL `persona_prompt` on a legacy reactivated
	// row decodes cleanly into `oboGrantModel.PersonaPrompt string`.
	// PR#131 R3 / YUJ-1740.
	var grant *oboGrantModel
	if _, readErr := tx.Select(oboGrantColumns).From("obo_grants").
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

// findScopeOwner — see oboStore. Single JOIN replaces the
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
