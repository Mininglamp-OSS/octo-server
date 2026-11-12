package message

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cliv2"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/richtext"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const cliMessageResponseEnvelopeKey = "cliv2.message.response_envelope"

// registerCLIMessageRoutes contributes the Token-user half of message routes.
// Bot handlers are supplied independently by BotAPI during CLI v2 assembly.
func (m *Message) registerCLIMessageRoutes(source *wkhttp.WKHttp) {
	for _, route := range []struct {
		method, path, action string
		handler              wkhttp.HandlerFunc
	}{
		{http.MethodPost, "/v2/cli/messages", cliv2.ActionMessagesWrite, m.cliSendMessage},
		{http.MethodPatch, "/v2/cli/messages/:message_id", cliv2.ActionMessagesWrite, m.messageEdit},
		{http.MethodPost, "/v2/cli/messages/_sync", cliv2.ActionMessagesRead, m.syncChannelMessage},
		{http.MethodPost, "/v2/cli/messages/_read_receipt", cliv2.ActionMessagesRead, m.messageReaded},
		{http.MethodPost, "/v2/cli/typing", cliv2.ActionMessagesWrite, m.typing},
	} {
		cliv2.Add(source, cliv2.Route{
			Method: route.method,
			Path:   route.path,
			Action: route.action,
			User:   route.handler,
			Middlewares: []wkhttp.HandlerFunc{
				cliMessageEnvelope,
				m.cliMessageResourceGuard,
			},
		})
	}
}

// cliMessageEnvelope converts the legacy handlers' success JSON into the v2
// envelope while leaving errors rendered by the v2 error renderer untouched.
func cliMessageEnvelope(c *wkhttp.Context) {
	if _, exists := c.Get(cliMessageResponseEnvelopeKey); exists {
		c.Next()
		return
	}
	c.Set(cliMessageResponseEnvelopeKey, true)
	original := c.Writer
	capture := &cliMessageCaptureWriter{ResponseWriter: original}
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
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
		return
	}
	var data json.RawMessage
	if !json.Valid(body) {
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
		return
	}
	data = body
	c.JSON(status, map[string]json.RawMessage{"data": data})
}

type cliMessageCaptureWriter struct {
	gin.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *cliMessageCaptureWriter) WriteHeader(status int) { w.status = status }
func (w *cliMessageCaptureWriter) WriteHeaderNow() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
}
func (w *cliMessageCaptureWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}
func (w *cliMessageCaptureWriter) WriteString(data string) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.WriteString(data)
}
func (w *cliMessageCaptureWriter) Status() int   { return w.status }
func (w *cliMessageCaptureWriter) Size() int     { return w.body.Len() }
func (w *cliMessageCaptureWriter) Written() bool { return w.status != 0 }

// cliMessageHasOnBehalfOf reports whether the raw request body carries an
// on_behalf_of field. Go's JSON decoder matches struct tags case-insensitively,
// so an exact-key lookup would let a mixed-case spelling (On_Behalf_Of) reach
// the legacy on-behalf-of path while the guard believes no delegation was
// requested.
func cliMessageHasOnBehalfOf(fields map[string]json.RawMessage) bool {
	for key := range fields {
		if strings.EqualFold(key, "on_behalf_of") {
			return true
		}
	}
	return false
}

// cliMessageGroupReadAllowed reports whether the matched CLI v2 route is a
// read-only channel operation. Disbanded groups and threads stay readable;
// every write route keeps the legacy read-only-after-disband gate.
func cliMessageGroupReadAllowed(c *wkhttp.Context) bool {
	switch c.FullPath() {
	case "/v2/cli/messages/_sync", "/v2/cli/messages/_read_receipt":
		return true
	}
	return false
}

// cliMessageResourceGuard checks the v2 Token Space and the effective
// resource actor before any legacy message handler runs. Auth middleware sets
// the actor to the Bot when a validated Bot binding exists.
func (m *Message) cliMessageResourceGuard(c *wkhttp.Context) {
	principal := cliv2.PrincipalFrom(c)
	if principal.BotID != "" {
		c.Next()
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, cardmsg.MaxSendBodyBytes))
	if err != nil {
		cliv2.Fail(c, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "Request body is too large.")
		c.Abort()
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))

	// A body that the guard cannot decode must fail closed: returning to the
	// legacy handler would skip the bound-Space check for fields the guard
	// cannot inspect (the original wrong-typed message_id bypass).
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "The request is invalid.")
		c.Abort()
		return
	}
	if cliMessageHasOnBehalfOf(fields) {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "On-behalf-of requests are not supported.")
		c.Abort()
		return
	}
	var req struct {
		ChannelID   string                 `json:"channel_id"`
		ChannelType uint8                  `json:"channel_type"`
		MessageID   string                 `json:"message_id"`
		MessageIDs  []string               `json:"message_ids"`
		MessageSeq  uint32                 `json:"message_seq"`
		Payload     map[string]interface{} `json:"payload"`
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
	if cardmsg.IsCardPayload(req.Payload) && c.FullPath() == "/v2/cli/messages" {
		// Preserve the established user-ingress rule: card denial precedes
		// channel/resource lookups and does not depend on database state.
		c.Next()
		return
	}
	if req.ChannelID == "" || req.ChannelType == 0 {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "A channel ID and channel type are required.")
		c.Abort()
		return
	}
	if !m.cliMessageChannelAllowed(c, principal, req.ChannelID, req.ChannelType) {
		c.Abort()
		return
	}
	// A DIRECT channel is keyed by a bare uid in WuKongIM and therefore carries
	// no Space of its own: payload.space_id is the authoritative marker. Edit
	// and read receipt mutate message-level state, so the channel gate alone
	// cannot stop a message_id that belongs to another Space.
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		switch c.FullPath() {
		case "/v2/cli/messages/:message_id":
			if !m.cliMessagePersonEditTargetAllowed(c, principal, req.ChannelID, req.MessageSeq) {
				c.Abort()
				return
			}
		case "/v2/cli/messages/_read_receipt":
			if !m.cliMessagePersonReadTargetsAllowed(c, principal, req.ChannelID, req.MessageIDs) {
				c.Abort()
				return
			}
		}
	}
	c.Next()
}

func (m *Message) cliMessageChannelAllowed(c *wkhttp.Context, principal cliv2.Principal, channelID string, channelType uint8) bool {
	actorID := c.GetLoginUID()
	if principal.UserID == "" || principal.SpaceID == "" || actorID == "" {
		cliv2.Fail(c, http.StatusServiceUnavailable, "UPSTREAM_UNAVAILABLE", "Authentication service unavailable.")
		return false
	}
	if actorID != principal.UserID {
		cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
		return false
	}
	switch channelType {
	case common.ChannelTypePerson.Uint8():
		return m.cliMessagePersonChannelAllowed(c, principal, channelID)
	case common.ChannelTypeGroup.Uint8(), common.ChannelTypeCommunityTopic.Uint8():
		groupNo := channelID
		if channelType == common.ChannelTypeCommunityTopic.Uint8() {
			var err error
			groupNo, _, err = thread.ParseChannelID(channelID)
			if err != nil || groupNo == "" {
				cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "The channel ID is invalid.")
				return false
			}
		}
		inSpace, err := m.groupInBoundSpace(groupNo, principal.UserID, principal.SpaceID)
		if err != nil {
			m.Error("CLI v2 group Space authorization query failed", zap.Error(err))
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
			return false
		}
		if !inSpace {
			cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			return false
		}
		member, err := m.groupService.ExistMemberActive(groupNo, actorID)
		if err != nil {
			m.Error("CLI v2 group membership authorization query failed", zap.Error(err))
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
			return false
		}
		if !member {
			cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			return false
		}
		// The v1 send/edit/typing handlers reject disbanded groups and threads.
		// The v2 adapter calls the dispatch services directly, so the guard must
		// carry the same read-only rule instead of letting a send through.
		// Channel sync and read receipts remain readable after disband.
		if !cliMessageGroupReadAllowed(c) {
			disbanded, err := m.isGroupDisbanded(groupNo)
			if err != nil {
				m.Error("CLI v2 group status authorization query failed", zap.Error(err))
				cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
				return false
			}
			if disbanded {
				cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
				return false
			}
		}
		return true
	default:
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "The channel type is invalid.")
		return false
	}
}

// cliMessagePersonPeer splits a DIRECT channel id into its optional Space
// prefix and the bare peer uid. ParseChannelID only recognises registered or
// 32-hex Space ids, so the bound Token Space prefix is also stripped verbatim;
// any other "s..._" spelling remains part of the peer uid and simply fails the
// membership/friend lookups later (fail-closed). boundScoped reports that the
// channel is explicitly scoped to the bound Token Space, which keeps the legacy
// Space-mode semantics distinct from the bare-uid personal mode.
func cliMessagePersonPeer(channelID, boundSpaceID string) (spaceID, peerID string, boundScoped bool) {
	spaceID, peerID = spacepkg.ParseChannelID(channelID)
	if spaceID != "" {
		return spaceID, peerID, spaceID == boundSpaceID
	}
	if boundSpaceID != "" && strings.HasPrefix(channelID, spacepkg.SpaceChannelPrefix+boundSpaceID+"_") {
		return "", strings.TrimPrefix(channelID, spacepkg.SpaceChannelPrefix+boundSpaceID+"_"), true
	}
	return "", peerID, false
}

// cliMessagePersonChannelAllowed applies the legacy DIRECT (person) channel
// rules while keeping the v2 Token Space authoritative. Notes-to-self and the
// caller's own Bot are the two exemptions the v1 DM access decision grants
// (personDMAccessDecision); neither peer has to be a Space member. Every other
// peer must belong to the bound Space together with the caller. For a bare-uid
// send, the legacy personal-mode two-way friend rule also applies. Other
// message operations retain their existing access rules.
func (m *Message) cliMessagePersonChannelAllowed(c *wkhttp.Context, principal cliv2.Principal, channelID string) bool {
	spaceID, peerID, boundScoped := cliMessagePersonPeer(channelID, principal.SpaceID)
	if spaceID != "" && spaceID != principal.SpaceID {
		cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
		return false
	}
	if peerID == "" {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "The channel ID is invalid.")
		return false
	}
	if peerID == principal.UserID {
		return true
	}
	// The bound Space is authoritative for every other peer. Friendship is a
	// personal-mode compatibility gate, not a cross-Space authorization, so a
	// friend outside the Token Space must not pass either.
	allowed, err := spacepkg.CheckBothMembers(m.ctx.DB(), principal.SpaceID, principal.UserID, peerID)
	if err != nil {
		m.Error("CLI v2 direct-message Space membership query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
		return false
	}
	if allowed {
		if boundScoped || c.FullPath() != "/v2/cli/messages" {
			return true
		}
		// Legacy personal-mode DM rule (sendMessage): a bare uid with no Space
		// prefix must also satisfy the established two-way friend relation.
		if m.userService == nil {
			m.Error("CLI v2 direct-message authorization service unavailable")
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
			return false
		}
		senderIsFriend, err := m.userService.IsFriend(principal.UserID, peerID)
		if err != nil {
			m.Error("CLI v2 direct-message friend query failed", zap.Error(err))
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
			return false
		}
		peerIsFriend, err := m.userService.IsFriend(peerID, principal.UserID)
		if err != nil {
			m.Error("CLI v2 direct-message reverse friend query failed", zap.Error(err))
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
			return false
		}
		if senderIsFriend && peerIsFriend {
			return true
		}
	}
	// The remaining cases are a peer outside the bound Space, or a bare-uid
	// peer that failed the legacy friend gate. Own Bot is a legitimate
	// non-member peer: the Bot is created by the caller and the v1 DM access
	// decision allows it without a Space member row or friendship.
	if m.userService == nil {
		m.Error("CLI v2 direct-message authorization service unavailable")
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
		return false
	}
	isRobot, creatorUID, err := m.userService.QueryPeerRobotInfo(peerID)
	if err != nil {
		m.Error("CLI v2 direct-message Bot lookup failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
		return false
	}
	if isRobot && creatorUID == principal.UserID {
		return true
	}
	cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
	return false
}

// cliMessagePositiveIDs parses the message ids of an edit or read-receipt
// request. Non-positive or non-numeric ids cannot address a stored message, so
// they are ignored exactly the way the legacy handlers ignore parse failures;
// such ids can neither load nor write message-level state.
func cliMessagePositiveIDs(messageIDs []string) []int64 {
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

// cliMessagePersonTargetsInBoundSpace checks that every DM message a v2 edit or
// read receipt would touch is visible in the Token-bound Space. DIRECT channels
// are stored under a bare uid, so payload.space_id is the only authoritative
// marker; the existing personSpaceAllows rule is reused so the write path keeps
// the same visibility semantics as channel sync (exact Space match, plus the
// legacy unlabeled-history rule for the user's default Space). One message
// outside the bound Space rejects the whole request, so a batch read receipt
// can never write read state for a message it was not authorized to see.
func (m *Message) cliMessagePersonTargetsInBoundSpace(c *wkhttp.Context, principal cliv2.Principal, channelID string, messages []*config.MessageResp) bool {
	if len(messages) == 0 {
		return true
	}
	// The default-Space rule only applies to unlabeled legacy messages; keep
	// the extra lookup out of the common fully-labeled case. Payload bytes can
	// be large, so each message is decoded exactly once.
	messageSpaceIDs := make([]string, 0, len(messages))
	needDefaultSpace := false
	for _, message := range messages {
		if message == nil {
			messageSpaceIDs = append(messageSpaceIDs, "")
			continue
		}
		messageSpaceID := payloadSpaceIDFromRaw(message.Payload)
		messageSpaceIDs = append(messageSpaceIDs, messageSpaceID)
		if messageSpaceID == "" {
			needDefaultSpace = true
		}
	}
	defaultSpaceID := principal.SpaceID
	if needDefaultSpace {
		resolved, err := space.GetUserDefaultSpaceIDE(m.ctx, principal.UserID)
		if err != nil {
			// Same compatibility posture as the DM sync filter: fall back to
			// the bound Space for the unlabeled-history rule, while the
			// authoritative labeled-Space check below is unaffected by the
			// lookup outcome.
			m.Warn("CLI v2 default Space lookup failed; unlabeled DMs fall back to the bound Space", zap.Error(err))
		} else {
			defaultSpaceID = resolved
		}
	}
	_, peerID, _ := cliMessagePersonPeer(channelID, principal.SpaceID)
	isSystemBot := spacepkg.IsSystemBot(peerID)
	for index, message := range messages {
		if message == nil {
			continue
		}
		if !personSpaceAllows(messageSpaceIDs[index], isSystemBot, principal.SpaceID, defaultSpaceID) {
			cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			return false
		}
	}
	return true
}

// cliMessagePersonEditTargetAllowed protects the user edit path. The legacy
// handler resolves the target by message_seq, so the guard inspects the same IM
// row before the handler can write message_extra or emit the edit CMD. A zero
// sequence never reaches IM in the legacy handler, so it needs no target check.
func (m *Message) cliMessagePersonEditTargetAllowed(c *wkhttp.Context, principal cliv2.Principal, channelID string, messageSeq uint32) bool {
	if messageSeq == 0 {
		return true
	}
	resp, err := m.ctx.IMGetWithChannelAndSeqs(channelID, common.ChannelTypePerson.Uint8(), principal.UserID, []uint32{messageSeq})
	if err != nil {
		m.Error("CLI v2 direct-message edit target query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusServiceUnavailable, "UPSTREAM_UNAVAILABLE", "Message service unavailable.")
		return false
	}
	if resp == nil {
		return true
	}
	return m.cliMessagePersonTargetsInBoundSpace(c, principal, channelID, resp.Messages)
}

// cliMessagePersonReadTargetsAllowed protects the user batch read receipt. It
// mirrors the legacy handler's IM lookup (same channel, message_ids and
// login_uid), so the rows the handler will insert are exactly the rows checked
// here.
func (m *Message) cliMessagePersonReadTargetsAllowed(c *wkhttp.Context, principal cliv2.Principal, channelID string, messageIDs []string) bool {
	ids := cliMessagePositiveIDs(messageIDs)
	if len(ids) == 0 {
		return true
	}
	resp, err := m.ctx.IMSearchMessages(&config.MsgSearchReq{
		ChannelID:   channelID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		MessageIds:  ids,
		LoginUID:    principal.UserID,
	})
	if err != nil {
		m.Error("CLI v2 direct-message read-receipt target query failed", zap.Error(err))
		cliv2.Fail(c, http.StatusServiceUnavailable, "UPSTREAM_UNAVAILABLE", "Message service unavailable.")
		return false
	}
	if resp == nil {
		return true
	}
	return m.cliMessagePersonTargetsInBoundSpace(c, principal, channelID, resp.Messages)
}

func (m *Message) cliSendMessage(c *wkhttp.Context) {
	if !m.ctx.GetConfig().Message.SendMessageOn {
		httperr.ResponseErrorL(c, errcode.ErrMessageProxySendUnsupported, nil, nil)
		return
	}
	var req struct {
		ChannelID   string                 `json:"channel_id"`
		ChannelType uint8                  `json:"channel_type"`
		StreamNo    string                 `json:"stream_no"`
		Payload     map[string]interface{} `json:"payload"`
	}
	if err := c.BindJSON(&req); err != nil || req.ChannelID == "" || req.ChannelType == 0 || len(req.Payload) == 0 {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "The request is invalid.")
		return
	}
	if cardmsg.IsCardPayload(req.Payload) {
		httperr.ResponseErrorL(c, errcode.ErrMessageCardSendForbidden, nil, nil)
		return
	}
	if err := richtext.Validate(req.Payload); err != nil {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "The message payload is invalid.")
		return
	}
	principal := cliv2.PrincipalFrom(c)
	if err := m.sendMessage(req.ChannelID, req.ChannelType, principal.UserID, req.Payload, principal.SpaceID); err != nil {
		m.Error("CLI v2 message dispatch failed", zap.Error(err))
		cliv2.Fail(c, http.StatusServiceUnavailable, "UPSTREAM_UNAVAILABLE", "Message service unavailable.")
		return
	}
	_ = req.StreamNo // Stream numbers remain supported by Bot messages only.
	cliv2.Respond(c, nil)
}
