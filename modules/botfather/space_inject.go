// Package botfather · YUJ-644 / Mininglamp-OSS#33
//
// PERSONAL DM 派发前服务端权威 space_id 注入。设计 / 失败模式见
// modules/bot_api/space_inject.go 顶部注释。本文件是 User Bot 路由
// （/v1/botfather/...）的等价实现 —— 仅查 botfatherDB.querySpaceIDByRobotID。
package botfather

import (
	"go.uber.org/zap"
)

func (bf *BotFather) enrichBotPayloadWithSpaceID(robotID string, payload map[string]interface{}) map[string]interface{} {
	if payload == nil {
		payload = make(map[string]interface{})
	}
	spaceID, err := bf.db.querySpaceIDByRobotID(robotID)
	if err != nil {
		bf.Warn("querySpaceIDByRobotID 失败，跳过 space_id 注入",
			zap.String("robotID", robotID), zap.Error(err))
		// 不强删客户端 space_id，与 PERSONAL 兼容路径对齐。
		return payload
	}
	if spaceID != "" {
		payload["space_id"] = spaceID
		return payload
	}
	if cur, _ := payload["space_id"].(string); cur == "" {
		bf.Warn("enrich_payload_space_id_empty",
			zap.Bool("enrich_payload_space_id_empty", true),
			zap.String("dispatcher", "botfather"),
			zap.String("robotID", robotID),
		)
	}
	return payload
}
