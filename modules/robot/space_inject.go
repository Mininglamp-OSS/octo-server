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
}

// querySpaceIDByRobotID 查询 Bot 当前激活的 SpaceID。逻辑与
// modules/botfather/db.go / modules/bot_api/db.go 同名函数一致：
// space_member ⨝ space，要求 sm.status=1 AND s.status=1。
func (d *robotDB) querySpaceIDByRobotID(robotID string) (string, error) {
	var spaceID string
	err := d.session.SelectBySql(
		"SELECT sm.space_id FROM space_member sm INNER JOIN space s ON s.space_id = sm.space_id WHERE sm.uid=? AND sm.status=1 AND s.status=1",
		robotID,
	).LoadOne(&spaceID)
	return spaceID, err
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
func (rb *Robot) resolveBotActiveSpaceID(robotID string) string {
	q := rb.spaceQuerierOrDefault()
	if q == nil {
		// Defensive: tests sometimes construct &Robot{} without DB wired.
		return ""
	}
	spaceID, err := q.querySpaceIDByRobotID(robotID)
	if err != nil {
		// YUJ-660 Medium-2: dbr.ErrNotFound 是合法的"Bot 无归属 Space"状态，
		// 不视为 DB 错误。其它 err 才记 warn；返回 "" 让调用方走 strip 分支。
		if !errors.Is(err, dbr.ErrNotFound) {
			rb.Warn("querySpaceIDByRobotID 失败，跳过 space_id 注入",
				zap.String("robotID", robotID), zap.Error(err))
		}
		return ""
	}
	return spaceID
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
