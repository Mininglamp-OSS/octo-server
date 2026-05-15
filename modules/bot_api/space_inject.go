// Package bot_api · YUJ-644 / Mininglamp-OSS#33 / YUJ-660 / YUJ-688
//
// PERSONAL DM 派发前为 payload 注入 Bot 的权威 SpaceID。WuKongIM 在 DM 上仅按
// 裸 uid 路由（无 Space 概念），收端客户端 SpaceFilter 唯一可信信号源是
// payload.space_id；任何客户端上送的值都不可信，必须服务端覆盖。
//
// 解析顺序（自上而下，最快路径优先）：
//  1. App Bot scope=space —— 直接读 gin-context 里 authAppBot 写入的
//     CtxKeyAppBotSpaceID（O(1)，无 DB 调用）。
//  2. 其它情况（User Bot、App Bot scope=platform）—— 用 querySpaceIDByRobotID
//     查 space_member ⨝ space。结果为空表示 Bot 当前没有归属 Space（孤儿 Bot
//     或非 Space 部署）。
//
// 失败模式：
//   - 真实 DB 错误 → warn + 不阻断发送（注入是优化，缺失走 fail-closed strip）。
//   - dbr.ErrNotFound（零结果）→ 视为"Bot 没有归属 Space"，不写 false-positive
//     DB 错误日志，fall through 到 strip-or-warn 分支。
//
// **YUJ-660 R3 Finding A — fail-closed strip语义（HIGH 修复）**：当 resolver 返
// 回 ""（任何原因：孤儿 Bot / DB 错误 / ErrNotFound），enrichBotPayloadWithSpaceID
// **必须删除** payload["space_id"]，并 emit `client_space_id_stripped=true` 监控
// warn（如果 client 上送过非空值）；payload 本就没有 space_id 时 emit
// `enrich_payload_space_id_empty=true`。
//
// 之前版本在 resolver 返回 "" 时保留 client payload —— 攻击者可以构造 DB 错误条
// 件（或孤儿 Bot 触发条件）伪造 payload.space_id="victim_space" 通过派发，realtime
// + offline push 都会信任这个值。strip 是唯一 fail-closed 行为；message 层 R2
// High-3 strip 只在 sendMsg 路径生效，bot_api / robot 路径需要本层独立 strip。
//
// **YUJ-688 / PR#43 R1 fix-up — platform App Bot validator gap (Critical from
// Jerry-Xin + lml2468)**: the X-Space-ID validator previously checked only
// `space_member`. Platform App Bots are inserted in `app_bot` with
// `scope='platform'` and never get a `space_member` row, yet they are
// legitimately visible in every active Space (`pkg/space/query.go:99`).
// Result: every valid platform App Bot dispatch with a valid X-Space-ID
// header was rejected by the validator and the caller's strip path downgraded
// the payload, sending the message as a personal DM with no SpaceID. The fix
// renames `isBotSpaceMember` to `isBotSpaceAuthorized` and broadens the SQL
// to honor the production `app_bot` rows (platform OR scope=space-with-match).
// Trim whitespace from the header value first to avoid noisy reject logs from
// "  space_X  " / trailing CR.
package bot_api

import (
	"errors"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// botSpaceQuerier is the minimal data dependency of resolveBotActiveSpaceID,
// extracted as an interface so unit tests can stub the DB call without
// constructing a full *botAPIDB. *botAPIDB satisfies it implicitly.
//
// Mininglamp-OSS/octo-server#36 expansion:
//   - querySpaceIDsByRobotID returns the full ordered match list so the
//     resolver can warn when a User Bot is in multiple Spaces (the ambiguity
//     case Option C makes deterministic but doesn't *fix*).
//   - isBotSpaceAuthorized validates an X-Space-ID header hint before honoring
//     it (Option B). Required because the legacy header path is not gated by
//     SpaceMiddleware on /v1/bot/sendMessage. Authorization is broader than
//     space_member: it also recognizes platform App Bots (scope='platform'
//     visible in every active Space) and scope=space App Bots dispatching
//     into their own Space — see modules/bot_api/db.go for the full rule.
type botSpaceQuerier interface {
	querySpaceIDByRobotID(robotID string) (string, error)
	querySpaceIDsByRobotID(robotID string) (string, []string, error)
	isBotSpaceAuthorized(robotID, spaceID string) (bool, error)
}

// enrichBotPayloadWithSpaceID 在 PERSONAL DM 派发前用 Bot 的权威 SpaceID 覆盖
// payload.space_id。仅在 channel_type == Person 时调用。
//
// YUJ-660 R3 Finding A — 当 resolver 返回 "" 时 fail-closed strip：删除任何
// client 上送的 payload["space_id"]，并发监控 warn。这是 bot_api 层独立的 strip
// 语义，不能依赖 message 层的 senderSpaceID="" strip（bot_api 不走 sendMsg）。
func (ba *BotAPI) enrichBotPayloadWithSpaceID(c *wkhttp.Context, robotID string, payload map[string]interface{}) map[string]interface{} {
	if payload == nil {
		payload = make(map[string]interface{})
	}
	spaceID := ba.resolveBotActiveSpaceID(c, robotID)
	if spaceID != "" {
		payload["space_id"] = spaceID
		return payload
	}
	// SpaceID 不可解析（孤儿 Bot / DB 错误 / ErrNotFound）：strip client 上送，
	// fail-closed。客户端 SpaceFilter 唯一可信信号是服务端 payload.space_id，
	// 服务端无可信值时绝不允许 client 注入信号。
	if cur, ok := payload["space_id"].(string); ok && cur != "" {
		delete(payload, "space_id")
		ba.Warn("client_space_id_stripped",
			zap.Bool("client_space_id_stripped", true),
			zap.String("dispatcher", "bot_api"),
			zap.String("robotID", robotID),
			zap.String("client_supplied", cur),
		)
	} else {
		ba.Warn("enrich_payload_space_id_empty",
			zap.Bool("enrich_payload_space_id_empty", true),
			zap.String("dispatcher", "bot_api"),
			zap.String("robotID", robotID),
		)
	}
	return payload
}

// resolveBotActiveSpaceID 优先读 gin-context（App Bot scope=space），其次接受
// /v1/bot/sendMessage 上 X-Space-ID 头（前提是 Bot 被授权在该 Space 派发），
// fallback 到 querySpaceIDByRobotID。返回 "" 表示 Bot 没有活跃 SpaceID
// （任何原因 — 孤儿 Bot、DB 错误、或 ErrNotFound）。调用方必须在 "" 返回时
// 执行 strip 而非 passthrough。
//
// 解析顺序（来自 Mininglamp-OSS/octo-server#36 推荐方案 B + C 混合）：
//
//  1. App Bot scope=space → CtxKeyAppBotSpaceID（O(1)，由 authAppBot 写入）。
//     这是最强的服务端权威信号，不接受任何 client override。
//
//  2. X-Space-ID 头（仅在 ctx 第一项缺失时才生效）→ 验证 Bot 在该 Space 被
//     授权后采纳。授权语义见 isBotSpaceAuthorized：space_member 成员 OR
//     published platform App Bot OR scope=space App Bot 派发到自身 Space。
//     不命中（未授权 / 头空 / 头被空白填充）则 fall through。
//
//  3. querySpaceIDByRobotID（DB） → 取 deterministic 首行。多归属时 emit
//     `multi_space_membership=true` warn 让运维定位需要走 Option B 的 Bot。
//
// querier 默认是 ba.db；测试可通过 ba.spaceQuerier 注入 stub。
func (ba *BotAPI) resolveBotActiveSpaceID(c *wkhttp.Context, robotID string) string {
	// (1) authAppBot 写入的 CtxKeyAppBotSpaceID（仅 App Bot scope=space）
	if scope, _ := c.Get(CtxKeyAppBotScope); scope == "space" {
		if v, ok := c.Get(CtxKeyAppBotSpaceID); ok {
			if s, _ := v.(string); s != "" {
				return s
			}
		}
	}
	q := ba.spaceQuerierOrDefault()
	if q == nil {
		// Defensive: tests sometimes construct &BotAPI{} with no db wired.
		// Treat as "no active space" instead of nil-dereferencing.
		return ""
	}
	// (2) X-Space-ID 头 — 仅当 Bot 确实是该 Space 的活跃成员才采纳，否则
	// fall through 到 deterministic DB query。这是 Mininglamp-OSS/octo-server#36
	// Option B 的精确实现：context-aware preference for Space-scoped routes
	// without taking a hard dependency on SpaceMiddleware (which the /v1/bot
	// route group does not currently mount).
	//
	// YUJ-688: trim whitespace from the header value before validation. Some
	// clients send "  space_X  " or trailing CR; without TrimSpace these were
	// rejected as non-member and emitted noisy reject warns.
	if c != nil && c.Request != nil {
		if hint := strings.TrimSpace(c.GetHeader("X-Space-ID")); hint != "" {
			isAuthorized, err := q.isBotSpaceAuthorized(robotID, hint)
			if err != nil {
				ba.Warn("isBotSpaceAuthorized 失败，回退到 deterministic DB 查询",
					zap.String("robotID", robotID), zap.String("hint", hint), zap.Error(err))
			} else if isAuthorized {
				return hint
			} else {
				// Header sent but Bot isn't authorized for that Space: log so
				// operators can detect bots that need to be added (or attackers
				// probing). Authorization spans space_member + active platform
				// App Bots + scope=space App Bots in their own active Space.
				ba.Warn("x_space_id_header_rejected_not_authorized",
					zap.Bool("x_space_id_header_rejected", true),
					zap.String("dispatcher", "bot_api"),
					zap.String("robotID", robotID),
					zap.String("hint", hint),
				)
			}
		}
	}
	// (3) Fallback：用户 Bot / 平台级 App Bot 查 space_member（deterministic）
	primary, allSpaces, err := q.querySpaceIDsByRobotID(robotID)
	if err != nil {
		// YUJ-660 Medium-2: dbr.ErrNotFound is "Bot has no Space" — a valid
		// state for orphan bots / non-Space deployments — NOT a DB error.
		// Don't pollute logs with false-positive DB warns; the caller's
		// strip-or-warn branch handles observability.
		if !errors.Is(err, dbr.ErrNotFound) {
			ba.Warn("querySpaceIDByRobotID 失败，跳过 space_id 注入",
				zap.String("robotID", robotID), zap.Error(err))
		}
		return ""
	}
	if len(allSpaces) > 1 {
		// Mininglamp-OSS/octo-server#36 — multi-Space User Bot. The result
		// IS deterministic now (earliest joined wins) but the caller may
		// have intended a different Space. Emit a structured warn so
		// operators can route the affected Bot through the X-Space-ID
		// header (Option B) or via authAppBot scope=space.
		ba.Warn("multi_space_membership",
			zap.Bool("multi_space_membership", true),
			zap.String("dispatcher", "bot_api"),
			zap.String("robotID", robotID),
			zap.String("chosen_space_id", primary),
			zap.Strings("all_space_ids", allSpaces),
		)
	}
	return primary
}

// spaceQuerierOrDefault returns the test-injected stub when present, otherwise
// the real *botAPIDB. Keeps test wiring unobtrusive in production code.
func (ba *BotAPI) spaceQuerierOrDefault() botSpaceQuerier {
	if ba.spaceQuerier != nil {
		return ba.spaceQuerier
	}
	if ba.db == nil {
		return nil
	}
	return ba.db
}
