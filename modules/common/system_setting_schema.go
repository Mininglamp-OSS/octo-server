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

// settingDef is the canonical definition of a system_setting key.
// The schema slice below is the single source of truth: admin UI reads it to
// render the form, the helper consults it for type info, and the manager
// API rejects writes whose (category, key) is not present here.
type settingDef struct {
	Category    string
	Key         string
	Type        string // settingTypeString | settingTypeBool | settingTypeInt | settingTypeEncrypted
	Description string
	// Effective returns the value that is currently in effect for this
	// setting, applying the DB → yaml → code-default fallback chain. The
	// listSystemSettings handler uses this to populate `effective_value`
	// in the GET response so the admin UI can render the actual running
	// value even when the DB row is absent.
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

	// Incoming webhook 总开关 + 核心阈值 — 与 env(DM_INCOMINGWEBHOOK_*) 等价，DB 为
	// 单一真源。enabled 关闭后 push 返回 404、管理写操作被拒、仅保留 list 只读；
	// 其余三项实时调阈值无需重启（SystemSettings 快照 60s 内多实例收敛）。
	{Category: "incomingwebhook", Key: "enabled", Type: settingTypeBool, Description: "是否开启群入站 Webhook（关闭后停止推送与管理写操作）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.IncomingWebhookEnabled()) }},
	{Category: "incomingwebhook", Key: "per_webhook_rps", Type: settingTypeFloat, Description: "单个 Webhook 每秒推送速率上限（令牌桶 rps）", Positive: true,
		Effective: func(s *SystemSettings) string { return floatToCanonical(s.IncomingWebhookPerWebhookRPS()) }},
	{Category: "incomingwebhook", Key: "per_webhook_burst", Type: settingTypeInt, Description: "单个 Webhook 推送突发上限（令牌桶 burst）", Positive: true,
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.IncomingWebhookPerWebhookBurst()) }},
	{Category: "incomingwebhook", Key: "max_per_group", Type: settingTypeInt, Description: "单个群最多可创建的 Webhook 数量", Positive: true,
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.IncomingWebhookMaxPerGroup()) }},
	{Category: "incomingwebhook", Key: "max_per_creator", Type: settingTypeInt, Description: "单个普通成员/机器人在一个群内最多可创建的 Webhook 数量（群主/管理员不受限）", Positive: true,
		Effective: func(s *SystemSettings) string { return strconv.Itoa(s.IncomingWebhookMaxPerCreator()) }},
	{Category: "incomingwebhook", Key: "member_can_broadcast", Type: settingTypeBool, Description: "非管理员成员创建的 Webhook 是否可用广播型 @（@所有人/@所有 AI）；关闭后即时收回成员广播，管理员创建的不受影响",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.IncomingWebhookMemberCanBroadcast()) }},

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

	// Loop(回路)模块展示开关。loop 依赖后端服务 + fleet 代理 + daemon runtime 一整套,未就绪前
	// 默认关闭;feature 分支合入 main 也不暴露,上线后由管理台切 dmloop.enabled 灰度放量。仅表达
	// 展示策略,不承担服务端鉴权(/fleet 鉴权在后端)。经 GET /v1/common/appconfig 的 dmloop_on 下发给客户端。
	{Category: "dmloop", Key: "enabled", Type: settingTypeBool, Description: "是否向客户端展示 Loop(回路)模块入口（后端服务上线前默认关闭）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.DmloopEnabled()) }},

	// 「我的 / 运行时」模块展示开关。与 dmloop.enabled 分开:「我的」后续会重新设计、脱离
	// loop 独立演进,故独立门控以便分阶段放量。默认关闭。经 appconfig 的 dmpersonal_on 下发。
	{Category: "dmpersonal", Key: "enabled", Type: settingTypeBool, Description: "是否向客户端展示「我的/运行时」模块入口（默认关闭；与 dmloop 分开以便独立放量）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.DmpersonalEnabled()) }},

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
	// space-new-user-welcome-message。这五个键必须构成一致的「启用组合」：
	// enabled=true 时，space_id 必须指向存在且未解散的 Space，active_from 可按
	// RFC3339(UTC) 解析，中英文文案 trim 后非空且 ≤2000 code points。写侧做
	// prospective 组合校验（merge 快照+入参），worker/reconciler 每周期再校验一次
	// fail-closed。调用方读取一律走 SpaceWelcomeConfig()（单快照原子读五键），
	// 不要逐键读，避免跨 Reload() 拼出不一致组合。默认关闭，随快照 60s 内多实例收敛。
	{Category: "onboarding", Key: "space_welcome_enabled", Type: settingTypeBool, Description: "是否开启「新成员加入指定 Space 时由通知助手发送一条欢迎语 DM」（默认关闭；启用需 space_id/active_from/中英文文案构成有效组合）",
		Effective: func(s *SystemSettings) string { return boolToCanonical(s.SpaceWelcomeConfig().Enabled) }},
	{Category: "onboarding", Key: "space_welcome_space_id", Type: settingTypeString, Description: "欢迎语目标 Space 的 space_id（必须存在且未解散）",
		Effective: func(s *SystemSettings) string { return s.SpaceWelcomeConfig().SpaceID }},
	{Category: "onboarding", Key: "space_welcome_active_from", Type: settingTypeString, Description: "欢迎语生效起点（RFC3339 UTC，如 2026-07-16T00:00:00Z）；仅 created_at>=此刻首次加入的成员会收到",
		Effective: func(s *SystemSettings) string { return s.SpaceWelcomeConfig().ActiveFromRaw }},
	{Category: "onboarding", Key: "space_welcome_message_zh_cn", Type: settingTypeString, Description: "欢迎语中文文案（纯文本，trim 后非空，≤2000 字符）",
		Effective: func(s *SystemSettings) string { return s.SpaceWelcomeConfig().MessageZhCN }},
	{Category: "onboarding", Key: "space_welcome_message_en_us", Type: settingTypeString, Description: "欢迎语英文文案（纯文本，trim 后非空，≤2000 字符）",
		Effective: func(s *SystemSettings) string { return s.SpaceWelcomeConfig().MessageEnUS }},

	// Email server config — formerly yaml-only (Support.* in config.go).
	{Category: "support", Key: "email", Type: settingTypeString, Description: "技术支持邮箱（发件人）",
		Effective: func(s *SystemSettings) string { return s.SupportEmail() }},
	{Category: "support", Key: "email_smtp", Type: settingTypeString, Description: "SMTP 服务器 host:port",
		Effective: func(s *SystemSettings) string { return s.SupportEmailSmtp() }},
	{Category: "support", Key: "email_pwd", Type: settingTypeEncrypted, Description: "SMTP 密码（加密存储）",
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
