// Package robot · YUJ-644 / Mininglamp-OSS#33 / YUJ-660
//
// PERSONAL DM 派发前服务端权威 space_id 注入。详见
// modules/bot_api/space_inject.go 顶部注释。本文件是 /v1/robot/... 路由
// 的等价实现。
//
// YUJ-660 Medium-2: dbr.ErrNotFound 不再被当成 DB 错误（孤儿 Bot 是合法状态）。
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

func (rb *Robot) enrichBotPayloadWithSpaceID(robotID string, payload map[string]interface{}) map[string]interface{} {
	if payload == nil {
		payload = make(map[string]interface{})
	}
	q := rb.spaceQuerierOrDefault()
	if q == nil {
		// Defensive: tests sometimes construct &Robot{} without DB wired.
		return payload
	}
	spaceID, err := q.querySpaceIDByRobotID(robotID)
	if err != nil {
		// YUJ-660 Medium-2: dbr.ErrNotFound 是合法的"Bot 无归属 Space"状态，不
		// 视为 DB 错误。其它 err 才记 warn。
		if !errors.Is(err, dbr.ErrNotFound) {
			rb.Warn("querySpaceIDByRobotID 失败，跳过 space_id 注入",
				zap.String("robotID", robotID), zap.Error(err))
			return payload
		}
		// fall through with spaceID == ""
	}
	if spaceID != "" {
		payload["space_id"] = spaceID
		return payload
	}
	if cur, _ := payload["space_id"].(string); cur == "" {
		rb.Warn("enrich_payload_space_id_empty",
			zap.Bool("enrich_payload_space_id_empty", true),
			zap.String("dispatcher", "robot"),
			zap.String("robotID", robotID),
		)
	}
	return payload
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
