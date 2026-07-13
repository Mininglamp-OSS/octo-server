package bot_api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardrevision"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/mentionrewrite"
	"github.com/Mininglamp-OSS/octo-server/pkg/richtext"
	"github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// BotSendMessageReq is the request for sendMessage.
type BotSendMessageReq struct {
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
	StreamNo    string `json:"stream_no"`
	// OnBehalfOf — YUJ-1166 / Mininglamp-OSS/octo-server#81 (Persona Clone v0).
	// When non-empty the bot is asking to dispatch as the real user
	// `OnBehalfOf`. Server validates an active OBO grant
	// (grantor=OnBehalfOf, grantee=robotID) AND a per-channel scope row
	// (channel_id, channel_type) before substituting FromUID. Empty / absent
	// preserves legacy behavior (FromUID = robotID). See RFC §5.1 / §5.2.
	OnBehalfOf string                 `json:"on_behalf_of,omitempty"`
	Payload    map[string]interface{} `json:"payload"`
}

// sendMessage handles POST /v1/bot/sendMessage.

func (ba *BotAPI) sendMessage(c *wkhttp.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, cardmsg.MaxSendBodyBytes)
	var req BotSendMessageReq
	if err := c.BindJSON(&req); err != nil {
		respondBotAPIRequestInvalid(c, "")
		return
	}
	if strings.TrimSpace(req.ChannelID) == "" {
		respondBotAPIRequestInvalid(c, "channel_id")
		return
	}
	if req.ChannelType == 0 {
		respondBotAPIRequestInvalid(c, "channel_type")
		return
	}
	if len(req.Payload) == 0 {
		respondBotAPIRequestInvalid(c, "payload")
		return
	}
	// PR#82 review #2 P1-2 + PR#121 R2 + PR#121 R3 — reject any
	// inbound payload that carries a reserved server-only key. Three
	// overlapping namespaces are reserved:
	//   - `__obo_*` (double-underscore prefix): home of the fan-out
	//     gate-3 marker `__obo_processed__` and any future
	//     server-injected OBO field;
	//   - explicit `obo_*` keys injected by buildFanoutCopyReq
	//     (obo_respond_as, obo_grantor_uid, obo_fanout, obo_origin_*,
	//     obo_system_hint);
	//   - `actual_sender_uid` — the prefix-less server-injected
	//     "real bot behind an OBO send" identity set below in the
	//     fan-out marker block (PR#121 R3).
	// Allowing a bot client to set any of them would let a malicious
	// bot suppress its own fan-out copy, spoof the OBO grantor /
	// fan-out routing context downstream, or forge the
	// authenticated-by-server sender identity downstream consumers
	// trust. Membership is owned by pkg/obopayload so this site
	// cannot drift from the user / robot ingress strip or the
	// fan-out listener's gate-3 check. Reject before
	// checkSendPermission / checkOBO so the error is fast and the
	// auth path doesn't run on poisoned input.
	if payloadHasReservedOBOKey(req.Payload) {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOReservedField, nil, nil)
		return
	}

	// card-message-protocol P1：InteractiveCard(=17) 入站 gate。排序在
	// checkSendPermission / checkOBO 之前：(a) Decision 2b 要求 OBO 卡片按
	// 「请求意图」拦截、先于 grant 校验，且覆盖 grantorReplyBypass 子路径
	// （该子路径 fromUID 仍是 bot —— 拒绝是刻意的过度拒绝，P2 复议）；
	// (b) 脏卡片 fail-fast，鉴权路径不跑在毒输入上（与上方 OBO 保留键同序）。
	if cardmsg.IsCardPayload(req.Payload) {
		if !cardmsg.Enabled() {
			// Decision 2 rollout gate：客户端渲染门禁发布前默认关闭。
			httperr.ResponseErrorL(c, errcode.ErrBotAPICardDisabled, nil, nil)
			return
		}
		if strings.TrimSpace(req.OnBehalfOf) != "" {
			httperr.ResponseErrorL(c, errcode.ErrBotAPICardOBOForbidden, nil, nil)
			return
		}
		if err := cardmsg.Validate(req.Payload); err != nil {
			ba.Warn("InteractiveCard payload 校验失败", zap.Error(err), zap.String("channelID", req.ChannelID))
			httperr.ResponseErrorL(c, errcode.ErrBotAPICardInvalid, nil, nil)
			return
		}
	}

	robotID := getRobotIDFromContext(c)
	botKind := getBotKindFromContext(c)

	// PR#82 R7 — the OBO friend-gate bypass is conditional on a
	// validated OBO context. We pledge that here based on the
	// `on_behalf_of` field; checkOBO below independently validates the
	// grant + scope + grantor channel access. If the pledge is false
	// (bot sends as itself) the friend gate falls back to plain
	// IsFriend with no bypass — preventing a bot that holds any
	// unrelated grant from skipping the user opt-in.
	hasOBOContext := strings.TrimSpace(req.OnBehalfOf) != ""

	// Permission check based on bot kind
	if err := ba.checkSendPermission(c, botKind, robotID, req.ChannelID, req.ChannelType, hasOBOContext); err != nil {
		respondSendPermissionError(c, err)
		return
	}

	channelID := ba.resolveSpaceChannelID(robotID, req.ChannelID, req.ChannelType)

	// YUJ-1166 / Mininglamp-OSS#81 Persona Clone OBO:
	// Resolve the dispatch identity. Default = the calling bot. If the bot
	// asks to act on behalf of a real user, validate the grant + scope
	// BEFORE we touch the payload (so a 403 short-circuits the dispatch).
	// Note the order: OBO check runs AFTER checkSendPermission — a bot that
	// can't legitimately reach this channel can't bypass that check by
	// invoking OBO.
	fromUID := robotID
	if strings.TrimSpace(req.OnBehalfOf) != "" {
		// YUJ-1418 — managed-persona DM grantor-reply bypass.
		//
		// When admin (the OBO grantor) DMs the persona-clone bot, the
		// persona service generates an AI reply and naturally calls
		// /v1/bot/sendMessage with on_behalf_of=admin (the persona IS
		// admin). The recipient (channel_id) is also admin — admin's own
		// DM with the bot. Running the standard OBO scope check on this
		// shape rejects: no scope row covers a "grantor speaks to
		// themselves" DM, and creating one would be semantic noise (it
		// would route admin→admin self-DM, not bot→admin reply). Without
		// this bypass every persona reply to its own grantor would 400
		// with `obo not authorized`.
		//
		// Detection: DM channel AND on_behalf_of == channel_id AND the
		// bot has an active grant from this user. When all three hold we
		// fall through to the legacy (non-OBO) bot send path — fromUID
		// stays as the bot, no OBO substitution, no `__obo_processed__`
		// marker, no fan-out machinery — exactly what the grantor would
		// expect when their persona "talks back" to them. Any other
		// shape (on_behalf_of != channel_id, channel is not a DM, no
		// active grant from the recipient) falls through to the strict
		// checkOBO below — the OBO scope check for third-party sends
		// MUST remain strict (issue YUJ-1418 explicitly forbids
		// loosening it).
		grantorReplyBypass := false
		if req.ChannelType == common.ChannelTypePerson.Uint8() && req.OnBehalfOf == req.ChannelID {
			hasGrant, err := ba.botHasActiveGrantFrom(robotID, req.OnBehalfOf)
			if err != nil {
				ba.Error("OBO grantor-reply bypass lookup failed",
					zap.String("bot", robotID),
					zap.String("grantor", req.OnBehalfOf),
					zap.Error(err))
				httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
				return
			}
			grantorReplyBypass = hasGrant
		}

		if !grantorReplyBypass {
			if err := ba.checkOBO(robotID, req.OnBehalfOf, req.ChannelID, req.ChannelType); err != nil {
				if errors.Is(err, ErrOBONotAuthorized) {
					ba.Warn("OBO denied: no active grant or scope",
						zap.String("bot", robotID),
						zap.String("on_behalf_of", req.OnBehalfOf),
						zap.String("channel_id", req.ChannelID),
						zap.Uint8("channel_type", req.ChannelType))
					httperr.ResponseErrorL(c, errcode.ErrBotAPIOBONotAuthorized, nil, nil)
					return
				}
				ba.Error("OBO check failed", zap.Error(err),
					zap.String("bot", robotID), zap.String("on_behalf_of", req.OnBehalfOf))
				httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
				return
			}
			fromUID = req.OnBehalfOf
		} else {
			ba.Info("OBO grantor-reply bypass: bot is replying to its own grantor in DM, sending as bot",
				zap.String("bot", robotID),
				zap.String("grantor", req.OnBehalfOf),
				zap.String("channel_id", req.ChannelID))
		}
	}

	// YUJ-644 / Mininglamp-OSS#33: PERSONAL DM 服务端权威 space_id 注入。
	// WuKongIM 对 DM 仅按裸 uid 路由（无 Space 概念），收端 SpaceFilter 只能依赖
	// payload.space_id；客户端上送任何值（包括缺省 / 伪造）都不可信。
	// 优先使用 gin-context 里 authAppBot 写入的 SpaceID（O(1)，无 DB 调用）；
	// 用户 Bot / 平台级 App Bot 落 querySpaceIDByRobotID。
	//
	// space_id 解析始终基于 robotID (bot)，而不是 OBO 替身的 fromUID。
	// 理由：grant 仅授权身份替换，不应改变租户隔离边界 — bot 的 Space 归属
	// 是部署时确定的，与 grantor 的 Space 归属解耦。
	payload := req.Payload
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		payload = ba.enrichBotPayloadWithSpaceID(c, robotID, payload)
	}

	// YUJ-202 / Mininglamp-OSS#94 / #142 — mention pass-through
	// chokepoint. Same contract as modules/message/api.go: post-#142
	// the helper no longer infers `mention.ais=1` from legacy
	// `mention.all=1` (legacy `@所有人` MUST NOT trigger bots). The
	// helper is now a pass-through that forwards `mention.all`,
	// `mention.humans`, `mention.ais`, and `mention.uids` untouched.
	// The call site is preserved so any future re-introduction of
	// chokepoint normalization (and the symmetric ingress on
	// modules/message/api.go + modules/robot/api.go) lands in one
	// place. ⚠️ F2 (PR#70 Jerry-Xin correctness-critical review): this
	// MUST stay OUTSIDE the `ChannelTypePerson` conditional above —
	// otherwise group / community-topic mention payloads would bypass
	// the chokepoint should we ever need to reinstate the rewrite.
	// Helper is idempotent and safe on nil — see pkg/mentionrewrite.
	payload = mentionrewrite.RewriteMention(payload)

	// card-message-protocol P1 Decision 8：server 权威 plain 收尾。排在信封级
	// enrich（space_id 注入 / mention 顶层键改写）之后，使 512KiB 复检覆盖真实
	// 出站 payload（与 richtext 的 PR#232 口径一致）。Decision 9 保证 enrich 只
	// 触碰信封顶层键、card 树永不被改写；下方 OBO 标记 enrich 对卡片不可达
	// （OBO 卡片已在入站 gate 拒绝）。非 type=17 为 no-op。
	if err := cardmsg.Finalize(payload); err != nil {
		ba.Error("InteractiveCard finalize 失败", zap.Error(err), zap.String("channelID", channelID))
		httperr.ResponseErrorL(c, errcode.ErrBotAPICardInvalid, nil, nil)
		return
	}

	// YUJ-1166 fan-out loop guard #3: mark this message so the fan-out
	// listener (see obo_fanout.go) skips it on the way back through the
	// listener pipeline. Marker key lives in the reserved `__obo_*`
	// namespace (see oboProcessedMarkerKey) which the inbound payload
	// validator above strips off client requests — so the marker is
	// server-only state that a bot cannot forge or suppress. Stored in
	// payload (= message_extra in the persisted MessageResp) so the
	// messages table itself doesn't need an ALTER (out-of-scope row).
	if fromUID != robotID {
		payload = ensureMap(payload)
		payload[oboProcessedMarkerKey] = true
		payload["actual_sender_uid"] = robotID
	}

	// Mininglamp-OSS/octo-server#144 + PR#145 review follow-up:
	// second-pass mention chokepoint (sister call to the user and
	// robot ingresses). When mention.ais=1 in a GROUP channel, expand
	// mention.uids to every bot member of the channel so legacy
	// adapter bots (#137) on the WuKongIM websocket recognise the
	// broadcast. PR #138 only rewrites the /v1/bot/events queue path;
	// this helper covers the websocket dispatch path. channelID uses
	// the space-resolved value so a multi-Space group still hits the
	// right member roster.
	//
	// ⚠️ PR#145 review (Jerry-Xin / lml2468 / yujiawei 2026-05-23):
	// the expansion MUST run on a clone of `payload`, not on `payload`
	// itself. ExpandAisToBotUIDs mutates the inner `mention` sub-map
	// in place, and the in-memory `payload` flows into the persisted
	// MessageResp + the listener pipeline (obo_fanout, reminder
	// writer at modules/message/api_reminders.go) — mutating it here
	// would create one human-visible `[有人@我]` reminder per
	// server-expanded bot member of the group. The clone is used ONLY
	// for the wire bytes; `payload` retains the original
	// caller-supplied `mention.uids`. See pkg/mentionrewrite/clone.go
	// for the clone contract.
	wirePayload := mentionrewrite.CloneForExpansion(payload)
	wirePayload = mentionrewrite.ExpandAisToBotUIDs(wirePayload, req.ChannelType, channelID, ba.fetchBotMemberUIDs)

	// card-message-protocol P1 Decision 3a：ExpandAisToBotUIDs 是 Finalize 之后
	// 唯一会增大 payload 的 mutation（追加频道 bot 成员 UID 到 mention 子表）。
	// Finalize 的 512KiB 复检发生在展开之前，覆盖不到真实出站字节，故对最终
	// wirePayload 再复检一次（PR#543 review：与 richtext PR#232「最后一次 mutation
	// 后复检」不变量对齐）。非 type=17 为 no-op。
	if err := cardmsg.RecheckPayloadSize(wirePayload); err != nil {
		ba.Error("InteractiveCard 出站 payload 超限", zap.Error(err), zap.String("channelID", channelID))
		httperr.ResponseErrorL(c, errcode.ErrBotAPICardInvalid, nil, nil)
		return
	}

	msgReq := &config.MsgSendReq{
		Header: config.MsgHeader{
			RedDot: 1,
		},
		StreamNo:    req.StreamNo,
		ChannelID:   channelID,
		ChannelType: req.ChannelType,
		FromUID:     fromUID,
		Payload:     []byte(util.ToJson(wirePayload)),
	}
	result, err := ba.dispatchMsgSendReq(msgReq)
	if err != nil {
		ba.Error("发送消息失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPISendFailed, nil, nil)
		return
	}

	// Reset typing throttle state
	ba.clearTypingThrottle(robotID, channelID, req.ChannelType)

	c.Response(result)
}

// ensureMap returns a non-nil map, allocating one if needed. Used by the
// OBO marker logic in sendMessage so we never NPE on a payload that arrived
// nil (validation above rejects len==0 but not nil-vs-empty after enrich).
func ensureMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return map[string]interface{}{}
	}
	return m
}

// checkSendPermission verifies the bot has permission to send to the target channel.
//
// PR#82 R7 — `hasOBOContext` signals that the inbound request carries a
// validated `on_behalf_of` field. Only sendMessage can set this true
// (it's the only handler whose request schema has the field, and the
// dispatch path validates it via `checkOBO` immediately after this
// returns). typing / readReceipt / messages-sync must pass false: they
// dispatch AS the bot, never AS a grantor, so they cannot legitimately
// take the OBO friend-gate bypass.
func (ba *BotAPI) checkSendPermission(c *wkhttp.Context, botKind, robotID, channelID string, channelType uint8, hasOBOContext bool) error {
	switch botKind {
	case BotKindApp:
		// Rule 1: App Bot only supports DM
		if channelType != common.ChannelTypePerson.Uint8() {
			return errBotSendPermAppBotDMOnly
		}
		// Rule 2: Must have friend relationship (user opt-in via /v1/robot/apply)
		isFriend, err := ba.userService.IsFriend(robotID, channelID)
		if err != nil {
			ba.Error("failed to verify relationship", zap.Error(err), zap.String("robotID", robotID))
			return errBotSendPermCheckFailed
		}
		if !isFriend {
			return errBotSendPermConvNotStarted
		}
		// Rule 3: Space bot — user must still be a space member (fail-closed)
		if scope, _ := c.Get(CtxKeyAppBotScope); scope == "space" {
			spaceIDStr, _ := c.Get(CtxKeyAppBotSpaceID)
			sid, _ := spaceIDStr.(string)
			if sid == "" {
				ba.Error("space bot missing space_id in context", zap.String("robotID", robotID))
				return errBotSendPermCheckFailed
			}
			isMember, memberErr := ba.isSpaceMember(channelID, sid)
			if memberErr != nil {
				ba.Error("failed to verify space membership", zap.Error(memberErr), zap.String("robotID", robotID))
				return errBotSendPermCheckFailed
			}
			if !isMember {
				return errBotSendPermNotSpaceMember
			}
		}
		return nil

	case BotKindUser:
		if channelType == common.ChannelTypeGroup.Uint8() {
			// Disband guard (WeChat-Work style read-only): once the group is
			// disbanded everyone is read-only, bots included — even an OBO send
			// on behalf of the (former) owner. The deployed WuKongIM
			// /message/send returns HTTP 200 with no failure signal on a disband
			// rejection, so the bot would otherwise believe its send succeeded.
			// We self-check group.status here and return an explicit error.
			// Placed BEFORE the OBO/membership branch so it applies regardless
			// of OBO context.
			if disbanded, err := ba.isGroupDisbanded(channelID); err != nil {
				return err
			} else if disbanded {
				return errBotSendPermGroupDisbanded
			}
			// OBO bypass: when the bot acts on behalf of a grantor who IS a
			// group member, skip the bot's own membership check. The downstream
			// checkOBO validates that the grantor has a legitimate grant+scope
			// (or implicit scope via global_enabled) for this channel.
			if !hasOBOContext {
				// Group: check bot is a group member
				var count int
				err := ba.db.session.SelectBySql(
					"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0",
					channelID, robotID,
				).LoadOne(&count)
				if err != nil {
					ba.Error("查询群成员失败", zap.Error(err))
					return errBotSendPermCheckFailed
				}
				if count == 0 {
					return errBotSendPermNotGroupMember
				}
			}
		} else if channelType == common.ChannelTypeCommunityTopic.Uint8() {
			// Thread: extract parent group_no and verify membership.
			//
			// Disband guard (parity with ChannelTypeGroup above): a disbanded
			// parent group makes its threads read-only too. Parse the parent
			// group_no and self-check status BEFORE the OBO/membership branch so
			// it applies regardless of OBO context.
			topicParts := strings.SplitN(channelID, threadChannelIDSeparator, 2)
			if len(topicParts) != 2 {
				return errBotSendPermBadThreadChan
			}
			if disbanded, err := ba.isGroupDisbanded(topicParts[0]); err != nil {
				return err
			} else if disbanded {
				return errBotSendPermGroupDisbanded
			}
			//
			// PR#121 R7 (YUJ-1671) — OBO bypass parity with Group above.
			// CommunityTopic fan-out (`obo_fanout.go` / mention-gated)
			// already delivers topic messages to clone bots that are
			// NOT parent-group members; the bot must be allowed to reply
			// on behalf of a grantor who IS a member. Without this
			// bypass, `checkSendPermission` rejects the OBO reply with
			// "bot is not a member of this group" before `checkOBO`
			// gets the chance to authorize the grantor. Mirrors the
			// `if !hasOBOContext` skip used by ChannelTypeGroup just
			// above. Live grantor membership is still re-verified by
			// `checkOBO` → `grantorCanReadChannel`, so the bypass does
			// not widen access (the grantor must currently be in the
			// parent group, or the implicit-scope membership branch
			// will fail).
			if !hasOBOContext {
				parts := strings.SplitN(channelID, threadChannelIDSeparator, 2)
				if len(parts) != 2 {
					return errBotSendPermBadThreadChan
				}
				var count int
				err := ba.db.session.SelectBySql(
					"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0",
					parts[0], robotID,
				).LoadOne(&count)
				if err != nil {
					ba.Error("查询群成员失败", zap.Error(err))
					return errBotSendPermCheckFailed
				}
				if count == 0 {
					return errBotSendPermNotGroupMember
				}
			}
		} else if channelType == common.ChannelTypePerson.Uint8() {
			// DM: creator can always talk to their bot; otherwise check friend
			// OR the OBO managed-persona implicit bypass (PR#82 R6 P0).
			robot := getRobotFromContext(c)
			isCreator := robot != nil && robot.CreatorUID == channelID
			if !isCreator {
				// isFriendOrOBOBypass tries the friend lookup first; if
				// the bot isn't a friend of the target AND the caller
				// signals OBO context, it falls back to the OBO bypass —
				// "any active grant covering this channel where the
				// grantor still has a relation with the target". The
				// bypass is required by the managed-persona path: admin
				// grants the clone bot james OBO over admin↔bob; james
				// MUST be able to send (as admin) to bob even though
				// james and bob are not friends. PR#82 R7 — the bypass
				// is GATED on hasOBOContext so plain bot sends, typing,
				// readReceipt, and messages-sync (which dispatch AS the
				// bot, not AS the grantor) cannot piggy-back on an
				// unrelated grant to skip the user opt-in friend gate.
				// See modules/bot_api/obo_friend_gate.go for the
				// rationale and the regression that motivated R7.
				allowed, err := ba.isFriendOrOBOBypass(robotID, channelID, channelType, hasOBOContext)
				if err != nil {
					ba.Error("查询好友关系失败", zap.Error(err))
					return errBotSendPermCheckFailed
				}
				if !allowed {
					return errBotSendPermNotFriend
				}
			}
		}
		return nil

	default:
		ba.Error("unknown bot kind", zap.String("botKind", botKind), zap.String("robotID", robotID))
		return errBotSendPermCheckFailed
	}
}

// isSpaceMember checks if a user is a member of the given space.
func (ba *BotAPI) isSpaceMember(uid, spaceID string) (bool, error) {
	var count int
	err := ba.db.session.SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1",
		spaceID, uid,
	).LoadOne(&count)
	if err != nil {
		ba.Error("isSpaceMember query failed", zap.String("uid", uid), zap.String("spaceID", spaceID), zap.Error(err))
		return false, err
	}
	return count > 0, nil
}

// isGroupDisbanded reports whether the group identified by groupNo is in the
// disbanded (read-only) state. Used by checkSendPermission to block bot sends
// to disbanded groups/threads — the deployed WuKongIM /message/send gives no
// failure signal on a disband rejection, so octo-server must self-check.
// Infra failures are logged and surfaced as errBotSendPermCheckFailed (fail
// closed) rather than letting a DB hiccup silently allow the send.
func (ba *BotAPI) isGroupDisbanded(groupNo string) (bool, error) {
	// db 未初始化时（如单元测试 stub 不注入 db），无法查询群状态。
	// 跳过 disband 检查而非 fail-closed：调用方（fanoutForMessage）在没有
	// 完整 DB 的环境下不应被 disband guard 阻断。生产环境 db 始终已初始化。
	if ba.db == nil || ba.db.session == nil {
		return false, nil
	}
	var status int
	err := ba.db.session.SelectBySql(
		"SELECT status FROM `group` WHERE group_no=?",
		groupNo,
	).LoadOne(&status)
	if err != nil {
		ba.Error("isGroupDisbanded query failed", zap.String("groupNo", groupNo), zap.Error(err))
		return false, errBotSendPermCheckFailed
	}
	return status == group.GroupStatusDisband, nil
}

// ==================== Read Receipt ====================

// BotReadReceiptReq is the request for readReceipt.
type BotReadReceiptReq struct {
	ChannelID   string   `json:"channel_id"`
	ChannelType uint8    `json:"channel_type"`
	MessageIDs  []string `json:"message_ids"`
}

// readReceipt handles POST /v1/bot/readReceipt.
func (ba *BotAPI) readReceipt(c *wkhttp.Context) {
	var req BotReadReceiptReq
	if err := c.BindJSON(&req); err != nil {
		respondBotAPIRequestInvalid(c, "")
		return
	}
	if strings.TrimSpace(req.ChannelID) == "" {
		respondBotAPIRequestInvalid(c, "channel_id")
		return
	}

	robotID := getRobotIDFromContext(c)
	channelType := uint8(common.ChannelTypePerson)
	if req.ChannelType > 0 {
		channelType = req.ChannelType
	}

	// Permission check: bot must have access to this channel.
	// PR#82 R7 — readReceipt has no `on_behalf_of` field and always
	// dispatches AS the bot, so the OBO friend-gate bypass MUST NOT
	// apply here (hasOBOContext=false).
	botKind := getBotKindFromContext(c)
	if err := ba.checkSendPermission(c, botKind, robotID, req.ChannelID, channelType, false); err != nil {
		respondSendPermissionError(c, err)
		return
	}

	// If channel_type was defaulted (0) for an App Bot, verify the channel_id is
	// not actually a group — otherwise callers could bypass the DM-only restriction.
	if req.ChannelType == 0 && botKind == BotKindApp {
		var groupCount int
		grpErr := ba.db.session.SelectBySql(
			"SELECT COUNT(*) FROM `group` WHERE group_no=? AND is_deleted=0", req.ChannelID,
		).LoadOne(&groupCount)
		if grpErr != nil {
			ba.Error("verify channel type failed", zap.Error(grpErr), zap.String("channelID", req.ChannelID))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
			return
		}
		if groupCount > 0 {
			httperr.ResponseErrorL(c, errcode.ErrBotAPIAppBotDMOnly, nil, nil)
			return
		}
	}

	channelID := ba.resolveSpaceChannelID(robotID, req.ChannelID, channelType)

	// 1. Clear conversation unread badge
	err := ba.ctx.IMClearConversationUnread(config.ClearConversationUnreadReq{
		UID:         robotID,
		ChannelID:   channelID,
		ChannelType: channelType,
		Unread:      0,
	})
	if err != nil {
		ba.Warn("清除未读计数失败", zap.Error(err))
	}

	// 2. Message-level read receipt
	if len(req.MessageIDs) > 100 {
		respondBotAPILimitExceeded(c, "message_ids", 100)
		return
	}
	if len(req.MessageIDs) > 0 {
		messageIDs := make([]int64, 0, len(req.MessageIDs))
		for _, idStr := range req.MessageIDs {
			mid, parseErr := strconv.ParseInt(idStr, 10, 64)
			if parseErr != nil {
				ba.Warn("解析消息ID失败", zap.String("id", idStr), zap.Error(parseErr))
				continue
			}
			messageIDs = append(messageIDs, mid)
		}
		if len(messageIDs) == 0 {
			c.ResponseOK()
			return
		}

		fakeChannelID := channelID
		if channelType == common.ChannelTypePerson.Uint8() {
			fakeChannelID = common.GetFakeChannelIDWith(channelID, robotID)
		}

		searchChannelID := channelID
		if channelType == common.ChannelTypePerson.Uint8() {
			searchChannelID = robotID
		}
		syncMsg, err := ba.ctx.IMSearchMessages(&config.MsgSearchReq{
			ChannelID:   searchChannelID,
			ChannelType: channelType,
			MessageIds:  messageIDs,
			LoginUID:    robotID,
		})
		if err != nil {
			ba.Error("查询消息失败", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
			return
		}
		if syncMsg != nil && len(syncMsg.Messages) > 0 {
			valueStrings := make([]string, 0, len(syncMsg.Messages))
			valueArgs := make([]interface{}, 0, len(syncMsg.Messages)*4)
			for _, msg := range syncMsg.Messages {
				valueStrings = append(valueStrings, "(?, ?, ?, ?)")
				valueArgs = append(valueArgs, msg.MessageID, fakeChannelID, channelType, robotID)
			}
			stmt := fmt.Sprintf(`INSERT INTO member_readed (message_id, channel_id, channel_type, uid) VALUES %s ON DUPLICATE KEY UPDATE message_id=VALUES(message_id)`,
				strings.Join(valueStrings, ","))
			_, err = ba.db.session.InsertBySql(stmt, valueArgs...).Exec()
			if err != nil {
				ba.Error("插入已读记录失败", zap.Error(err))
				httperr.ResponseErrorL(c, errcode.ErrBotAPIStoreFailed, nil, nil)
				return
			}

			// Write Redis cache for read receipt aggregation
			go func() {
				defer func() {
					if r := recover(); r != nil {
						ba.Error("goroutine panic",
							zap.Any("recover", r),
							zap.String("stack", string(debug.Stack())),
						)
					}
				}()
				for _, msg := range syncMsg.Messages {
					messageIDStr := strconv.FormatInt(msg.MessageID, 10)
					cacheData := map[string]interface{}{
						"MessageID":      msg.MessageID,
						"MessageIDStr":   messageIDStr,
						"MessageSeq":     msg.MessageSeq,
						"FromUID":        msg.FromUID,
						"ChannelID":      fakeChannelID,
						"ChannelType":    channelType,
						"LoginUID":       robotID,
						"ReqChannelID":   channelID,
						"ReqChannelType": channelType,
					}
					jsonStr, _ := json.Marshal(cacheData)
					ba.ctx.GetRedisConn().SetAndExpire(
						fmt.Sprintf("readedCount:%s", messageIDStr),
						string(jsonStr),
						time.Hour*24*7,
					)
				}
			}()
		}
	}

	c.ResponseOK()
}

// ==================== Message Edit ====================

// botMessageEdit handles POST /v1/bot/message/edit.
func (ba *BotAPI) botMessageEdit(c *wkhttp.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, cardmsg.MaxSendBodyBytes)
	var req struct {
		MessageID   string `json:"message_id"`
		MessageSeq  uint32 `json:"message_seq"`
		ChannelID   string `json:"channel_id"`
		ChannelType uint8  `json:"channel_type"`
		ContentEdit string `json:"content_edit"`
	}
	if err := c.BindJSON(&req); err != nil {
		respondBotAPIRequestInvalid(c, "")
		return
	}
	if req.MessageID == "" {
		respondBotAPIRequestInvalid(c, "message_id")
		return
	}
	if req.ChannelID == "" {
		respondBotAPIRequestInvalid(c, "channel_id")
		return
	}
	if strings.TrimSpace(req.ContentEdit) == "" {
		respondBotAPIRequestInvalid(c, "content_edit")
		return
	}

	robotID := getRobotIDFromContext(c)
	if robotID == "" {
		ba.respondBotAPIIdentityMissing(c)
		return
	}

	// 解散守卫（企业微信式只读）：群解散后禁止 bot 编辑历史消息。
	if req.ChannelType == common.ChannelTypeGroup.Uint8() {
		if disbanded, err := ba.isGroupDisbanded(req.ChannelID); err != nil {
			ba.Error("查询群是否已解散错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
			return
		} else if disbanded {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIGroupDisbanded, nil, nil)
			return
		}
	} else if req.ChannelType == common.ChannelTypeCommunityTopic.Uint8() {
		parts := strings.SplitN(req.ChannelID, "____", 2)
		if len(parts) != 2 || parts[0] == "" {
			// fail-closed：畸形 thread channel_id 拒绝操作，
			// 与 message/api.go 的 thread disband guard 保持一致。
			ba.Error("解析子区频道ID失败（edit）", zap.String("channelID", req.ChannelID))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
			return
		}
		if disbanded, err := ba.isGroupDisbanded(parts[0]); err != nil {
			ba.Error("查询父群是否已解散错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
			return
		} else if disbanded {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIGroupDisbanded, nil, nil)
			return
		}
	}

	// Permission: bot can only edit its own messages
	var msgFromUID string
	var msgPayload []byte // 原消息 payload（Decision 7 需要判定目标消息是否卡片）
	if req.MessageSeq > 0 {
		resp, err := ba.ctx.IMGetWithChannelAndSeqs(req.ChannelID, req.ChannelType, robotID, []uint32{req.MessageSeq})
		if err != nil {
			ba.Error("查询消息错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
			return
		}
		if resp == nil || len(resp.Messages) == 0 {
			httperr.ResponseErrorL(c, errcode.ErrBotAPIMessageNotFound, nil, nil)
			return
		}
		if req.MessageID != strconv.FormatInt(resp.Messages[0].MessageID, 10) {
			// P0（PR#548 review）：硬拒绝 message_id/message_seq 不匹配 —— 与用户编辑
			// 路径 ErrMessageIDSeqMismatch 对称。所有权只在 (channel, seq) 上校验,而
			// message_extra 写入按调用方另给的 message_id 落行（UNIQUE(message_id) 单
			// 表,ON DUPLICATE KEY UPDATE 命中任意归属行）。warn-only 会形成 confused-
			// deputy:攻击 bot 用自己拥有的 seq + 受害 message_id 覆盖他人卡片的
			// content_edit,进而伪造该卡被点击时下发给受害 bot 的动作面。规范推荐 bot
			// 省略 message_seq 由服务端解析（走下方 seq==0 安全分支）,合法调用方不受影响。
			ba.Warn("message_id与message_seq不匹配,拒绝",
				zap.String("req_message_id", req.MessageID),
				zap.Int64("actual_message_id", resp.Messages[0].MessageID),
				zap.Uint32("message_seq", req.MessageSeq),
			)
			respondBotAPIRequestInvalid(c, "message_id")
			return
		}
		msgFromUID = resp.Messages[0].FromUID
		msgPayload = resp.Messages[0].Payload
	} else {
		msgIDInt, parseErr := strconv.ParseInt(req.MessageID, 10, 64)
		if parseErr != nil {
			ba.Warn("message_id格式错误", zap.String("message_id", req.MessageID), zap.Error(parseErr))
			respondBotAPIRequestInvalid(c, "message_id")
			return
		}
		syncResp, err := ba.ctx.IMSearchMessages(&config.MsgSearchReq{
			ChannelID:   req.ChannelID,
			ChannelType: req.ChannelType,
			MessageIds:  []int64{msgIDInt},
			LoginUID:    robotID,
		})
		if err != nil {
			ba.Error("查询消息错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
			return
		}
		if syncResp == nil || len(syncResp.Messages) == 0 {
			httperr.ResponseErrorL(c, errcode.ErrBotAPIMessageNotFound, nil, nil)
			return
		}
		if syncResp.Messages[0].MessageSeq == 0 {
			httperr.ResponseErrorL(c, errcode.ErrBotAPIMessageNotDelivered, nil, nil)
			return
		}
		msgFromUID = syncResp.Messages[0].FromUID
		msgPayload = syncResp.Messages[0].Payload
		req.MessageSeq = syncResp.Messages[0].MessageSeq
	}
	if msgFromUID != robotID {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIMessageEditForbidden, nil, nil)
		return
	}

	// App Bot: DM-only + must have friend relationship
	botKind := getBotKindFromContext(c)
	if botKind == BotKindApp {
		if req.ChannelType != common.ChannelTypePerson.Uint8() {
			httperr.ResponseErrorL(c, errcode.ErrBotAPIAppBotDMOnly, nil, nil)
			return
		}
		isFriend, fErr := ba.userService.IsFriend(robotID, req.ChannelID)
		if fErr != nil {
			ba.Error("verify app bot friend failed", zap.Error(fErr), zap.String("robotID", robotID), zap.String("channelID", req.ChannelID))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
			return
		}
		if !isFriend {
			httperr.ResponseErrorL(c, errcode.ErrBotAPIConversationNotStarted, nil, nil)
			return
		}
	}

	// content_edit 收敛：编辑语义为整体替换正文，plain 服务端重算不信客户端。
	// P2 D6：bot 卡片编辑路径解锁 —— 目标消息与编辑体的「是否卡片」必须一致
	// （跨类型变异 card↔非card 双向拒绝，不变量 (a)）；两者皆卡片走 cardmsg 与 send
	// 对称的 write-strict 校验 + 权威 plain 重算 + D9 card_seq；两者皆非卡片走
	// richtext（=14）原有路径。user/robot 编辑路径对卡片仍永久拒绝（各自守卫）。
	origIsCard := cardmsg.IsCardRawPayload(msgPayload)
	editIsCard := cardmsg.IsCardContentEdit(req.ContentEdit)
	if origIsCard != editIsCard {
		ba.Warn("卡片编辑跨类型变异,拒绝", zap.String("messageID", req.MessageID),
			zap.Bool("origIsCard", origIsCard), zap.Bool("editIsCard", editIsCard))
		respondBotAPIRequestInvalid(c, "content_edit")
		return
	}
	var (
		err        error
		cardSeq    int64
		hasCardSeq bool
	)
	if editIsCard {
		// P2（PR#548 review）：撤回/删除门禁 —— 已撤回或全局删除的卡片不可再编辑,
		// 与动作端点（api_card_action.go）的撤回门禁对称,避免在已回收的卡片上重填
		// content_edit（该 content_edit 是动作端点信任的生效帧）。行不存在=未编辑过,
		// 非撤回,放行。
		var lifecycle struct {
			Revoke    int `db:"revoke"`
			IsDeleted int `db:"is_deleted"`
		}
		if lErr := ba.ctx.DB().SelectBySql("SELECT `revoke`, is_deleted FROM message_extra WHERE message_id=?", req.MessageID).LoadOne(&lifecycle); lErr != nil && lErr != dbr.ErrNotFound {
			ba.Error("查询卡片撤回/删除状态失败", zap.Error(lErr), zap.String("messageID", req.MessageID))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
			return
		}
		if lifecycle.Revoke == 1 || lifecycle.IsDeleted == 1 {
			ba.Warn("卡片已撤回/删除,拒绝编辑", zap.String("messageID", req.MessageID),
				zap.Int("revoke", lifecycle.Revoke), zap.Int("isDeleted", lifecycle.IsDeleted))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIMessageNotFound, nil, nil)
			return
		}
		// P2 D6：type-17 content_edit 跑与 send 同一套 cardmsg 校验（白名单/大小/
		// URL/profile 协商，v2 帧在此合法）+ Finalize 重算权威 plain，返回 canonical
		// JSON 落库。card_seq（D9）随后做 CAS。
		normalized, cErr := cardmsg.NormalizeContentEdit(req.ContentEdit)
		if cErr != nil {
			ba.Warn("InteractiveCard content_edit 校验失败", zap.Error(cErr), zap.String("messageID", req.MessageID))
			httperr.ResponseErrorL(c, errcode.ErrBotAPICardInvalid, nil, nil)
			return
		}
		req.ContentEdit = normalized
		cardSeq, hasCardSeq = cardmsg.CardSeqFromContentEdit(normalized)
	} else {
		// 图文混排 RichText(=14)：编辑写入口对 content_edit 做与 send 路径对称的
		// write-strict 校验 + 权威 plain 重算。非 14 / 非 JSON 体为 no-op。MD5 去重
		// hash 落在 normalize 后的 canonical 体上。
		normalizedEdit, rErr := richtext.NormalizeContentEdit(req.ContentEdit)
		if rErr != nil {
			ba.Warn("RichText content_edit 校验失败", zap.Error(rErr), zap.String("messageID", req.MessageID))
			respondBotAPIRequestInvalid(c, "content_edit")
			return
		}
		req.ContentEdit = normalizedEdit
	}

	contentEdit := dbr.NewNullString(req.ContentEdit).String
	contentMD5 := util.MD5(contentEdit)

	// content_edit_hash 去重短路（发生在下方 D9 card_seq CAS 之前）。这**不会**掩盖
	// D9 stale 冲突：card_seq 是信封字段，已包含在 normalize 后的 canonical 体里，
	// 故也计入 contentMD5。因此去重命中 ⟺ 与已存最新帧逐字节相同（含相同 card_seq）
	// = 幂等重发，返回 OK 正确；任何 stale/乱序帧（更低或同 seq 但不同内容）hash 必
	// 不同 → 不命中 → 走下方 CAS 得到 409。（PR#548 review 非阻塞项，已加测试佐证。）
	var existCount int
	err = ba.ctx.DB().Select("count(*)").From("message_extra").Where("message_id=? and content_edit_hash=?", req.MessageID, contentMD5).LoadOne(&existCount)
	if err != nil {
		ba.Error("查询是否存在相同正文失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}
	if existCount > 0 {
		c.ResponseOK()
		return
	}

	fakeChannelID := req.ChannelID
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		fakeChannelID = common.GetFakeChannelIDWith(robotID, req.ChannelID)
	}

	editedAt := int(time.Now().Unix())
	if hasCardSeq {
		// P2 D9：带 card_seq 的卡片编辑走条件 CAS。并发首帧在 InnoDB 下可能因
		// insert-intention gap-lock 互相死锁（1213）—— 死锁是瞬时的，有界重试即可
		// 化解：一旦某帧提交、行已存在，后续事务的 SELECT ... FOR UPDATE 只取记录锁，
		// 不再产生 gap-lock 死锁；重试中重读到已提交的更高 seq 后要么应用要么按 CAS
		// 拒绝，"最高 seq 必胜"不变量成立。
		var (
			conflict bool
			casErr   error
		)
		for attempt := 0; attempt < cardSeqCASMaxAttempts; attempt++ {
			conflict, casErr = ba.cardSeqCASWrite(req.MessageID, req.MessageSeq, fakeChannelID, req.ChannelType, contentEdit, contentMD5, editedAt, cardSeq)
			if casErr == nil || !isRetriableMySQLLockErr(casErr) {
				break
			}
			time.Sleep(time.Duration(attempt+1) * 3 * time.Millisecond)
		}
		if casErr != nil {
			ba.Error("card_seq CAS 写入编辑内容失败！", zap.Error(casErr), zap.String("messageID", req.MessageID))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIStoreFailed, nil, nil)
			return
		}
		if conflict {
			ba.Warn("card_seq 乱序/迟到帧,拒绝", zap.String("messageID", req.MessageID), zap.Int64("incoming", cardSeq))
			httperr.ResponseErrorL(c, errcode.ErrBotAPICardSeqConflict, nil, nil)
			return
		}
	} else {
		// 非 card_seq 帧:last-write-wins(D9,单写 bot 与既有编辑零行为变化)。仍须在
		// 行锁内分配 version —— 锁外前置分配会让并发写(与 CAS 帧、或与另一非 CAS 帧)
		// 的 version 分配序 ≠ 提交序:低 version 的 upsert 若后提交会覆盖已提交的高
		// version,令 delta-sync(version>? 游标)永久漏掉终帧(PR#548 review P1，与 CAS
		// 分支同一类单调性缺陷)。有界重试化解 InnoDB 死锁(与 CAS 写同口径)。
		var werr error
		for attempt := 0; attempt < cardSeqCASMaxAttempts; attempt++ {
			werr = ba.cardVersionInLockWrite(req.MessageID, req.MessageSeq, fakeChannelID, req.ChannelType, contentEdit, contentMD5, editedAt)
			if werr == nil || !isRetriableMySQLLockErr(werr) {
				break
			}
			time.Sleep(time.Duration(attempt+1) * 3 * time.Millisecond)
		}
		if werr != nil {
			ba.Error("card 编辑(无 card_seq)写入失败！", zap.Error(werr), zap.String("messageID", req.MessageID))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIStoreFailed, nil, nil)
			return
		}
	}

	// P2 D10：非 transient 卡片帧追加修订历史（best-effort —— content_edit 已是
	// 权威状态，history 是次级面，append 失败只记日志、不阻断编辑）。richtext 编辑
	// 与 transient 进度帧不入历史。
	if editIsCard && !cardmsg.TransientFromContentEdit(req.ContentEdit) {
		rev := cardrevision.Revision{
			MessageID:   req.MessageID,
			ChannelID:   fakeChannelID,
			ChannelType: req.ChannelType,
			Content:     dbr.NewNullString(req.ContentEdit),
			Plain:       cardmsg.PlainFromContentEdit(req.ContentEdit),
			EditorUID:   robotID,
			EditedAt:    int64(editedAt),
		}
		if hasCardSeq {
			rev.CardSeq = dbr.NewNullInt64(cardSeq)
		}
		if rerr := ba.cardRevisions.AppendFrame(rev); rerr != nil {
			ba.Error("卡片修订历史追加失败(不影响编辑)", zap.Error(rerr), zap.String("messageID", req.MessageID))
		}
	}

	err = ba.ctx.SendCMD(config.MsgCMDReq{
		NoPersist:   true,
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		FromUID:     robotID,
		CMD:         common.CMDSyncMessageExtra,
	})
	if err != nil {
		ba.Error("发送 CMD 同步失败！", zap.Error(err))
	}

	c.ResponseOK()
}

// cardSeqCASMaxAttempts 是 D9 CAS 遇 InnoDB 死锁/锁等待超时的有界重试次数。
const cardSeqCASMaxAttempts = 5

// cardSeqCASWrite 执行 P2 D9 的 card_seq 条件 CAS 写：事务内 SELECT ... FOR UPDATE
// 锁住该消息的 message_extra 行（不存在则取 next-key 锁串行化并发首帧），比对已存
// card_seq —— 新值 ≤ 已存（非 NULL）→ 返回 conflict=true（乱序/迟到帧，什么都不写）；
// 否则连 card_seq 一并 upsert 并提交。返回的 err 若是可重试锁错误（1213/1205），
// 由调用方有界重试化解（死锁瞬时；重试后重读已提交的更高 seq，仍按 CAS 判定）。
//
// P1-2（PR#548 review）：message_extra.version 在**拿到行锁之后**才分配。GenSeq
// 是每-channel 单调递增,而 delta-sync 按 version 升序取增量帧
// （db_message_extra.go:110 `where version>? order by version asc`）。若 version
// 在锁外前置分配,一个更低 version 的帧可能赢下 card_seq CAS 却以更小 version 覆盖行
// —— 已同步到更高 version 的客户端从此收不到该终帧（行在库里正确、但对 delta-sync
// 不可见,违反 D6"各端收敛"/D9"no lost-update"）。锁内分配把竞争写的 GenSeq 调用按
// 提交顺序串行化：赢家写（最大 seq、最后一次 advancing 写）必得最大 version。
func (ba *BotAPI) cardSeqCASWrite(messageID string, messageSeq uint32, fakeChannelID string, channelType uint8, contentEdit, contentMD5 string, editedAt int, cardSeq int64) (conflict bool, err error) {
	tx, err := ba.ctx.DB().Begin()
	if err != nil {
		return false, err
	}
	defer tx.RollbackUnlessCommitted()

	var stored struct {
		CardSeq dbr.NullInt64 `db:"card_seq"`
		Hash    string        `db:"content_edit_hash"`
	}
	if selErr := tx.SelectBySql("SELECT card_seq, content_edit_hash FROM message_extra WHERE message_id=? FOR UPDATE", messageID).LoadOne(&stored); selErr != nil && selErr != dbr.ErrNotFound {
		return false, selErr
	}
	if stored.CardSeq.Valid && stored.CardSeq.Int64 >= cardSeq {
		// stored == cardSeq 且内容逐字节相同 → 并发/重复的幂等重试（两个相同帧都过了
		// 事务前 content_edit_hash 短路、在此串行化），已存的就是这帧：返回成功、不再写，
		// 避免对幂等重试误报 409（PR#548 review P2）。stored > cardSeq、或相等但内容不同，
		// 才是真正的乱序/迟到帧 → conflict。
		if stored.CardSeq.Int64 == cardSeq && stored.Hash == contentMD5 {
			return false, nil
		}
		return true, nil
	}
	// 锁内分配 version（见函数注释 P1-2；跨副本单调性范围见 cardVersionInLockWrite 注释
	// H2/P1-b）：仅 advancing 写才消费 version,冲突帧不消费。
	version, err := ba.ctx.GenSeq(fmt.Sprintf("%s:%s", common.MessageExtraSeqKey, fakeChannelID))
	if err != nil {
		return false, err
	}
	if _, err = tx.InsertBySql(
		"INSERT INTO message_extra (message_id,message_seq,channel_id,channel_type,content_edit,content_edit_hash,edited_at,version,card_seq) VALUES (?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE content_edit=VALUES(content_edit),content_edit_hash=VALUES(content_edit_hash),edited_at=VALUES(edited_at),version=VALUES(version),card_seq=VALUES(card_seq)",
		messageID, messageSeq, fakeChannelID, channelType, contentEdit, contentMD5, editedAt, version, cardSeq,
	).Exec(); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return false, nil
}

// cardVersionInLockWrite 无 card_seq 的卡片编辑(LWW)写入：在行锁内分配 version 后
// upsert content_edit —— 与 cardSeqCASWrite 取同一把 message_id 行锁(不存在则 next-key
// 锁),使 version 分配序 == 提交序,杜绝锁外前置分配的低 version 覆盖并发写(CAS 或非
// CAS)已提交的高 version 造成的 delta-sync 终帧丢失(PR#548 review P1)。不比较、不写
// card_seq(保持 LWW 语义与既有 card_seq 值不变)。
//
// 单调性范围(PR#548 review H2/P1-b，纠正过度声明)：version 取自 ctx.GenSeq —— octo-lib
// 的**进程内 HiLo 分配器**(进程级 seqMap+Mutex,每进程预留 seqStep=1000 的号段,DB 只补
// 号段)。故「分配序==提交序」仅在**单进程内**成立:多副本部署下,持较低号段的实例可能在较
// 高 version 提交后才抢到行锁、写入更低的 version;delta-sync(db_message_extra.go
// `where version>? order by version asc`)会永久跳过该终帧,直到该客户端整表 resync。此为
// **既有性质**(origin/main 富文本编辑亦用同一 GenSeq,本 PR 不触 config/seq.go),非本 PR
// 引入 —— 本函数只关掉**进程内**竞态,TestBotCardEditMixedFrameVersionMonotonicIM 亦只验
// 单进程。彻底的跨副本单调需把 version 换成频道级全序源(DB/Redis 原子计数),牵动所有
// version 载体(含富文本),超出 #548 范围,列为后续。
func (ba *BotAPI) cardVersionInLockWrite(messageID string, messageSeq uint32, fakeChannelID string, channelType uint8, contentEdit, contentMD5 string, editedAt int) error {
	tx, err := ba.ctx.DB().Begin()
	if err != nil {
		return err
	}
	defer tx.RollbackUnlessCommitted()

	// 取行锁串行化 version 分配(与 cardSeqCASWrite 同一把锁);行不存在时 FOR UPDATE
	// 取 next-key 锁,让并发首帧也串行。查出的 card_seq 仅作 LoadOne 目标、刻意不参与下方
	// upsert(LWW 不动 card_seq) —— 本 SELECT 唯一目的是持锁(PR#548 review nit)。
	var lockHold dbr.NullInt64
	if selErr := tx.SelectBySql("SELECT card_seq FROM message_extra WHERE message_id=? FOR UPDATE", messageID).LoadOne(&lockHold); selErr != nil && selErr != dbr.ErrNotFound {
		return selErr
	}
	version, err := ba.ctx.GenSeq(fmt.Sprintf("%s:%s", common.MessageExtraSeqKey, fakeChannelID))
	if err != nil {
		return err
	}
	if _, err = tx.InsertBySql(
		"INSERT INTO message_extra (message_id,message_seq,channel_id,channel_type,content_edit,content_edit_hash,edited_at,version) VALUES (?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE content_edit=VALUES(content_edit),content_edit_hash=VALUES(content_edit_hash),edited_at=VALUES(edited_at),version=VALUES(version)",
		messageID, messageSeq, fakeChannelID, channelType, contentEdit, contentMD5, editedAt, version,
	).Exec(); err != nil {
		return err
	}
	return tx.Commit()
}

// isRetriableMySQLLockErr 识别 InnoDB 可重试锁错误：1213 死锁 / 1205 锁等待超时
// （与 modules/conversation_ext 同口径，errors.As 兼容包装错误）。
func isRetriableMySQLLockErr(err error) bool {
	var mysqlErr *mysql.MySQLError
	if !errors.As(err, &mysqlErr) {
		return false
	}
	return mysqlErr.Number == 1213 || mysqlErr.Number == 1205
}
