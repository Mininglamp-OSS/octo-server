package bot_api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	spacesvc "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cliv2"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gin-gonic/gin"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

const cliBotMessageResponseEnvelopeKey = "cliv2.message.response_envelope"

// registerCLIMessageRoutes contributes the User-Bot half of the shared CLI v2
// message routes. It is called by the module assembly after BotAPI construction.
func (ba *BotAPI) registerCLIMessageRoutes(source *wkhttp.WKHttp) {
	for _, route := range []struct {
		method, path, action string
		handler              wkhttp.HandlerFunc
	}{
		{http.MethodPost, "/v2/cli/messages", cliv2.ActionMessagesWrite, ba.sendMessage},
		{http.MethodPatch, "/v2/cli/messages/:message_id", cliv2.ActionMessagesWrite, ba.botMessageEdit},
		{http.MethodPost, "/v2/cli/messages/_sync", cliv2.ActionMessagesRead, ba.syncMessages},
		{http.MethodPost, "/v2/cli/messages/_read_receipt", cliv2.ActionMessagesRead, ba.readReceipt},
		{http.MethodPost, "/v2/cli/typing", cliv2.ActionMessagesWrite, ba.typing},
	} {
		cliv2.Add(source, cliv2.Route{
			Method: route.method,
			Path:   route.path,
			Action: route.action,
			Bot:    route.handler,
			Middlewares: []wkhttp.HandlerFunc{
				botCLIMessageEnvelope,
				ba.cliBotMessageResourceGuard,
			},
		})
	}
}

// botCLIMessageEnvelope is intentionally idempotent with the Message module's
// response adapter: both contributions may be mounted on the same route.
func botCLIMessageEnvelope(c *wkhttp.Context) {
	if _, exists := c.Get(cliBotMessageResponseEnvelopeKey); exists {
		c.Next()
		return
	}
	c.Set(cliBotMessageResponseEnvelopeKey, true)
	original := c.Writer
	capture := &botCLIMessageCaptureWriter{ResponseWriter: original}
	c.Writer = capture
	c.Next()
	c.Writer = original

	status := capture.status
	if status == 0 {
		status = http.StatusOK
	}
	body := capture.body.Bytes()
	if len(body) == 0 {
		body = []byte(`{}`)
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) == nil && (envelope["data"] != nil || envelope["error"] != nil) {
		original.WriteHeader(status)
		_, _ = original.Write(body)
		return
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices || !json.Valid(body) {
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
		return
	}
	c.JSON(status, map[string]json.RawMessage{"data": body})
}

type botCLIMessageCaptureWriter struct {
	gin.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *botCLIMessageCaptureWriter) WriteHeader(status int) { w.status = status }
func (w *botCLIMessageCaptureWriter) WriteHeaderNow() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
}
func (w *botCLIMessageCaptureWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}
func (w *botCLIMessageCaptureWriter) WriteString(data string) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.WriteString(data)
}
func (w *botCLIMessageCaptureWriter) Status() int   { return w.status }
func (w *botCLIMessageCaptureWriter) Size() int     { return w.body.Len() }
func (w *botCLIMessageCaptureWriter) Written() bool { return w.status != 0 }

// cliBotMessageHasOnBehalfOf reports whether the raw request body carries an
// on_behalf_of field. Go's JSON decoder matches struct tags case-insensitively,
// so an exact-key lookup would let a mixed-case spelling (On_Behalf_Of) reach
// the legacy delegated send path.
func cliBotMessageHasOnBehalfOf(fields map[string]json.RawMessage) bool {
	for key := range fields {
		if strings.EqualFold(key, "on_behalf_of") {
			return true
		}
	}
	return false
}

// cliBotMessageGroupReadAllowed reports whether the matched CLI v2 route is the
// channel-sync read. Disbanded groups and threads stay readable; every other
// Bot message route keeps the legacy disband gate.
func cliBotMessageGroupReadAllowed(c *wkhttp.Context) bool {
	return c.FullPath() == "/v2/cli/messages/_sync"
}

// cliBotMessagePayloadSpaceID reads the payload space tag from raw message
// bytes. Malformed/missing/typed-wrong values are treated as unlabeled, the
// same "no space_id" posture as the user DM sync filter.
func cliBotMessagePayloadSpaceID(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var payload struct {
		SpaceID string `json:"space_id"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return payload.SpaceID
}

func cliBotMessageSpaceAllows(messageSpaceID string, isSystemBot bool, spaceID, defaultSpaceID string) bool {
	if spaceID == "" {
		return false
	}
	if messageSpaceID != "" {
		return messageSpaceID == spaceID
	}
	return !isSystemBot && defaultSpaceID != "" && defaultSpaceID == spaceID
}

// filterCLIBotSyncMessagesBySpace keeps the legacy /v1 Bot behavior by using
// the bound Space as its unlabeled-history default. CLI v2 instead lazily
// resolves the Token owner's default Space only when an unlabeled message is
// present; tagged messages need no extra query.
func (ba *BotAPI) filterCLIBotSyncMessagesBySpace(messages []*config.MessageResp, channelID, spaceID string, principal cliv2.Principal) ([]*config.MessageResp, error) {
	if spaceID == "" || len(messages) == 0 {
		return messages, nil
	}
	isSystemBot := spacepkg.IsSystemBot(channelID)
	if principal.BotID != "" {
		_, peerID := cliBotMessagePersonPeer(channelID, spaceID)
		isSystemBot = spacepkg.IsSystemBot(peerID)
	}
	defaultSpaceID := spaceID
	defaultSpaceResolved := principal.BotID == "" || isSystemBot
	if principal.BotID != "" {
		defaultSpaceID = ""
	}
	filtered := make([]*config.MessageResp, 0, len(messages))
	for _, message := range messages {
		if message == nil {
			continue
		}
		messageSpaceID := cliBotMessagePayloadSpaceID(message.Payload)
		if messageSpaceID == "" && principal.BotID != "" && !isSystemBot && !defaultSpaceResolved {
			defaultSpaceResolved = true
			var err error
			defaultSpaceID, err = spacesvc.GetUserDefaultSpaceIDE(ba.ctx, principal.UserID)
			if err != nil {
				return nil, err
			}
		}
		if cliBotMessageSpaceAllows(messageSpaceID, isSystemBot, spaceID, defaultSpaceID) {
			filtered = append(filtered, message)
		}
	}
	return filtered, nil
}

// cliBotMessageResourceGuard validates the SDK-bound Bot identity and then
// checks every requested channel against that Token's Space and Bot membership.
func (ba *BotAPI) cliBotMessageResourceGuard(c *wkhttp.Context) {
	principal := cliv2.PrincipalFrom(c)
	if principal.BotID == "" {
		c.Next()
		return
	}
	if principal.UserID == "" || principal.SpaceID == "" || ba.db == nil || c.GetLoginUID() != principal.BotID {
		cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
		c.Abort()
		return
	}
	bot, err := ba.db.queryRobotByRobotID(principal.BotID)
	if err != nil {
		ba.Error("CLI v2 Bot lookup failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
		c.Abort()
		return
	}
	if bot == nil || bot.Status != 1 || bot.CreatorUID != principal.UserID {
		cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
		c.Abort()
		return
	}
	c.Set(CtxKeyRobotID, bot.RobotID)
	c.Set(CtxKeyBotKind, BotKindUser)
	c.Set(CtxKeyRobot, bot)
	// The bound Token Space is authoritative for this v2 request. The legacy
	// resolver already supports this trusted scoped-space context for payload
	// enrichment, without consulting an arbitrary client header or Bot default.
	c.Set(CtxKeyAppBotScope, "space")
	c.Set(CtxKeyAppBotSpaceID, principal.SpaceID)

	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, cardmsg.MaxSendBodyBytes))
	if err != nil {
		cliv2.Fail(c, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "Request body is too large.")
		c.Abort()
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "The request is invalid.")
		c.Abort()
		return
	}
	if cliBotMessageHasOnBehalfOf(fields) {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "On-behalf-of requests are not supported.")
		c.Abort()
		return
	}
	var req struct {
		ChannelID   string   `json:"channel_id"`
		ChannelType uint8    `json:"channel_type"`
		MessageID   string   `json:"message_id"`
		MessageIDs  []string `json:"message_ids"`
		MessageSeq  uint32   `json:"message_seq"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "The request is invalid.")
		c.Abort()
		return
	}
	if messageID := c.Param("message_id"); messageID != "" {
		if req.MessageID != "" && req.MessageID != messageID {
			cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "The message ID is invalid.")
			c.Abort()
			return
		}
		if req.MessageID == "" {
			fields["message_id"], _ = json.Marshal(messageID)
			body, err = json.Marshal(fields)
			if err != nil {
				cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "The request is invalid.")
				c.Abort()
				return
			}
			// Authorize the same serialized fields the legacy handler will read.
			if err := json.Unmarshal(body, &req); err != nil {
				cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "The request is invalid.")
				c.Abort()
				return
			}
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			c.Request.ContentLength = int64(len(body))
		}
	}
	if req.ChannelID == "" || req.ChannelType == 0 {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "A channel ID and channel type are required.")
		c.Abort()
		return
	}
	// The legacy edit handler accepts the id from either the path parameter or
	// the body; the guard validates the same resolved id.
	targetMessageID := req.MessageID
	if targetMessageID == "" {
		targetMessageID = c.Param("message_id")
	}
	if !ba.cliBotMessageChannelAllowed(c, principal, bot.RobotID, req.ChannelID, req.ChannelType) {
		c.Abort()
		return
	}
	// DIRECT channels carry no Space in WuKongIM; payload.space_id is the only
	// authoritative marker. Edit and read receipt mutate message-level state, so
	// the channel gate alone cannot stop a message_id owned by another Space.
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		switch c.FullPath() {
		case "/v2/cli/messages/:message_id":
			if !ba.cliBotMessagePersonEditTargetAllowed(c, principal, bot.RobotID, targetMessageID, req.ChannelID, req.MessageSeq) {
				c.Abort()
				return
			}
		case "/v2/cli/messages/_read_receipt":
			if !ba.cliBotMessagePersonReadTargetsAllowed(c, principal, bot.RobotID, req.ChannelID, req.MessageIDs) {
				c.Abort()
				return
			}
		}
	}
	c.Next()
}

func (ba *BotAPI) cliBotMessageChannelAllowed(c *wkhttp.Context, principal cliv2.Principal, botID, channelID string, channelType uint8) bool {
	switch channelType {
	case common.ChannelTypePerson.Uint8():
		spaceID, peerID := spacepkg.ParseChannelID(channelID)
		if spaceID != "" && spaceID != principal.SpaceID {
			cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			return false
		}
		if spaceID == "" && strings.HasPrefix(channelID, spacepkg.SpaceChannelPrefix+principal.SpaceID+"_") {
			peerID = strings.TrimPrefix(channelID, spacepkg.SpaceChannelPrefix+principal.SpaceID+"_")
		}
		if peerID == "" {
			cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "The channel ID is invalid.")
			return false
		}
		// The validated Token owner is the Bot's creator. The legacy Bot DM gate
		// always allows creator↔Bot conversations, and the creator is the actor
		// whose Space membership the remaining checks would only re-prove.
		if peerID == principal.UserID {
			return true
		}
		for _, uid := range []string{principal.UserID, peerID} {
			member, err := ba.isSpaceMember(uid, principal.SpaceID)
			if err != nil {
				ba.Error("CLI v2 direct-message Space membership lookup failed", zap.Error(err))
				cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
				return false
			}
			if !member {
				cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
				return false
			}
		}
		return true
	case common.ChannelTypeGroup.Uint8(), common.ChannelTypeCommunityTopic.Uint8():
		groupNo := channelID
		if channelType == common.ChannelTypeCommunityTopic.Uint8() {
			parts := strings.SplitN(channelID, threadChannelIDSeparator, 2)
			if len(parts) != 2 || parts[0] == "" {
				cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "The channel ID is invalid.")
				return false
			}
			groupNo = parts[0]
		}
		var rows []struct {
			SpaceID string `db:"space_id"`
			Status  int    `db:"status"`
		}
		_, err := ba.db.session.Select("space_id, status").From("`group`").Where("group_no=?", groupNo).Load(&rows)
		if err != nil {
			ba.Error("CLI v2 group Space lookup failed", zap.Error(err))
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
			return false
		}
		if len(rows) == 0 {
			cliv2.Fail(c, http.StatusNotFound, "NOT_FOUND", "Resource not found.")
			return false
		}
		inSpace := rows[0].SpaceID == principal.SpaceID
		if !inSpace {
			var sourceSpaceID string
			err := ba.db.session.Select("source_space_id").From("group_member").
				Where("group_no=? AND uid=? AND is_external=1 AND is_deleted=0", groupNo, principal.UserID).
				LoadOne(&sourceSpaceID)
			if err != nil && !errors.Is(err, dbr.ErrNotFound) {
				ba.Error("CLI v2 external group Space lookup failed", zap.Error(err))
				cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
				return false
			}
			inSpace = sourceSpaceID == principal.SpaceID
		}
		if !inSpace {
			cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			return false
		}
		// Disbanded groups and threads are read-only, not inaccessible: the
		// legacy Bot sync handler deliberately has no disband gate, while every
		// Bot write path rejects them. Mirror that per-action rule so history
		// stays readable without reopening sends/edits/typing/receipts.
		if rows[0].Status == group.GroupStatusDisband && !cliBotMessageGroupReadAllowed(c) {
			cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			return false
		}
		count, err := ba.queryBotActiveGroupMemberCount(groupNo, botID)
		if err != nil {
			ba.Error("CLI v2 Bot group membership lookup failed", zap.Error(err))
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
			return false
		}
		if count == 0 {
			cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			return false
		}
		return true
	default:
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "The channel type is invalid.")
		return false
	}
}

// cliBotMessagePersonPeer splits a DIRECT channel id into its optional Space
// prefix and the bare peer uid. ParseChannelID only recognises registered or
// 32-hex Space ids, so the bound Token Space prefix is also stripped verbatim;
// any other "s..._" spelling stays part of the peer uid and fails closed in the
// membership lookups.
func cliBotMessagePersonPeer(channelID, boundSpaceID string) (spaceID, peerID string) {
	spaceID, peerID = spacepkg.ParseChannelID(channelID)
	if spaceID == "" && boundSpaceID != "" && strings.HasPrefix(channelID, spacepkg.SpaceChannelPrefix+boundSpaceID+"_") {
		peerID = strings.TrimPrefix(channelID, spacepkg.SpaceChannelPrefix+boundSpaceID+"_")
	}
	return spaceID, peerID
}

// cliBotMessagePositiveIDs parses edit/read-receipt message ids. Non-positive
// or non-numeric ids cannot address a stored message, so they are ignored the
// same way the legacy handlers ignore parse failures; they can neither load nor
// write message-level state.
func cliBotMessagePositiveIDs(messageIDs []string) []int64 {
	parsed := make([]int64, 0, len(messageIDs))
	for _, raw := range messageIDs {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			continue
		}
		parsed = append(parsed, value)
	}
	return parsed
}

// cliBotMessagePersonTargetsInBoundSpace checks that every DM message a Bot v2
// edit or read receipt would touch is visible in the Token-bound Space. DIRECT
// channels are stored under a bare uid, so payload.space_id is the only
// authoritative marker. Tagged messages require an exact bound-Space match;
// legacy unlabeled history is visible only in the Token owner's default Space.
// One message outside the Space rejects the whole request, so a batch read
// receipt cannot write state for a message the Bot was not authorized to see.
func (ba *BotAPI) cliBotMessagePersonTargetsInBoundSpace(c *wkhttp.Context, principal cliv2.Principal, channelID string, messages []*config.MessageResp) bool {
	if len(messages) == 0 {
		return true
	}
	_, peerID := cliBotMessagePersonPeer(channelID, principal.SpaceID)
	isSystemBot := spacepkg.IsSystemBot(peerID)
	defaultSpaceID := ""
	defaultSpaceResolved := isSystemBot
	for _, message := range messages {
		if message == nil {
			continue
		}
		messageSpaceID := cliBotMessagePayloadSpaceID(message.Payload)
		if messageSpaceID == "" && !isSystemBot && !defaultSpaceResolved {
			defaultSpaceResolved = true
			var err error
			defaultSpaceID, err = spacesvc.GetUserDefaultSpaceIDE(ba.ctx, principal.UserID)
			if err != nil {
				ba.Error("CLI v2 Bot default Space lookup failed", zap.Error(err))
				cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
				return false
			}
		}
		if cliBotMessageSpaceAllows(messageSpaceID, isSystemBot, principal.SpaceID, defaultSpaceID) {
			continue
		}
		cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
		return false
	}
	return true
}

// cliBotMessagePersonEditTargetAllowed protects the Bot edit path. The legacy
// handler resolves the target by message_seq when one is supplied and by
// message_id otherwise, so the guard mirrors both lookups before the handler
// can write message_extra or emit the edit CMD.
func (ba *BotAPI) cliBotMessagePersonEditTargetAllowed(c *wkhttp.Context, principal cliv2.Principal, robotID, messageID, channelID string, messageSeq uint32) bool {
	var (
		resp *config.SyncChannelMessageResp
		err  error
	)
	if messageSeq > 0 {
		resp, err = ba.ctx.IMGetWithChannelAndSeqs(channelID, common.ChannelTypePerson.Uint8(), robotID, []uint32{messageSeq})
	} else {
		ids := cliBotMessagePositiveIDs([]string{messageID})
		if len(ids) == 0 {
			return true
		}
		resp, err = ba.ctx.IMSearchMessages(&config.MsgSearchReq{
			ChannelID:   channelID,
			ChannelType: common.ChannelTypePerson.Uint8(),
			MessageIds:  ids,
			LoginUID:    robotID,
		})
	}
	if err != nil {
		ba.Error("CLI v2 Bot direct-message edit target query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusServiceUnavailable, "UPSTREAM_UNAVAILABLE", "Message service unavailable.")
		return false
	}
	if resp == nil {
		return true
	}
	return ba.cliBotMessagePersonTargetsInBoundSpace(c, principal, channelID, resp.Messages)
}

// cliBotMessagePersonReadTargetsAllowed protects the Bot batch read receipt. It
// mirrors the legacy handler's IM lookup (the Bot-side DM channel id, the same
// message_ids and login_uid), so the rows the handler will insert are exactly
// the rows checked here. The legacy handler's 100-id limit rejects oversized
// batches before any message-level write, so the guard leaves those requests to
// the handler.
func (ba *BotAPI) cliBotMessagePersonReadTargetsAllowed(c *wkhttp.Context, principal cliv2.Principal, robotID, channelID string, messageIDs []string) bool {
	if len(messageIDs) > 100 {
		return true
	}
	ids := cliBotMessagePositiveIDs(messageIDs)
	if len(ids) == 0 {
		return true
	}
	resp, err := ba.ctx.IMSearchMessages(&config.MsgSearchReq{
		ChannelID:   robotID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		MessageIds:  ids,
		LoginUID:    robotID,
	})
	if err != nil {
		ba.Error("CLI v2 Bot direct-message read-receipt target query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusServiceUnavailable, "UPSTREAM_UNAVAILABLE", "Message service unavailable.")
		return false
	}
	if resp == nil {
		return true
	}
	return ba.cliBotMessagePersonTargetsInBoundSpace(c, principal, channelID, resp.Messages)
}

// queryBotActiveGroupMemberCount counts only normal, non-deleted Bot membership
// rows. A blacklisted Bot keeps group_member with is_deleted=0 (status=blacklist),
// so the legacy count predicate would still let it pull group and thread
// history. The CLI v2 gate uses the same active-membership whitelist as the
// user ExistMemberActive checks without changing the legacy v1 queries.
func (ba *BotAPI) queryBotActiveGroupMemberCount(groupNo, botID string) (int, error) {
	if ba.groupMemberCountOverride != nil {
		return ba.groupMemberCountOverride(groupNo, botID)
	}
	var count int
	err := ba.db.session.SelectBySql(
		"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0 AND status=?",
		groupNo, botID, int(common.GroupMemberStatusNormal),
	).LoadOne(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}
