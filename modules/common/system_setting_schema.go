package common

import (
	"strconv"
	"strings"
)

// Value types accepted by system_setting.value_type.
const (
	settingTypeString    = "string"
	settingTypeBool      = "bool"
	settingTypeInt       = "int"
	settingTypeFloat     = "float"
	settingTypeEncrypted = "encrypted"
)

// settingIntMin / settingIntMax bound every settingTypeInt value, applied both
// on the admin write path (api_manager_system_setting.go) and in the clamping
// getters (getIntClamped). Today all int settings are day-window counts
// (sidebar.recent_filter_*_days), for which [0, 3650] (0 .. ~10 years) is a
// generous sane range; 0 is the documented "disable filter" sentinel. Adding
// an int setting that needs a different range should move this to a per-key
// field on settingDef — until then a single shared bound keeps the write path
// simple and closes the pre-existing "no bounds check" gap (issue #289).
const (
	settingIntMin = 0
	settingIntMax = 3650
)

// defaultBotCardSwitchEnabled is the code default for every per-bot card
// capability switch (display / interaction / reasoning). See the "botcard"
// entries in systemSettingSchema for why true is the safe choice here.
const defaultBotCardSwitchEnabled = true

// Sidebar recent-tab activity-filter defaults (issue #289). The recent tab of
// POST /v1/sidebar/sync hides conversations whose last activity is older than
// a per-channel-type window. These defaults reproduce the historical
// hard-coded behaviour exactly (groups/threads = 3-day window, DMs unfiltered)
// so the feature is zero-impact until an operator opts in. A value of 0
// disables the window for that channel type (return all, no time limit).
const (
	defaultSidebarRecentFilterGroupDays  = 3
	defaultSidebarRecentFilterThreadDays = 3
	defaultSidebarRecentFilterPersonDays = 0
)

// Thread auto-archive policy defaults + env fallbacks (task
// inactive-hiding-user-control, Batch 1 / P1).
//
// The archive worker's *policy* knobs (on/off + staleness window) live here
// rather than in modules/thread because they must resolve through the same
// DB → env → code-default chain the sidebar windows already use: a per-user
// override (Batch 2) needs a configuration layer it can sit on top of, and an
// env var cannot be that layer. The worker's *operational* knobs (tick
// interval, batch size, batch sleep) stay in modules/thread/archive_config.go —
// they tune how the sweep runs, not when a thread is considered finished.
//
// The env vars keep their original names so an existing deployment keeps
// working untouched: nothing is written to system_setting by this change, so
// the resolved value on rollout is byte-identical to today's.
const (
	envThreadAutoArchiveEnabled = "DM_THREAD_AUTO_ARCHIVE_ENABLED"
	envThreadAutoArchiveDays    = "DM_THREAD_AUTO_ARCHIVE_DAYS"

	defaultThreadAutoArchiveEnabled = false
	defaultThreadAutoArchiveDays    = 3
)

// settingDef is the canonical definition of a system_setting key.
// The schema slice below is the single source of truth: admin UI reads it to
// render the form, the helper consults it for type info, and the manager
// API rejects writes whose (category, key) is not present here.
type settingDef struct {
	Category    string
	Key         string
	Type        string // settingTypeString | settingTypeBool | settingTypeInt | settingTypeEncrypted
	Description string
	// Effective returns the generic value that is currently in effect for this
	// setting, applying the DB → yaml → code-default fallback chain. The
	// listSystemSettings handler uses this for ordinary settings. It
	// intentionally overrides the three manager-facing support.email* rows
	// with their database-only snapshot, while the ordinary SupportEmail*
	// getters continue to provide YAML fallback to user-facing mail flows.
	//
	// For settingTypeEncrypted, the returned string is plaintext — the
	// API layer is responsible for masking before serialisation; never
	// surface this value directly.
	Effective func(*SystemSettings) string
	// Positive, when set on a settingTypeInt / settingTypeFloat key, requires a
	// strictly-positive finite value on the admin write path and OPTS OUT of the
	// shared [settingIntMin, settingIntMax] bound (which exists for the
	// day-window int settings where 0 is a valid "disable" sentinel). Used by
	// rate-limit / quota knobs (incomingwebhook.*) where 0 / negative / NaN / Inf
	// would silently disable the control — the schema comment on settingIntMin
	// anticipated this per-key override. No artificial upper bound is imposed
	// (matches the env semantics these keys fall back to). Read-side defence is
	// in the typed getters (clamp ≤0 / non-finite → default).
	Positive bool
}

// systemSettingSchema enumerates every admin-tunable setting backed by the
// system_setting table. To add a new setting, append a row here and use the
// generic SystemSettings.getBool / getString / getInt / getEncrypted getter
// — no schema migration is required.
var systemSettingSchema = []settingDef{
	// Registration toggles — formerly yaml-only (Register.* in config.go).
	{Category: "register", Key: "off", Type: settingTypeBool, Description: "是否关闭注册",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.RegisterOff()) }},
	{Category: "register", Key: "only_china", Type: settingTypeBool, Description: "仅中国手机号可以注册",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.RegisterOnlyChina()) }},
	{Category: "register", Key: "username_on", Type: settingTypeBool, Description: "是否开启用户名注册",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.RegisterUsernameOn()) }},
	{Category: "register", Key: "email_on", Type: settingTypeBool, Description: "是否开启邮箱注册/登录",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.RegisterEmailOn()) }},

	// Local-account login master toggle — when on, hides local login UI and
	// rejects /v1/user/login, /v1/user/usernamelogin, /v1/user/emaillogin so
	// SSO-only deployments can route all users through OIDC/GitHub/Gitee.
	{Category: "login", Key: "local_off", Type: settingTypeBool, Description: "是否关闭本地账号登录入口",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.LocalLoginOff()) }},
	{Category: "login", Key: "scan_enabled", Type: settingTypeBool, Description: "是否开启扫码登录（默认关闭，客户端适配后显式开启）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.ScanLoginEnabled()) }},
	{Category: "login", Key: "manager_email_mfa_on", Type: settingTypeBool, Description: "是否开启管理控制台邮箱二次验证（仅保护管理控制台登录端点，默认关闭）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.ManagerEmailMFAOn()) }},

	// Space user-facing creation toggle — admin 关闭后客户端隐藏创建入口,
	// 后端 POST /v1/space/create 直接 403。env DM_SPACE_DISABLE_USER_CREATE
	// 仍作 fallback,DB 行为单一真源。
	{Category: "space", Key: "disable_user_create", Type: settingTypeBool, Description: "是否关闭普通用户创建空间入口",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.SpaceDisableUserCreate()) }},

	// Sidebar recent-tab activity filter — per-channel-type window in days for
	// POST /v1/sidebar/sync 的 recent tab。0 = 关闭该类型的时间过滤（全量返回）。
	// 默认复刻历史硬编码行为：群/话题 3 天窗口、DM 不过滤（issue #289）。
	{Category: "sidebar", Key: "recent_filter_group_days", Type: settingTypeInt, Description: "最近会话-群聊活跃过滤窗口(天)，0=不过滤",
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.SidebarRecentFilterGroupDays()) }},
	{Category: "sidebar", Key: "recent_filter_thread_days", Type: settingTypeInt, Description: "最近会话-话题(社区话题)活跃过滤窗口(天)，0=不过滤",
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.SidebarRecentFilterThreadDays()) }},
	{Category: "sidebar", Key: "recent_filter_person_days", Type: settingTypeInt, Description: "最近会话-单聊(DM)活跃过滤窗口(天)，0=不过滤(默认)",
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.SidebarRecentFilterPersonDays()) }},

	// 子区自动归档策略 — 从 env(DM_THREAD_AUTO_ARCHIVE_*) 迁入，DB 为单一真源、
	// env 降级为 fallback（task inactive-hiding-user-control / P1）。归档 worker
	// 每个 tick 重读，改值无需重启；effective_value 让「现网到底开没开、窗口几天」
	// 成为可查询事实，这是迁移的主要动机之一。
	//
	// days 与 sidebar.recent_filter_thread_days 之间存在偏序约束
	// （archive_days >= recent_filter_thread_days），在写路径强制：
	// api_manager_system_setting.go 的 updateSystemSettings 里，per-item 校验之后、
	// 开启事务之前的那段 orderingIncoming 收集 + ApplyThreadArchiveOrderingOverlay
	// + ViolatesThreadArchiveOrdering。三个 key 的写入都会经过它。
	{Category: "thread", Key: "auto_archive_enabled", Type: settingTypeBool, Description: "是否开启子区不活跃自动归档",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.ThreadAutoArchiveEnabled()) }},
	{Category: "thread", Key: "auto_archive_days", Type: settingTypeInt, Description: "子区自动归档的不活跃阈值(天)，0=禁用时间阈值",
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.ThreadAutoArchiveDays()) }},

	// Incoming webhook 总开关 + 核心阈值 — 与 env(DM_INCOMINGWEBHOOK_*) 等价，DB 为
	// 单一真源。enabled 关闭后 push 返回 404、管理写操作被拒、仅保留 list 只读；
	// 其余三项实时调阈值无需重启（SystemSettings 快照 60s 内多实例收敛）。
	{Category: "incomingwebhook", Key: "enabled", Type: settingTypeBool, Description: "是否开启群入站 Webhook（关闭后停止推送与管理写操作）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.IncomingWebhookEnabled()) }},
	{Category: "incomingwebhook", Key: "per_webhook_rps", Type: settingTypeFloat, Description: "单个 Webhook 每秒推送速率上限（令牌桶 rps）", Positive: true,
		Effective: func(s *SystemSettings) string { return floatToCanonical(s.IncomingWebhookPerWebhookRPS()) }},
	{Category: "incomingwebhook", Key: "per_webhook_burst", Type: settingTypeInt, Description: "单个 Webhook 推送突发上限（令牌桶 burst）", Positive: true,
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.IncomingWebhookPerWebhookBurst()) }},
	{Category: "incomingwebhook", Key: "max_per_group", Type: settingTypeInt, Description: "群本体作用域最多可创建的 Webhook 数量（子区另见 max_per_thread）", Positive: true,
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.IncomingWebhookMaxPerGroup()) }},
	{Category: "incomingwebhook", Key: "max_per_thread", Type: settingTypeInt, Description: "单个子区作用域最多可创建的 Webhook 数量（未配置或 0/≤0 时回退到群本体 max_per_group，并非「禁止子区建 Webhook」；写入要求为正整数）", Positive: true,
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.IncomingWebhookMaxPerThread()) }},
	{Category: "incomingwebhook", Key: "max_per_creator", Type: settingTypeInt, Description: "单个普通成员/机器人在一个投递作用域（群本体或每个子区）内最多可创建的 Webhook 数量（群主/管理员不受限）", Positive: true,
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.IncomingWebhookMaxPerCreator()) }},
	// 群级聚合天花板：非 Positive（允许 0=关闭），写入范围 [0, 3650]。0 表示不限制群 webhook
	// 总量，只受 max_per_group / max_per_thread 约束；> 0 才封顶单群跨所有作用域的 webhook 总数。
	{Category: "incomingwebhook", Key: "max_total_per_group", Type: settingTypeInt, Description: "单个群跨群本体+所有子区的 Webhook 总数上限；0=不限制（默认）",
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.IncomingWebhookMaxTotalPerGroup()) }},
	{Category: "incomingwebhook", Key: "member_can_broadcast", Type: settingTypeBool, Description: "非管理员成员创建的 Webhook 是否可用广播型 @（@所有人/@所有 AI）；关闭后即时收回成员广播，管理员创建的不受影响",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.IncomingWebhookMemberCanBroadcast()) }},

	// Bot API 三条限流通道（issue #696）。全部默认关闭 + 默认影子模式：
	//   enabled  —— kill switch。误配的后果按通道递增，register 层最严重（全部 bot
	//               无法注册），翻开关比回滚发版快一个数量级，故每层独立可关。
	//   dry_run  —— 影子模式：跑完整判定并上报观测，但不拦截、不下发 X-RateLimit-*
	//               头。「不下发头」是硬要求：影子期若下发，客户端会据此自行降频，
	//               观测到的就不再是真实流量，定值随之失去意义。
	// 初始配额是按端点语义推的保守偏宽值，不是实测结论；上线后按影子数据收敛。
	// rps/burst 标 Positive：≤0 会让令牌桶 Lua 走 rate<=0 短路，等于把整条路由打死
	// （不是"关闭限流"，是 100% 拒绝），读取侧另有回退兜底。
	{Category: "botratelimit", Key: "business_enabled", Type: settingTypeBool, Description: "是否对 /v1/bot 业务端点启用 per-bot 限流（关闭=完全旁路，不查 Redis 不设响应头）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.BotRateLimitBusinessEnabled()) }},
	{Category: "botratelimit", Key: "business_dry_run", Type: settingTypeBool, Description: "业务端点限流是否影子运行（只观测不拦截，用于定配额）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.BotRateLimitBusinessDryRun()) }},
	{Category: "botratelimit", Key: "business_rps", Type: settingTypeFloat, Description: "单个 Bot 在业务端点上的每秒速率上限（令牌桶 rps）", Positive: true,
		Effective: func(s *SystemSettings) string { return floatToCanonical(s.BotRateLimitBusinessRPS()) }},
	{Category: "botratelimit", Key: "business_burst", Type: settingTypeInt, Description: "单个 Bot 在业务端点上的突发上限（令牌桶 burst）", Positive: true,
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.BotRateLimitBusinessBurst()) }},

	{Category: "botratelimit", Key: "heartbeat_enabled", Type: settingTypeBool, Description: "是否对 /v1/bot/heartbeat 启用 per-bot 限流（按 bot 身份分配额）。该端点已移出全局 per-IP 桶，但鉴权前另有一道常开的 per-IP 底线（strict:bot_heartbeat，env 可调），所以关闭本开关并不等于该端点无限制",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.BotRateLimitHeartbeatEnabled()) }},
	{Category: "botratelimit", Key: "heartbeat_dry_run", Type: settingTypeBool, Description: "心跳限流是否影子运行（只观测不拦截）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.BotRateLimitHeartbeatDryRun()) }},
	{Category: "botratelimit", Key: "heartbeat_rps", Type: settingTypeFloat, Description: "单个 Bot 的心跳速率上限（令牌桶 rps）；读取侧强制不低于 0.1——心跳 key TTL 为 60s，配额过低会让这条保命通道自己制造断联", Positive: true,
		Effective: func(s *SystemSettings) string { return floatToCanonical(s.BotRateLimitHeartbeatRPS()) }},
	{Category: "botratelimit", Key: "heartbeat_burst", Type: settingTypeInt, Description: "单个 Bot 的心跳突发上限（令牌桶 burst）", Positive: true,
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.BotRateLimitHeartbeatBurst()) }},

	{Category: "botratelimit", Key: "register_enabled", Type: settingTypeBool, Description: "是否对 /v1/bot/register 启用 per-token 限流（该端点是掉线自愈的最后一环，同时是未鉴权可达的写入口，误配会让全部 Bot 无法注册）。它已随 heartbeat 一并移出全局 per-IP 桶——否则邻居 Bot 打满配额时它会被连坐，掉线的 Bot 换不到新 token、永远无法恢复；鉴权前另有一道常开的 per-IP 底线（strict:bot_register，env 可调）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.BotRateLimitRegisterEnabled()) }},
	{Category: "botratelimit", Key: "register_dry_run", Type: settingTypeBool, Description: "注册/换 token 限流是否影子运行（只观测不拦截）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.BotRateLimitRegisterDryRun()) }},
	{Category: "botratelimit", Key: "register_rps", Type: settingTypeFloat, Description: "单个 Bot token 的注册/刷新速率上限（令牌桶 rps）；读取侧强制不低于 0.05——它与 register_burst 的比值决定令牌桶 key 的 TTL，配额过低会放大 keyspace", Positive: true,
		Effective: func(s *SystemSettings) string { return floatToCanonical(s.BotRateLimitRegisterRPS()) }},
	{Category: "botratelimit", Key: "register_burst", Type: settingTypeInt, Description: "单个 Bot token 的注册/刷新突发上限（令牌桶 burst）；读取侧夹到 register_rps×20 以内——TTL 是 ceil(burst/rps×2)，而 register 的 key 由客户端提供的 token 决定基数，所以放大比值会放大每 IP 的 live key 数。调低 rps 会同时压低有效 burst，实际生效值见本列", Positive: true,
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.BotRateLimitRegisterBurst()) }},

	// Bot 卡片能力的**服务端全局默认**（task bot-setting-store）。这一层介于
	// bot_setting 的 per-Bot 覆盖与代码默认之间：某个 Bot 没有写过覆盖时读到这里，
	// 这里也没配时落到代码默认 true。
	//
	// 三个键都不含「总闸」——总闸是 cardmsg.BotEnabled()（OCTO_CARD_MESSAGE_ENABLED
	// AND OCTO_BOT_CARD_ENABLED），派生只读，不可写；它为假时下面三项的有效值恒为假。
	// 正因为总闸默认 fail-closed，这三项默认 true 才是安全的：运维开总闸即得到完整
	// 卡片能力，不必再逐 Bot 开一次。
	//
	// display/interaction 只作用于 **raw 卡路径**（Bot 自拼 card JSON），
	// reasoning 只作用于 **Registry 模板卡**：raw 这一组与 reasoning 正交，但
	// **display 与 interaction 之间不正交** —— display 是 raw 档内的下限（octo/v1 要
	// display，octo/v2 要 display AND interaction，因为 octo/v2 是 octo/v1 的严格超集）。
	// 判定收口在 modules/robot 的 AllowsRawDisplayCard / AllowsRawInteractiveCard；
	// 这里只是这两个开关的**全局默认值**，不重述判定。
	// 绝不可实现成「按 wire profile 一刀切」：推理卡自身横跨两档（active/error 是
	// octo/v2、result 是 octo/v1），按 profile 切会把它砍成只剩终态或只剩过程。
	{Category: "botcard", Key: "display_enabled", Type: settingTypeBool, Description: "Bot 未单独配置时，是否允许其发送展示型 raw 卡片（octo/v1，Bot 自拼卡片 JSON）；受卡片总闸 OCTO_CARD_MESSAGE_ENABLED 支配，总闸关闭时本项无效",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.BotCardDisplayEnabledDefault()) }},
	{Category: "botcard", Key: "interaction_enabled", Type: settingTypeBool, Description: "Bot 未单独配置时，是否允许其发送交互型 raw 卡片（octo/v2，含 Action.Submit / Input.*）；不影响服务端模板渲染出的推理进度卡",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.BotCardInteractionEnabledDefault()) }},
	{Category: "botcard", Key: "reasoning_enabled", Type: settingTypeBool, Description: "Bot 未单独配置时，是否允许其发送推理进度卡（服务端 Registry 模板卡）；关闭只阻止新建，已发出的卡仍可编辑到终态",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.BotCardReasoningEnabledDefault()) }},

	// App Bot 共享鉴权缓存的安全网 TTL（秒）。吊销靠共享 DEL 即时生效，此 TTL 仅兜底
	// DEL 失败 / 极窄的失效-回填竞态（见 modules/bot_api/registry_redis.go）。实时热更新
	// 无需重启；读取侧再夹紧到 [30, 86400]，超界回落默认值。标记 Positive 以放开
	// settingTypeInt 默认的 [0,3650] 上界（本键上限 86400）。
	{Category: "app_bot", Key: "auth_cache_ttl_seconds", Type: settingTypeInt, Description: "App Bot 鉴权缓存安全网 TTL(秒)，吊销经共享墓碑即时生效，此值仅兜底孤儿键/撤销写失败；调高会按比例拉长撤销写失败时被吊销 token 仍可全簇鉴权的最坏窗口；有效范围[30,600]，超出范围的写入会被接受但运行时回落默认 60s", Positive: true,
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.AppBotAuthCacheTTLSeconds()) }},

	// 每用户自定义贴纸数量上限（modules/sticker）。Positive 放开 settingTypeInt
	// 默认的 [0,3650] 上界并要求写入为正整数 —— 配额=0/负数会让用户一张都加不了
	// （暗关）。读取侧再夹紧（≤0 回落默认）。默认 100，无 env fallback。
	{Category: "sticker", Key: "user_max_count", Type: settingTypeInt, Description: "每个用户可创建的自定义贴纸数量上限", Positive: true,
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.StickerUserMaxCount()) }},

	// 自定义贴纸管理入口展示开关。仅用于通过 appconfig 告诉客户端是否展示入口，
	// 不改变 /v1/sticker/user 已有服务端读写权限；默认关闭，便于新能力灰度放量。
	{Category: "sticker", Key: "custom_enabled", Type: settingTypeBool, Description: "是否向客户端展示自定义贴纸管理入口",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.StickerCustomEnabled()) }},

	// 普通 Web/iOS/Android 客户端的消息 Reaction 能力。read/write 独立灰度：默认可读
	// 不可写；Bot 身份覆盖后续由独立 /v1/bot/profile 处理，不在公共 appconfig 推断身份。
	{Category: "message_reaction", Key: "read", Type: settingTypeBool, Description: "是否向普通客户端展示和下发消息 Reaction（默认开启；Bot 能力另行下发）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.MessageReactionReadEnabled()) }},
	{Category: "message_reaction", Key: "write", Type: settingTypeBool, Description: "是否向普通客户端展示添加/取消 Reaction 入口（默认关闭；Bot 能力另行下发）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.MessageReactionWriteEnabled()) }},

	// 自定义贴纸上传句柄强制开关（P0: Sticker Handle Enforcement Rollout）。这是「强制
	// 策略」，与「签名能力」OCTO_MASTER_KEY 彻底解耦——能力是部署级 env，策略是运营可
	// 在管理台热切的 DB 真源，互不派生。关闭（默认）= 兼容期：缺 handle 暂放行并记
	// compat_missing 指标；开启 = 强制：缺/伪造 handle 一律拒。放 DB 而非 env 才能灰度
	// toggle + 60s 多实例收敛 + 免重启回滚。客户端经 GET /v1/common/appconfig 的
	// sticker_handle_required 读取实时策略。
	{Category: "sticker", Key: "handle_required", Type: settingTypeBool, Description: "新增自定义贴纸是否强制校验上传句柄 handle（关闭=兼容期放行缺失句柄并观测，开启=缺/伪造一律拒；需服务端配有效 OCTO_MASTER_KEY 才有校验能力）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.StickerHandleRequired()) }},

	// docs 模块展示开关（客户端据此决定是否展示 docs 入口）。新增的 octo-docs-backend
	// 服务尚未上线，默认关闭；上线后由管理台切 docs.enabled 灰度放量。仅表达展示策略，
	// 不承担任何服务端鉴权。经 GET /v1/common/appconfig 的 docs_on 下发给客户端。
	{Category: "docs", Key: "enabled", Type: settingTypeBool, Description: "是否向客户端展示文档(docs)模块入口（octo-docs-backend 上线前默认关闭）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.DocsEnabled()) }},

	// Agent Mail 模块展示开关。默认关闭；部署完成 octo-mail 与网关配置后由管理台切
	// mail.enabled 放量。仅控制客户端入口展示，不替代 Agent Mail 既有鉴权。
	// 经 GET /v1/common/appconfig 的 mail_on 下发给客户端。
	{Category: "mail", Key: "enabled", Type: settingTypeBool, Description: "是否向客户端展示 Agent Mail 模块入口（默认关闭）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.MailEnabled()) }},

	// docs 全文搜索展示开关，与 docs.enabled 解耦：搜索端点由 octo-docs-backend #131
	// 独立提供，可晚于 docs 模块本体上线。默认关闭；上线且索引灰度完成后由管理台
	// 切 docs.search_enabled 放量。仅表达展示策略，不承担任何服务端鉴权（搜索鉴权在
	// octo-docs-backend 自身）。经 GET /v1/common/appconfig 的 docs_search_on 下发给客户端。
	{Category: "docs", Key: "search_enabled", Type: settingTypeBool, Description: "是否向客户端展示云文档全文搜索（与 docs.enabled 解耦；搜索端点上线+索引就绪前默认关闭）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.DocsSearchEnabled()) }},

	// drive(网盘)模块展示开关。独立部署的 octo-drive 服务尚未上线，默认关闭；上线后由管理台
	// 切 drive.enabled 灰度放量。仅表达展示策略，服务端鉴权在 octo-drive 自身。经 GET
	// /v1/common/appconfig 的 drive_on 下发给客户端。
	{Category: "drive", Key: "enabled", Type: settingTypeBool, Description: "是否向客户端展示网盘(drive)模块入口（octo-drive 上线前默认关闭）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.DriveEnabled()) }},

	// Loop(回路)模块展示开关。loop 依赖后端服务 + fleet 代理 + daemon runtime 一整套,未就绪前
	// 默认关闭;feature 分支合入 main 也不暴露,上线后由管理台切 dmloop.enabled 灰度放量。仅表达
	// 展示策略,不承担服务端鉴权(/fleet 鉴权在后端)。经 GET /v1/common/appconfig 的 dmloop_on 下发给客户端。
	{Category: "dmloop", Key: "enabled", Type: settingTypeBool, Description: "是否向客户端展示 Loop(回路)模块入口（后端服务上线前默认关闭）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.DmloopEnabled()) }},

	// 「我的 / 运行时」模块展示开关。与 dmloop.enabled 分开:「我的」后续会重新设计、脱离
	// loop 独立演进,故独立门控以便分阶段放量。默认关闭。经 appconfig 的 dmpersonal_on 下发。
	{Category: "dmpersonal", Key: "enabled", Type: settingTypeBool, Description: "是否向客户端展示「我的/运行时」模块入口（默认关闭；与 dmloop 分开以便独立放量）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.DmpersonalEnabled()) }},

	// 前端埋点(octo-dap)采集总开关。默认关闭，埋点层随发布上线但静默(fail-closed)：
	// octo-web 只有在 appconfig 下发 tracking_enabled 为真时才开始采集。采集器 collector
	// 与 TRACK_API_URL egress 在集群内验证通过前，运维保持此开关为关。仅表达采集策略，
	// 不承担服务端鉴权(collector 自身鉴权)。经 GET /v1/common/appconfig 的 tracking_enabled 下发给客户端。
	{Category: "tracking", Key: "enabled", Type: settingTypeBool, Description: "是否开启前端埋点(octo-dap)采集（collector 与 egress 在集群内验证通过前默认关闭）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.TrackingEnabled()) }},

	// 自定义贴纸上传限制（sticker-upload-compression 任务）。原先硬编码在
	// modules/file/const.go；挪进 system_setting 后可灰度/回滚，且每键都有
	// 服务端硬上限（stickerUpload*HardCap / stickerCompress*HardCap），误配也不会
	// 越出资源上限。全部 Positive:true 走"必须正整数"admin 写侧校验，同时放开
	// settingTypeInt 默认的 [0,3650] 上界（本组键上限单独校验，见读侧 clamp）。
	{Category: "sticker", Key: "upload_max_size_kb", Type: settingTypeInt, Description: "自定义贴纸单文件大小上限(KB)，服务端硬上限 5120(5MB)；默认 1024", Positive: true,
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.StickerUploadMaxSizeKB()) }},
	{Category: "sticker", Key: "upload_max_dimension", Type: settingTypeInt, Description: "自定义贴纸解码后单边像素上限，服务端硬上限 1024；默认 512", Positive: true,
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.StickerUploadMaxDimension()) }},
	// upload_allowed_formats 用字符串存 CSV，读侧与内置位图白名单求交集(只能收窄,
	// 不能放开非位图)。取消 Positive/整数校验，写侧不校验内容(anything goes)，非法
	// 项在读侧被丢弃；全部非法时读侧回退默认全 5 种，避免误配把功能暗关。
	{Category: "sticker", Key: "upload_allowed_formats", Type: settingTypeString, Description: "自定义贴纸允许扩展名(逗号分隔，如 gif,png,jpg,jpeg,webp)；只能与内置位图白名单取交集(收窄)，非位图会被读侧丢弃",
		Effective: func(s *SystemSettings) string { return strings.Join(s.StickerUploadAllowedFormats(), ",") }},

	// 自定义贴纸服务端压缩开关与调参（sticker-upload-compression 任务，方案 C：只压
	// 静态 jpg/png；webp/gif/动图 validate-only）。compress_enabled 默认关闭，运营
	// 灰度开启；compress_target_kb 是压缩目标(压缩后仍超即拒)；max_concurrency 与
	// timeout_ms 是稳定性闸(饱和/超时都 fail-open，走原路径不阻塞主链路)。
	{Category: "sticker", Key: "compress_enabled", Type: settingTypeBool, Description: "是否开启贴纸服务端压缩(仅静态 jpg/png；webp/gif 恒不压)；默认关闭以支持灰度",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.StickerCompressEnabled()) }},
	{Category: "sticker", Key: "compress_target_kb", Type: settingTypeInt, Description: "压缩目标(KB)；压缩后仍超此值将拒绝上传；硬上限 5120(5MB)，默认 1024", Positive: true,
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.StickerCompressTargetKB()) }},
	{Category: "sticker", Key: "compress_max_concurrency", Type: settingTypeInt, Description: "同时进行的贴纸压缩数量上限；饱和时该请求跳过压缩走原路径(fail-open)；硬上限 32，默认 4", Positive: true,
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.StickerCompressMaxConcurrency()) }},
	{Category: "sticker", Key: "compress_timeout_ms", Type: settingTypeInt, Description: "单次贴纸压缩超时(毫秒)；超时该请求跳过压缩走原路径(fail-open)；硬上限 10000，默认 2000", Positive: true,
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.StickerCompressTimeoutMs()) }},
	// compress_max_dimension 是压缩缩放目标边长(仅静态 jpg/png)：压缩开启后，大于此
	// 边长的静态图等比缩小到此值再重编码落库。默认 512 —— 让「>512 缩到 512」成为压缩
	// 开启后的开箱行为。维度门对 jpg/png 在压缩开启时放宽到硬上限 1024(随后缩到此值)，
	// gif/webp 及压缩关闭时仍受 upload_max_dimension 约束。硬上限 1024。
	{Category: "sticker", Key: "compress_max_dimension", Type: settingTypeInt, Description: "贴纸压缩缩放目标边长(px，仅静态 jpg/png)；压缩开启后大于此值等比缩小再存；硬上限 1024，默认 512。建议 ≤ upload_max_dimension，否则压后仍超 upload_max_dimension 的图会被 fail-closed 拒绝", Positive: true,
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.StickerCompressMaxDimension()) }},

	// Space 新成员欢迎语（onboarding.space_welcome_*）— task
	// space-new-user-welcome-message。这四个键必须构成一致的「启用组合」：
	// enabled=true 时，space_id 必须指向存在且未解散的 Space，active_from 可按
	// RFC3339(UTC) 解析，message trim 后非空且 ≤2000 code points。写侧做
	// prospective 组合校验（merge 快照+入参），worker/reconciler 每周期再校验一次
	// fail-closed。调用方读取一律走 SpaceWelcomeConfig()（单快照原子读四键），
	// 不要逐键读，避免跨 Reload() 拼出不一致组合。默认关闭，随快照 60s 内多实例收敛。
	// 文案是「所有人同一份纯文本」，不区分语言；支持换行（\n 原样保留、客户端 type:1
	// 文本渲染换行），不渲染 markdown。
	{Category: "onboarding", Key: "space_welcome_enabled", Type: settingTypeBool, Description: "是否开启「新成员加入指定 Space 时由通知助手发送一条欢迎语 DM」（默认关闭；启用需 space_id/active_from/message 构成有效组合）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.SpaceWelcomeConfig().Enabled) }},
	{Category: "onboarding", Key: "space_welcome_space_id", Type: settingTypeString, Description: "欢迎语目标 Space 的 space_id（必须存在且未解散）",
		Effective: func(s *SystemSettings) string { return s.SpaceWelcomeConfig().SpaceID }},
	{Category: "onboarding", Key: "space_welcome_active_from", Type: settingTypeString, Description: "欢迎语生效起点（RFC3339 UTC，如 2026-07-16T00:00:00Z）；仅 created_at>=此刻首次加入的成员会收到",
		Effective: func(s *SystemSettings) string { return s.SpaceWelcomeConfig().ActiveFromRaw }},
	{Category: "onboarding", Key: "space_welcome_message", Type: settingTypeString, Description: "欢迎语文案（纯文本，所有人同一份，trim 后非空，≤2000 字符；支持换行 \\n，不渲染 markdown）",
		Effective: func(s *SystemSettings) string { return s.SpaceWelcomeConfig().Message }},

	// Group 入群欢迎语总开关（onboarding.group_welcome_enabled）— task
	// group-welcome-message。平台级 dark-launch 开关：默认关闭；开启后，各群由群主/
	// 管理员在 /v1/groups/:group_no/welcome 自助配置的欢迎语，才会在成员首次入群时被
	// 公开发到群频道。回落 false 即为即时 kill（事件停写 + worker 停发，随快照多实例
	// 收敛），且不触及任何 per-group 配置行。与 space_welcome_* 不同：此开关仅管「启用」，
	// 没有平台级文案兜底（群文案恒来自各群自己的行）。是「总开关 AND 每群 enabled」的外层。
	{Category: "onboarding", Key: "group_welcome_enabled", Type: settingTypeBool, Description: "是否开启「群入群欢迎语」总功能（默认关闭；开启后各群由群主/管理员自助配置的欢迎语才会在成员首次入群时公开发到群频道；关闭=即时停发，不影响各群已保存的配置）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.GroupWelcomeEnabled()) }},

	// Email server config — formerly yaml-only (Support.* in config.go).
	{Category: "support", Key: "email", Type: settingTypeString, Description: "技术支持邮箱（发件人）",
		Effective: func(s *SystemSettings) string { return s.SupportEmail() }},
	{Category: "support", Key: "email_smtp", Type: settingTypeString, Description: "SMTP 服务器 host:port",
		Effective: func(s *SystemSettings) string { return s.SupportEmailSmtp() }},
	{Category: "support", Key: "email_pwd", Type: settingTypeEncrypted, Description: "SMTP 密码（可选，加密存储）",
		Effective: func(s *SystemSettings) string { return s.SupportEmailPwd() }},
}

// boolToCanonical normalises a bool to the same "0"/"1" representation that
// normaliseBool writes to the DB, so GET effective_value and POST request
// payloads use a single spelling end-to-end.
func boolToCanonical(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// floatToCanonical 把 float 规范成最短十进制表示（5.0 → "5"，0.5 → "0.5"），与
// settingTypeFloat 的 DB 存储 / POST 入参拼写保持一致，供 GET effective_value 用。
func floatToCanonical(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// schemaKey returns the canonical "category.key" string used as map key in
// the helper snapshot.
func schemaKey(category, key string) string {
	return category + "." + key
}

// findSchemaDef returns the schema entry for (category, key), or nil if not
// registered. Manager API write path uses this to reject unknown keys.
func findSchemaDef(category, key string) *settingDef {
	for i := range systemSettingSchema {
		d := &systemSettingSchema[i]
		if d.Category == category && d.Key == key {
			return d
		}
	}
	return nil
}
