package botfather

import (
	"context"
	"embed"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/modules/base/common/msgtmpl"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"go.uber.org/zap"
)

// msgLog is a package logger for the outbound-message helpers, which are
// package functions (no commandHandler receiver) and so cannot use h.Log.
var msgLog = log.NewTLog("BotFather")

// templatesFS holds the per-language BotFather outbound-message templates.
// Layout: templates/{lang}/{domain}.tmpl, each file a set of {{define "key"}}
// blocks. See modules/base/common/msgtmpl for the rendering mechanism.
//
//go:embed templates
var templatesFS embed.FS

// botMessages is the BotFather outbound-message catalog. MustNew enforces the
// completeness matrix at init: if any supported language is missing a message
// the process fails loud at startup, so a half-translated build cannot ship a
// blank or wrong-language DM. The bodies are Markdown/plain text delivered over
// WuKongIM, NOT HTTP error envelopes — this is the fourth i18n category (#304),
// parallel to error codes (#279) and email templates (#221).
var botMessages = msgtmpl.MustNew(templatesFS, "templates")

// Message names — the {{define "..."}} keys. Kept as constants so call sites
// and the template tree cannot silently drift; allBotMessageKeys backs the
// completeness test.
const (
	// welcome.go
	MsgWelcome = "welcome"

	// command.go — generic / reused
	MsgUnknownCommand       = "unknown_command"
	MsgSendHelpHint         = "send_help_hint"
	MsgOperationCancelled   = "operation_cancelled"
	MsgOperationError       = "operation_error"
	MsgQueryFailedRetry     = "query_failed_retry"
	MsgQueryFailedCancelled = "query_failed_cancelled"
	MsgOperationFailedRetry = "operation_failed_retry"
	MsgNoBotsYet            = "no_bots_yet"
	MsgNoBotsShort          = "no_bots_short"
	MsgNameLengthInvalid    = "name_length_invalid"

	// command.go — newbot / create
	MsgNewBotPrompt      = "newbot_prompt"
	MsgCreateFailedRetry = "create_failed_retry"
	MsgBotCreatedNoInfo  = "bot_created_no_info"
	MsgCreatedPrompt     = "created_prompt"

	// command.go — mybots / selection
	MsgMyBotsList       = "mybots_list"
	MsgBotSelectionList = "bot_selection_list"
	MsgBotNotFoundRetry = "bot_not_found_retry"

	// command.go — connect / disconnect
	MsgSelectBotConnect      = "select_bot_connect"
	MsgSelectBotDisconnect   = "select_bot_disconnect"
	MsgConnectPrompt         = "connect_prompt"
	MsgDisconnectFailedRetry = "disconnect_failed_retry"
	MsgBotDisconnected       = "bot_disconnected"

	// command.go — setname / setdescription
	MsgSelectBotSetName  = "select_bot_setname"
	MsgSelectBotSetDesc  = "select_bot_setdesc"
	MsgSetNamePrompt     = "set_name_prompt"
	MsgSetDescPrompt     = "set_desc_prompt"
	MsgUpdateFailedRetry = "update_failed_retry"
	MsgBotNameUpdated    = "bot_name_updated"
	MsgDescTooLong       = "desc_too_long"
	MsgDescUpdated       = "desc_updated"

	// command.go — deletebot
	MsgSelectBotDelete   = "select_bot_delete"
	MsgDeleteConfirm     = "delete_confirm"
	MsgDeleteCancelled   = "delete_cancelled"
	MsgDeleteFailedRetry = "delete_failed_retry"
	MsgBotDeleted        = "bot_deleted"

	// command.go — token / revoke
	MsgSelectBotToken    = "select_bot_token"
	MsgSelectBotRevoke   = "select_bot_revoke"
	MsgTokenDisplay      = "token_display"
	MsgRevokeConfirm     = "revoke_confirm"
	MsgRevokeFailedRetry = "revoke_failed_retry"
	MsgTokenRevoked      = "token_revoked"

	// command.go — quickstart / install / help
	MsgQuickstart = "quickstart"
	MsgInstall    = "install"
	MsgHelp       = "help"

	// api_apply.go — apply flow (IM notifications + HTTP success messages)
	MsgApplyAutoApproved       = "apply_auto_approved"
	MsgApplySubmitted          = "apply_submitted"
	MsgNotifyOwnerNewApply     = "notify_owner_new_apply"
	MsgNotifyApplicantApproved = "notify_applicant_approved"
	MsgNotifyApplicantRejected = "notify_applicant_rejected"

	// api_apply.go / friend_approve.go — shared friend-added tip
	MsgFriendAddedTip = "friend_added_tip"

	// friend_approve.go — friend-apply DM replies & notifications
	MsgFriendBotNotFound     = "friend_bot_not_found"
	MsgFriendNotCreator      = "friend_not_creator"
	MsgFriendBotNotInSpace   = "friend_bot_not_in_space"
	MsgFriendApplyNotFound   = "friend_apply_not_found"
	MsgFriendAddFailed       = "friend_add_failed"
	MsgFriendApproved        = "friend_approved"
	MsgFriendRejected        = "friend_rejected"
	MsgFriendQueryFailed     = "friend_query_failed"
	MsgFriendPendingEmpty    = "friend_pending_empty"
	MsgFriendNoPendingForBot = "friend_no_pending_for_bot"
	MsgFriendApproveUsage    = "friend_approve_usage"
	MsgFriendRejectUsage     = "friend_reject_usage"
	MsgFriendApplyNotify     = "friend_apply_notify"
	MsgFriendPendingList     = "friend_pending_list"
	MsgFriendAgoJustNow      = "friend_ago_just_now"
	MsgFriendAgoDays         = "friend_ago_days"
	MsgFriendAgoHours        = "friend_ago_hours"
	MsgFriendAgoMinutes      = "friend_ago_minutes"
)

// allBotMessageKeys lists every key the code renders. The completeness test
// asserts each one resolves in every supported language AND equals the set the
// templates define (no orphan template, no missing key); keep it in sync when
// adding a message.
var allBotMessageKeys = []string{
	MsgWelcome,
	MsgUnknownCommand, MsgSendHelpHint, MsgOperationCancelled, MsgOperationError,
	MsgQueryFailedRetry, MsgQueryFailedCancelled, MsgOperationFailedRetry,
	MsgNoBotsYet, MsgNoBotsShort, MsgNameLengthInvalid,
	MsgNewBotPrompt, MsgCreateFailedRetry, MsgBotCreatedNoInfo, MsgCreatedPrompt,
	MsgMyBotsList, MsgBotSelectionList, MsgBotNotFoundRetry,
	MsgSelectBotConnect, MsgSelectBotDisconnect, MsgConnectPrompt, MsgDisconnectFailedRetry, MsgBotDisconnected,
	MsgSelectBotSetName, MsgSelectBotSetDesc, MsgSetNamePrompt, MsgSetDescPrompt, MsgUpdateFailedRetry, MsgBotNameUpdated, MsgDescTooLong, MsgDescUpdated,
	MsgSelectBotDelete, MsgDeleteConfirm, MsgDeleteCancelled, MsgDeleteFailedRetry, MsgBotDeleted,
	MsgSelectBotToken, MsgSelectBotRevoke, MsgTokenDisplay, MsgRevokeConfirm, MsgRevokeFailedRetry, MsgTokenRevoked,
	MsgQuickstart, MsgInstall, MsgHelp,
	MsgApplyAutoApproved, MsgApplySubmitted, MsgNotifyOwnerNewApply, MsgNotifyApplicantApproved, MsgNotifyApplicantRejected,
	MsgFriendAddedTip,
	MsgFriendBotNotFound, MsgFriendNotCreator, MsgFriendBotNotInSpace, MsgFriendApplyNotFound, MsgFriendAddFailed,
	MsgFriendApproved, MsgFriendRejected, MsgFriendQueryFailed, MsgFriendPendingEmpty, MsgFriendNoPendingForBot,
	MsgFriendApproveUsage, MsgFriendRejectUsage, MsgFriendApplyNotify, MsgFriendPendingList,
	MsgFriendAgoJustNow, MsgFriendAgoDays, MsgFriendAgoHours, MsgFriendAgoMinutes,
}

// botListItem is a row in the mybots / bot-selection list templates. Num is the
// 1-based index, computed in Go so the template needs no arithmetic helper; an
// empty Desc renders the template's localized "no description" fallback.
type botListItem struct {
	Num     int
	Display string
	Desc    string
}

// pendingApplyItem is a row in the /pending friend-request list template. Ago is
// a pre-rendered, localized relative-time phrase (see relativeAgo) so the
// list template carries no time/plural logic.
type pendingApplyItem struct {
	Num       int
	ApplyName string
	ApplyUID  string
	RobotID   string
	Ago       string
	Remark    string
}

// langResolveTimeout bounds the per-recipient language lookup so a slow
// Redis/DB never stalls a DM send; on timeout we fall back to the default
// language rather than block the conversational flow.
const langResolveTimeout = 2 * time.Second

// newBotLanguageService builds the per-recipient language resolver. It shares
// the same user_language:{uid} Redis cache as the auth-path resolver wired in
// main.go, so a user's stored preference is observed consistently here. Returns
// nil when no cache is configured (e.g. a degraded test ctx); recipientLanguage
// treats nil as "use the deployment default", never panicking.
func newBotLanguageService(ctx *config.Context) *user.LanguageService {
	if ctx == nil || ctx.Cache() == nil {
		return nil
	}
	return user.NewLanguageService(user.NewDB(ctx), ctx.Cache())
}

// recipientLanguage resolves the outbound language for a DM recipient.
//
// The BotFather message pipeline runs in a background goroutine (messagesListen
// → HandleMessage, or a lifecycle event) with no request context, so language
// comes from the recipient's stored preference (LanguageService →
// user_language:{uid} → DB), falling back to the deployment default
// (OCTO_DEFAULT_LANGUAGE) when unset or unresolvable.
//
// toUID may be Space-prefixed (s{space}_{uid}). extractRealUID strips the prefix
// ONLY when this goroutine pre-registered it via setSpacePrefixForUID: the
// message-processing path always does, while lifecycle events (welcome) pass a
// bare uid that needs no stripping. A raw s{space}_{uid} handed in WITHOUT that
// registration would resolve against the prefixed string and fall back to the
// default language — all current callers are covered, but keep this in mind for
// any new async entry point that synthesizes a Space-prefixed uid itself.
func recipientLanguage(langSvc *user.LanguageService, toUID string) string {
	realUID := extractRealUID(toUID)
	if langSvc != nil && realUID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), langResolveTimeout)
		defer cancel()
		lang, err := langSvc.Resolve(ctx, realUID)
		switch {
		case err != nil:
			// A chronically failing language service would otherwise silently
			// pin every recipient to the default language with no signal. Debug
			// (not Warn) keeps transient Redis/DB blips from flooding logs.
			msgLog.Debug("解析收件人语言失败，回退默认语言",
				zap.String("uid", realUID), zap.Error(err))
		case lang != "":
			return lang
		}
	}
	// Background ctx carries no negotiated language, so OutboundLanguage returns
	// OCTO_DEFAULT_LANGUAGE — the correct fallback for a recipient with no stored
	// preference.
	return octoi18n.OutboundLanguage(context.Background())
}

// replyL renders the named message in the recipient's language and sends it as
// a DM. It is the localized counterpart of reply: callers pass a message key +
// template data instead of a pre-built string. A render error is logged and the
// send is skipped — the completeness matrix + tests make this a build-time
// guarantee, so a runtime miss is a bug, not a user-facing blank message.
func (h *commandHandler) replyL(toUID, name string, data any) {
	lang := recipientLanguage(h.langSvc, toUID)
	content, err := botMessages.Render(name, lang, data)
	if err != nil {
		h.Error("渲染 BotFather 消息失败，跳过发送",
			zap.String("name", name), zap.String("lang", lang), zap.Error(err))
		return
	}
	h.reply(toUID, content)
}
