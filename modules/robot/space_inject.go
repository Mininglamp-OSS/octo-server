// Package robot · YUJ-644 / Mininglamp-OSS#33 / YUJ-660
//
// PERSONAL DM 派发前服务端权威 space_id 注入。详见
// modules/bot_api/space_inject.go 顶部注释。本文件是 /v1/robot/... 路由
// 的等价实现。
//
// YUJ-660 R3 Finding A — fail-closed strip: 当 querySpaceIDByRobotID 因任何
// 原因（DB 错误 / ErrNotFound / 孤儿 Bot 返回 ""）无法解析 SpaceID 时，本层
// **必须删除** payload["space_id"]，并 emit `client_space_id_stripped=true`
// 监控 warn。之前版本在 DB 错误路径 preserve client payload，攻击者可借此通
// 过 forged payload.space_id 跨 Space 派发。
package robot

import (
	"errors"

	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// robotSpaceQuerier is the minimal data dependency of enrichBotPayloadWithSpaceID,
// extracted as an interface so unit tests can stub the DB call. *robotDB
// satisfies it implicitly.
type robotSpaceQuerier interface {
	querySpaceIDByRobotID(robotID string) (string, error)
	querySpaceIDsByRobotID(robotID string) (string, []string, error)
}

// querySpaceIDByRobotID 查询 Bot 当前激活的 SpaceID。逻辑与
// modules/botfather/db.go / modules/bot_api/db.go 同名函数一致：
// space_member ⨝ space，要求 sm.status=1 AND s.status=1。
//
// Mininglamp-OSS/octo-server#36（PR#35 deep-review High-2）— deterministic
// ORDER BY (Option C). Bot 在多个活跃 Space 时，按 `sm.created_at ASC, sm.space_id ASC`
// 取首行 — earliest joined wins，`space_id` 兜底破并列。legacy /v1/robot/.../sendMessage
// 路由没有 SpaceMiddleware / Space 上下文，本层只能依赖 deterministic ordering，
// 不能用 Option B（context-aware）。多归属告警由调用方
// `resolveBotActiveSpaceID` / `enrichBotPayloadWithSpaceID` 通过单独的检测路径输出。
func (d *robotDB) querySpaceIDByRobotID(robotID string) (string, error) {
	spaceID, _, err := d.querySpaceIDsByRobotID(robotID)
	return spaceID, err
}

// querySpaceIDsByRobotID 多行变体 — 返回 deterministic 首行 + 全部匹配的有序列表，
// 让调用方一次查询就能 observe 多归属（`len(spaceIDs) > 1`），无需第二次 round-trip。
// 空集合返回 `dbr.ErrNotFound`，保留 LoadOne 时代的调用方契约。
func (d *robotDB) querySpaceIDsByRobotID(robotID string) (string, []string, error) {
	var spaceIDs []string
	_, err := d.session.SelectBySql(
		"SELECT sm.space_id FROM space_member sm INNER JOIN space s ON s.space_id = sm.space_id WHERE sm.uid=? AND sm.status=1 AND s.status=1 ORDER BY sm.created_at ASC, sm.space_id ASC",
		robotID,
	).Load(&spaceIDs)
	if err != nil {
		return "", nil, err
	}
	if len(spaceIDs) == 0 {
		return "", nil, dbr.ErrNotFound
	}
	return spaceIDs[0], spaceIDs, nil
}

// enrichBotPayloadWithSpaceID injects the bot's authoritative SpaceID into the
// PERSONAL DM payload. When the resolver cannot produce a SpaceID for any
// reason (DB error, ErrNotFound, orphan bot), payload["space_id"] is stripped
// server-side (fail-closed), with a structured zap warn for observability.
func (rb *Robot) enrichBotPayloadWithSpaceID(robotID string, payload map[string]interface{}) map[string]interface{} {
	if payload == nil {
		payload = make(map[string]interface{})
	}
	spaceID := rb.resolveBotActiveSpaceID(robotID)
	if spaceID != "" {
		payload["space_id"] = spaceID
		return payload
	}
	// Resolver returned "" (orphan / DB error / ErrNotFound). Fail-closed:
	// strip any client-supplied space_id, never trust client value when server
	// has no authoritative source.
	if cur, ok := payload["space_id"].(string); ok && cur != "" {
		delete(payload, "space_id")
		rb.Warn("client_space_id_stripped",
			zap.Bool("client_space_id_stripped", true),
			zap.String("dispatcher", "robot"),
			zap.String("robotID", robotID),
			zap.String("client_supplied", cur),
		)
	} else {
		rb.Warn("enrich_payload_space_id_empty",
			zap.Bool("enrich_payload_space_id_empty", true),
			zap.String("dispatcher", "robot"),
			zap.String("robotID", robotID),
		)
	}
	return payload
}

// resolveBotActiveSpaceID 通过 querier 查 Bot 的激活 SpaceID。返回 "" 表示
// 解析不到（任意原因 — 孤儿 Bot、DB 错误、ErrNotFound）。调用方必须在 ""
// 返回时执行 strip 而不是 passthrough。
//
// Mininglamp-OSS/octo-server#36：legacy /v1/robot/... 路由没有 SpaceMiddleware
// 也不挂 Bot ctx，无法走 Option B。多归属时返回 deterministic 首行（earliest
// joined）并 emit `multi_space_membership=true` warn 让运维定位需要迁移到新
// Bot API 的 Bot。
func (rb *Robot) resolveBotActiveSpaceID(robotID string) string {
	q := rb.spaceQuerierOrDefault()
	if q == nil {
		// Defensive: tests sometimes construct &Robot{} without DB wired.
		return ""
	}
	primary, allSpaces, err := q.querySpaceIDsByRobotID(robotID)
	if err != nil {
		// YUJ-660 Medium-2: dbr.ErrNotFound 是合法的"Bot 无归属 Space"状态，
		// 不视为 DB 错误。其它 err 才记 warn；返回 "" 让调用方走 strip 分支。
		if !errors.Is(err, dbr.ErrNotFound) {
			rb.Warn("querySpaceIDByRobotID 失败，跳过 space_id 注入",
				zap.String("robotID", robotID), zap.Error(err))
		}
		return ""
	}
	if len(allSpaces) > 1 {
		rb.Warn("multi_space_membership",
			zap.Bool("multi_space_membership", true),
			zap.String("dispatcher", "robot"),
			zap.String("robotID", robotID),
			zap.String("chosen_space_id", primary),
			zap.Strings("all_space_ids", allSpaces),
		)
	}
	return primary
}

// spaceQuerierOrDefault returns the test-injected stub when present, otherwise
// the embedded *robotDB.
func (rb *Robot) spaceQuerierOrDefault() robotSpaceQuerier {
	if rb.spaceQuerier != nil {
		return rb.spaceQuerier
	}
	// rb.db is a value (not pointer); take its address.
	return &rb.db
}
