package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// InteractiveCard(=17) 卡片消息协议错误码（card-message-protocol P1，spec:
// .octospec/tasks/card-message-protocol/brief.md）。POC 期集中在本文件；
// 实现 PR 定稿时可按仓库惯例拆回各 <module>.go。DefaultMessage 为 en-US 源
// （D4），zh-CN 运行时翻译在 pkg/i18n/locales/active.zh-CN.toml。
var (
	// ---- modules/message（用户 ingress / 编辑路径）-------------------------

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

	// ---- modules/bot_api（bot ingress / 编辑路径）--------------------------

	// ErrBotAPICardDisabled Decision 2 rollout gate：OCTO_CARD_MESSAGE_ENABLED
	// 未开启（默认关闭，客户端渲染门禁发布前不得开启）。
	ErrBotAPICardDisabled = register(codes.Code{
		ID:             "err.server.bot_api.card_disabled",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Card messages are not enabled on this server.",
	})
	// ErrBotAPICardInvalid 卡片信封/白名单/大小/URL 校验失败（具体原因进日志，
	// 不逐因扩码）。
	ErrBotAPICardInvalid = register(codes.Code{
		ID:             "err.server.bot_api.card_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid card payload.",
	})
	// ErrBotAPICardOBOForbidden Decision 2b：OBO 路径（含 grantorReplyBypass
	// 子路径）拒绝卡片 —— 按请求意图拦截，先于 OBO grant 校验。
	ErrBotAPICardOBOForbidden = register(codes.Code{
		ID:             "err.server.bot_api.card_obo_forbidden",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Card messages cannot be sent on behalf of a user.",
	})
	// ErrBotAPICardEditForbidden Decision 7：P1 卡片不可变（P2 sibling D6 以
	// cardmsg 对称校验 + card_seq CAS 解锁 bot 编辑路径后此码退役）。
	ErrBotAPICardEditForbidden = register(codes.Code{
		ID:             "err.server.bot_api.card_edit_forbidden",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Card messages cannot be edited yet.",
	})

	// ---- modules/robot（robot 编辑路径）------------------------------------

	// ErrRobotCardEditForbidden Decision 7 的 robot 编辑入口对称拦截。
	ErrRobotCardEditForbidden = register(codes.Code{
		ID:             "err.server.robot.card_edit_forbidden",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Card messages cannot be edited.",
	})
)
