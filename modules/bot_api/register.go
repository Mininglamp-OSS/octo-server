package bot_api

import (
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

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
//
// The bound counts BYTES while VARCHAR(64) counts CHARACTERS. That is exact, not
// merely conservative, only because normalizeAgentHosting rejects non-ASCII
// before this comparison can matter — the ASCII property is load-bearing for the
// column-width invariant, not just for tidiness.
const maxAgentHostingLen = 64

// agentHostingPattern is the accepted shape: a lowercase ASCII slug.
//
// This is what keeps caller-controlled bytes out of the column and out of the
// owner-facing response — quotes, angle brackets, whitespace and control
// characters all fail it, without the server needing to know which vendors
// exist. An enum would have blocked those too, but only as a side effect of
// blocking everything it had not been told about.
//
// It is a DATA-QUALITY constraint, not an authorization one. Nothing here
// establishes that the caller is entitled to the value it reports: any holder of
// the bot's `bf_` token can claim `octo_hosted`, and a whitelist could not have
// prevented that either — it validates "the value is in the set", never "you are
// allowed to say it". Which is exactly why this column must not feed authz.
var agentHostingPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// isASCII reports whether s contains only single-byte characters.
//
// Checked BEFORE case folding, which is the whole point — see
// normalizeAgentHosting. Also what makes the byte-vs-character bound sound: an
// all-ASCII string's byte length equals its character count, so comparing
// len(s) against a VARCHAR(n) character width is exact rather than merely
// conservative.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// normalizeAgentHosting validates the reported value's shape.
//
// Order is load-bearing, in two separate ways.
//
// **Length before folding**, because the field is entirely caller-controlled and
// register takes an unbounded JSON body: strings.ToLower allocates a copy the
// size of its input, so a 10MB value must be rejected without paying for a 10MB
// lowercase copy of it. strings.TrimSpace is safe to run first — it returns a
// sub-slice and allocates nothing.
//
// **ASCII before folding**, because Go's simple lowercase mapping is not
// confined to ASCII: U+212A KELVIN SIGN folds to `k` and U+0130 LATIN CAPITAL I
// WITH DOT ABOVE folds to `i`, so `"\u212A_hosted"` would satisfy an
// ASCII-only regexp applied *after* the fold. That is not an injection (what
// would land in the column is a clean ASCII slug, and every such input has an
// ASCII twin the caller could simply report directly), but it silently collapses
// two distinct inputs onto one stored value and made this function's own
// documented invariant false. Rejecting non-ASCII up front is what makes
// "confusables are rejected" true rather than aspirational.
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
	if !isASCII(trimmed) {
		return "", false
	}
	folded := strings.ToLower(trimmed)
	if !agentHostingPattern.MatchString(folded) {
		return "", false
	}
	return folded, true
}

// BotRegisterReq is the optional request body for register. Every field is
// optional; a caller may send an empty body, no body at all, or malformed JSON —
// registration succeeds regardless (see registerUserBot).
//
// All four are POINTERS so that "absent" is distinguishable from "reported as
// empty". Absent leaves the column untouched *in SQL*; present overwrites,
// including with "" to clear. The three version fields were plain strings before
// this feature, merged against the stored value with "empty means unchanged" —
// which is observably identical to "absent from the statement" for a single
// writer, and strictly better under concurrency (see updateRobotAgentInfo).
// The wire contract is unchanged by the switch: omitting a field and sending it
// as "" were already indistinguishable to those three.
type BotRegisterReq struct {
	AgentPlatform *string `json:"agent_platform"`
	AgentVersion  *string `json:"agent_version"`
	PluginVersion *string `json:"plugin_version"`
	// AgentHosting differs from the three above in one way: for them an empty
	// report is meaningless, while here it is the way a runtime clears a stale
	// shape. On a self-hosted → platform-hosted switch a stale value is more
	// harmful than an empty one, so clearing must be expressible.
	AgentHosting *string `json:"agent_hosting"`
}

// maxRegisterBodyBytes caps the register request body.
//
// The body is pure telemetry — every field is optional and register succeeds
// without any of it — but until this cap existed, `binding.JSON`
// (json.NewDecoder with no limit) would decode whatever a token holder sent,
// and this route installs no other bound. Four sibling bot_api routes already
// cap theirs (send.go, batch_users.go, voice_adapter.go); this one did not,
// which mattered most on the App Bot branch, where the decode buys nothing but
// a log line.
//
// 4 KiB is generous for four short strings and small enough that the decode
// cannot be turned into an allocation lever. Overflow is treated as "no
// telemetry reported", never as a failed registration — see readAgentReport.
const maxRegisterBodyBytes = 4 << 10

// readAgentReport decodes the optional telemetry body under a size cap.
//
// Every failure mode — no body, empty body, malformed JSON, body over the cap —
// yields the zero value, i.e. "nothing was reported". None of them can fail the
// registration: this is the only channel a bot has to recover after losing its
// connection (#696). MaxBytesReader is applied rather than the error inspected,
// so an oversized body is cut off instead of buffered.
func readAgentReport(c *wkhttp.Context) BotRegisterReq {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRegisterBodyBytes)
	var req BotRegisterReq
	_ = c.ShouldBindJSON(&req)
	return req
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

	// Optional: parse agent runtime info (telemetry only; never fails register)
	req := readAgentReport(c)
	ba.applyAgentReport(robot.RobotID, &req)

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
//
// Note there is no merge against the stored row here, and no read of `robot`'s
// agent columns: an absent field must stay absent all the way into the SQL, so
// substituting the previously-read value would reintroduce exactly the lost
// update updateRobotAgentInfo's doc comment describes.
func (ba *BotAPI) applyAgentReport(robotID string, req *BotRegisterReq) {
	report := agentReport{
		Platform: req.AgentPlatform,
		Version:  req.AgentVersion,
		Plugin:   req.PluginVersion,
	}

	if req.AgentHosting != nil {
		normalized, valid := normalizeAgentHosting(*req.AgentHosting)
		if valid {
			report.Hosting = &normalized
		} else {
			// A malformed value leaves the stored one ALONE — it is not treated as
			// a report at all. The alternative (store "") would let one client
			// release that spells the value `self-hosted` (hyphenated, the exact
			// spelling of the GitHub Actions convention this naming follows) blank
			// the column fleet-wide, with nothing but a log line to show for it.
			// Clearing stays available, but only by reporting "" deliberately —
			// which is a well-formed report, not a rejected one.
			//
			// Never block register either: it is the only channel a bot has to
			// recover after losing its connection, and issue #696's second incident
			// was register being refused and bots never coming back up (see the
			// regression assertions in ratelimit_integration_test.go). The raw
			// value is NOT logged — it is caller-controlled and would carry
			// arbitrary bytes into the log.
			ba.Warn("Agent 上报的 agent_hosting 形状非法（应为小写 ASCII slug），本次忽略该字段，已存值保持不变",
				zap.String("robotID", robotID),
				zap.Int("rejectedLen", len(*req.AgentHosting)))
		}
	}

	if report.isEmpty() {
		return
	}
	if err := ba.db.updateRobotAgentInfo(robotID, report); err != nil {
		ba.Warn("更新Agent信息失败", zap.Error(err), zap.String("robotID", robotID))
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
	req := readAgentReport(c)
	if req.AgentHosting != nil || req.AgentPlatform != nil || req.AgentVersion != nil || req.PluginVersion != nil {
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
