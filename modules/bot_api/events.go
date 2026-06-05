package bot_api

import (
	"fmt"
	"strconv"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis"
	"go.uber.org/zap"
)

// BotEventsReq is the request for getEvents.
type BotEventsReq struct {
	EventID int64 `json:"event_id"`
	Limit   int64 `json:"limit"`
}

type eventResp struct {
	EventID   int64                  `json:"event_id"`
	Message   *messageResp           `json:"message,omitempty"`
	EventType string                 `json:"event_type,omitempty"`
	EventData map[string]interface{} `json:"event_data,omitempty"`
}

type messageResp struct {
	MessageID   int64       `json:"message_id"`
	MessageSeq  uint32      `json:"message_seq"`
	FromUID     string      `json:"from_uid"`
	ChannelID   string      `json:"channel_id,omitempty"`
	ChannelType uint8       `json:"channel_type,omitempty"`
	Timestamp   int32       `json:"timestamp"`
	Payload     interface{} `json:"payload"`
}

// getEvents handles POST /v1/bot/events.
func (ba *BotAPI) getEvents(c *wkhttp.Context) {
	var req BotEventsReq
	if err := c.BindJSON(&req); err != nil {
		respondBotAPIRequestInvalid(c, "")
		return
	}

	robotID := getRobotIDFromContext(c)
	botKind := getBotKindFromContext(c)
	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	results, err := ba.getEventsResult(robotID, req.EventID, limit)
	if err != nil {
		c.Response(gin.H{
			"status": 0,
			"msg":    err.Error(),
		})
		return
	}

	// App Bot: filter out non-DM events (defense in depth — App Bot is DM-only).
	// In practice, App Bot queues should never contain group events because:
	// - App Bot cannot join groups (all group/thread ops are denied)
	// - Event push upstream only routes DM events to App Bot queues
	// This filter is purely defensive — if triggered, it indicates an infrastructure bug.
	// Filtered events are auto-ACK'd (ZREM) to prevent unbounded queue growth.
	if botKind == BotKindApp && len(results) > 0 {
		filtered := make([]*eventResp, 0, len(results))
		var filteredIDs []string
		for _, r := range results {
			if r.Message != nil && r.Message.ChannelType != 0 && r.Message.ChannelType != common.ChannelTypePerson.Uint8() {
				filteredIDs = append(filteredIDs, fmt.Sprintf("%d", r.EventID))
				continue
			}
			filtered = append(filtered, r)
		}
		if len(filteredIDs) > 0 {
			key := fmt.Sprintf("%s%s", robotEventPrefix, robotID)
			for _, id := range filteredIDs {
				if err := ba.ctx.GetRedisConn().ZRemRangeByScore(key, id, id); err != nil {
					ba.Warn("auto-ACK filtered event failed", zap.String("eventID", id), zap.Error(err))
				}
			}
		}
		results = filtered
	}

	c.Response(gin.H{
		"status":  1,
		"results": results,
	})
}

func (ba *BotAPI) getEventsResult(robotID string, eventID int64, limit int64) ([]*eventResp, error) {
	key := fmt.Sprintf("%s%s", robotEventPrefix, robotID)
	robotEventJsons, err := ba.ctx.GetRedisConn().ZRangeByScore(key, redis.ZRangeBy{
		Max:   "+inf",
		Min:   fmt.Sprintf("(%d", eventID),
		Count: limit,
	})
	if err != nil {
		return nil, err
	}

	results := make([]*eventResp, 0)
	if len(robotEventJsons) > 0 {
		type robotEvent struct {
			EventID   int64                  `json:"event_id,omitempty"`
			Message   *config.MessageResp    `json:"message,omitempty"`
			EventType string                 `json:"event_type,omitempty"`
			EventData map[string]interface{} `json:"event_data,omitempty"`
			Expire    int64                  `json:"expire,omitempty"`
		}

		events := make([]*robotEvent, 0)
		for _, jsonStr := range robotEventJsons {
			var ev robotEvent
			err = util.ReadJsonByByte([]byte(jsonStr), &ev)
			if err != nil {
				ba.Error("解码事件失败", zap.Error(err))
				continue
			}
			events = append(events, &ev)
		}

		// ZRangeByScore returns events already sorted by score (eventID). No need to re-sort.

		for _, ev := range events {
			resp := &eventResp{
				EventID: ev.EventID,
			}
			if ev.Message != nil {
				resp.Message = &messageResp{
					MessageID:  ev.Message.MessageID,
					MessageSeq: ev.Message.MessageSeq,
					FromUID:    ev.Message.FromUID,
					Timestamp:  ev.Message.Timestamp,
				}
				if ev.Message.ChannelType != common.ChannelTypePerson.Uint8() {
					resp.Message.ChannelID = ev.Message.ChannelID
					resp.Message.ChannelType = ev.Message.ChannelType
				}
				var payloadMap map[string]interface{}
				if err := util.ReadJsonByByte(ev.Message.Payload, &payloadMap); err == nil {
					resp.Message.Payload = payloadMap
				}
			}
			if ev.EventType != "" {
				resp.EventType = ev.EventType
				resp.EventData = ev.EventData
			}
			results = append(results, resp)
		}
	}
	return results, nil
}

// eventAck handles POST /v1/bot/events/:event_id/ack.
func (ba *BotAPI) eventAck(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	eventIDStr := c.Param("event_id")
	eventID, err := strconv.ParseInt(eventIDStr, 10, 64)
	if err != nil {
		respondBotAPIRequestInvalid(c, "event_id")
		return
	}

	key := fmt.Sprintf("%s%s", robotEventPrefix, robotID)
	err = ba.ctx.GetRedisConn().ZRemRangeByScore(key, fmt.Sprintf("%d", eventID), fmt.Sprintf("%d", eventID))
	if err != nil {
		ba.Error("ack event failed", zap.Error(err), zap.String("robotID", robotID), zap.Int64("eventID", eventID))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}
