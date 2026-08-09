package bot_api

// card-message-interaction P2 D12（spec: .octospec/tasks/card-message-interaction/
// brief.md）：生产者能力发现。
//
// GET /v1/bot/card/profile —— 只读，挂在既有 botAPI 组（ba.authBot() bot-token 鉴权），
// 不加新限流、不经 SpaceMiddleware（清单是**部署级**能力常量，无租户/用户/消息数据可
// 隔离）。返回本部署的卡片能力清单，供 producer（bot SDK / OpenClaw 适配器）在启动时
// feature-detect，而非用「发一条卡看是否 400」探测 —— 400 无法区分「卡片功能关闭」与
// 「卡片非法」。
//
// 清单值全部取自 pkg/cardmsg 常量（**单一权威**，D12.2）—— 绝不在此重抄字面量，否则
// 常量变更时清单会与校验器静默漂移。profiles 取 cardmsg.AcceptedProfiles()（与校验器
// interactiveByProfile 的接受集同源）；elements/inputs/actions 取 cardmsg.DisplayElements()/
// InputElements()/DisplayActions()（与校验器白名单同源，反漂移由 pkg/cardmsg 守卫测试锁定）。
//
// enabled 取 cardmsg.BotEnabled()（部署级总开关 OCTO_CARD_MESSAGE_ENABLED AND bot 子
// 开关 OCTO_BOT_CARD_ENABLED 的有效门禁）：即便关闭也返回 200 + 全清单 —— feature
// detection 正是目的。清单与实际发卡门禁**同源**：send/edit 路径同样 gate 在
// BotEnabled()（send.go / card_revision.go），故 enabled 必然与「发卡是否会被受理」
// 一致，绝不出现「报 enabled 却发被拒」。
//
// wire contract 只增不改（additive-only，同 event_data 演进规则）：字段可新增，绝不
// 改名/删除/改类型 —— SDK 与适配器据此做能力探测与上限读取，改名等同破坏 event_data。

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// botCardConfigResolver is the narrow slice of robot.IService the card paths
// need: resolve one bot's effective card switches. Both the manifest and the
// send gate go through it, so what this deployment advertises and what it
// accepts are computed from one source.
type botCardConfigResolver interface {
	BotCardConfig(robotID string) (robot.BotCardConfig, error)
}

// resolveBotCardConfig reads the caller's effective switches, failing closed.
//
// Fail-closed matters in both directions: an unwired resolver or a DB error
// must not be answered with "everything is on", because that would re-open a
// capability the owner switched off, and it would make the manifest disagree
// with the send gate for the duration of the blip.
func (ba *BotAPI) resolveBotCardConfig(robotID string) (robot.BotCardConfig, error) {
	if ba.cardConfig == nil {
		return robot.BotCardConfig{}, errBotCardConfigUnavailable
	}
	return ba.cardConfig.BotCardConfig(robotID)
}

var errBotCardConfigUnavailable = errors.New("bot_api: card config resolver unavailable")

// botCardProfile handles GET /v1/bot/card/profile (D12).
func (ba *BotAPI) botCardProfile(c *wkhttp.Context) {
	// authBot 已在中间件层校验 bot-token；此处再确认存在 bot 身份（与同组 GET 端点
	// 一致的防御）。缺身份 → 既有 bot-auth 拒绝。
	//
	// 注意本端点**保持 botToken 鉴权、不做 owner 化**：它的语义是「Bot 读自己的
	// 有效配置」，而插件手里只有 bot token、没有 user session。owner 化等于废掉它。
	// 配置的**写**面才是 owner 专属（/v1/robot/:robot_id/settings）。
	robotID := getRobotIDFromContext(c)
	if robotID == "" {
		ba.respondBotAPIIdentityMissing(c)
		return
	}
	// 卡片能力的 per-Bot 有效配置（task bot-setting-store）。读失败必须 fail-closed：
	// 用默认值顶上会让一次 DB 抖动把 owner 关掉的能力静默放开，且此刻下发的清单会
	// 与 sendMessage 的门禁背离。
	cardConfig, err := ba.resolveBotCardConfig(robotID)
	if err != nil {
		ba.Error("查询 Bot 卡片配置失败", zap.Error(err), zap.String("robot", robotID))
		httperr.ResponseErrorL(c, errcode.ErrSharedInternal, nil, nil)
		return
	}
	// 本响应自带 `config` 起就是**Bot 私有**的，而 URL 对所有 Bot 完全相同——区分调用
	// 者的只有 Authorization 头。任何按 URL 缓存的共享代理都会把 Bot A 的配置回给
	// Bot B。private 禁掉共享缓存，no-store 连私有缓存也不留副本（配置改动要即时生效，
	// 本就不该有陈旧副本）。加在成功路径的最前面，确保和响应体一起写出。
	c.Header("Cache-Control", "private, no-store")
	// Belt-and-braces for an intermediary that honours Vary but mishandles
	// no-store: the response body is a function of the bot token, so a cache
	// keyed on URL alone would be keyed on the wrong thing.
	c.Header("Vary", "Authorization")
	// The manifest is resolved for *this* Bot and Space, not read off the
	// boot-time constant — review blocker on #720 (Jerry-Xin), and the whole
	// reason CapabilityFor exists.
	//
	// The static manifest is only correct while no dynamic version has shadowed
	// an ID. Once one has, `Capability()` advertises the static version that
	// `sendMessage` now refuses, and omits the granted dynamic templates that it
	// accepts — the exact "manifest says yes, send says 400" divergence this
	// endpoint exists to prevent. That divergence is what makes feature
	// detection worth doing at all, so an endpoint that answers from a
	// different source than the send path is not a lesser version of this
	// endpoint; it is a misleading one.
	//
	// Fail-closed on an unreadable runtime, for the same reason the send path
	// does: an unreadable catalog cannot prove an ID is *not* shadowed, and
	// answering with the static manifest would be asserting exactly that.
	templating, capErr := ba.cardTemplates.CapabilityFor(
		c.Request.Context(), ba.botCatalogPrincipalFor(c, robotID))
	if capErr != nil {
		ba.Error("解析 Bot 卡片能力清单失败", zap.Error(capErr), zap.String("robot", robotID))
		httperr.ResponseErrorL(c, errcode.ErrSharedInternal, nil, nil)
		return
	}
	c.Response(map[string]interface{}{
		"enabled":      cardmsg.BotEnabled(),
		"card_version": cardmsg.CardVersion,
		"profiles":     cardmsg.AcceptedProfiles(),
		// elements/inputs：本部署接受的展示元素 / 交互输入白名单（源自 pkg/cardmsg 权威
		// 列表，D12.2 单一权威）。producer / J3 gate 据此按**元素粒度**做前向兼容协商——
		// 即便 card_version 停在 "1.5"，也能探测本部署是否接受 Input.Number/Date/Time 等
		// additive 新增元素，消除「版本号不变 → gate 无法分辨新旧 1.5 部署」的错配。
		"elements": cardmsg.DisplayElements(),
		"inputs":   cardmsg.InputElements(),
		// actions：本部署接受的 octo/v1 **本地动作**白名单（OpenUrl / ToggleVisibility /
		// CopyToClipboard —— 无服务端回调，源自 pkg/cardmsg 权威列表 DisplayActions()，
		// 反漂移由 TestDisplayActionsAuthority 锁定）。producer 据此按**动作粒度**前向兼容
		// 协商——即便 card_version 停在 "1.5"、profiles 不变，也能探测本部署是否接受
		// ToggleVisibility / CopyToClipboard 等 additive 新增本地动作。Action.Submit 属
		// octo/v2 交互档，经 profiles 隐式发现、不在此列。
		"actions": cardmsg.DisplayActions(),
		// config is the per-Bot effective card policy (task bot-setting-store).
		// Values are already AND-ed with the deployment master switch, so a
		// producer reads one field instead of recombining `enabled` itself.
		"config": ba.botCardConfigResponse(cardConfig, templating),
		// templating is additive to the raw-card capability manifest. It advertises
		// only the explicit Bot catalog, never the broader Registry.List(). A nil
		// catalog occurs only in focused tests that do not install the production
		// composition root and reports supported:false rather than inventing refs.
		"templating": templating,
		"limits": map[string]interface{}{
			"max_payload_bytes":    cardmsg.MaxPayloadBytes,
			"max_nodes":            cardmsg.MaxNodes,
			"max_depth":            cardmsg.MaxDepth,
			"max_input_text_bytes": cardmsg.MaxInputTextBytes,
			"max_inputs_bytes":     cardmsg.MaxInputsBytes,
			// Action.CopyToClipboard.text 的 UTF-8 字节上限（与 actions 里的 CopyToClipboard
			// 对称，供 producer 按阈值 feature-detect，免去「发一条超限卡看 400」探测）。
			"max_copy_text_bytes": cardmsg.MaxCopyTextBytes,
		},
	})
}

// botCardConfigResponse renders the per-Bot effective card policy.
//
// Two invariants this function is responsible for, both asserted in tests:
//
//  1. reasoning_enabled=false ⟹ reasoning_template_ref is null. A ref alongside
//     a false switch invites a producer to send it anyway.
//  2. reasoning_enabled=true ⟹ the ref is one this deployment also advertises
//     under `templating.templates` in the same response, and therefore one the
//     send path will accept.
//
// Invariant 2 is why the ref is read out of the *same* manifest value this
// response carries, rather than resolved separately. It used to come from a
// boot-time lookup (botCardTemplateCatalog.AdvertisedRef, since retired), which
// made the two fields answer from different sources: once a dynamic version
// shadows ai.reasoning-process, `templating`
// would list the dynamic one while `reasoning_template_ref` still named the
// static version — and a producer that trusts the ref, as this field exists to
// be trusted, would send a version the gate refuses. Deriving both from one
// value makes the invariant hold by construction instead of by coincidence.
//
// A deployment whose catalog does not advertise the reasoning template at all
// reports reasoning_enabled=false regardless of configuration: the switch says
// "allowed", the catalog says "possible", and sending needs both.
func (ba *BotAPI) botCardConfigResponse(
	cfg robot.BotCardConfig,
	templating botTemplatingCapability,
) map[string]interface{} {
	reasoningEnabled := cfg.ReasoningEnabled
	var templateRef interface{}
	if reasoningEnabled {
		if ref, ok := advertisedRefIn(templating, aireasoningprocess.TemplateID); ok {
			templateRef = map[string]interface{}{
				"id": string(ref.ID), "version": ref.Version,
			}
		} else {
			reasoningEnabled = false
		}
	}
	return map[string]interface{}{
		"card_enabled": cfg.CardEnabled,
		// Both booleans come from the same predicates the send/edit gates call,
		// never from the raw struct fields. interaction_enabled in particular is
		// display AND interaction: advertising the raw switch while the gate
		// requires both is exactly the "manifest says yes, send says 400"
		// contradiction this endpoint exists to prevent.
		"display_enabled":        cfg.AllowsRawDisplayCard(),
		"interaction_enabled":    cfg.AllowsRawInteractiveCard(),
		"reasoning_enabled":      reasoningEnabled,
		"reasoning_template_ref": templateRef,
	}
}

// advertisedRefIn finds a template ID in an already-resolved manifest.
//
// D6 allows at most one advertised send version per ID, so the first match is
// the answer; a manifest carrying two would be a policy-construction bug the
// catalog constructor already refuses.
func advertisedRefIn(manifest botTemplatingCapability, id cardtmpl.ID) (botTemplateRef, bool) {
	for _, template := range manifest.Templates {
		if cardtmpl.ID(template.ID) == id {
			return botTemplateRef{ID: id, Version: template.Version}, true
		}
	}
	return botTemplateRef{}, false
}
