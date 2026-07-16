package messages_search

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"go.uber.org/zap"
)

// checkChannelAccess enforces the channel-membership gate shared by all four
// /_search* endpoints: a caller may only search conversations they can
// already read. The four-step parity goal here is "search must not return
// hits the ordinary read path would have hidden", so the checks below
// mirror the gates in modules/message/api_channel_files.go (group + p2p)
// and modules/thread (thread parent + status).
//
//   - p2p (1)   — caller may search the DM iff one of the following passes
//     (mirrors modules/user/api_pinned.go::validateChannelAccess so the
//     gate stays consistent with the read/pinned path):
//     1. peer == caller   — notes-to-self;
//     2. peer is a bot created by the caller (own bot, blacklist skipped);
//     3. peer is a bot NOT created by the caller — must also be friends
//     (matches modules/robot/event.go "用户与Bot非好友关系，拒绝转发消息");
//     4. peer is a real user AND (same-Space membership OR friend) —
//     Space membership covers the enterprise contact-book deployment
//     where the friend table is empty; friend fallback preserves the
//     legacy non-Space deployments;
//     Then bidirectional blacklist is consulted on every non-own-bot
//     path (real-user AND other-owned-bot friend): blacklist hides past
//     messages and a search-through-DM attack needs the same gate as a
//     read. Own-bot path skips blacklist (a user can't blacklist their
//     own bot meaningfully).
//
//   - group (2) — group must exist AND caller must be an *active* member.
//     Disbanded groups are NOT rejected: 企业微信式 disband 语义（产品决策 2026-06）
//     keeps history & search readable after disband. A leftover group_member row
//     on a disbanded group does not leak access because ExistMemberActive
//     excludes removed (is_deleted=1) / blacklisted (status!=Normal) users.
//
//   - thread (5) — channel_id must parse, the thread must still exist
//     (GetThread maps "not found" + "deleted" to err), AND caller must be
//     an active member of the parent group. Archived threads (status=2)
//     remain readable, matching the read path.
//
// Non-members get NOT_FOUND with resource=channel (anti-enumeration: the
// response must not reveal whether the group / thread / peer exists).
// Lookup errors fail closed with INTERNAL_ERROR for the friend/blacklist
// and group lookups; thread GetThread errors collapse with the existence
// check into NOT_FOUND so we don't leak whether the thread row is present
// or only the DB happened to be down (anti-enumeration over operational
// signal).
func (h *Handler) checkChannelAccess(c *wkhttp.Context, channelType uint8, channelID, loginUID string) bool {
	switch channelType {
	case channelTypePerson:
		return h.checkP2PAccess(c, channelID, loginUID)
	case channelTypeGroup:
		return h.checkGroupAccess(c, channelID, loginUID)
	case channelTypeThread:
		return h.checkThreadAccess(c, channelID, loginUID)
	default:
		// Unreachable in practice: validate.go rejects unknown channel
		// types before this check runs. Kept fail-closed (defense in
		// depth) so a future caller that bypasses validation can never
		// inherit implicit access.
		h.Warn("checkChannelAccess: unexpected channel_type",
			zap.Uint8("channel_type", channelType),
			zap.String("channel_id", channelID),
			zap.String("uid", loginUID))
		respondNotFound(c, "channel")
		return false
	}
}

// checkP2PAccess gates DM search behind the same access semantics as
// modules/user/api_pinned.go::validateChannelAccess: notes-to-self → own
// bot → other-user bot (must be friends) → real-user (same-Space OR
// friend) → bidirectional blacklist.
//
// Why bot judgement runs before Space judgement: bots have no
// `space_member` row, so consulting Space first would deny a caller
// searching their own bot's DM.
//
// Why Space + friend (not Space only): in Space (enterprise contact-book)
// mode the friend table is near-empty (mostly system bots); in non-Space
// deployments friend is authoritative. Either-or covers both.
//
// All denials render NOT_FOUND/resource=channel (anti-enumeration);
// every DB error fail-closes with INTERNAL_ERROR. Bidirectional
// blacklist applies on both the real-user path AND the other-owned-bot
// friend path — once a bot is conversational like any peer, blacklisting
// it (or being blacklisted by its owner) must hide DM history the same
// way as for a real user. Only the own-bot path skips blacklist.
func (h *Handler) checkP2PAccess(c *wkhttp.Context, peerUID, loginUID string) bool {
	if peerUID == loginUID {
		// "Notes-to-self" channel; mirrors the read path's `if peer != self`
		// guard. Friend / blacklist / bot / Space checks are not meaningful.
		return true
	}

	// Step 1: bot classification (BEFORE Space, see func doc). Own bot
	// short-circuits past Space, friend, and blacklist.
	isRobot, creatorUID, err := h.userService.QueryPeerRobotInfo(peerUID)
	if err != nil {
		h.Error("p2p access check failed: QueryPeerRobotInfo",
			zap.Error(err),
			zap.String("uid", loginUID),
			zap.String("peer", peerUID))
		respondInternal(c)
		return false
	}
	if isRobot {
		if creatorUID == loginUID {
			// Own bot: skip blacklist (you can't blacklist your own bot
			// meaningfully) and skip Space/friend gating.
			return true
		}
		// Other user's bot: must be friends, matching the robot module's
		// "用户与Bot非好友关系，拒绝转发消息" rule. Friend-only is not
		// enough on its own — fall through to the bidirectional blacklist
		// gate below so a friend bot that has since been blocked (either
		// direction) stays hidden from search, matching the legacy
		// pre-Space behavior.
		isFriend, ferr := h.userService.IsFriend(loginUID, peerUID)
		if ferr != nil {
			h.Error("p2p access check failed: IsFriend (other-bot path)",
				zap.Error(ferr),
				zap.String("uid", loginUID),
				zap.String("peer", peerUID))
			respondInternal(c)
			return false
		}
		if !isFriend {
			respondNotFound(c, "channel")
			return false
		}
		// fall through to Step 3 (blacklist); skip Step 2 since bots
		// have no space_member row.
	} else {
		// Step 2: real-user path — allow if same Space OR friend. Space
		// covers enterprise contact-book deployments where friend is empty;
		// friend fallback preserves legacy non-Space deployments.
		allowed := false
		// spaceID 走 principal（决策十）：真人等价于 GetSpaceID(c)；bot 无 space_member
		// 行、Space 为空（as-bot 的 P2P 分支由 #C 用 bot 谓词接管，不落此真人分支）。
		spaceID := h.principal(c).SpaceID()
		if spaceID != "" {
			sameSpace, serr := h.userService.AreSpaceMembers(spaceID, loginUID, peerUID)
			if serr != nil {
				h.Error("p2p access check failed: AreSpaceMembers",
					zap.Error(serr),
					zap.String("uid", loginUID),
					zap.String("peer", peerUID),
					zap.String("space_id", spaceID))
				respondInternal(c)
				return false
			}
			allowed = sameSpace
		}
		if !allowed {
			isFriend, ferr := h.userService.IsFriend(loginUID, peerUID)
			if ferr != nil {
				h.Error("p2p access check failed: IsFriend (real-user fallback)",
					zap.Error(ferr),
					zap.String("uid", loginUID),
					zap.String("peer", peerUID))
				respondInternal(c)
				return false
			}
			allowed = isFriend
		}
		if !allowed {
			respondNotFound(c, "channel")
			return false
		}
	}

	// Step 3: bidirectional blacklist (real-user + other-owned-bot friend
	// paths; own-bot path returned earlier). Either side blocking the
	// other hides DM history both for the blocker (their preference) and
	// the blocked party (anti-harassment). Search must respect both, since
	// search bypasses IM kernel's blacklist filter.
	blockedByMe, err := h.userService.ExistBlacklist(loginUID, peerUID)
	if err != nil {
		h.Error("p2p access check failed: ExistBlacklist (me→peer)",
			zap.Error(err),
			zap.String("uid", loginUID),
			zap.String("peer", peerUID))
		respondInternal(c)
		return false
	}
	if blockedByMe {
		respondNotFound(c, "channel")
		return false
	}
	blockedByPeer, err := h.userService.ExistBlacklist(peerUID, loginUID)
	if err != nil {
		h.Error("p2p access check failed: ExistBlacklist (peer→me)",
			zap.Error(err),
			zap.String("uid", loginUID),
			zap.String("peer", peerUID))
		respondInternal(c)
		return false
	}
	if blockedByPeer {
		respondNotFound(c, "channel")
		return false
	}
	return true
}

// botCheckP2PAccess gates DM search for an as-bot principal (#C / YUJ-50). The
// bot subject's rule is deliberately narrower than the real-user
// checkP2PAccess: allow iff the bot and the peer are friends, and NOTHING else.
//
//   - No Space segment: a bot has no `space_member` row, so the same-Space
//     branch of the real-user gate can never fire for it. Consulting Space
//     here would only add a lookup that is structurally always false.
//   - No P2P blacklist, either direction (决策已确认):
//     · bot→peer  — a bot can't blacklist anyone (addBlacklist is a real-user
//     session feature, user/api.go), so this side is always empty;
//     · peer→bot  — deliberately NOT consulted: a bot must stay able to search
//     a conversation it is party to even if the peer has blocked it.
//     So blacklistPolicy for userBotPrincipal is `none` (principal.go) and this
//     gate never calls ExistBlacklist.
//   - No bot-classification of the peer (QueryPeerRobotInfo): friendship alone
//     decides. The peer may be a real user or another bot; the single IsFriend
//     edge is the whole predicate.
//
// Direction: IsFriend(botUID, peer) — the same single-direction query the
// real-user gate uses (authz.go, real-user path) and the exact enumeration dual
// of #E's GetFriends(botUID) in buildBotAllowlist. Keeping this one edge (not a
// two-call bidirectional check) is what makes 决策九 hold: 单频道门放行 ⇔ 出现在
// global allowlist. Friend rows are stored as mutual pairs, so this still means
// "互为好友".
//
// Denials render NOT_FOUND/resource=channel (anti-enumeration, identical to the
// real-user path); an IsFriend lookup error fail-closes with INTERNAL_ERROR.
func (h *Handler) botCheckP2PAccess(c *wkhttp.Context, botUID, peerUID string) bool {
	isFriend, err := h.userService.IsFriend(botUID, peerUID)
	if err != nil {
		h.Error("as-bot p2p access check failed: IsFriend",
			zap.Error(err),
			zap.String("bot_uid", botUID),
			zap.String("peer", peerUID))
		respondInternal(c)
		return false
	}
	if !isFriend {
		respondNotFound(c, "channel")
		return false
	}
	return true
}

// checkGroupAccess fail-closes if the group is missing, then gates on active
// membership. Disbanded groups are NOT rejected here: per the 企业微信式 disband
// semantics (产品决策 2026-06), history & search stay readable after disband —
// 发消息由 WuKongIM 的 disband flag 拦截，搜索是只读路径。Leftover group_member
// rows cannot leak read access to non-members because ExistMemberActive below
// excludes removed/blacklisted users; disband does not clear member rows but a
// removed member's row is is_deleted=1 so ExistMemberActive returns false.
func (h *Handler) checkGroupAccess(c *wkhttp.Context, groupNo, loginUID string) bool {
	groupModel, err := h.groupService.GetGroupWithGroupNo(groupNo)
	if err != nil {
		h.Error("group access check failed: GetGroupWithGroupNo",
			zap.Error(err),
			zap.String("group_no", groupNo))
		respondInternal(c)
		return false
	}
	if groupModel == nil {
		respondNotFound(c, "channel")
		return false
	}
	active, err := h.groupService.ExistMemberActive(groupNo, loginUID)
	if err != nil {
		h.Error("group access check failed: ExistMemberActive",
			zap.Error(err),
			zap.String("group_no", groupNo))
		respondInternal(c)
		return false
	}
	if !active {
		respondNotFound(c, "channel")
		return false
	}
	return true
}

// checkThreadAccess parses the composite `{group_no}____{short_id}` and
// gates on (a) the thread row still existing (GetThread collapses
// not-found / deleted / underlying DB error into err), (b) caller being
// an active member of the parent group. Archived threads are still
// searchable because the read path still surfaces them.
//
// GetThread error → NOT_FOUND is intentional even on transient DB
// failure: leaking "the thread exists but DB is down" (vs "thread does
// not exist") gives an enumeration oracle. Operators see the cause in
// the upstream (group / thread service) logs.
func (h *Handler) checkThreadAccess(c *wkhttp.Context, channelID, loginUID string) bool {
	parsedGroup, shortID, err := thread.ParseChannelID(channelID)
	if err != nil {
		respondNotFound(c, "channel")
		return false
	}
	if _, err := h.threadService.GetThread(parsedGroup, shortID, loginUID); err != nil {
		// Three-way collapse (not-found / deleted / DB error) per the
		// thread.IService contract — see thread/service.go::GetThread.
		// We also want anti-enumeration over operational signal here, so
		// keep all three on the NOT_FOUND surface even though the DB
		// case is technically a transient infra failure.
		respondNotFound(c, "channel")
		return false
	}
	active, err := h.groupService.ExistMemberActive(parsedGroup, loginUID)
	if err != nil {
		h.Error("thread access check failed: ExistMemberActive",
			zap.Error(err),
			zap.String("group_no", parsedGroup))
		respondInternal(c)
		return false
	}
	if !active {
		respondNotFound(c, "channel")
		return false
	}
	return true
}
