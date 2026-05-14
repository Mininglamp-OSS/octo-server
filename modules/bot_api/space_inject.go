// Package bot_api · YUJ-644 / Mininglamp-OSS#33 / YUJ-660
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
package bot_api

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// botSpaceQuerier is the minimal data dependency of resolveBotActiveSpaceID,
// extracted as an interface so unit tests can stub the DB call without
// constructing a full *botAPIDB. *botAPIDB satisfies it implicitly.
type botSpaceQuerier interface {
	querySpaceIDByRobotID(robotID string) (string, error)
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

// resolveBotActiveSpaceID 优先读 gin-context（App Bot scope=space），fallback 到
// querySpaceIDByRobotID。返回 "" 表示 Bot 没有活跃 SpaceID（任何原因 — 孤儿
// Bot、DB 错误、或 ErrNotFound）。调用方必须在 "" 返回时执行 strip 而非
// passthrough。
//
// querier 默认是 ba.db；测试可通过 ba.spaceQuerier 注入 stub。
func (ba *BotAPI) resolveBotActiveSpaceID(c *wkhttp.Context, robotID string) string {
	// 优先：authAppBot 写入的 CtxKeyAppBotSpaceID（仅 App Bot scope=space）
	if scope, _ := c.Get(CtxKeyAppBotScope); scope == "space" {
		if v, ok := c.Get(CtxKeyAppBotSpaceID); ok {
			if s, _ := v.(string); s != "" {
				return s
			}
		}
	}
	// Fallback：用户 Bot / 平台级 App Bot 查 space_member
	q := ba.spaceQuerierOrDefault()
	if q == nil {
		// Defensive: tests sometimes construct &BotAPI{} with no db wired.
		// Treat as "no active space" instead of nil-dereferencing.
		return ""
	}
	spaceID, err := q.querySpaceIDByRobotID(robotID)
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
	return spaceID
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
