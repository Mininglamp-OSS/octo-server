// Package cardmsg owns the server-side authoritative handling of
// InteractiveCard (ContentType=17) message payloads.
//
// card-message-protocol P1（spec: .octospec/tasks/card-message-protocol/brief.md，
// 交互契约见 sibling .octospec/tasks/card-message-interaction/brief.md）：
// 卡片消息信封 {type:17, card:<标准 Adaptive Cards 1.5 JSON, octo/v1 白名单子集>,
// plain:<server 权威纯文本>, card_version:"1.5", profile:"octo/v1"}。
//
// 本包与 pkg/richtext 同构（Validate 入站 gate + Finalize enrich 后收尾），是
// octo/v1 白名单的唯一强制权威（docs/card-protocol.md 只是它的镜像）：
//
//  1. Validate —— 入站 write-strict 校验：profile/card_version 协商（Decision 10）、
//     512KiB 上限作用在「完整」payload 字节（Decision 3a）、元素/动作白名单
//     （TextBlock/Image/Container/ColumnSet/Column/FactSet + Action.OpenUrl +
//     selectAction 仅限 OpenUrl）、递归节点数/嵌套深度上限（Decision 3c）、
//     http(s) 正向 URL allowlist 含 TextBlock markdown 链接（Decision 3d/6）。
//  2. Finalize —— 所有 server 端 enrich 之后调用：按 Decision 8 派生规则重算权威
//     plain 覆盖端上不可信 plain（永不为空），并对真实出站完整 payload 复检
//     512KiB 上限。
//
// P1 纪律（Decision 2/2b/7）：普通用户 ingress、bot OBO 路径、以及所有编辑路径
// （user /v1/message/edit、robot、bot_api /v1/bot/message/edit）一律拒绝 type=17
// —— P1 卡片不可变；P2 才对 bot 编辑路径开放（cardmsg 对称校验 + card_seq CAS）。
package cardmsg

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"

	"github.com/Mininglamp-OSS/octo-lib/common"
)

// InteractiveCard 是卡片消息的 ContentType(=17)。
//
// 命名（Decision 1）：显式叫 InteractiveCard —— octo-lib 已有 Card=7（名片），
// 两者无关，绝不可把新逻辑接到 common.Card 上。选号依据：15 是 Forward 的历史
// 保留槽、9/10 为历史空洞，17 经 octo-lib / octo-server / octo-im 三仓 grep 证明
// 未被占用（客户端仓复核是实现 PR 的 merge precondition）。
// P1 常量先落本包，octo-lib 上游化是 companion PR（Decision 5）。
const InteractiveCard common.ContentType = 17

const (
	// ProfileV1 是 P1 唯一接受的 profile 值（Decision 10；P2 增加 octo/v2）。
	ProfileV1 = "octo/v1"
	// CardVersion 是 P1 唯一接受的 card_version / 卡内 version 值。
	CardVersion = "1.5"

	// MaxPayloadBytes 完整 payload 序列化后的硬上限（512KiB，Decision 3a）——
	// 严格低于 modules/message 同步路径的 1MiB hardParsePayloadLimit 占位线。
	MaxPayloadBytes = 512 << 10
	// MaxSendBodyBytes 是 bot/robot send+edit 路由的 pre-decode HTTP body 上限
	// （Decision 3b）。BindJSON 在校验前完整解码，必须先钉住请求体大小。
	//
	// **不变量：必须 > 同路由上最大的合法 payload。** 这些路由同时承载 RichText
	// （octo-lib RichTextMaxPayloadBytes = 1MiB payload 上限）——若 body 上限取
	// 1MiB 会把一条恰好 1MiB 的合法 RichText（叠加信封/JSON 转义后 body > 1MiB）
	// 在 pre-decode 就 413 掉，即回归既有非卡片流量。取 2MiB 给 1MiB RichText +
	// 512KiB 卡片都留足信封余量，同时仍把无界滥用挡在解码前。voice 路由沿用
	// 自己的 5MiB。
	MaxSendBodyBytes = 2 << 20
	// MaxNodes 卡片树的递归节点数上限（Decision 3c）。
	MaxNodes = 200
	// MaxDepth 卡片树的嵌套深度上限（Decision 3c）。
	MaxDepth = 16

	// MaxCopyTextBytes Action.CopyToClipboard.text 的字节上限（octo/v1 自定义本地动作；
	// 复制到剪贴板的明文，4KiB 与单条 Input.Text 同量级）。text 逐字复制、不渲染，无
	// URL/markdown 面，故只做「必填 + 大小」结构校验。
	MaxCopyTextBytes = 4 << 10

	// PlaceholderCard 空卡兜底 / 内容型展示占位（Decision 8：plain 永不为空）。
	PlaceholderCard = "[卡片]"
	// PlaceholderImage Image 元素在 plain 里的占位，与 RichText 的 [图片] 一致。
	PlaceholderImage = "[图片]"

	// EnvEnabled 部署级开关（Decision 2 rollout gate，默认关闭）。
	// 前缀用 OCTO_（维护者决策：DM_ 为历史遗留，新开关不再使用）。
	EnvEnabled = "OCTO_CARD_MESSAGE_ENABLED"

	// EnvBotEnabled bot 侧卡片生成子开关（OCTO_ 前缀，同 EnvEnabled 约定）。
	// 与部署级总开关 EnvEnabled 的关系是「AND 且从属」：默认（未设/非法值）视为开启，
	// 只有显式 false 才单独禁掉 bot 发/改卡——语义见 BotEnabled。
	EnvBotEnabled = "OCTO_BOT_CARD_ENABLED"
)

// 校验错误（sentinel，调用方用 errors.Is 判定；细节经 %w 包装携带）。
var (
	// ErrCardPayloadTooLarge 完整 payload 超过 512KiB 上限。
	ErrCardPayloadTooLarge = errors.New("cardmsg: payload 超过 512KiB 上限")
	// ErrCardMissing 信封缺 card 字段 / card 不是非空 JSON 对象。
	ErrCardMissing = errors.New("cardmsg: card 必填且必须是非空对象")
	// ErrCardProfileUnsupported profile / card_version 不在接受集合（Decision 10）。
	ErrCardProfileUnsupported = errors.New("cardmsg: profile 或 card_version 不受支持")
	// ErrCardUnknownElement 卡片 body 中出现白名单之外的元素类型。
	ErrCardUnknownElement = errors.New("cardmsg: 元素类型不在 octo/v1 白名单")
	// ErrCardUnknownAction 动作类型不在白名单（P1 仅 Action.OpenUrl；selectAction 同）。
	ErrCardUnknownAction = errors.New("cardmsg: 动作类型不在 octo/v1 白名单")
	// ErrCardBadURLScheme URL 不满足正向 allowlist（仅绝对 http/https，Decision 3d）。
	// 覆盖 Image.url、Action.OpenUrl.url、selectAction 以及 TextBlock markdown 链接
	// （Decision 6）—— data:/javascript:/vbscript:/intent:/file:/相对路径 全部拒绝。
	ErrCardBadURLScheme = errors.New("cardmsg: url 只接受绝对 http/https")
	// ErrCardTooManyNodes 递归节点数超上限（Decision 3c）。
	ErrCardTooManyNodes = errors.New("cardmsg: 卡片节点数超过上限")
	// ErrCardTooDeep 嵌套深度超上限（Decision 3c）。
	ErrCardTooDeep = errors.New("cardmsg: 卡片嵌套深度超过上限")
	// ErrCardBadShape 结构性非法（body/actions/items/columns/facts 非期望形状）。
	ErrCardBadShape = errors.New("cardmsg: 卡片结构非法")
	// ErrCardInputInvalid card/action 上行 inputs 违反 D11 信任边界（未声明键 /
	// 类型不符 / 值超限 / 序列化总量超限）。端点侧统一映射到单一 400 invalid
	// （防枚举），细节只进日志。
	ErrCardInputInvalid = errors.New("cardmsg: inputs 不符合生效帧声明")
)

// IsCardPayload 判断 payload map 的 type 字段是否为 InteractiveCard(=17)。
// 兼容 json.Number / float64 / int（与 pkg/richtext.IsRichTextPayload 同口径）；
// string "17" 不识别，避免误命中。
func IsCardPayload(payload map[string]interface{}) bool {
	switch v := payload["type"].(type) {
	case float64:
		return int(v) == InteractiveCard.Int()
	case int:
		return v == InteractiveCard.Int()
	case json.Number:
		i, err := v.Int64()
		return err == nil && int(i) == InteractiveCard.Int()
	}
	return false
}

// IsCardRawPayload 是 IsCardPayload 的 []byte 便捷形态（消息表 / IM 查询返回的
// 原始 payload 字节 —— 编辑路径的「原消息是否卡片」判定用它）。解析失败视为非卡片。
func IsCardRawPayload(raw []byte) bool {
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false
	}
	return IsCardPayload(payload)
}

// IsCardContentEdit 判断编辑体（content_edit JSON 字符串）是否为 type=17 卡片信封。
//
// P1 Decision 7：卡片不可变 —— 所有编辑入口在调 richtext.NormalizeContentEdit
// 之前先用本函数拦截。背景（PR#525 round-2 finding #1）：NormalizeContentEdit
// 以 IsRichTextPayload 为门，type=17 的编辑体会「原样、零校验」通过，使编辑通道
// 成为绕过 Validate 的卡片 ingress。非 JSON / 非 17 返回 false，老编辑路径不变。
func IsCardContentEdit(contentEdit string) bool {
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(contentEdit), &payload); err != nil {
		return false
	}
	return IsCardPayload(payload)
}

// RejectsCardEdit 报告一次消息编辑是否必须按 P1 Decision 7（卡片不可变）拒绝：
// 要么被编辑的目标消息本身已是卡片（originalRaw，把卡片改掉），要么编辑体本身
// 是卡片信封（contentEdit，把普通消息改写成卡片、绕过 Validate）。两个编辑入口
// （bot_api、robot）都必须以本谓词单点门控，避免两条路各自拼守卫而漂移 —— PR#543
// review 发现 robot 路径漏了「目标是卡片」这半边。P2（D6）以 cardmsg 对称校验 +
// card_seq CAS 解锁编辑，届时调用方移除本 reject。
func RejectsCardEdit(originalRaw []byte, contentEdit string) bool {
	return IsCardRawPayload(originalRaw) || IsCardContentEdit(contentEdit)
}

// Enabled 报告部署级卡片开关（Decision 2 rollout gate）。默认关闭：客户端渲染
// 门禁发布前不得在生产开启。非法取值按关闭处理（fail-closed）。
func Enabled() bool {
	v := os.Getenv(EnvEnabled)
	if v == "" {
		return false
	}
	on, err := strconv.ParseBool(v)
	return err == nil && on
}

// BotEnabled 报告 bot 侧是否允许生成卡片。bot_api 的发卡（send）/改卡（card edit）、
// legacy robot ingress，以及 /v1/bot/card/profile 的 `enabled` 字段共用这一个「有效
// 门禁」，三者永不背离——profile 是 bot 的能力清单，若它报 enabled 而发卡却被拒，就
// 等于清单撒谎，破坏 feature detection。
//
// 语义是「总闸 AND 子开关」，与全局卡片能力解耦但从属于它：
//   - 部署级总开关 Enabled()（OCTO_CARD_MESSAGE_ENABLED）为假时恒为假——客户端渲染
//     门禁没放开，任何生产者都不该发卡，bot 不例外。
//   - 总开关为真时再看 bot 子开关 OCTO_BOT_CARD_ENABLED：未设或非法值默认「开启」
//     （向后兼容，行为同历史上「总开关开 ⇒ bot 能发」），仅显式 false 才单独把 bot
//     发/改卡关掉；此时内部通知 / incoming webhook / card_action 仍按总开关照常发卡。
func BotEnabled() bool {
	if !Enabled() {
		return false
	}
	v := os.Getenv(EnvBotEnabled)
	if v == "" {
		return true
	}
	on, err := strconv.ParseBool(v)
	// 非法值不误伤既有 bot 生产者：回退到「开启」（跟随总开关），只有可解析的 false
	// 才是明确的「单独关闭 bot 发卡」意图。
	return err != nil || on
}

// DisplayText 返回卡片消息的内容型占位文案，供会话摘要/置顶/引用等「按内容类型
// 描述消息」的服务端文案面使用（PR#525 round-2 finding #3 的本地 helper；
// octo-lib GetDisplayText 的 card 分支随 companion PR 上游化）。
func DisplayText() string {
	return PlaceholderCard
}
