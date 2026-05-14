// Package bot_api · YUJ-644 / Mininglamp-OSS#33
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
//     或非 Space 部署），保留客户端原始 payload，向前兼容。
//
// 失败模式：
//   - 真实 DB 错误 → warn + 不阻断发送（注入是优化，缺失走老语义）。
//   - dbr.ErrNotFound（零结果）→ 视为"Bot 没有归属 Space"，不写 warn（不是错误，
//     是正常孤儿 Bot 状态），fall through 到 empty-space_id observability warn。
//
// 空 SpaceID 时不强删客户端 space_id —— 与 enrichPayloadWithSpaceIDCore PERSONAL
// 兼容路径对齐。YUJ-660：High-3 fail-open 修复在 message 层完成（authoritative
// strip when senderSpaceID == ""），bot 路径 senderSpaceID 一定非空（Bot 自身
// 归属 Space）或 fall through 到 message 层的 strip 逻辑。
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
func (ba *BotAPI) enrichBotPayloadWithSpaceID(c *wkhttp.Context, robotID string, payload map[string]interface{}) map[string]interface{} {
	if payload == nil {
		payload = make(map[string]interface{})
	}
	spaceID := ba.resolveBotActiveSpaceID(c, robotID)
	if spaceID != "" {
		payload["space_id"] = spaceID
		return payload
	}
	// SpaceID 为空：保留客户端 payload.space_id（与 enrichPayloadWithSpaceIDCore
	// PERSONAL 兼容路径对齐）。同时发监控 warn 作为稳态指标 —— 稳态下应为 0。
	if cur, _ := payload["space_id"].(string); cur == "" {
		ba.Warn("enrich_payload_space_id_empty",
			zap.Bool("enrich_payload_space_id_empty", true),
			zap.String("dispatcher", "bot_api"),
			zap.String("robotID", robotID),
		)
	}
	return payload
}

// resolveBotActiveSpaceID 优先读 gin-context（App Bot scope=space），fallback 到
// querySpaceIDByRobotID。返回 "" 表示 Bot 没有活跃 SpaceID。
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
		// Don't pollute logs with false-positive DB warns; fall through and
		// let the caller's empty-space_id observability warn fire instead.
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
