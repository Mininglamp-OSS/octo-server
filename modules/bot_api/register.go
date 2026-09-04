package bot_api

import (
	"encoding/json"
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

	// AgentHostingNone is how a runtime RETRACTS a previously reported shape.
	//
	// An explicit "" would have been the obvious choice and is the wrong one. In
	// this request body "" already means "unchanged" for agent_platform /
	// agent_version / plugin_version — precisely because treating it as a clear
	// destroyed stored values on every reconnect for clients whose serializer
	// emits "" for fields they do not populate (PR #837 round 2, blocking). Giving
	// "" the opposite meaning for the fourth field in the same JSON object would:
	//
	//   - reproduce that same data loss for this column, just without pre-existing
	//     data to lose. A client that never populates the key but always emits it
	//     would land in ('', non-NULL) on every reconnect — the state the
	//     migration COMMENT and the response field both define as "reported, then
	//     deliberately cleared". The two-meanings-of-empty distinction, which is
	//     the entire reason the timestamp column exists, would read a serializer
	//     default as a deliberate retraction.
	//   - leave one wire shape meaning "keep" for three fields and "wipe" for the
	//     fourth, which is a footgun for client authors rather than a contract.
	//
	// A reserved slug costs the client one extra literal and makes retraction
	// explicit and greppable. It fits the open slug space (it satisfies
	// agentHostingPattern) and needs no separate field.
	AgentHostingNone = "none"
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
// The bound counts BYTES while VARCHAR(64) counts CHARACTERS. The length gate
// actually runs BEFORE the ASCII check (see normalizeAgentHosting — cheap
// rejection first), so on a non-ASCII input this comparison is merely
// conservative: it can only reject early, never admit something the column could
// not hold. It becomes exact for everything that survives, because non-ASCII is
// rejected unconditionally a line later. So the ASCII property is still
// load-bearing for the invariant — the ordering just means the bound is a floor
// before it, not that it is checked afterwards.
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
// **Length before folding.** The 4 KiB body cap now bounds the input ahead of
// this function, so the original argument (a 10MB value must not cost a 10MB
// lowercase copy to reject) no longer applies — but the ordering stays, because
// it costs nothing and keeps the bound independent of a limit enforced two call
// frames away. strings.TrimSpace is safe to run first: it returns a sub-slice
// and allocates nothing.
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
// same intent, and folding avoids a silently-dropped report.
//
// One honest exception to "non-ASCII is rejected": strings.TrimSpace uses
// unicode.IsSpace, so non-ASCII whitespace (U+00A0, U+3000) is stripped before
// isASCII ever sees it — `"\u00a0self_hosted"` is accepted, not rejected. Harmless
// in effect (what lands in the column is still a clean ASCII slug) and it makes a
// copy-pasted value work rather than fail cryptically, but it is an exception
// rather than a consequence of the rule, so it is written down here and pinned by a
// row in TestNormalizeAgentHosting.
//
// The empty string is NOT accepted as a report: it means "unchanged", and the
// caller is dropped before this function by applyAgentReport. Retraction comes in
// as AgentHostingNone and is normalized to "" here, which is what the column
// stores for "no shape" — so the stored representation stays a single empty value
// and readers need no knowledge of the sentinel.
func normalizeAgentHosting(v string) (normalized string, ok bool) {
	trimmed := strings.TrimSpace(v)
	if trimmed == "" {
		// Defensive: applyAgentReport does not call us with "". Treat it as
		// not-a-report rather than as a clear, matching the field contract.
		return "", false
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
	if folded == AgentHostingNone {
		// Retraction: store the empty value.
		return "", true
	}
	return folded, true
}

// BotRegisterReq is the optional request body for register. Every field is
// optional; a caller may send an empty body, no body at all, or malformed JSON —
// registration succeeds regardless (see registerUserBot).
//
// All four are POINTERS so that "absent" is distinguishable from "reported as
// empty" — but the two groups then treat that distinction differently, and only
// one of them changes behaviour relative to the pre-feature contract:
//
//   - The three version fields keep "empty means unchanged" exactly as they had
//     it when they were plain strings: omitted and "" are both no-ops.
//     updateRobotAgentInfo skips them on empty. The pointer switch buys the
//     lost-update fix (an absent field stays absent from the SQL rather than
//     being written back from a stale read) without changing what any existing
//     client observes.
//   - agent_hosting behaves the same way: "" means unchanged. Retracting a stale
//     shape is done by reporting the reserved slug AgentHostingNone ("none"), not
//     by reporting "" — see that constant for why.
//
// So "" is a no-op for all four fields, and no field in this body has a
// destructive empty value. An earlier revision gave "" clear-semantics, first for
// all four (which wiped stored version columns on every reconnect — both
// reviewers reproduced it end to end, PR #837 round 2) and then for agent_hosting
// alone (which reproduced the same shape for the new column and made one wire
// value mean two opposite things in one object, round 4).
type BotRegisterReq struct {
	AgentPlatform *string `json:"agent_platform"`
	AgentVersion  *string `json:"agent_version"`
	PluginVersion *string `json:"plugin_version"`
	// AgentHosting: report a slug to set it, AgentHostingNone to retract it. ""
	// means unchanged, same as the three above.
	AgentHosting *string `json:"agent_hosting"`
}

// MaxRegisterBodyBytes caps the register request body.
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
// Exported so the boundary test in modules/botfather can assert the EXACT byte
// count instead of a range. A range-based test ("~3.9 KiB adopted, 8 KiB
// rejected") leaves the constant free to move anywhere in between without going
// red, which is how round 5 found this unpinned.
const MaxRegisterBodyBytes = 4 << 10

// readAgentReport decodes the optional telemetry body under a size cap.
//
// Every failure mode — no body, nil body, empty body, malformed JSON, body over
// the cap — yields the zero value, i.e. "nothing was reported". None of them can
// fail the registration: this is the only channel a bot has to recover after
// losing its connection (#696). MaxBytesReader is applied rather than the error
// inspected, so an oversized body is cut off instead of buffered.
//
// The decode is ALL-OR-NOTHING, which is why it goes through a temporary:
// json.Decoder populates the fields it has already parsed before returning a type
// error, so binding straight into the result and ignoring the error would adopt a
// prefix of a malformed body — `{"agent_platform":"OpenClaw","agent_version":123}`
// would store the platform and silently drop the rest. Partial telemetry from a
// body the client got wrong is worse than none: it half-updates columns while
// looking like a successful report. Decoding into `staged` and requiring a clean
// decode before adopting it is what provides that property.
//
// DELIBERATELY NOT CHECKED: end-of-input. An earlier round added a
// `dec.Token()` call after Decode to also reject a valid object followed by
// trailing bytes. Do not restore it. Decode returns as soon as it has parsed one
// complete value; Token must read at least one more byte to tell "another token"
// from "end of input". MaxBytesReader bounds bytes, not TIME, and this route is
// served by a zero-value http.Server (gin's r.Run() — no ReadTimeout), so a
// client that sends one complete object and then stops without terminating the
// body leaves Token blocked on the socket forever, pinning a goroutine and a
// connection. On the User Bot branch that hold lands after UpdateIMToken has
// already mutated state. Trailing garbage after a valid object is therefore
// ignored, not rejected — a strictly smaller loss than making the bots'
// self-heal endpoint hangable. (PR #837 P1.)
func readAgentReport(c *wkhttp.Context) BotRegisterReq {
	// net/http guarantees a non-nil Body for server requests, but handlers are
	// also driven in-process by tests via http.NewRequest(..., nil), which leaves
	// it nil. gin's jsonBinding.Bind returns early on that; MaxBytesReader would
	// instead wrap a nil ReadCloser and panic on first Read.
	if c.Request.Body == nil {
		return BotRegisterReq{}
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxRegisterBodyBytes)
	var staged BotRegisterReq
	if err := json.NewDecoder(c.Request.Body).Decode(&staged); err != nil {
		return BotRegisterReq{}
	}
	return staged
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

// reportsAnything reports whether the body carries at least one non-empty value.
//
// "Present" is not the same as "reports something": "" means unchanged for every
// field in this body, so a client that always emits all four keys with empty
// values has reported nothing at all.
func reportsAnything(req *BotRegisterReq) bool {
	for _, f := range []*string{req.AgentPlatform, req.AgentVersion, req.PluginVersion, req.AgentHosting} {
		if f != nil && strings.TrimSpace(*f) != "" {
			return true
		}
	}
	return false
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

	// Optional: parse agent runtime info (telemetry only; never fails register).
	//
	// `stored` is passed ONLY so an identical legacy value can be skipped instead
	// of issuing a no-op write (see changed()). It is deliberately built here, at
	// the call site, rather than read inside applyAgentReport: those values must
	// never end up substituted for an absent field, and keeping the read out of
	// that function is what the source guards in agent_report_sql_test.go pin.
	req := readAgentReport(c)
	ba.applyAgentReport(robot.RobotID, &req, agentReport{
		Platform: &robot.AgentPlatform,
		Version:  &robot.AgentVersion,
		Plugin:   &robot.PluginVersion,
	})

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
func (ba *BotAPI) applyAgentReport(robotID string, req *BotRegisterReq, stored agentReport) {
	report := agentReport{
		Platform: req.AgentPlatform,
		Version:  req.AgentVersion,
		Plugin:   req.PluginVersion,
	}

	// An empty agent_hosting is "unchanged", exactly like its three siblings — so it
	// is dropped here rather than reaching normalizeAgentHosting. Retraction is
	// reported as AgentHostingNone.
	if req.AgentHosting != nil && strings.TrimSpace(*req.AgentHosting) != "" {
		normalized, valid := normalizeAgentHosting(*req.AgentHosting)
		if valid {
			report.Hosting = &normalized
		} else {
			// A malformed value leaves the stored one ALONE — it is not treated as
			// a report at all. The alternative (store "") would let one client
			// release that spells the value `self-hosted` (hyphenated, the exact
			// spelling of the GitHub Actions convention this naming follows) blank
			// the column fleet-wide, with nothing but a log line to show for it.
			// Retraction stays available, but only by reporting AgentHostingNone —
			// a well-formed report, not a rejected one.
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
	if err := ba.db.updateRobotAgentInfo(robotID, report, stored); err != nil {
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
	// Parsing the body here exists ONLY to emit this warning, and the warning is
	// for OPERATORS — the client still receives a bare 200 and cannot read server
	// logs, so it does NOT close the loop for the reporting client. What it buys is
	// that "a bot is reporting telemetry nobody stores" becomes greppable instead of
	// invisible. Telling the caller would need a response field, which is a wire
	// change this task did not take.
	//
	// This is an explicit non-support, not an oversight: App Bots really are
	// driven by OpenClaw (app_bot.go's connectInfo hands the agent the
	// plugin_package + api_url connect guide). If symmetry is wanted later, the
	// place to add it is columns on app_bot — never a write into the robot table,
	// whose rows belong to User Bots.
	req := readAgentReport(c)
	// Only warn for a field that actually carries a value. A client that always
	// emits the keys with "" is reporting nothing (that is what "" means for all
	// four fields), so warning on it would fire on every reconnect and say
	// something untrue.
	if reportsAnything(&req) {
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
