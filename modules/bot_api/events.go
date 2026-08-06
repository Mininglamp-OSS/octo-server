package bot_api

import (
	"fmt"
	"strconv"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/botevent"
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
	// Wait is the optional long-poll hold, in seconds.
	//
	// Zero (or absent) preserves the historical behavior exactly: read the
	// queue once and return, empty batch included. That default is not
	// conservatism for its own sake — the OpenClaw channel plugin caps every
	// /v1/bot/events request at a hard 10s client timeout, so a server that
	// held by default would make existing bots abort and log on every poll.
	// Benefiting from the hold is an explicit opt-in by the caller.
	//
	// Clamped server-side to [0, maxEventWaitSeconds]; an out-of-range value is
	// clamped, never rejected.
	Wait int64 `json:"wait"`
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

	// robotID comes from the authBot() middleware, never from the request body.
	// Both the event-queue key and the doorbell key are derived from it, so a
	// bot can only ever read — or wait on — its own queue.
	robotID := getRobotIDFromContext(c)
	botKind := getBotKindFromContext(c)
	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	wait := clampEventWait(req.Wait)

	// Read before waiting: a caller with a backlog must never pay the hold.
	var results []*eventResp
	page, err := ba.readEventPage(robotID, botKind, req.EventID, limit)
	if err == nil {
		results = page.visible
		// The entry page is handed to the hold rather than discarded: it carries
		// how far the read advanced and whether it filled, and the loop needs
		// both to guarantee it makes progress. See waitForEvents.
		if len(results) == 0 && wait > 0 {
			results, err = ba.waitForEvents(c, robotID, botKind, limit, wait, page)
		}
	}
	// Single error exit. The legacy `status:0` shape is kept verbatim for wire
	// compatibility, but it is deliberately written once: a second copy on the
	// long-poll branch would be a *new* raw error response, which the repo's
	// error-handling rule forbids even where the surrounding legacy shape stays.
	if err != nil {
		c.Response(gin.H{
			"status": 0,
			"msg":    err.Error(),
		})
		return
	}

	c.Response(gin.H{
		"status":  1,
		"results": results,
	})
}

// filterAppBotEvents applies the App Bot DM-only guard.
//
// App Bot: filter out non-DM events (defense in depth — App Bot is DM-only).
// In practice, App Bot queues should never contain group events because:
// - App Bot cannot join groups (all group/thread ops are denied)
// - Event push upstream only routes DM events to App Bot queues
// This filter is purely defensive — if triggered, it indicates an infrastructure bug.
// Filtered events are auto-ACK'd (ZREM) to prevent unbounded queue growth.
//
// Extracted so the immediate path and the long-poll path share one copy: a
// filter that applied to only one of them would be a silent hole the moment a
// caller sets `wait`.
func (ba *BotAPI) filterAppBotEvents(botKind string, robotID string, results []*eventResp) []*eventResp {
	if botKind != BotKindApp || len(results) == 0 {
		return results
	}
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
		key := botevent.QueueKey(robotID)
		for _, id := range filteredIDs {
			if err := ba.ackEvent(key, id); err != nil {
				ba.Warn("auto-ACK filtered event failed", zap.String("eventID", id), zap.Error(err))
			}
		}
	}
	return filtered
}

// ackEvent removes one auto-ACK'd event from the queue.
//
// It routes through the optional ackFilteredEvent field rather than calling
// Redis directly so a test can inject the state this endpoint's progress
// guarantee is built to survive: a Redis whose **reads succeed while writes
// fail** (MISCONF after a failed snapshot, a READONLY replica, a write-denying
// ACL). That asymmetry cannot be produced against a healthy Redis — re-seeding
// the events afterwards looks the same to the eye but not to the code, since
// both ZREMs still succeeded — and PR#685's round-3 blocker was precisely a loop
// whose progress silently depended on this write.
//
// The seam is a struct field, not a package-level var: a rebindable global would
// become a data race the moment this package uses t.Parallel(). Nil falls back
// to the real write, matching cardSeqCASWrite's handling of BotAPI literals in
// existing tests.
func (ba *BotAPI) ackEvent(key string, eventID string) error {
	if ba.ackFilteredEvent != nil {
		return ba.ackFilteredEvent(key, eventID)
	}
	return ba.ctx.GetRedisConn().ZRemRangeByScore(key, eventID, eventID)
}

// eventPage is one authoritative read of a bot's event queue: what this caller
// may see, how far the queue cursor moved, and whether the page filled.
//
// cursor is the load-bearing field. It advances to the highest event id the read
// *observed* — before the App Bot filter, and regardless of whether the auto-ACK
// ZREM that the filter issues afterwards succeeded. That is what makes the
// long-poll loop's forward progress structural: it is derived from the read the
// loop just performed, not from a write whose error is only logged.
//
// # The two assumptions underneath the cursor
//
// **Equality.** The cursor is a payload `event_id` (`getEventsResult` decodes
// it), while the read that consumes it is bounded by the sorted-set **score**
// (`Min: "(cursor"`). The two are the same number by construction at every
// producer — each does `ZAdd(key, float64(seq), payload{EventID: seq})` — and
// both id sources return integers well inside float64's exact range, which
// `tools/botevent-seq` keeps true by refusing a cutover floor near 2^53. (The
// count was "five" until `addInlineQuery` was found to be a sixth; naming a
// number here just invites the next one to be missed.) A future
// writer that scored by, say, timestamp while keeping a separate `event_id`
// would break this silently: the cursor would jump past members the caller never
// received. The same equality is what makes the auto-ACK's
// `ZRemRangeByScore(key, id, id)` address the member it means to.
//
// **Uniqueness, which is the stronger one.** Exclusive `Min: "(cursor"`
// pagination is lossless only if scores are strictly unique, and until #697 they
// were not. Ids came from `GenSeq` (octo-lib `config/seq.go`), a DB-backed *block*
// allocator: it reads `min_seq` and writes back an absolute value computed from
// process-local state, guarded only by a process-local mutex. Two replicas whose
// blocks race — a concurrent cold start, or a concurrent extend — hand out the same
// id. Two members sharing score 42 means a page ending on the first advances the
// cursor to 42 and the second is **never delivered**; the auto-ACK and `eventAck`
// likewise remove both. Measured in production: 2624 colliding scores across three
// queues, still accumulating, plus 19 time-inverted ids on block boundaries.
//
// `pkg/botevent`'s allocator replaces that source with a per-bot monotonic counter,
// so **once activated** the uniqueness this cursor depends on holds by construction
// rather than by assumption. Two things about that are worth knowing here:
//
//   - Before activation the allocator delegates to GenSeq, so on a merged-but-not-
//     activated deployment everything in the paragraph above is still live.
//   - Activation does not make the exclusive cursor lossless, and the sentence this
//     paragraph replaced named the reason: closing it needs "a Redis-side allocator
//     **or** a re-delivery window below the cursor". #697 delivered the allocator,
//     which removes the collision and cross-restart-inversion classes. It does not
//     remove the reordering window — allocation and publication are two operations,
//     so a producer that allocates `N` and stalls while another publishes `N+1`
//     still loses `N` once this cursor passes it, and the doorbell makes that *more*
//     likely because the wake is triggered by the very ZADD that creates the
//     inversion. That residual is pinned by
//     `TestKnownResidualZaddReorderingCanStillSkip` in `pkg/botevent` and needs the
//     re-delivery window, i.e. a change to this file, not that one.
//
// Existing duplicate members are also deliberately left in place: an ack deletes
// every member sharing a score, and there is no record of which of a pair was ever
// delivered, so re-scoring them would mean re-delivering both.
type eventPage struct {
	// visible is what may be returned to the caller.
	visible []*eventResp
	// cursor is the exclusive lower bound for the next read.
	cursor int64
	// advanced reports that this read moved the cursor. It is what licenses
	// skipping the next doorbell block: a page that advanced nothing would be
	// re-read identically, so re-reading it without pacing is a spin.
	advanced bool
	// full reports that the read returned a whole page of sorted-set members, so
	// more may be queued directly behind it. Counted from the *raw* members, not
	// from the decoded events or from visible: a member that failed to decode
	// still occupied a slot in the page.
	full bool
}

// readEventPage performs one authoritative read at cursor and applies the App
// Bot filter. Both the immediate path and the long-poll loop go through it, so
// the two cannot drift in what they read, what they filter, or how far they
// advance.
func (ba *BotAPI) readEventPage(robotID string, botKind string, cursor int64, limit int64) (eventPage, error) {
	raw, members, err := ba.getEventsResult(robotID, cursor, limit)
	if err != nil {
		return eventPage{}, err
	}
	page := eventPage{cursor: cursor, full: int64(members) >= limit}
	// Advance before filtering. ZRangeByScore returns ascending, so the last
	// event carries the maximum, but scanning does not depend on that ordering.
	//
	// Note what this does and does not clear. `raw` is the *decoded* slice, so a
	// member that failed to decode never appears here: the cursor clears one only
	// when a decodable member with a higher id shares the page. A page in which
	// nothing decodes leaves the cursor where it was — see waitForEvents, which
	// falls back to blocking exactly so that case stays paced.
	for _, r := range raw {
		if r.EventID > page.cursor {
			page.cursor = r.EventID
		}
	}
	page.advanced = page.cursor > cursor
	page.visible = ba.filterAppBotEvents(botKind, robotID, raw)
	return page, nil
}

// getEventsResult reads one page of the queue. It returns the decoded events and
// the number of raw sorted-set members the read returned; the two differ when a
// member fails to decode, and callers need the raw count to tell a full page
// from a short one.
func (ba *BotAPI) getEventsResult(robotID string, eventID int64, limit int64) ([]*eventResp, int, error) {
	key := botevent.QueueKey(robotID)
	robotEventJsons, err := ba.ctx.GetRedisConn().ZRangeByScore(key, redis.ZRangeBy{
		Max:   "+inf",
		Min:   fmt.Sprintf("(%d", eventID),
		Count: limit,
	})
	if err != nil {
		return nil, 0, err
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
	return results, len(robotEventJsons), nil
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

	key := botevent.QueueKey(robotID)
	err = ba.ctx.GetRedisConn().ZRemRangeByScore(key, fmt.Sprintf("%d", eventID), fmt.Sprintf("%d", eventID))
	if err != nil {
		ba.Error("ack event failed", zap.Error(err), zap.String("robotID", robotID), zap.Int64("eventID", eventID))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}
