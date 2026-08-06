package robot

// bot_setting —— Bot 级配置的通用存储与解析（task bot-setting-store）。
//
// 为什么不继续给 `robot` 表加列：本仓此前的一维 Bot 配置一律是列
// （inline_on / auto_approve / placeholder / bot_commands / app_bot.welcome_msg），
// 每加一个开关一次 ALTER，滚动发布还要手写并发守卫。本文件把后续 Bot 级配置收敛到
// KV，新增配置项只改注册表、不动 DDL。
//
// 与 bot_mention_pref 的关系：那张表是 (robot_id, group_no) **二维**，`robot` 扩列
// 在结构上做不到，开表是唯一选择，因此它不是本文件的先例——但它的**周边形状**是本
// 文件照搬的样板：owner 守卫、删除即回落、写后推事件失效 adapter 缓存。
//
// 解析链（三层，短路即返回）：
//
//	bot_setting 该 Bot 的显式覆盖 → system_setting 服务端全局默认 → 代码默认
//
// 「删除覆盖」== 回落上一层，**不是**「设为 false」。这个区分是 owner UI「恢复默认」
// 能正确渲染的前提，也是读接口必须同时返回 value / effective_value / source 的原因。

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	commonmodule "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// 配置值来源。owner 读接口把它原样下发，UI 据此渲染「跟随默认 / 已自定义」。
const (
	botSettingSourceBot     = "bot"     // 该 Bot 在 bot_setting 里的显式覆盖
	botSettingSourceGlobal  = "global"  // system_setting 的服务端全局默认
	botSettingSourceDefault = "default" // 代码默认
	botSettingSourceEnv     = "env"     // 派生自部署环境变量，不可写
)

const botSettingTypeBool = "bool"

// bot_setting 的 bool 字面量。与 system_setting 同口径（读侧另接受 true/false），
// 写入一律归一化成 "1"/"0"，避免库里出现三种写法。
const (
	botSettingTrue  = "1"
	botSettingFalse = "0"
)

// 已注册的 Bot 配置键。**新增配置项只改这里**：写入校验、owner 目录、
// 解析链三处共用同一份定义，不可能漂移。
//
// 卡片能力四键的分工（brief 的 load-bearing 项，改动前先读）：
//   - card_enabled 是**派生只读**值，等于 cardmsg.BotEnabled()，不落库、不可写。
//     做成只读是为了堵一个具体的坑：否则库里可以写着 true 而 env 关着，profile 报
//     能发、发出去被 card_disabled 拒，正好违反 pkg/cardmsg 已确立的「清单与发卡
//     门禁同源」不变量。
//   - display/interaction 只作用于 **raw 卡路径**（Bot 自拼 card JSON）；
//     reasoning 只作用于 **Registry 模板卡**。**raw 这一组与 reasoning 正交，但
//     display 与 interaction 之间不正交**：display 是 raw 档内的下限，octo/v1 要
//     display，octo/v2 要 display AND interaction。把三者一律说成「正交」是 round-1
//     评审推翻过的说法，照着它改会重开旁路——判定一律走 AllowsRawDisplayCard /
//     AllowsRawInteractiveCard，理由见那两个方法上的注释。
//     绝不可实现成「按 wire profile 一刀切」：推理卡自身横跨两档
//     （active/error 是 octo/v2、result 是 octo/v1），按 profile 切会把它砍成只剩
//     终态或只剩过程。
const (
	BotSettingKeyCardEnabled        = "bot.card_enabled"
	BotSettingKeyDisplayEnabled     = "bot.display_enabled"
	BotSettingKeyInteractionEnabled = "bot.interaction_enabled"
	BotSettingKeyReasoningEnabled   = "bot.reasoning_enabled"
)

// botSettingDef 是一个配置键的完整定义。
type botSettingDef struct {
	Key  string
	Type string
	// Editable=false 表示派生只读键：不落 bot_setting，写入必须被拒。
	Editable bool
	// GlobalDefault 读 system_setting 层。派生键为 nil。
	// 返回 (值, 是否已配置)——第二个返回值让解析器能区分「全局显式配了 false」
	// 与「全局没意见、落到代码默认」，否则 source 会报错层。
	GlobalDefault func(*commonmodule.SystemSettings) (bool, bool)
	// CodeDefault 是解析链最后一层。
	CodeDefault bool
	// Derived 非 nil 表示该键的值完全由服务端环境派生，忽略前两层。
	Derived func() bool
}

var botSettingDefs = []botSettingDef{
	{
		Key: BotSettingKeyCardEnabled, Type: botSettingTypeBool, Editable: false,
		// 总闸：OCTO_CARD_MESSAGE_ENABLED AND OCTO_BOT_CARD_ENABLED。未设即 false
		// （fail-closed）——正因为总闸保守，下面三项默认 true 才是安全的。
		Derived: cardmsg.BotEnabled,
	},
	{
		Key: BotSettingKeyDisplayEnabled, Type: botSettingTypeBool, Editable: true,
		GlobalDefault: func(s *commonmodule.SystemSettings) (bool, bool) {
			return s.SettingBoolOK("botcard", "display_enabled")
		},
		CodeDefault: true,
	},
	{
		Key: BotSettingKeyInteractionEnabled, Type: botSettingTypeBool, Editable: true,
		GlobalDefault: func(s *commonmodule.SystemSettings) (bool, bool) {
			return s.SettingBoolOK("botcard", "interaction_enabled")
		},
		CodeDefault: true,
	},
	{
		Key: BotSettingKeyReasoningEnabled, Type: botSettingTypeBool, Editable: true,
		GlobalDefault: func(s *commonmodule.SystemSettings) (bool, bool) {
			return s.SettingBoolOK("botcard", "reasoning_enabled")
		},
		CodeDefault: true,
	},
}

// findBotSettingDef 返回 key 的定义；未注册返回 nil。写入路径据此拒绝野键。
func findBotSettingDef(key string) *botSettingDef {
	for i := range botSettingDefs {
		if botSettingDefs[i].Key == key {
			return &botSettingDefs[i]
		}
	}
	return nil
}

// normalizeBotSettingBool 把入站 bool 字面量归一化成 "1"/"0"。第二个返回值为 false
// 表示非法值，调用方必须 400 —— 不接受「猜一个默认」，否则 owner 以为设成了 A，
// 实际存的是 B。
//
// 这是本 feature bool 字面量的**唯一词法**：trim + 折叠大小写，词表 {1,0,true,false}。
// 写侧（本函数）、读侧（parseBotSettingBool）、以及下一层 system_setting 的
// SettingBoolOK 三处必须完全一致——它们串在同一条解析链上，任一处的口径不同，都会
// 出现「这个字面量在这层算数、在那层不算数」。评审逮到过两次同型问题：一次是
// SettingBoolOK 放宽后管理台 getter 没跟上，一次就是本函数与 parseBotSettingBool
// 各写各的（后者当时既不 trim 也不认 True）。所以这里不再逐个列举大小写变体，
// 而是折叠——列举法正是上次分叉的成因：少列一个变体不会报错，只会静默不生效。
func normalizeBotSettingBool(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true":
		return botSettingTrue, true
	case "0", "false":
		return botSettingFalse, true
	default:
		return "", false
	}
}

// normalizeBotSettingValue 归一化一个入站 JSON value。
//
// 同时接受 JSON bool（`true`）与字符串字面量（`"true"` / `"1"`），使读接口下发的
// 形状可以原样写回：读侧给的是 bool，若写侧只认字符串，「读目录→改一项→PUT 回去」
// 这条最自然的客户端路径会直接 400。空值 / null / 其它类型一律拒绝。
func normalizeBotSettingValue(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", false
	}
	// JSON bool：读接口的下发形状。
	var asBool bool
	if err := json.Unmarshal(raw, &asBool); err == nil {
		if asBool {
			return botSettingTrue, true
		}
		return botSettingFalse, true
	}
	// 字符串字面量："1"/"0"/"true"/"false"，兼容既有调用方。
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return normalizeBotSettingBool(asString)
	}
	return "", false
}

// parseBotSettingBool 解析库里的 bool 值。非法值（人工改库等）视为未配置，
// 让解析器落到下一层，而不是把脏值当 false 生效。
//
// **委托给写侧的 normalizeBotSettingBool，不另写一份 switch**：读侧接受的集合从此
// 恒等于写侧接受的集合。此前两处各写各的，写侧 trim 且认 True、读侧两样都不认，于是
// 手工写入 value='True' 会被读成「覆盖值非法、已忽略」，而同一个字面量走写接口是被
// 接受的——同一列两套词法。委托是唯一能让这种漂移在语法上无法发生的写法。
func parseBotSettingBool(raw string) (value bool, ok bool) {
	normalized, ok := normalizeBotSettingBool(raw)
	if !ok {
		return false, false
	}
	return normalized == botSettingTrue, true
}

// botSettingResolution 是单个键的解析结果，同时是 owner 读接口的一行。
type botSettingResolution struct {
	Key string `json:"key"`
	Typ string `json:"type"`
	// Override 是该 Bot 在 bot_setting 里的显式覆盖；未设为 nil（JSON null）。
	// 用指针而非 bool 是刻意的：合并成单值后，UI 就分不出「我显式设成了 false」
	// 与「我没设、上层默认 false」，「恢复默认」也就无从渲染。
	Override *bool  `json:"value"`
	Eff      bool   `json:"effective_value"`
	Source   string `json:"source"`
	Editable bool   `json:"editable"`
}

// resolveBotSettingBool 是三层解析的**纯函数**核心：给定该 Bot 的覆盖行与全局默认，
// 算出有效值与来源。抽成纯函数是为了让优先级、来源标注与派生键短路都能无 DB 单测。
//
// override 为 nil 表示该 Bot 没有覆盖行（或行里是脏值）；
// globalValue/globalOK 来自 system_setting。
func resolveBotSettingBool(def botSettingDef, override *bool, globalValue, globalOK bool) botSettingResolution {
	out := botSettingResolution{Key: def.Key, Typ: def.Type, Editable: def.Editable}
	// 派生键短路：环境说了算，前两层一律忽略（也不该有覆盖行）。
	if def.Derived != nil {
		out.Eff = def.Derived()
		out.Source = botSettingSourceEnv
		return out
	}
	switch {
	case override != nil:
		out.Override = override
		out.Eff = *override
		out.Source = botSettingSourceBot
	case globalOK:
		out.Eff = globalValue
		out.Source = botSettingSourceGlobal
	default:
		out.Eff = def.CodeDefault
		out.Source = botSettingSourceDefault
	}
	return out
}

// BotCardConfig 是一个 Bot 的卡片能力有效配置——**已经和总闸 AND 过**。
// 消费方（/v1/bot/card/profile 下发、sendMessage 门禁）直接用，不再自行组合，
// 避免两处各算一遍而漂移。
type BotCardConfig struct {
	CardEnabled        bool
	DisplayEnabled     bool
	InteractionEnabled bool
	ReasoningEnabled   bool
}

// AllowsRawDisplayCard / AllowsRawInteractiveCard 是 raw 卡两个档位的**唯一判定**。
// 发送门、编辑门与 /v1/bot/card/profile 的清单都必须经它们，绝不各自组合布尔 ——
// round-2 评审逮到的正是这个：门改成了「v2 要 display AND interaction」，清单却仍
// 直接回显 InteractionEnabled，于是出现 profile 报 interaction_enabled:true、发卡却
// 被拒的自相矛盾，恰好是该端点存在的意义（免去「发一条卡看是否 400」）的反面。
//
// display 是**下限**而非并列档：octo/v2 是 octo/v1 的严格超集（cardmsg.validateCard
// 里的 interactive 只放宽校验、从不要求必须有交互元素），所以一张 v2 卡首先是展示卡，
// 只是额外带了交互面。若两者并列，关掉 display 就能被「改一个 profile 字符串」绕过。
func (c BotCardConfig) AllowsRawDisplayCard() bool { return c.DisplayEnabled }

func (c BotCardConfig) AllowsRawInteractiveCard() bool {
	return c.DisplayEnabled && c.InteractionEnabled
}

// rejectRobotCardBySetting 报告一个 legacy robot ingress 的 payload 是否需要经过
// per-Bot 卡片策略判定（即它是不是一张卡）。非卡片消息完全不查配置，零开销。
func rejectRobotCardBySetting(payload map[string]interface{}) bool {
	return cardmsg.IsCardPayload(payload)
}

// robotCardPolicyAllows 判定一张 legacy ingress 的 raw 卡是否被该 Bot 的配置放行。
//
// 与 bot_api 的 raw 门共用 AllowsRawDisplayCard / AllowsRawInteractiveCard，所以两条
// ingress 不可能对同一张卡给出不同结论 —— 这正是 payloadIsVail 注释里那句「三个入口
// 对称」要求的。未知 profile 一律拒（cardmsg.Validate 已经把 profile 钉在接受集内，
// 走到这里说明形状校验被绕过）。
//
// 本 ingress 不接受 Registry 模板（template_ref 只在 bot_api 实现），故无需 reasoning
// 判定；模板 ref 出现在这里会先被 cardmsg.Validate 按未知字段/形状拒掉。
func robotCardPolicyAllows(payload map[string]interface{}, cfg BotCardConfig) bool {
	profile, _ := payload["profile"].(string)
	switch profile {
	case cardmsg.ProfileV1:
		return cfg.AllowsRawDisplayCard()
	case cardmsg.ProfileV2:
		return cfg.AllowsRawInteractiveCard()
	default:
		return false
	}
}

// applyCardMasterSwitch 把总闸支配关系落到实处：总闸为假时三个子开关的有效值恒为假。
// 单独抽出来是因为它是 brief 的 load-bearing 不变量，需要被单测直接钉住。
func applyCardMasterSwitch(cfg BotCardConfig) BotCardConfig {
	if cfg.CardEnabled {
		return cfg
	}
	return BotCardConfig{}
}

// queryBotSettingOverrides 读该 Bot 的全部覆盖行。无行返回空 map，不是错误。
func queryBotSettingOverrides(ctx *config.Context, robotID string) (map[string]string, error) {
	type row struct {
		KeyName string `db:"key_name"`
		Value   string `db:"value"`
	}
	var rows []row
	_, err := ctx.DB().Select("key_name", "value").From("bot_setting").
		Where("robot_id=?", robotID).Load(&rows)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		// 空值 == 未配置，是本表 DDL 自己写下的定义（见 sql/20260806000001_bot_setting.sql）。
		// 在这里就丢掉，而不是留给 parseBotSettingBool 落进 default 分支——否则一行空值
		// 会被当成脏值告警「覆盖值非法」，与迁移里的定义自相矛盾。回落行为两种写法一致，
		// 差别只在日志说了真话还是假话。与 system_settings.lookup 把 "" 视作未命中同义。
		if r.Value == "" {
			continue
		}
		out[r.KeyName] = r.Value
	}
	return out, nil
}

// malformedBotSettingSeen 去重「脏覆盖值」告警：脏值是持久状态（人工改库 / 迁移
// 出错），而解析发生在每次发卡与每次 profile 读上，不去重就是每请求一条 Warn。
// 只有真正脏的 (robot, key) 才会入表，条目数被损坏行数界定，修好数据后重启即清。
var malformedBotSettingSeen sync.Map

// warnMalformedBotSetting 每个 (robot, key) 只喊一次。
func warnMalformedBotSetting(warn func(string, ...zap.Field), robotID, key, raw string) {
	if warn == nil {
		return
	}
	if _, dup := malformedBotSettingSeen.LoadOrStore(robotID+"\x00"+key, struct{}{}); dup {
		return
	}
	warn("bot_setting 覆盖值非法，已忽略并回落上一层（该 Bot 的这项配置未按你设置的生效）",
		zap.String("robot_id", robotID), zap.String("key", key), zap.String("value", raw))
}

// resolveBotSettings 解析该 Bot 的全部已注册键。DB 故障向上抛——配置读不到时
// **不能**假装成「用默认值」：那会让一次 DB 抖动静默放开本该关闭的能力。
//
// warn 收「脏值回落」的告警。它是参数而不是包级 logger，因为两个调用方
// （*Service 供 bot_api、*Robot 供 legacy ingress）各自有 tag 化的 log.Log，
// 日志里能看出是哪条路读到的脏值；测试传 nil 即静默。
func resolveBotSettings(
	ctx *config.Context,
	settings *commonmodule.SystemSettings,
	robotID string,
	warn func(string, ...zap.Field),
) ([]botSettingResolution, error) {
	overrides, err := queryBotSettingOverrides(ctx, robotID)
	if err != nil {
		return nil, err
	}
	out := make([]botSettingResolution, 0, len(botSettingDefs))
	for _, def := range botSettingDefs {
		var override *bool
		if raw, ok := overrides[def.Key]; ok && def.Editable {
			if v, valid := parseBotSettingBool(raw); valid {
				override = &v
			} else {
				// 脏值当作「未配置」回落，而不是当 false 生效——但必须留痕：
				// 静默回落时 owner 只会看到「跟随默认」，而他明明写过一个值
				// （round-3 评审 P2-6）。
				warnMalformedBotSetting(warn, robotID, def.Key, raw)
			}
		}
		var globalValue, globalOK bool
		if def.GlobalDefault != nil && settings != nil {
			globalValue, globalOK = def.GlobalDefault(settings)
		}
		out = append(out, resolveBotSettingBool(def, override, globalValue, globalOK))
	}
	return out, nil
}

// botCardConfigFrom 把解析结果投影成卡片四键，并施加总闸支配。
func botCardConfigFrom(resolutions []botSettingResolution) BotCardConfig {
	var cfg BotCardConfig
	for _, r := range resolutions {
		switch r.Key {
		case BotSettingKeyCardEnabled:
			cfg.CardEnabled = r.Eff
		case BotSettingKeyDisplayEnabled:
			cfg.DisplayEnabled = r.Eff
		case BotSettingKeyInteractionEnabled:
			cfg.InteractionEnabled = r.Eff
		case BotSettingKeyReasoningEnabled:
			cfg.ReasoningEnabled = r.Eff
		}
	}
	return applyCardMasterSwitch(cfg)
}

// BotCardConfig — IService — 返回该 Bot 的卡片能力有效配置（已 AND 总闸）。
// bot_api 的 profile 下发与 sendMessage 门禁共用它。
func (s *Service) BotCardConfig(robotID string) (BotCardConfig, error) {
	resolutions, err := resolveBotSettings(s.ctx, s.systemSettings, robotID, s.Warn)
	if err != nil {
		return BotCardConfig{}, err
	}
	return botCardConfigFrom(resolutions), nil
}

// BotCardConfig — IService — *Robot 变体，与 Service 走同一份解析。
func (rb *Robot) BotCardConfig(robotID string) (BotCardConfig, error) {
	resolutions, err := resolveBotSettings(rb.ctx, rb.systemSettings, robotID, rb.Warn)
	if err != nil {
		return BotCardConfig{}, err
	}
	return botCardConfigFrom(resolutions), nil
}

// listBotSettings 处理 GET /v1/robot/:robot_id/settings。
//
// 返回**全部已注册键**（不只是有覆盖行的），因为这份响应同时是 owner UI 的
// 「可配置项目录」——服务端注册一个新键，客户端不发版就能看到它。客户端遇到不认识
// 的 key 应跳过，不阻塞渲染。展示文案由客户端按 key 自持，服务端只给结构。
func (rb *Robot) listBotSettings(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	robotID := c.Param("robot_id")

	if rb.assertRobotOwner(c, robotID, loginUID) {
		return
	}

	resolutions, err := resolveBotSettings(rb.ctx, rb.systemSettings, robotID, rb.Warn)
	if err != nil {
		rb.Error("查询 bot_setting 失败", zap.Error(err), zap.String("robot_id", robotID))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return
	}
	// 该响应是某个 Bot 的私有配置，且只有登录态区分调用者：private 禁共享代理
	// 缓存，no-store 不留副本 —— 改完配置立刻重读要看到新值。
	//
	// Vary 命名的是**本路由实际使用的**鉴权头。用户会话走 `token`
	// （octo-lib/pkg/wkhttp/http.go 的 c.GetHeader("token")），不是 Authorization ——
	// /v1/bot/card/profile 才是 Authorization（bot token）。照抄那边会声明一个不影响
	// 响应的头，读起来像做了隔离其实没有。
	c.Header("Cache-Control", "private, no-store")
	c.Header("Vary", "token")
	c.Response(map[string]interface{}{"list": resolutions})
}

// botSettingWritePlan 是一条已校验、已归一化的待写项。
type botSettingWritePlan struct{ key, value string }

// botSettingUpdateReq 是批量写入体。批量而非单键，是为了让「同时关展示卡和交互卡」
// 这类操作只产生一次事件推送。
//
// Value 是 json.RawMessage 而非 string：读接口把 value 下发成 JSON bool
// （`true` / `false` / `null`），而 owner UI 的自然写法就是「读目录 → 改一项 → 写回」。
// 若写侧只收字符串，那条往返会在 BindJSON 就 400，每个客户端都得先自己 stringify。
// 收原始 JSON 后由 normalizeBotSettingValue 同时接受 bool 与字符串字面量。
type botSettingUpdateReq struct {
	Items []struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	} `json:"items"`
}

// updateBotSettings 处理 PUT /v1/robot/:robot_id/settings。
//
// 全量先校验、再统一写：一条 item 非法就整批拒绝，绝不半应用——否则 owner 在 UI 上
// 一次改三项、失败后看到的是三项里随机两项生效的中间态。
func (rb *Robot) updateBotSettings(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	robotID := c.Param("robot_id")

	var req botSettingUpdateReq
	if err := c.BindJSON(&req); err != nil {
		respondRobotRequestInvalid(c, "")
		return
	}
	if len(req.Items) == 0 {
		respondRobotRequestInvalid(c, "items")
		return
	}

	// 属主校验放在逐项校验**之前**（round-2/3 评审反复提到）：否则一个非属主用户
	// 能靠 400 与 403 的差别探测「某个键是否已注册且可写」，且对不存在 / 非自己的
	// Bot 会收到 400 而不是文档承诺的 404 / 403 —— 削弱的是本端点自己声明的属主语义。
	// 代价只是给形状错误的请求多一次查库，可以忽略。
	if rb.assertRobotOwner(c, robotID, loginUID) {
		return
	}

	plans := make([]botSettingWritePlan, 0, len(req.Items))
	seen := make(map[string]struct{}, len(req.Items))
	for _, item := range req.Items {
		def := findBotSettingDef(item.Key)
		if def == nil {
			// 未注册键：拒绝，不得长出野键（同 system_setting 的 schema 白名单契约）。
			respondRobotRequestInvalid(c, "key")
			return
		}
		if !def.Editable {
			// 派生只读键（card_enabled）：写入必须失败，否则库里会出现与 env
			// 相矛盾的值，profile 与发卡门禁随之背离。
			respondRobotRequestInvalid(c, "key")
			return
		}
		if _, dup := seen[item.Key]; dup {
			respondRobotRequestInvalid(c, "key")
			return
		}
		seen[item.Key] = struct{}{}

		normalized, ok := normalizeBotSettingValue(item.Value)
		if !ok {
			respondRobotRequestInvalid(c, "value")
			return
		}
		plans = append(plans, botSettingWritePlan{key: item.Key, value: normalized})
	}

	// 按 key 排序后再写：升级成单事务后，行锁要持有到 commit，两个并发批次若以相反
	// 顺序提交同样两个键（[display, interaction] vs [interaction, display]）就能互等成
	// 死锁（MySQL 1213）。逐条自动提交时锁立即释放、窗口可忽略，事务化之后不再是。
	// 统一锁序是本仓既有做法（见 send.go 的 "#627 D4 uniform lock order"）。
	sort.Slice(plans, func(i, j int) bool { return plans[i].key < plans[j].key })

	// 单事务提交整批。逐条自动提交会让「校验全过、第 2 条写失败」留下半应用状态：
	// 调用方收到 500 以为什么都没生效，实际前面几项已经落库；更糟的是错误路径会跳过
	// notifyBotSettingChanged，adapter 继续用旧缓存直到自己的 TTL 到期——正是该事件
	// 要消灭的那个窗口。事务失败即整批回滚，"绝不半应用"才真正成立。
	if err := writeBotSettingOverrides(rb.ctx, robotID, loginUID, plans); err != nil {
		rb.Error("写入 bot_setting 失败", zap.Error(err), zap.String("robot_id", robotID))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}

	// 事件只在提交成功之后推：提交失败时没有任何变更生效，推事件会让 adapter 白白
	// 重拉一次并读到与它已有缓存相同的值。
	rb.notifyBotSettingChanged(robotID)
	c.ResponseOK()
}

// writeBotSettingOverrides 在单事务里 upsert 整批覆盖。
//
// 抽成函数不只是为了短：它让「事务中途失败 → 整批回滚」能被直接测到。经 HTTP 走的
// 话，唯一可达的失败是校验失败，而校验在 Begin() 之前就返回了——那条路径对修复前后
// 的代码表现完全相同，测不出事务本身。
//
// 调用方负责：先校验、先排序（统一锁序）、先过属主门；提交成功后才推变更事件。
func writeBotSettingOverrides(
	ctx *config.Context,
	robotID, updatedBy string,
	plans []botSettingWritePlan,
) error {
	tx, err := ctx.DB().Begin()
	if err != nil {
		return err
	}
	defer tx.RollbackUnlessCommitted()

	for _, p := range plans {
		// dbr 的 InsertStmt 不暴露 Suffix，用 InsertBySql + ON DUPLICATE KEY UPDATE
		// 完成 upsert（同 setMentionPref）。updated_at 走列默认自动更新。
		if _, err := tx.InsertBySql(
			"INSERT INTO bot_setting (robot_id, key_name, value, updated_by) "+
				"VALUES (?, ?, ?, ?) "+
				"ON DUPLICATE KEY UPDATE value=VALUES(value), updated_by=VALUES(updated_by)",
			robotID, p.key, p.value, updatedBy,
		).Exec(); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// deleteBotSetting 处理 DELETE /v1/robot/:robot_id/settings/:key。
// 删除覆盖 == 回落上一层（全局默认 → 代码默认），**不是**设为 false。
// 幂等：删不存在的覆盖也返回 200（同 deleteMentionPref）。
func (rb *Robot) deleteBotSetting(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	robotID := c.Param("robot_id")
	key := c.Param("key")

	if rb.assertRobotOwner(c, robotID, loginUID) {
		return
	}

	def := findBotSettingDef(key)
	if def == nil || !def.Editable {
		respondRobotRequestInvalid(c, "key")
		return
	}

	_, err := rb.ctx.DB().DeleteFrom("bot_setting").
		Where("robot_id=? AND key_name=?", robotID, key).Exec()
	if err != nil {
		rb.Error("删除 bot_setting 失败", zap.Error(err),
			zap.String("robot_id", robotID), zap.String("key", key))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}

	rb.notifyBotSettingChanged(robotID)
	c.ResponseOK()
}

// botSettingChangedEventType 是配置变更事件的 event_type。
const botSettingChangedEventType = "bot_setting_updated"

// notifyBotSettingChanged 在配置变更后给该 Bot 推一条事件，让 adapter 即时重拉
// /v1/bot/card/profile，消除「改了开关要等插件缓存 TTL」的窗口。
//
// 用 EnqueueBotTypedEvent 而非 mention_pref 那条**群频道消息**路径：免@偏好本身是
// 群维度的，adapter 需要知道是哪个群，走群频道自然；Bot 级配置没有频道上下文，硬造
// 一个频道既不自然、又要求 Bot 是该频道成员（不成立时事件会静默丢失，退化成 TTL）。
// 类型化事件走同一个 GenSeq / ZAdd 收口，不需要伪造消息载荷。
//
// best-effort：异步 + recover，投递失败只记日志。写接口已经成功返回，事件只是加速
// 生效；插件侧的缓存 TTL 是兜底。
func (rb *Robot) notifyBotSettingChanged(robotID string) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				rb.Error("notifyBotSettingChanged panic", zap.Any("recover", r))
			}
		}()
		if _, err := rb.EnqueueBotTypedEvent(
			robotID, botSettingChangedEventType, buildBotSettingChangedEventData(),
		); err != nil {
			rb.Error("投递 bot_setting_updated 事件失败", zap.Error(err),
				zap.String("robot_id", robotID))
		}
	}()
}

// buildBotSettingChangedEventData 构造配置变更事件的 event_data。
//
// 刻意**不携带具体新值**：事件只是「你的配置变了，去重拉」的信号。带值就意味着两条
// 下发路径（事件 与 profile 接口）各自维护同一份语义，一旦漂移，adapter 会拿事件里
// 的旧形状去覆盖 profile 的权威结果。scope 字段留给未来区分变更域（卡片 / 其它），
// 让 adapter 能只重拉相关面而不是全量。
func buildBotSettingChangedEventData() map[string]interface{} {
	return map[string]interface{}{"scope": "bot_setting"}
}
