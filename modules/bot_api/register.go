package bot_api

import (
	"regexp"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/botutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// Agent runtime hosting shape, self-reported at register time.
//
// The two constants below are the values this project itself uses. They are
// CONVENTIONS, not the permitted set: `agentHostingPattern` decides what is
// accepted, and any `<vendor>_hosted` slug passes. A third-party hosting
// provider must not need an octo-server release to identify itself, and hosting
// providers are a thing that grows — an enum here would turn "a new provider
// appeared" into a server deploy. Keeping the value space open also keeps
// provider names out of this open-source repo entirely: a provider's name lives
// in its own config and in the rows it writes, never in this file.
//
// Naming: `self_hosted` / `octo_hosted` mirrors the GitHub Actions runner
// convention (`self-hosted` vs `github-hosted`) and expresses *who is
// responsible for the runtime* — a self-hosted runtime going offline is the
// user's business, a platform-hosted one is ours. Deliberately NOT
// `local` / `cloud`: this product is deployable on-premise (README's
// "cloud is a choice, not a requirement"), so on a customer's own cluster the
// platform-hosted runtime is not in any "cloud". NOTE that with an open value
// space this is advice a client can ignore, not something the server enforces —
// `cloud` now passes validation. That is deliberate: the server has no business
// vetoing a client's vocabulary, and a stored odd value is easier to find and
// fix than a silently dropped one.
const (
	AgentHostingSelfHosted = "self_hosted"
	AgentHostingOctoHosted = "octo_hosted"
)

// maxAgentHostingLen MUST equal the robot.agent_hosting column width (64).
//
// Not "the column width plus headroom": the DB runs STRICT_TRANS_TABLES and the
// agent_* fields share ONE update statement, so a value that passes validation
// but overflows the column fails the write with 1406 and takes the whole
// agent_* group down with it — the caller's agent_platform / agent_version /
// plugin_version silently fail to store too (see the known limitation in the
// task brief). Headroom above the column width is not headroom, it is a delayed
// failure. TestAgentHostingBoundMatchesColumnWidth pins the equality.
const maxAgentHostingLen = 64

// agentHostingPattern is the accepted shape: a lowercase slug.
//
// This is what actually keeps caller-controlled bytes out of the column and out
// of the owner-facing response — quotes, angle brackets, whitespace, control
// characters and Unicode confusables all fail it, without the server needing to
// know which vendors exist. An enum would have blocked those too, but only as a
// side effect of blocking everything it had not been told about.
//
// It is a DATA-QUALITY constraint, not an authorization one. Nothing here
// establishes that the caller is entitled to the value it reports: any holder of
// the bot's `bf_` token can claim `octo_hosted`, and a whitelist could not have
// prevented that either — it validates "the value is in the set", never "you are
// allowed to say it". Which is exactly why this column must not feed authz.
var agentHostingPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// normalizeAgentHosting validates the reported value's shape.
//
// Order is load-bearing. TrimSpace first (returns a sub-slice, allocates
// nothing), then the length bound, and only then ToLower + the regexp: the field
// is entirely caller-controlled and register takes an unbounded JSON body, so a
// 10MB value must be rejected without ToLower allocating a 10MB copy of it.
//
// Case is folded rather than rejected: `Self_Hosted` and `self_hosted` carry the
// same intent, and folding avoids a silently-dropped report. Reporting an empty
// value is legal and means "clear it".
func normalizeAgentHosting(v string) (normalized string, ok bool) {
	trimmed := strings.TrimSpace(v)
	if trimmed == "" {
		return "", true
	}
	if len(trimmed) > maxAgentHostingLen {
		return "", false
	}
	folded := strings.ToLower(trimmed)
	if !agentHostingPattern.MatchString(folded) {
		return "", false
	}
	return folded, true
}

// BotRegisterReq is the optional request body for register.
type BotRegisterReq struct {
	AgentPlatform string `json:"agent_platform"`
	AgentVersion  string `json:"agent_version"`
	PluginVersion string `json:"plugin_version"`
	// AgentHosting is a POINTER on purpose, unlike the three strings above.
	//
	// Those merge field-wise with "empty keeps the stored value", which is right
	// for a version number. It is wrong here: on a self-hosted → platform-hosted
	// switch, a new runtime that omits the field would leave the stale
	// `self_hosted` in place, and for this field a stale value is more harmful
	// than an empty one. So: absent (nil) keeps the stored value, present
	// overwrites it — including with "" to clear.
	AgentHosting *string `json:"agent_hosting"`
}

// BotRegisterResp is the response for bot registration.
type BotRegisterResp struct {
	RobotID        string `json:"robot_id"`
	Name           string `json:"name"`
	IMToken        string `json:"im_token"`
	WSURL          string `json:"ws_url"`
	APIURL         string `json:"api_url"`
	OwnerUID       string `json:"owner_uid"`
	OwnerChannelID string `json:"owner_channel_id"`
}

// register handles POST /v1/bot/register for both User Bot and App Bot.
func (ba *BotAPI) register(c *wkhttp.Context) {
	token := extractBotToken(c)
	if token == "" {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIAuthFailed, nil, nil)
		return
	}

	if strings.HasPrefix(token, "app_") {
		ba.registerAppBot(c, token)
	} else {
		ba.registerUserBot(c, token)
	}
}

// registerUserBot handles registration for User Bot (bf_ token).
func (ba *BotAPI) registerUserBot(c *wkhttp.Context, token string) {
	robot, err := ba.db.queryRobotByBotToken(token)
	if err != nil {
		ba.Error("查询机器人失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIAuthCheckFailed, nil, nil)
		return
	}
	if robot == nil {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIAuthFailed, nil, nil)
		return
	}

	// Use bot_token as im_token — single token design
	imToken := robot.BotToken
	resp, tokenErr := ba.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         robot.RobotID,
		Token:       imToken,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
	})
	if tokenErr != nil || resp.Status != config.UpdateTokenStatusSuccess {
		ba.Error("获取IM Token失败", zap.Any("error", tokenErr), zap.String("robotID", robot.RobotID), zap.Any("status", resp))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIIMTokenFailed, nil, nil)
		return
	}
	if robot.IMTokenCache != imToken {
		ba.db.updateRobotIMTokenCache(robot.RobotID, imToken)
	}

	// Optional: parse agent runtime info
	var req BotRegisterReq
	_ = c.ShouldBindJSON(&req)
	ba.applyAgentReport(robot, &req)

	cfg := ba.ctx.GetConfig()
	apiURL := botutil.DeriveAPIURL(cfg)
	wsURL := botutil.DeriveWSURL(cfg)

	botName := ""
	if u, _ := ba.userService.GetUser(robot.RobotID); u != nil {
		botName = u.Name
	}

	c.Response(&BotRegisterResp{
		RobotID:        robot.RobotID,
		Name:           botName,
		IMToken:        imToken,
		WSURL:          wsURL,
		APIURL:         apiURL,
		OwnerUID:       robot.CreatorUID,
		OwnerChannelID: robot.CreatorUID,
	})
}

// applyAgentReport persists the optional agent runtime fields carried by a
// register call. No-op when the caller reported none of them.
//
// Extracted from registerUserBot so that function stays about registration
// (token → IM token → response) rather than growing a second concern.
func (ba *BotAPI) applyAgentReport(robot *robotModel, req *BotRegisterReq) {
	if req.AgentPlatform == "" && req.AgentVersion == "" && req.PluginVersion == "" && req.AgentHosting == nil {
		return
	}

	// The three version strings keep their long-standing merge contract:
	// an empty field means "unchanged", so fall back to the stored value.
	platform, version, plugin := req.AgentPlatform, req.AgentVersion, req.PluginVersion
	if platform == "" {
		platform = robot.AgentPlatform
	}
	if version == "" {
		version = robot.AgentVersion
	}
	if plugin == "" {
		plugin = robot.PluginVersion
	}

	// Pointer semantics: only a present field overwrites (see BotRegisterReq).
	//
	// The pointer is carried all the way down to the SetMap rather than being
	// resolved into "the value we just read" here: an absent field must leave the
	// column untouched in SQL. Reading the stored value and writing it back would
	// look equivalent but loses concurrent updates — two runtimes registering the
	// same bot (which nothing prevents today, see the occupancy-lock gap in the
	// task brief) can interleave as "A reads old → B writes new → A writes old
	// back".
	var hosting *string
	if req.AgentHosting != nil {
		normalized, valid := normalizeAgentHosting(*req.AgentHosting)
		if !valid {
			// Never block register on this: it is the only channel a bot has to
			// recover after losing its connection, and issue #696's second incident
			// was register being refused and bots never coming back up (see the
			// regression assertions in ratelimit_integration_test.go). A rejected
			// value degrades to "not reported" plus a log line, never to a failed
			// register. The raw value is NOT logged — it is caller-controlled and
			// would carry arbitrary bytes into the log.
			ba.Warn("Agent 上报的 agent_hosting 形状非法（应为小写 slug），按未上报处理",
				zap.String("robotID", robot.RobotID),
				zap.Int("rejectedLen", len(*req.AgentHosting)))
		}
		hosting = &normalized
	}

	// Unconditional write, even when nothing changed: agent_reported_at means
	// "when we last received a report", so it has to advance on every report. The
	// previous "skip the UPDATE if all values are equal" shortcut is gone on
	// purpose. Cost is acceptable — register is only called on (re)connect, not on
	// a heartbeat cadence (see modules/common/system_settings.go's note on the
	// heartbeat rate floor).
	if err := ba.db.updateRobotAgentInfo(robot.RobotID, platform, version, plugin, hosting); err != nil {
		ba.Warn("更新Agent信息失败", zap.Error(err), zap.String("robotID", robot.RobotID))
	}
}

// registerAppBot handles registration for App Bot (app_ token).
func (ba *BotAPI) registerAppBot(c *wkhttp.Context, token string) {
	appBot, err := ba.db.queryAppBotByToken(token)
	if err != nil {
		ba.Error("查询App Bot失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIAuthCheckFailed, nil, nil)
		return
	}
	if appBot == nil {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIAuthFailed, nil, nil)
		return
	}

	// Only published App Bots can register
	if appBot.Status != 1 {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIAuthFailed, nil, nil)
		return
	}

	// App Bot does not support the agent runtime fields: the app_bot table has no
	// agent_* columns (see modules/app_bot/sql), so there is nowhere to put them.
	// Parsing the body here exists ONLY to emit this warning — without it a
	// reporting client can never find out its report went nowhere.
	//
	// This is an explicit non-support, not an oversight: App Bots really are
	// driven by OpenClaw (app_bot.go's connectInfo hands the agent the
	// plugin_package + api_url connect guide). If symmetry is wanted later, the
	// place to add it is columns on app_bot — never a write into the robot table,
	// whose rows belong to User Bots.
	var req BotRegisterReq
	_ = c.ShouldBindJSON(&req)
	if req.AgentHosting != nil || req.AgentPlatform != "" || req.AgentVersion != "" || req.PluginVersion != "" {
		ba.Warn("App Bot 上报了 Agent 运行时信息，已忽略（App Bot 不支持 agent_* 字段）",
			zap.String("uid", appBot.UID))
	}

	// Design: App Bot uses the same token for API auth and IM WebSocket connection.
	// This is intentional — simpler than managing two tokens, and the caller already
	// possesses the token (used it to authenticate this request). Token rotation via
	// the admin API invalidates both simultaneously. Tradeoff acknowledged:
	// intercepting the WS connection reveals the API credential.
	imToken := appBot.Token
	resp, tokenErr := ba.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         appBot.UID,
		Token:       imToken,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
	})
	if tokenErr != nil || resp.Status != config.UpdateTokenStatusSuccess {
		ba.Error("App Bot IM Token注册失败", zap.Any("error", tokenErr), zap.String("uid", appBot.UID), zap.Any("status", resp))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIIMTokenFailed, nil, nil)
		return
	}

	cfg := ba.ctx.GetConfig()
	apiURL := botutil.DeriveAPIURL(cfg)
	wsURL := botutil.DeriveWSURL(cfg)

	c.Response(&BotRegisterResp{
		RobotID:        appBot.UID,
		Name:           appBot.DisplayName,
		IMToken:        imToken,
		WSURL:          wsURL,
		APIURL:         apiURL,
		OwnerUID:       appBot.CreatedBy,
		OwnerChannelID: appBot.CreatedBy,
	})
}
