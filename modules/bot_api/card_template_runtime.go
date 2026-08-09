package bot_api

// PR-C D6 — merging the code-reviewed static Bot policy with dynamic grants.
//
// Two things meet here, and both are easy to get subtly wrong:
//
//  1. *Which version of a template ID may this Bot send right now.* Before
//     PR-C the answer was a process constant. Now the activation pointer can
//     shadow it with a dynamic version, but only when this Bot actually holds
//     a send grant in this Space. The full decision table lives in
//     resolveSendRef below and is the single implementation both the capability
//     manifest and the send path call — an advertised set that disagreed with
//     what send accepts is a support incident waiting to happen.
//
//  2. *Which Space this Bot is acting in.* The legacy resolver in
//     space_inject.go is deliberately forgiving: on an unauthorized header, a
//     DB error or an ambiguous multi-Space Bot it falls back to a
//     deterministic first Space so that DM delivery keeps working. That
//     forgiveness is fine for "which Space tag do we stamp on a message"; it is
//     not fine as an authorization input, because it would let an unauthorized
//     X-Space-ID header silently downgrade into some *other* Space's grants.
//     resolveBotGrantSpaceID below is the strict twin: same signals, but every
//     ambiguity is a refusal.
//
// Nothing here loosens the static path. With the new-send gate closed the
// resolver returns the static policy without touching the runtime DB at all,
// so a deployment that has not opted into the dynamic catalog cannot acquire a
// new database dependency — or a new failure mode — from this file.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	liblog "github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// errBotTemplateRuntimeUnavailable marks a runtime-catalog read that could not
// be completed. Callers must fail closed on it: the whole point of the dynamic
// overlay is that a template ID may have been shadowed, and a failed read
// cannot tell us whether it was.
var errBotTemplateRuntimeUnavailable = errors.New("bot template runtime catalog unavailable")

// botCatalogSpaceState is what this request could establish about the Space a
// grant would be read in.
//
// Three states rather than a bool, because "this target has no Space" and "we
// could not tell which Space this target is in" lead to opposite decisions and
// were previously the same `false`. Collapsing them meant a transient group-row
// read failure handed back the static card of a template ID that had already
// been shadowed by an active dynamic version — the exact fallback D6 forbids,
// because the static card is materially different content under a name the
// producer believes it has replaced.
type botCatalogSpaceState uint8

const (
	// botSpaceUnavailable — the Space could not be established, or was
	// established and then refused: a DB error, an ambiguous multi-Space Bot, a
	// rejected X-Space-ID, a DM peer outside the Bot's Space. No grant may be
	// read for it, and no dynamic decision may be answered from static policy.
	// It is the zero value on purpose: a principal nobody filled in authorizes
	// nothing.
	botSpaceUnavailable botCatalogSpaceState = iota
	// botSpaceGlobalOnly — established, and there is no Space: a group that
	// belongs to no Space, or a Bot with no membership at all. Only
	// global-scoped grants can apply, which is what the empty scope sentinel
	// already means in the grant table.
	botSpaceGlobalOnly
	// botSpaceScoped — established, with an identity. Exact-over-global.
	botSpaceScoped
)

// botCatalogPrincipal is the authenticated identity one decision is made for.
// It is always server-derived: the bot ID from the bot token, the Space from
// the strict resolver below. No part of it comes from the request payload.
type botCatalogPrincipal struct {
	BotID   string
	SpaceID string
	Space   botCatalogSpaceState
}

func (p botCatalogPrincipal) catalogPrincipal() cardtmpl.CatalogPrincipal {
	return cardtmpl.CatalogPrincipal{
		Kind: cardtmpl.CatalogPrincipalBot, ID: p.BotID, SpaceID: p.SpaceID,
	}
}

// grantReadable reports whether a grant lookup for this principal means
// anything. It is false only for botSpaceUnavailable, where the scope the grant
// would be read in is precisely what we failed to establish.
func (p botCatalogPrincipal) grantReadable() bool {
	return p.Space != botSpaceUnavailable
}

// access is the single place a render's authorization identity comes from.
//
// The ref gate and cardtmpl.Render both re-resolve the grant, and until this
// existed they resolved it from *different* Space values — the gate from the
// target-authoritative principal, the render from env.SpaceID, which send.go
// populates for DMs only. A Bot holding an exact grant for the target's Space
// therefore passed the gate and was then refused by the render, making
// per-Space grants — the primary Bot grant shape — inert for every group and
// thread. Handing the render the same value the gate decided on is what makes
// the two agree by construction instead of by coincidence.
func (p botCatalogPrincipal) access(purpose cardtmpl.CatalogPurpose) cardtmpl.CatalogAccess {
	return cardtmpl.CatalogAccess{Purpose: purpose, Principal: p.catalogPrincipal()}
}

// resolveBotGrantSpaceID is the authorization-grade Space resolver.
//
// It shares its signals with resolveBotActiveSpaceID but not its fallbacks:
//
//   - App Bot scope=space — the strongest server-authoritative signal, taken
//     directly from the authenticated context.
//   - An explicit X-Space-ID header — honoured only when the Bot is authorized
//     for that Space. An unauthorized or unverifiable header is a hard refusal
//     and never falls through to a different Space, which is precisely the
//     downgrade the legacy resolver would allow.
//   - Otherwise the Bot's own membership, and only when it is unambiguous. A
//     Bot in several Spaces with no hint has no single answer, and picking the
//     earliest-joined one would silently move a producer's grants between
//     tenants.
func (ba *BotAPI) resolveBotGrantSpaceID(c *wkhttp.Context, robotID string) (string, botCatalogSpaceState) {
	// One guard up front rather than a mix of guarded and unguarded uses: a nil
	// context has no signals at all, and "no signals" is a refusal.
	if c == nil {
		return "", botSpaceUnavailable
	}
	querier := ba.spaceQuerierOrDefault()
	if querier == nil {
		return "", botSpaceUnavailable
	}
	if scope, _ := c.Get(CtxKeyAppBotScope); scope == "space" {
		if v, ok := c.Get(CtxKeyAppBotSpaceID); ok {
			if s, _ := v.(string); s != "" {
				// The Space still has to be active (review P2-2, yujiawei).
				// Every sibling resolver requires s.status = 1 —
				// queryGroupSpaceID, isBotSpaceAuthorized, isUserSpaceMember —
				// and this fast path did not, so a grant scoped to a Space an
				// operator had disabled stayed advertised by the profile while
				// send refused it: the same manifest/send divergence as P1-1,
				// reached a different way.
				//
				// isBotSpaceAuthorized is the right check rather than a bare
				// status lookup: its app_bot branch is exactly "scope=space Bot
				// whose own SpaceID matches the target, and the target is
				// active", so it verifies the claim rather than only the Space.
				//
				// This is a DB read on what used to be a context-only path, and
				// it is affordable because it is unreachable with the gates off:
				// botCatalogPrincipalFor and botSendCatalogPrincipal both return
				// before calling this when dynamicCatalogEnabled() is false, so
				// no ungated deployment acquires a query here.
				authorized, err := querier.isBotSpaceAuthorized(robotID, s)
				if err != nil {
					ba.Warn("bot_grant_space_appbot_unverifiable",
						zap.String("robotID", robotID), zap.String("space", s), zap.Error(err))
					return "", botSpaceUnavailable
				}
				if !authorized {
					ba.Warn("bot_grant_space_appbot_rejected",
						zap.String("robotID", robotID), zap.String("space", s))
					return "", botSpaceUnavailable
				}
				return s, botSpaceScoped
			}
		}
	}
	if c.Request != nil {
		if hint := strings.TrimSpace(c.GetHeader("X-Space-ID")); hint != "" {
			authorized, err := querier.isBotSpaceAuthorized(robotID, hint)
			if err != nil {
				ba.Warn("bot_grant_space_hint_unverifiable",
					zap.String("robotID", robotID), zap.String("hint", hint), zap.Error(err))
				return "", botSpaceUnavailable
			}
			if !authorized {
				ba.Warn("bot_grant_space_hint_rejected",
					zap.Bool("x_space_id_header_rejected", true),
					zap.String("robotID", robotID), zap.String("hint", hint))
				return "", botSpaceUnavailable
			}
			return hint, botSpaceScoped
		}
	}
	primary, all, err := querier.querySpaceIDsByRobotID(robotID)
	if err != nil {
		// Including dbr.ErrNotFound, which this query returns for "no rows".
		//
		// An earlier revision read that sentinel as "this Bot belongs to no
		// Space" and answered botSpaceGlobalOnly. It is not that determination,
		// because a whole production population has no space_member row *and is
		// still authorized for Spaces*: platform App Bots are visible in every
		// active Space and never get a membership insert (db.go's
		// isBotSpaceAuthorized, branch 2). For them the sentinel meant "global
		// scope" while isBotSpaceAuthorized would happily have confirmed any
		// active Space — so the two answers disagreed, and which one a request
		// got was decided by whether it sent an X-Space-ID header. Omitting the
		// header selected the global scope, where an exact revoke tombstone for
		// the target Space is not even visible to the grant query. A revoke was
		// bypassable by dropping one header.
		//
		// Membership absence therefore cannot be read as a Space determination
		// here at all. It is one more thing this resolver could not establish.
		if !errors.Is(err, dbr.ErrNotFound) {
			ba.Warn("bot_grant_space_lookup_failed",
				zap.String("robotID", robotID), zap.Error(err))
		}
		return "", botSpaceUnavailable
	}
	if len(all) > 1 {
		// The caller must disambiguate with X-Space-ID rather than have the
		// server guess: picking one would silently move a producer's grants
		// between tenants.
		ba.Warn("bot_grant_space_ambiguous",
			zap.Bool("multi_space_membership", true),
			zap.String("robotID", robotID), zap.Strings("all_space_ids", all))
		return "", botSpaceUnavailable
	}
	if primary == "" {
		// A membership row that names no Space contradicts the query that
		// produced it. Treat it as unreadable rather than inventing a meaning.
		return "", botSpaceUnavailable
	}
	return primary, botSpaceScoped
}

// dynamicCatalogEnabled reports whether any dynamic decision can be reached at
// all. When it is false there is nothing to authorize against, so resolving the
// caller's Space would be pure cost: a Space lookup this deployment did not
// perform before PR-C, plus — for a multi-Space Bot — an ambiguity warn on
// every poll of an endpoint that never touched the database.
//
// It goes through the catalog's own accessor rather than the process global.
// The two can differ — an instance resolver exists precisely to override the
// global one — and if the gate that decides whether to resolve a Space read a
// different source than the gate that decides whether to use it, grants would
// be silently ignored in one direction and paid for pointlessly in the other.
func (c *botCardTemplateCatalog) dynamicCatalogEnabled() bool {
	source := c.authorizationSource()
	return source != nil && source.NewSendEnabled()
}

func (ba *BotAPI) botCatalogPrincipalFor(c *wkhttp.Context, robotID string) botCatalogPrincipal {
	if !ba.cardTemplates.dynamicCatalogEnabled() {
		return botCatalogPrincipal{BotID: robotID}
	}
	spaceID, state := ba.resolveBotGrantSpaceID(c, robotID)
	return botCatalogPrincipal{BotID: robotID, SpaceID: spaceID, Space: state}
}

// botSendCatalogPrincipal resolves the Space one *send* is authorized against.
//
// The target decides, not the sender. A Bot can belong to Space A and still be
// a member of a group that lives in Space B; authorizing that send against
// Space A's grants would let a grant in one tenant deliver cards into another.
// So for a group the Space comes from the authoritative group row, for a thread
// from its parent group's row, and only a personal DM falls back to the Bot's
// own context — where there is no target row to consult.
//
// Every failure is a refusal rather than a fallback: a malformed thread channel
// ID, a missing group row and a DB error all leave the principal unresolved,
// which fails the dynamic decision closed while leaving static policy intact.
func (ba *BotAPI) botSendCatalogPrincipal(
	c *wkhttp.Context,
	robotID, channelID string,
	channelType uint8,
) botCatalogPrincipal {
	if !ba.cardTemplates.dynamicCatalogEnabled() {
		// Same reasoning as botCatalogPrincipalFor: with the gate closed the
		// send path resolves purely from static policy, so it must not start
		// reading group rows to answer a question nobody will ask.
		return botCatalogPrincipal{BotID: robotID}
	}
	switch channelType {
	case common.ChannelTypeGroup.Uint8():
		return ba.botCatalogPrincipalForGroup(robotID, channelID)
	case common.ChannelTypeCommunityTopic.Uint8():
		parts := strings.SplitN(channelID, threadChannelIDSeparator, 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
			return botCatalogPrincipal{BotID: robotID}
		}
		return ba.botCatalogPrincipalForGroup(robotID, parts[0])
	default:
		// A DM has no target row to read a Space from, so the Bot's own context
		// supplies it — but the peer has to actually be in that Space. D6 asks
		// for *both* principals to be valid members: without the peer check a
		// Bot could use its own Space's grant to deliver a card to somebody
		// outside that Space, which is a cross-tenant delivery dressed up as a
		// DM.
		principal := ba.botCatalogPrincipalFor(c, robotID)
		if principal.Space != botSpaceScoped {
			// Nothing to check the peer against: either we could not establish
			// a Space (already refusing) or there is none to be outside of.
			return principal
		}
		return ba.requireDMPeerInSpace(principal, channelID, robotID)
	}
}

// requireDMPeerInSpace withdraws the Space unless the DM's peer holds an active
// membership in it. Every outcome here is botSpaceUnavailable rather than
// botSpaceGlobalOnly: a peer we could not verify, and a peer we verified to be
// outside the Space, are both reasons this Bot's grants must not be read — not
// evidence that the target has no Space. Reading the global grant instead would
// turn "you may send this anywhere" into a way around the tenant check that
// just refused.
func (ba *BotAPI) requireDMPeerInSpace(
	principal botCatalogPrincipal,
	channelID, robotID string,
) botCatalogPrincipal {
	peer := strings.TrimSpace(dmPeerUID(channelID, robotID))
	if peer == "" {
		return botCatalogPrincipal{BotID: robotID}
	}
	querier := ba.spaceQuerierOrDefault()
	if querier == nil {
		return botCatalogPrincipal{BotID: robotID}
	}
	member, err := querier.isUserSpaceMember(peer, principal.SpaceID)
	if err != nil {
		ba.Warn("bot_grant_dm_peer_lookup_failed",
			zap.String("robotID", robotID), zap.String("spaceID", principal.SpaceID), zap.Error(err))
		return botCatalogPrincipal{BotID: robotID}
	}
	if !member {
		ba.Warn("bot_grant_dm_peer_outside_space",
			zap.Bool("dm_peer_outside_space", true),
			zap.String("robotID", robotID), zap.String("spaceID", principal.SpaceID))
		return botCatalogPrincipal{BotID: robotID}
	}
	return principal
}

// dmPeerUID extracts the other side of a personal DM channel. WuKongIM routes
// person channels by the peer's bare uid, so for a Bot-initiated DM the channel
// ID is the recipient; the guard against a self-channel keeps a malformed
// request from being read as "the Bot is its own peer".
func dmPeerUID(channelID, robotID string) string {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" || channelID == strings.TrimSpace(robotID) {
		return ""
	}
	return channelID
}

func (ba *BotAPI) botCatalogPrincipalForGroup(robotID, groupNo string) botCatalogPrincipal {
	querier := ba.spaceQuerierOrDefault()
	if querier == nil {
		return botCatalogPrincipal{BotID: robotID}
	}
	spaceID, spaceActive, err := querier.queryGroupSpaceID(groupNo)
	if err != nil {
		// Including dbr.ErrNotFound: a group row we cannot read tells us nothing
		// about which tenant this send lands in, so the grant scope is
		// unavailable rather than absent.
		if !errors.Is(err, dbr.ErrNotFound) {
			ba.Warn("bot_grant_group_space_lookup_failed",
				zap.String("robotID", robotID), zap.String("groupNo", groupNo), zap.Error(err))
		}
		return botCatalogPrincipal{BotID: robotID}
	}
	if strings.TrimSpace(spaceID) == "" {
		// The row loaded and names no Space. That is a determination: this group
		// belongs to no tenant, so only a global grant can authorize a send into
		// it. It is emphatically not the same as the read failing above.
		return botCatalogPrincipal{BotID: robotID, Space: botSpaceGlobalOnly}
	}
	if !spaceActive {
		// The group names a Space that is no longer active. Reading that Space's
		// grants would honour a scope the rest of the system already refuses —
		// membership, the X-Space-ID header and the DM peer check all require an
		// active Space. A disabled Space is not "no Space" either: falling
		// through to global scope would let a global grant deliver where the
		// scoped one has been switched off.
		ba.Warn("bot_grant_group_space_inactive",
			zap.Bool("group_space_inactive", true),
			zap.String("robotID", robotID), zap.String("groupNo", groupNo),
			zap.String("spaceID", spaceID))
		return botCatalogPrincipal{BotID: robotID}
	}
	return botCatalogPrincipal{BotID: robotID, SpaceID: spaceID, Space: botSpaceScoped}
}

// resolveSendRef implements the D6 decision table for one template ID. The
// rows, in the order the code checks them:
//
//	new-send gate closed          → static policy only, no runtime DB access
//	no activation row             → static policy version, if the policy has one
//	pointer → a static exact      → that exact, but only if the policy lists it;
//	                                a static exact is code-reviewed policy, so no
//	                                business grant is consulted for it
//	pointer → dynamic, granted    → the dynamic exact shadows the static version
//	pointer → dynamic, ungranted  → nothing for this ID; the static version does
//	                                NOT reappear, because the operator's intent
//	                                was to replace it
//	pointer → dynamic, blocked    → nothing for this ID, same reasoning
//	pointer → dynamic, Space
//	  unavailable                 → nothing for this ID, same reasoning: an
//	                                unreadable grant scope is an unreadable
//	                                grant
//	runtime DB unreachable        → typed error; callers fail closed
//
// The "nothing for this ID" rows are the ones worth restating: once an operator
// points a template ID at a dynamic version, falling back to the static card of
// the same ID would send materially different content under a name the producer
// believes it has replaced. Refusing is the safe answer.
//
// Note where the Space state is consulted, and where it is not. Only the last
// dynamic row reads it. An earlier revision short-circuited to static policy the
// moment the Space was unresolved, before any read — which meant a transient
// group-row failure produced exactly the fallback the rows above forbid, and
// did it silently. Whether an ID has been shadowed is a fact about the
// activation pointer, not about the caller, so it is now always read when the
// gate is open; the Space state only decides whether a grant can be honoured.
func (c *botCardTemplateCatalog) resolveSendRef(
	ctx context.Context,
	principal botCatalogPrincipal,
	id cardtmpl.ID,
) (botTemplateRef, error) {
	return c.resolveSendRefFrom(ctx, principal, id, nil)
}

// resolveSendRefFrom is resolveSendRef with an optional pre-resolved snapshot
// for this principal. The manifest resolves every policy ID in one batch and
// passes the result here; the send path passes nil and reads the single ID it
// needs. Both then run the identical decision, so the manifest cannot come to a
// different conclusion than the send it is advertising.
func (c *botCardTemplateCatalog) resolveSendRefFrom(
	ctx context.Context,
	principal botCatalogPrincipal,
	id cardtmpl.ID,
	batch map[cardtmpl.ID]cardtmpl.RuntimeAuthorization,
) (botTemplateRef, error) {
	staticRef, hasStatic := c.staticSendVersion(id)
	source := c.authorizationSource()
	// Gate closed or no resolver installed: no activation pointer can exist to
	// shadow anything, so the static half of the answer is the whole answer and
	// this deployment acquires no runtime DB dependency. The caller's Space is
	// deliberately *not* part of this condition — see the table above.
	if source == nil || !source.NewSendEnabled() {
		if hasStatic {
			return staticRef, nil
		}
		return botTemplateRef{}, nil
	}
	auth, ok := batch[id]
	if !ok {
		var err error
		auth, err = source.LoadAuthorization(ctx, cardtmpl.RuntimeAuthorizationQuery{
			ID: id, Principal: principal.catalogPrincipal(),
		})
		if err != nil {
			if errors.Is(err, cardtmpl.ErrRuntimeCatalogDisabled) {
				// A disabled pointer is a deliberate operator state rather than
				// an outage, but the outcome matches the ungranted row: this ID
				// is not sendable, and it does not revert to its static version.
				return botTemplateRef{}, nil
			}
			return botTemplateRef{}, errors.Join(errBotTemplateRuntimeUnavailable, err)
		}
	}
	return c.decideSendRef(id, staticRef, hasStatic, auth, principal.Space), nil
}

// resolveStaticSendAuthorizations pre-resolves every statically advertised ID in
// one round trip when the store offers a batch read. A nil result is not a
// failure: the caller falls back to one read per ID, which is what a store
// without the batch interface gets.
func (c *botCardTemplateCatalog) resolveStaticSendAuthorizations(
	ctx context.Context,
	principal botCatalogPrincipal,
) (map[cardtmpl.ID]cardtmpl.RuntimeAuthorization, error) {
	source := c.authorizationSource()
	if source == nil || !source.NewSendEnabled() || len(c.staticSendIDs) == 0 {
		return nil, nil
	}
	batch, ok := source.(cardtmpl.RuntimeAuthorizationBatchStore)
	if !ok {
		return nil, nil
	}
	resolved, err := batch.LoadAuthorizations(ctx, c.staticSendIDs, principal.catalogPrincipal())
	if err != nil {
		// Per contract a disabled template is reported per ID rather than as a
		// batch-wide error, so anything that reaches here really is an outage
		// and the manifest fails closed rather than advertising a version that
		// may already have been shadowed.
		return nil, errors.Join(errBotTemplateRuntimeUnavailable, err)
	}
	return resolved, nil
}

// decideSendRef is the pure half of the table above, split out so the matrix is
// testable without a store.
func (c *botCardTemplateCatalog) decideSendRef(
	id cardtmpl.ID,
	staticRef botTemplateRef,
	hasStatic bool,
	auth cardtmpl.RuntimeAuthorization,
	space botCatalogSpaceState,
) botTemplateRef {
	if auth.Activation.Status == cardtmpl.RuntimeActivationDisabled {
		// A disabled pointer is a deliberate operator state rather than an
		// outage, but the outcome matches the ungranted row: this ID is not
		// sendable, and it does not revert to its static version. The per-ID
		// resolver learns this from ErrRuntimeCatalogDisabled; the batch
		// resolver reports it as an activation status, because failing a whole
		// batch over one disabled template would cost every other template in
		// it a separate round trip.
		return botTemplateRef{}
	}
	if auth.Version == "" {
		if hasStatic {
			return staticRef
		}
		return botTemplateRef{}
	}
	pointed := botTemplateRef{ID: id, Version: auth.Version}
	if auth.Artifact.Source == cardtmpl.RuntimeSourceStatic {
		if _, listed := c.sendAllowed[pointed]; listed {
			return pointed
		}
		return botTemplateRef{}
	}
	if space == botSpaceUnavailable {
		// The pointer says this ID is now a dynamic version, and we could not
		// establish the scope its grant would be read in. Both answers are
		// therefore unavailable — the dynamic one because no grant is readable,
		// the static one because it has been replaced. Returning staticRef here
		// is the D6 violation this guard exists to prevent, and it is reachable
		// from an ordinary transient failure: a group-row read that times out.
		return botTemplateRef{}
	}
	if auth.Artifact.Blocked || !auth.Grant.Allows(cardtmpl.CatalogPurposeNewSend) {
		return botTemplateRef{}
	}
	return pointed
}

// staticSendVersion returns the code-reviewed advertised version for an ID.
func (c *botCardTemplateCatalog) staticSendVersion(id cardtmpl.ID) (botTemplateRef, bool) {
	ref, ok := c.staticSendByID[id]
	return ref, ok
}

func (c *botCardTemplateCatalog) authorizationSource() cardtmpl.RuntimeAuthorizationSource {
	if c == nil {
		return nil
	}
	if c.authorization != nil {
		return c.authorization
	}
	return cardtmpl.DefaultAuthorizationSource()
}

// advertisedSendRefs is the effective advertised set for one principal: every
// static-policy ID resolved through the table above, plus every additional
// template the Bot holds an active send grant on.
//
// A capability manifest is a hint, not a permit. It is assembled from several
// snapshots on purpose — binding it into one would suggest a guarantee that
// send deliberately does not honour, since send always re-resolves.
func (c *botCardTemplateCatalog) advertisedSendRefs(
	ctx context.Context,
	principal botCatalogPrincipal,
) ([]botTemplateRef, error) {
	if c == nil {
		return nil, errBotTemplateCatalogUnavailable
	}
	seen := make(map[cardtmpl.ID]struct{}, len(c.staticSendByID))
	refs := make([]botTemplateRef, 0, len(c.staticSendByID))
	staticAuth, err := c.resolveStaticSendAuthorizations(ctx, principal)
	if err != nil {
		return nil, err
	}
	for _, id := range c.staticSendIDs {
		ref, err := c.resolveSendRefFrom(ctx, principal, id, staticAuth)
		if err != nil {
			return nil, err
		}
		seen[id] = struct{}{}
		if ref.ID != "" {
			refs = append(refs, ref)
		}
	}

	source := c.authorizationSource()
	// Beyond the static policy IDs the manifest only has grants to go on, so an
	// unreadable grant scope really does end the list here — unlike the loop
	// above, there is no activation pointer to check for an ID we only learn
	// about from a grant.
	if source == nil || !source.NewSendEnabled() || !principal.grantReadable() {
		return switchMappedRefs(principal, refs), nil
	}
	granted, err := source.ListAuthorizedTemplates(ctx, principal.catalogPrincipal(), 0)
	if err != nil {
		return nil, errors.Join(errBotTemplateRuntimeUnavailable, err)
	}
	for _, candidate := range granted {
		if _, done := seen[candidate.ID]; done {
			continue
		}
		seen[candidate.ID] = struct{}{}
		if ref := c.decideSendRef(candidate.ID, botTemplateRef{}, false,
			candidate.Authorization, principal.Space); ref.ID != "" {
			refs = append(refs, ref)
		}
	}
	return switchMappedRefs(principal, refs), nil
}

// errBotTemplateSwitchUnmapped explains a manifest omission in the log. It is
// never returned to a caller: an ID with no per-Bot switch is simply not part
// of this deployment's advertised set.
var errBotTemplateSwitchUnmapped = errors.New(
	"template id has no per-Bot switch, so the send path fails it closed")

// switchMappedRefs drops any ref whose ID has no per-Bot switch mapping.
//
// This closes the last half of the manifest-versus-send divergence (review
// P1-1, yujiawei). `rejectByBotCardPolicy` fails closed on an unmapped template
// ID *before* the runtime resolver is reached, so a Bot granted send on a
// dynamic template other than ai.reasoning-process used to see it advertised
// here and get a 400 on every send — the exact contradiction
// /v1/bot/card/profile exists to prevent, and the one CapabilityFor's own
// comment promises does not happen ("everything advertised here is sendable").
//
// Two ways to close it, and this is the narrower one: the manifest stops
// advertising what the switch refuses. The alternative — having the switch
// defer to the grant for non-static IDs — would move the per-Bot policy check
// after the ref resolves, which is a change to the send path's ordering rather
// than to a hint. That is a milestone decision (D0: Bot grants do not reach
// template IDs beyond the static policy in this milestone), and it can be taken
// later by deleting this filter; taking it the other way first could not be
// undone as cheaply.
//
// The switch lookup is config-independent in its second return, so this needs
// no per-Bot configuration: "is there a switch for this ID at all" is a
// property of the deployment, not of the Bot. Whether that switch is *on* stays
// where it was — the send gate, and `reasoning_enabled` in the same profile
// response.
func switchMappedRefs(principal botCatalogPrincipal, refs []botTemplateRef) []botTemplateRef {
	kept := refs[:0]
	for _, ref := range refs {
		if _, mapped := botTemplateSwitchFor(ref.ID, robot.BotCardConfig{}); !mapped {
			// Logged rather than dropped in silence: an operator who wrote a
			// grant, saw the row land, and then cannot find the template in the
			// profile would otherwise have nothing to go on.
			logBotTemplateManifestDrop(principal, ref, errBotTemplateSwitchUnmapped)
			continue
		}
		kept = append(kept, ref)
	}
	return kept
}

// CapabilityFor is the request-scoped manifest. Before PR-C this was a process
// constant; it is now resolved per authenticated Bot and Space, because two
// Bots in the same deployment can legitimately be granted different templates.
//
// A runtime read failure is reported to the caller rather than degraded into
// the static manifest: advertising the static version of an ID that may have
// been shadowed would tell a producer it can send something the send path will
// refuse, which is worse than admitting the catalog is briefly unreadable.
func (c *botCardTemplateCatalog) CapabilityFor(
	ctx context.Context,
	principal botCatalogPrincipal,
) (botTemplatingCapability, error) {
	if c == nil {
		return c.Capability(), nil
	}
	refs, err := c.advertisedSendRefs(ctx, principal)
	if err != nil {
		return botTemplatingCapability{}, err
	}
	manifest := botTemplatingCapability{
		Supported: true, Wire: botTemplateWireV1,
		Templates: make([]botTemplateCapability, 0, len(refs)),
	}
	staticByRef := make(map[botTemplateRef]botTemplateCapability, len(c.capability.Templates))
	for _, template := range c.capability.Templates {
		staticByRef[botTemplateRef{ID: cardtmpl.ID(template.ID), Version: template.Version}] = template
	}
	for _, ref := range refs {
		if precomputed, ok := staticByRef[ref]; ok {
			manifest.Templates = append(manifest.Templates, cloneTemplateCapability(precomputed))
			continue
		}
		meta, err := c.catalog.MetaExact(ctx, cardtmpl.CatalogExactRequest{
			Access: botCatalogAccess(cardtmpl.CatalogPurposeNewSend, principal.BotID, principal.SpaceID),
			ID:     ref.ID, Version: ref.Version,
		})
		if err != nil {
			// Drop the template, do not fail the manifest — review P1-1.
			//
			// The layer below already chose this policy and documented the
			// reason: loadAdvertisableAuthorizations reads with skipBrokenRow so
			// "a single bad row cannot blank a Bot's entire capability
			// manifest", while an *authorization* read still fails hard. Failing
			// the whole manifest here re-introduced exactly the failure mode
			// that policy exists to prevent, one call up — and since the profile
			// handler turns the error into a 500, one uncompilable artifact
			// among a Bot's grants would take feature detection down for that
			// Bot entirely, on the endpoint whose whole job is to answer when
			// things are partly unavailable.
			//
			// Dropping is also the honest answer: a template whose artifact will
			// not compile is one the send path would refuse anyway, so omitting
			// it keeps the manifest's promise — everything advertised here is
			// sendable — rather than breaking it.
			logBotTemplateManifestDrop(principal, ref, err)
			continue
		}
		capability, err := templateCapabilityFromMeta(meta)
		if err != nil {
			logBotTemplateManifestDrop(principal, ref, err)
			continue
		}
		manifest.Templates = append(manifest.Templates, capability)
	}
	sort.Slice(manifest.Templates, func(i, j int) bool {
		left, right := manifest.Templates[i], manifest.Templates[j]
		if left.ID != right.ID {
			return left.ID < right.ID
		}
		return left.Version < right.Version
	})
	return manifest, nil
}

// requireSendableRef is the send-path authorization boundary for a
// caller-supplied template_ref. It re-resolves rather than trusting whatever
// the producer read from a capability manifest: the manifest may be arbitrarily
// old, and an activate, block or revoke since then must take effect now.
func (c *botCardTemplateCatalog) requireSendableRef(
	ctx context.Context,
	principal botCatalogPrincipal,
	value any,
) (botTemplateRef, error) {
	if c == nil || c.catalog == nil {
		return botTemplateRef{}, errBotTemplateCatalogUnavailable
	}
	ref, err := parseBotTemplateRef(value)
	if err != nil {
		return botTemplateRef{}, err
	}
	allowed, err := c.resolveSendRef(ctx, principal, ref.ID)
	if err != nil {
		return botTemplateRef{}, err
	}
	if allowed != ref {
		// One message for "unknown template", "wrong version" and "not granted"
		// alike: a producer must not be able to use send rejections to
		// enumerate which templates exist or which other Bots hold grants.
		return botTemplateRef{}, fmt.Errorf("%w: not Bot-callable for new send", errBotTemplateRequestInvalid)
	}
	return ref, nil
}

func cloneTemplateCapability(template botTemplateCapability) botTemplateCapability {
	out := botTemplateCapability{
		ID: template.ID, Version: template.Version,
		Views: make([]botTemplateViewCapability, len(template.Views)),
	}
	for i, view := range template.Views {
		out.Views[i] = botTemplateViewCapability{
			Name: view.Name, WireProfile: view.WireProfile,
			States:        append(make([]string, 0, len(view.States)), view.States...),
			SubmitActions: append(make([]string, 0, len(view.SubmitActions)), view.SubmitActions...),
		}
	}
	return out
}

// logBotTemplateManifestDrop records a template omitted from an advertised
// manifest. Silence here would leave an operator who granted a template, saw
// the row land, and then could not find it in the profile with nothing to go
// on — the same reason StaticDiscoverGrants logs its own truncation.
func logBotTemplateManifestDrop(principal botCatalogPrincipal, ref botTemplateRef, err error) {
	liblog.NewTLog("BotCardManifest").Warn("bot card manifest dropped an unreadable template",
		zap.String("bot_id", principal.BotID),
		zap.String("space_id", principal.SpaceID),
		zap.String("template_id", string(ref.ID)),
		zap.String("template_version", ref.Version),
		zap.Error(err))
}
