package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.message.* — modules/message business error codes (api.go,
// api_manager.go, api_pinned.go, api_conversation.go, api_message_get.go,
// api_reminders.go, api_channel_files.go, api_sidebar.go). DefaultMessage holds
// the en-US source (D4); zh-CN runtime translations live in
// pkg/i18n/locales/active.zh-CN.toml. Internal=true codes never surface their
// message on the wire — callers MUST log the underlying err with full context
// before responding.
var (
	// ---- validation (400) ----------------------------------------------------

	// ErrMessageRequestInvalid is the catch-all for missing / malformed request
	// input (empty ids, bad channel-id / message-id / seq format, BindJSON
	// failure, unsupported channel type, etc.). The offending field is surfaced
	// via Details when the caller can identify it.
	ErrMessageRequestInvalid = register(codes.Code{
		ID:             "err.server.message.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request.",
		SafeDetailKeys: []string{"field"},
	})
	ErrMessageIDSeqMismatch = register(codes.Code{
		ID:             "err.server.message.id_seq_mismatch",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Message ID does not match the message sequence.",
	})

	// ---- permission / authorization (403) ------------------------------------

	ErrMessageNotFriend = register(codes.Code{
		ID:             "err.server.message.not_friend",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You are not friends with this user.",
	})
	ErrMessagePeerNotInSpace = register(codes.Code{
		ID:             "err.server.message.peer_not_in_space",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The other party is not in this space.",
	})
	ErrMessageConversationForbidden = register(codes.Code{
		ID:             "err.server.message.conversation_forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You do not have permission to operate on this conversation.",
	})
	ErrMessageCannotDeleteSelfConversation = register(codes.Code{
		ID:             "err.server.message.cannot_delete_self_conversation",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "You cannot delete the conversation with yourself.",
	})
	ErrMessageChannelAccessDenied = register(codes.Code{
		ID:             "err.server.message.channel_access_denied",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You do not have permission to operate on this channel.",
	})
	ErrMessageNotGroupMember = register(codes.Code{
		ID:             "err.server.message.not_group_member",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You are not a member of this group.",
	})
	// ErrMessageGroupDisbanded covers the disbanded-group send guard on the
	// regular user path (/v1/message/send). After a group is disbanded
	// (WeChat-Work style — history preserved, everyone read-only), no one may
	// send. The deployed WuKongIM /message/send returns HTTP 200 with no
	// failure signal on a disband rejection, so octo-server self-checks
	// group.status here (parity with the bot path's ErrBotAPIGroupDisbanded).
	ErrMessageGroupDisbanded = register(codes.Code{
		ID:             "err.server.message.group_disbanded",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The group has been disbanded; messages can no longer be sent.",
	})
	ErrMessageDeleteForbidden = register(codes.Code{
		ID:             "err.server.message.delete_forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You do not have permission to delete this message.",
	})
	ErrMessageRecallForbidden = register(codes.Code{
		ID:             "err.server.message.recall_forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You do not have permission to recall this message.",
	})
	ErrMessagePinnedForbidden = register(codes.Code{
		ID:             "err.server.message.pinned_forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You do not have permission to pin or unpin messages.",
	})
	ErrMessageEditOwnOnly = register(codes.Code{
		ID:             "err.server.message.edit_own_only",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You can only edit your own messages.",
	})
	ErrMessageProxySendUnsupported = register(codes.Code{
		ID:             "err.server.message.proxy_send_unsupported",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Sending messages on behalf of another user is not supported.",
	})

	// ---- not found (404) -----------------------------------------------------

	ErrMessageNotFound = register(codes.Code{
		ID:             "err.server.message.not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Message not found or already deleted.",
	})
	ErrMessageGroupNotFound = register(codes.Code{
		ID:             "err.server.message.group_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "The target group does not exist or has been deleted.",
	})
	ErrMessageReceiverNotFound = register(codes.Code{
		ID:             "err.server.message.receiver_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "The message recipient does not exist.",
	})
	ErrMessageBanwordNotFound = register(codes.Code{
		ID:             "err.server.message.banword_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "The banned word does not exist.",
	})

	// ---- limit / time window (400) -------------------------------------------

	ErrMessagePinnedLimitExceeded = register(codes.Code{
		ID:             "err.server.message.pinned_limit_exceeded",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The pinned-message limit has been reached.",
		SafeDetailKeys: []string{"max"},
	})
	ErrMessageRecallTimeExceeded = register(codes.Code{
		ID:             "err.server.message.recall_time_exceeded",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The message is past the recall time window.",
	})

	// ---- internal (500, Internal=true) ---------------------------------------

	// ErrMessageQueryFailed covers read-path failures (DB SELECT/count, cache
	// GET, membership / permission checks, search-result decoding, sync reads).
	ErrMessageQueryFailed = register(codes.Code{
		ID:             "err.server.message.query_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to query message data.",
		Internal:       true,
	})
	// ErrMessageStoreFailed covers mutation-path failures (DB write, transaction
	// begin/commit, sequence generation, cache SET, offset/read/pin/recall/edit
	// persistence).
	ErrMessageStoreFailed = register(codes.Code{
		ID:             "err.server.message.store_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to persist message data.",
		Internal:       true,
	})
	// ErrMessageNotifyFailed covers outbound IM command / sync-command / recall
	// dispatch failures.
	ErrMessageNotifyFailed = register(codes.Code{
		ID:             "err.server.message.notify_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to dispatch the message command.",
		Internal:       true,
	})
	// ErrMessageSearchFailed covers failures calling / parsing the external
	// search service.
	ErrMessageSearchFailed = register(codes.Code{
		ID:             "err.server.message.search_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Message search failed.",
		Internal:       true,
	})
	// ---- InteractiveCard(=17) 卡片消息协议 P1（spec: .octospec/tasks/
	// card-message-protocol/brief.md）----------------------------------------

	// ErrMessageCardSendForbidden Decision 2 layer (a)：卡片仅 bot/webhook 可发，
	// 用户 /v1/message/send 一律拒绝。
	ErrMessageCardSendForbidden = register(codes.Code{
		ID:             "err.server.message.card_send_forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Card messages can only be sent by bots or webhooks.",
	})
	// ErrMessageCardEditForbidden Decision 7：P1 卡片不可变，用户编辑路径拒绝
	// type-17 content_edit（该路径对卡片永久关闭，P2 也不开放）。
	ErrMessageCardEditForbidden = register(codes.Code{
		ID:             "err.server.message.card_edit_forbidden",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Card messages cannot be edited.",
	})
	// ErrMessageCardActionInvalid P2 D3/D7/D11：card/action 上行的单一 400 归并码
	// （防枚举）——非卡片 / sender 非 bot / iwh_ 发送者 / action_id 不在生效帧 /
	// inputs 违反声明 / 卡片开关关闭 等具体原因只进日志，对客户端一律 invalid。
	ErrMessageCardActionInvalid = register(codes.Code{
		ID:             "err.server.message.card_action_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid card action.",
	})
	// ErrMessageCardActionDenied P2 D3：操作者不是卡片所在频道的成员。唯一的
	// 403 语义（与 invalid 分开，成员资格是授权面，不归并进防枚举）。
	ErrMessageCardActionDenied = register(codes.Code{
		ID:             "err.server.message.card_action_denied",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You are not allowed to act on this card.",
	})
	// ErrMessageCardActionInProgress P2 D4（PR#548 review）：并发下同一 (message_id,
	// action_id, operator_uid) 的首请求尚在处理、只占了 pending 位（未 confirm 入队）。
	// 回可重试的 409 而非虚假 replay 成功 —— 客户端按 D8 超时重试：彼时首请求要么已
	// confirm（→ replay），要么已释放（→ 本请求可重新 claim 正常处理），有效动作不丢。
	ErrMessageCardActionInProgress = register(codes.Code{
		ID:             "err.server.message.card_action_in_progress",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "This card action is being processed, please retry.",
	})
)
