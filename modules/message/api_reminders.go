package message

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// 提醒已完成
func (m *Message) reminderDone(c *wkhttp.Context) {
	var ids []int64
	if err := c.BindJSON(&ids); err != nil {
		respondMessageRequestInvalid(c, "")
		return
	}
	if len(ids) == 0 {
		respondMessageRequestInvalid(c, "")
		return
	}
	loginUID := c.GetLoginUID()
	tx, err := m.ctx.DB().Begin()
	if err != nil {
		m.Error("开启事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.RollbackUnlessCommitted()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	err = m.remindersDB.insertDonesTx(ids, loginUID, tx)
	if err != nil {
		tx.Rollback()
		m.Error("添加done失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
		return
	}
	for _, id := range ids {
		version, err := m.nextReminderSeq()
		if err != nil {
			m.Error("生成提醒项序列号失败", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
			return
		}
		err = m.remindersDB.updateVersionTx(version, id, tx)
		if err != nil {
			tx.Rollback()
			m.Error("更新提醒项版本失败！", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		tx.RollbackUnlessCommitted()
		m.Error("提交事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
		return
	}
	err = m.ctx.SendCMD(config.MsgCMDReq{
		NoPersist:   true,
		ChannelID:   loginUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		CMD:         common.CMDSyncReminders,
	})
	if err != nil {
		m.Error("发送同步提醒项cmd失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageNotifyFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

// 提醒内容同步
func (m *Message) reminderSync(c *wkhttp.Context) {
	var req struct {
		Version    int64    `json:"version"`
		Limit      uint64   `json:"limit"`
		ChannelIDs []string `json:"channel_ids"`
	}
	if err := c.BindJSON(&req); err != nil {
		m.Error("数据格式有误！", zap.Error(err))
		respondMessageRequestInvalid(c, "")
		return
	}
	loginUID := c.GetLoginUID()
	reminders, err := m.remindersDB.sync(loginUID, req.Version, req.Limit, req.ChannelIDs)
	if err != nil {
		m.Error("同步提醒项失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}

	// YUJ-1377 / Mininglamp-OSS/octo-server#101 — drop channel-level
	// (UID=="") broadcast reminders authored by the viewer itself, so
	// the sender of `@所有人` does not receive their own red-dot.
	// Per-uid reminders (apply-join-group, explicit `mention.uids`) are
	// untouched: they carry UID!="" and pass through verbatim.
	//
	// Primary fix is in remindersDB.sync (SQL predicate) — that keeps
	// the version/limit cursor advancing past hidden self-broadcasts so
	// the client never stalls. This call is a defense-in-depth filter
	// for any future read path that forgets the SQL predicate; it is a
	// no-op when sync() has already done its job.
	reminders = filterChannelLevelByPublisher(reminders, loginUID)

	groupIds := make([]string, 0)
	if len(reminders) > 0 {
		for _, reminder := range reminders {
			if reminder.ChannelType == common.ChannelTypeGroup.Uint8() {
				groupIds = append(groupIds, reminder.ChannelID)
			}
		}
	}
	members := make([]*group.MemberResp, 0)
	if len(groupIds) > 0 {
		members, err = m.groupService.GetMembersWithUIDAndGroupIds(loginUID, groupIds)
		if err != nil {
			m.Error("查询登录用户加入群成员信息错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return
		}
	}
	reminderResps := make([]*reminderResp, 0, len(reminders))
	for _, reminder := range reminders {
		if len(members) > 0 && reminder.ChannelType == common.ChannelTypeGroup.Uint8() {
			for _, member := range members {
				if member.GroupNo == reminder.ChannelID && time.Time(reminder.CreatedAt).Unix() < member.CreatedAt {
					reminder.Done = 1
					break
				}
			}
		}
		reminderResps = append(reminderResps, newReminderResp(reminder))
	}
	c.JSON(http.StatusOK, reminderResps)
}

func (m *Message) listenerMessages(messages []*config.MessageResp) {

	reminders := m.getReminders(messages) // 提醒
	if len(reminders) > 0 {
		m.handleReminders(reminders)
	}

}

// nextReminderSeq returns the next monotonically increasing version
// number used to seed remindersModel.Version. Production path delegates
// to ctx.GenSeq(common.RemindersKey) (backed by the seq table); unit
// tests inject reminderSeqOverride to skip the DB and exercise the
// fan-out / matrix helpers in isolation. Keeping this seam local to
// the reminders module avoids leaking the stub through Message's
// exported surface.
func (m *Message) nextReminderSeq() (int64, error) {
	if m.reminderSeqOverride != nil {
		return m.reminderSeqOverride()
	}
	return m.ctx.GenSeq(common.RemindersKey)
}

// isBotUID reports whether `uid` belongs to a bot in the robot
// registry. Used by getReminders to filter out bot UIDs from the
// per-uid reminder fan-out (PR#145 R3 / YUJ-1810): the persisted
// payload that the WuKongIM listener replays already contains the
// server-expanded `mention.ais` → `mention.uids` bot UIDs, and bots
// must never receive a `[有人@我]` reminder row regardless of how
// the UID landed in the mention list (server-expanded `ais=1` or
// explicit user-typed `@bot_x`).
//
// Best-effort semantics:
//
//   - robotService unwired (unit tests that build &Message{} without
//     ctor) → returns false; no filter is applied. Existing
//     getReminders fan-out matrix tests (TestGetReminders_FanoutMatrix)
//     supply human-only UIDs and don't wire robotService, so they keep
//     working unchanged.
//   - ExistRobot returns an error (transient robot-row corruption,
//     DB blip) → log warn and treat the UID as not-a-bot. We err on
//     the side of EMITTING the reminder for a human rather than
//     SILENTLY DROPPING a reminder. The alternative (drop on error)
//     would lose legitimate human notifications whenever the robot
//     table hiccups, which is a worse failure mode than the
//     temporary noise this guards against.
func (m *Message) isBotUID(uid string) bool {
	if m.robotService == nil {
		return false
	}
	ok, err := m.robotService.ExistRobot(uid)
	if err != nil {
		m.Warn("ExistRobot lookup failed during reminder emission; treating UID as not-a-bot",
			zap.String("uid", uid),
			zap.Error(err))
		return false
	}
	return ok
}

func (m *Message) getReminders(messages []*config.MessageResp) []*remindersModel {
	reminders := make([]*remindersModel, 0, len(messages))
	for _, message := range messages {
		payloadMap, err := message.GetPayloadMap()
		if err != nil {
			m.Warn("解码消息payload失败！,跳过", zap.Error(err))
			continue
		}
		if payloadMap == nil {
			continue
		}
		if m.hasMention(payloadMap) {
			all, uids := m.getMention(payloadMap)
			if all {
				// Plan X (YUJ-1389) + #142 follow-up: only create a
				// channel-level reminder when humans=1 is explicitly
				// set. ais-only broadcasts do NOT create human-visible
				// reminders (bots respond via the message delivery
				// path, so a "[有人@我]" red-dot for human members
				// would be noise). Post-#142 there is no longer a
				// rewrite chokepoint that turns legacy `all=1` into
				// `ais=1` — `all=1` is treated as plain `@所有人`
				// rendering metadata for legacy read-side clients and
				// likewise does NOT trigger a reminder. `humans=1` is
				// the single human-notification signal here.
				mentionMap2, _ := payloadMap["mention"].(map[string]interface{})
				hasHumans := mentionFlagTruthy(mentionMap2["humans"])
				if hasHumans {
					version, err := m.nextReminderSeq()
					if err != nil {
						m.Warn("GenSeq failed", zap.Error(err))
						continue
					}
					reminders = append(reminders, &remindersModel{
						ChannelID:    message.ChannelID,
						ChannelType:  message.ChannelType,
						ClientMsgNo:  message.ClientMsgNo,
						Publisher:    message.FromUID,
						MessageID:    fmt.Sprintf("%d", message.MessageID),
						MessageSeq:   message.MessageSeq,
						ReminderType: ReminderTypeMentionMe,
						IsLocate:     1,
						Version:      version,
						Text:         "[有人@我]",
					})
				}
				// Fall through to uid processing below regardless — a
				// broadcast can still carry explicit @uid mentions
				// (`@所有人 + @alice`) that need per-user reminders.
			}
			if len(uids) > 0 {
				for _, uid := range uids {
					// PR#145 R3 blocker (YUJ-1810): the persisted payload
					// that lands here on the WuKongIM listener callback
					// already contains the server-side
					// ExpandAisToBotUIDs expansion (the wire bytes carry
					// the expanded UIDs even though CloneForExpansion
					// keeps the send-side in-memory map clean — the
					// reminder writer runs on the persisted-payload path,
					// not the in-memory one). Without filtering here,
					// every `@所有 AI` broadcast writes one
					// `[有人@我]` reminder row per bot member of the
					// group, which (a) pollutes the reminders table and
					// (b) violates the `ais=1` contract that bots
					// respond via the delivery path and MUST NOT trigger
					// human-visible red-dots.
					//
					// The filter also covers user-typed explicit
					// `@bot_x` mentions — bots never need reminder rows
					// regardless of how their UID landed in
					// `mention.uids`. Skipping a bot here is the
					// single source of truth for "no reminders for
					// bots", complementing CloneForExpansion (which
					// only protects the send-side map).
					if m.isBotUID(uid) {
						continue
					}
					version, err := m.nextReminderSeq()
					if err != nil {
						m.Warn("GenSeq failed", zap.Error(err))
						continue
					}
					reminders = append(reminders, &remindersModel{
						ChannelID:    message.ChannelID,
						ChannelType:  message.ChannelType,
						Publisher:    message.FromUID,
						MessageID:    fmt.Sprintf("%d", message.MessageID),
						MessageSeq:   message.MessageSeq,
						ReminderType: ReminderTypeMentionMe,
						UID:          uid,
						IsLocate:     1,
						Version:      version,
						Text:         "[有人@我]",
					})
				}
			}
		}
		// 申请入群
		contentType := m.contentType(payloadMap)
		if contentType == common.GroupMemberInvite.Int() {
			if payloadMap["visibles"] != nil {
				visibleObjs, ok := payloadMap["visibles"].([]interface{})
				if !ok {
					continue
				}
				for _, visibleObj := range visibleObjs {
					uid, ok := visibleObj.(string)
					if !ok {
						continue
					}
					version, err := m.nextReminderSeq()
					if err != nil {
						m.Warn("GenSeq failed", zap.Error(err))
						continue
					}
					reminders = append(reminders, &remindersModel{
						ChannelID:    message.ChannelID,
						ChannelType:  message.ChannelType,
						MessageID:    fmt.Sprintf("%d", message.MessageID),
						MessageSeq:   message.MessageSeq,
						ReminderType: ReminderTypeApplyJoinGroup,
						UID:          uid,
						IsLocate:     1,
						Version:      version,
						Text:         "[进群申请]",
					})
				}
			}
		}
	}
	return reminders
}

// filterChannelLevelByPublisher removes channel-level reminders
// (UID=="") whose Publisher equals the viewer. Used as a
// defense-in-depth pass after remindersDB.sync, which already enforces
// the same predicate at the SQL layer for cursor correctness
// (YUJ-1377 / Mininglamp-OSS/octo-server#101).
//
// Per-uid reminders (UID!="") are returned untouched. This preserves
// other reminder types — notably ReminderTypeApplyJoinGroup, which is
// always emitted with an explicit UID — so the filter is a no-op for
// anything except the @-broadcast fan-out.
//
// Returns the input slice unchanged when no row matches the filter,
// avoiding an allocation on the common path.
func filterChannelLevelByPublisher(reminders []*remindersDetailModel, viewerUID string) []*remindersDetailModel {
	if len(reminders) == 0 || viewerUID == "" {
		return reminders
	}
	// Fast path: scan first to decide whether we need to allocate.
	drop := false
	for _, r := range reminders {
		if r != nil && r.UID == "" && r.Publisher == viewerUID {
			drop = true
			break
		}
	}
	if !drop {
		return reminders
	}
	out := make([]*remindersDetailModel, 0, len(reminders))
	for _, r := range reminders {
		if r != nil && r.UID == "" && r.Publisher == viewerUID {
			continue
		}
		out = append(out, r)
	}
	return out
}

func (m *Message) handleReminders(reminders []*remindersModel) {
	if len(reminders) > 0 {
		err := m.remindersDB.inserts(reminders)
		if err != nil {
			m.Error("插入提醒项失败！", zap.Error(err))
		}
		channels := make([]*config.ChannelReq, 0)
		uids := make([]string, 0)
		for _, reminder := range reminders {
			if reminder.UID == "" {
				channels = append(channels, &config.ChannelReq{
					ChannelID:   reminder.ChannelID,
					ChannelType: reminder.ChannelType,
				})
			} else {
				uids = append(uids, reminder.UID)
			}
		}
		if len(channels) > 0 {
			for _, channel := range channels {
				err = m.ctx.SendCMD(config.MsgCMDReq{
					NoPersist:   true,
					ChannelID:   channel.ChannelID,
					ChannelType: channel.ChannelType,
					CMD:         common.CMDSyncReminders,
				})
				if err != nil {
					m.Error("发送cmd[CMDSyncReminders]失败！", zap.Error(err))
				}
			}
		}
		if len(uids) > 0 {
			err = m.ctx.SendCMD(config.MsgCMDReq{
				NoPersist:   true,
				Subscribers: uids,
				CMD:         common.CMDSyncReminders,
			})
			if err != nil {
				m.Error("发送cmd[CMDSyncReminders]失败！", zap.Error(err))
			}
		}
	}
}

func (m *Message) hasMention(payloadMap map[string]interface{}) bool {
	return payloadMap["mention"] != nil
}

func (m *Message) getMention(payloadMap map[string]interface{}) (all bool, uids []string) {
	mentionMap, ok := payloadMap["mention"].(map[string]interface{})
	if !ok {
		return false, nil
	}
	// YUJ-202 / Mininglamp-OSS#94 / YUJ-1389 / #142 — mention three-
	// state read side. The chokepoint (pkg/mentionrewrite.RewriteMention)
	// is now a pass-through after Mininglamp-OSS/octo-server#142: legacy
	// `mention.all=1` is no longer auto-rewritten into `mention.ais=1`,
	// and the read side must treat `all`, `ais`, and `humans` as three
	// independent client-emitted signals. A new client can send any
	// combination (`humans=1` for a human-only ping, `ais=1` for an AI
	// broadcast, both for `@所有人 + @所有 AI`, neither + uids for a
	// targeted mention). getMention here returns all=true if ANY of
	// {humans, ais, all} = 1 so the caller (getReminders) can then gate
	// the channel-level reminder on the explicit `humans=1` signal —
	// see the call site for the reminder-emission logic. Reasoning
	// matrix (Plan X + post-#142 pass-through):
	//
	//   humans=1                       → human members see reminder,
	//                                    bots silent (humans-only path)
	//   ais=1                          → bot members respond via
	//                                    message delivery, NO channel-
	//                                    level reminder is created
	//   humans=1 AND ais=1             → humans see reminder, bots
	//                                    respond via delivery
	//   all=1 (legacy `@所有人`, no    → render-only metadata for old
	//   ais / humans set by client)      read-side clients; NO human
	//                                    reminder AND NO bot summon —
	//                                    post-#142 the legacy field is
	//                                    NOT auto-promoted to ais=1.
	//                                    See pkg/mentionrewrite/rewrite.go
	//                                    + modules/bot_api/obo_fanout.go
	//                                    for the matching pass-through.
	//
	// getMention itself stays "any broadcast → all=true"; the
	// humans-gate lives at the call site so we never lose the per-uid
	// fan-out branch when a broadcast also carries explicit @uid
	// mentions.
	if mentionAnyBroadcast(mentionMap) {
		all = true
	}
	if mentionMap["uids"] != nil {
		uidObjs, ok := mentionMap["uids"].([]interface{})
		if !ok {
			return all, nil
		}
		uids = make([]string, 0, len(uidObjs))
		for _, uidObj := range uidObjs {
			if uid, ok := uidObj.(string); ok {
				uids = append(uids, uid)
			}
		}
	}
	return
}

// mentionAnyBroadcast reports whether the parsed `mention` map carries
// any of the three broadcast flags (humans / ais / all) set to 1. See
// getMention's doc comment for the per-flag semantics. Defensive:
// accepts json.Number / float / int / bool forms for each flag so
// callers that decoded without UseNumber don't silently miss the
// broadcast.
func mentionAnyBroadcast(mentionMap map[string]interface{}) bool {
	return mentionFlagTruthy(mentionMap["humans"]) ||
		mentionFlagTruthy(mentionMap["ais"]) ||
		mentionFlagTruthy(mentionMap["all"])
}

// mentionFlagTruthy reports whether a parsed mention.* flag value is
// the numeric/boolean form of 1. Mirrors pkg/mentionrewrite.isTruthyOne
// but kept local to avoid leaking the helper through an internal API —
// the read side and the rewrite side intentionally share the same
// "truthy = 1" semantics so a round-trip through the chokepoint is
// observable.
func mentionFlagTruthy(v interface{}) bool {
	switch x := v.(type) {
	case nil:
		return false
	case json.Number:
		n, err := x.Int64()
		return err == nil && n == 1
	case float64:
		return x == 1
	case float32:
		return x == 1
	case int:
		return x == 1
	case int8:
		return x == 1
	case int16:
		return x == 1
	case int32:
		return x == 1
	case int64:
		return x == 1
	case uint:
		return x == 1
	case uint8:
		return x == 1
	case uint16:
		return x == 1
	case uint32:
		return x == 1
	case uint64:
		return x == 1
	case bool:
		return x
	default:
		return false
	}
}

func (m *Message) contentType(payloadMap map[string]interface{}) int {
	if payloadMap["type"] != nil {
		switch v := payloadMap["type"].(type) {
		case json.Number:
			contentTypeI, _ := v.Int64()
			return int(contentTypeI)
		case float64:
			return int(v)
		}
	}
	return 0
}

type reminderResp struct {
	ID           int64                  `json:"id"`
	ChannelID    string                 `json:"channel_id"`
	ChannelType  uint8                  `json:"channel_type"`
	Publisher    string                 `json:"publisher"`
	MessageSeq   uint32                 `json:"message_seq"`
	MessageID    string                 `json:"message_id"`
	ReminderType ReminderType           `json:"reminder_type"`
	UID          string                 `json:"uid"`
	Text         string                 `json:"text"`
	Data         map[string]interface{} `json:"data,omitempty"`
	IsLocate     int                    `json:"is_locate"`
	Version      int64                  `json:"version"`
	Done         int                    `json:"done"`
}

func newReminderResp(m *remindersDetailModel) *reminderResp {

	var dataMap map[string]interface{}
	if m.Data != "" {
		dataMap, _ = util.JsonToMap(m.Data)
	}

	return &reminderResp{
		ID:           m.Id,
		ChannelID:    m.ChannelID,
		ChannelType:  m.ChannelType,
		MessageSeq:   m.MessageSeq,
		MessageID:    m.MessageID,
		ReminderType: ReminderType(m.ReminderType),
		Publisher:    m.Publisher,
		UID:          m.UID,
		Text:         m.Text,
		Data:         dataMap,
		IsLocate:     m.IsLocate,
		Version:      m.Version,
		Done:         m.Done,
	}
}
