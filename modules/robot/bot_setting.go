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
	"errors"
	"strings"

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
//     reasoning 只作用于 **Registry 模板卡**。三者正交。
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
func normalizeBotSettingBool(raw string) (string, bool) {
	switch strings.TrimSpace(raw) {
	case "1", "true", "TRUE", "True":
		return botSettingTrue, true
	case "0", "false", "FALSE", "False":
		return botSettingFalse, true
	default:
		return "", false
	}
}

// parseBotSettingBool 解析库里的 bool 值。非法值（人工改库等）视为未配置，
// 让解析器落到下一层，而不是把脏值当 false 生效。
func parseBotSettingBool(raw string) (value bool, ok bool) {
	switch raw {
	case botSettingTrue, "true", "TRUE":
		return true, true
	case botSettingFalse, "false", "FALSE":
		return false, true
	default:
		return false, false
	}
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
		out[r.KeyName] = r.Value
	}
	return out, nil
}

// resolveBotSettings 解析该 Bot 的全部已注册键。DB 故障向上抛——配置读不到时
// **不能**假装成「用默认值」：那会让一次 DB 抖动静默放开本该关闭的能力。
func resolveBotSettings(
	ctx *config.Context,
	settings *commonmodule.SystemSettings,
	robotID string,
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
	resolutions, err := resolveBotSettings(s.ctx, s.systemSettings, robotID)
	if err != nil {
		return BotCardConfig{}, err
	}
	return botCardConfigFrom(resolutions), nil
}

// BotCardConfig — IService — *Robot 变体，与 Service 走同一份解析。
func (rb *Robot) BotCardConfig(robotID string) (BotCardConfig, error) {
	resolutions, err := resolveBotSettings(rb.ctx, rb.systemSettings, robotID)
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

	resolutions, err := resolveBotSettings(rb.ctx, rb.systemSettings, robotID)
	if err != nil {
		rb.Error("查询 bot_setting 失败", zap.Error(err), zap.String("robot_id", robotID))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return
	}
	c.Response(map[string]interface{}{"list": resolutions})
}

// botSettingUpdateReq 是批量写入体。批量而非单键，是为了让「同时关展示卡和交互卡」
// 这类操作只产生一次事件推送。
type botSettingUpdateReq struct {
	Items []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
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

	type plan struct{ key, value string }
	plans := make([]plan, 0, len(req.Items))
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

		normalized, ok := normalizeBotSettingBool(item.Value)
		if !ok {
			respondRobotRequestInvalid(c, "value")
			return
		}
		plans = append(plans, plan{key: item.Key, value: normalized})
	}

	// 属主校验放在形状校验之后、写库之前：形状错误无需查库即可拒，而任何写入都
	// 必须先过属主门。
	if rb.assertRobotOwner(c, robotID, loginUID) {
		return
	}

	for _, p := range plans {
		// dbr 的 InsertStmt 不暴露 Suffix，用 InsertBySql + ON DUPLICATE KEY UPDATE
		// 完成 upsert（同 setMentionPref）。updated_at 走列默认自动更新。
		_, err := rb.ctx.DB().InsertBySql(
			"INSERT INTO bot_setting (robot_id, key_name, value, updated_by) "+
				"VALUES (?, ?, ?, ?) "+
				"ON DUPLICATE KEY UPDATE value=VALUES(value), updated_by=VALUES(updated_by)",
			robotID, p.key, p.value, loginUID,
		).Exec()
		if err != nil {
			rb.Error("写入 bot_setting 失败", zap.Error(err),
				zap.String("robot_id", robotID), zap.String("key", p.key))
			httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
			return
		}
	}

	rb.notifyBotSettingChanged(robotID)
	c.ResponseOK()
}

// deleteBotSetting 处理 DELETE /v1/robot/:robot_id/settings/:key。
// 删除覆盖 == 回落上一层（全局默认 → 代码默认），**不是**设为 false。
// 幂等：删不存在的覆盖也返回 200（同 deleteMentionPref）。
func (rb *Robot) deleteBotSetting(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	robotID := c.Param("robot_id")
	key := c.Param("key")

	def := findBotSettingDef(key)
	if def == nil || !def.Editable {
		respondRobotRequestInvalid(c, "key")
		return
	}

	if rb.assertRobotOwner(c, robotID, loginUID) {
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
