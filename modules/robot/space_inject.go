// Package robot · YUJ-644 / Mininglamp-OSS#33
//
// PERSONAL DM 派发前服务端权威 space_id 注入。详见
// modules/bot_api/space_inject.go 顶部注释。本文件是 /v1/robot/... 路由
// 的等价实现。
package robot

import (
	"go.uber.org/zap"
)

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
	spaceID, err := rb.db.querySpaceIDByRobotID(robotID)
	if err != nil {
		rb.Warn("querySpaceIDByRobotID 失败，跳过 space_id 注入",
			zap.String("robotID", robotID), zap.Error(err))
		return payload
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
