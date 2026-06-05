package bot_api

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/mentionrewrite"
	"github.com/Mininglamp-OSS/octo-server/pkg/richtext"
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

	// Permission: bot can only edit its own messages
	var msgFromUID string
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
			ba.Warn("message_id与message_seq不匹配，保持旧行为继续执行",
				zap.String("req_message_id", req.MessageID),
				zap.Int64("actual_message_id", resp.Messages[0].MessageID),
				zap.Uint32("message_seq", req.MessageSeq),
			)
		}
		msgFromUID = resp.Messages[0].FromUID
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

	// 图文混排 RichText(=14)：编辑写入口对 content_edit 做与 send 路径对称的
	// write-strict 校验 + 权威 plain 重算（契约 §2，plain 服务端重算不信客户端）。
	// 编辑语义为整体替换 content blocks；非 14 / 非 JSON 体为 no-op。脏/超限 payload
	// 落库前以错误拒绝。MD5 去重 hash 落在 normalize 后的 canonical 体上。
	normalizedEdit, err := richtext.NormalizeContentEdit(req.ContentEdit)
	if err != nil {
		ba.Warn("RichText content_edit 校验失败", zap.Error(err), zap.String("messageID", req.MessageID))
		respondBotAPIRequestInvalid(c, "content_edit")
		return
	}
	req.ContentEdit = normalizedEdit

	contentEdit := dbr.NewNullString(req.ContentEdit).String
	contentMD5 := util.MD5(contentEdit)

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

	version, err := ba.ctx.GenSeq(fmt.Sprintf("%s:%s", common.MessageExtraSeqKey, fakeChannelID))
	if err != nil {
		ba.Error("生成消息扩展序列号失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIStoreFailed, nil, nil)
		return
	}

	_, err = ba.ctx.DB().InsertBySql(
		"INSERT INTO message_extra (message_id,message_seq,channel_id,channel_type,content_edit,content_edit_hash,edited_at,version) VALUES (?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE content_edit=VALUES(content_edit),content_edit_hash=VALUES(content_edit_hash),edited_at=VALUES(edited_at),version=VALUES(version)",
		req.MessageID, req.MessageSeq, fakeChannelID, req.ChannelType, contentEdit, contentMD5, int(time.Now().Unix()), version,
	).Exec()
	if err != nil {
		ba.Error("添加或修改编辑内容失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIStoreFailed, nil, nil)
		return
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
