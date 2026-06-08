package opanalytics

import (
	"sort"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

const (
	memberTypeHuman uint8 = 1
	memberTypeAgent uint8 = 2

	convTypeHHGroup   uint8 = 1
	convTypeHAGroup   uint8 = 2
	convTypeHHPrivate uint8 = 3
	convTypeHAPrivate uint8 = 4

	channelTypePerson uint8 = 1 // = octo-lib common.ChannelTypePerson
	channelTypeGroup  uint8 = 2 // = octo-lib common.ChannelTypeGroup

	dimStatusNormal   uint8 = 1
	dimStatusDisband  uint8 = 2
	privateMemberSize       = 2
)

// excludedSeedUIDs 固化的系统/测试账号排除种子(口径3：测试账号·系统机器人不算)。
var excludedSeedUIDs = map[string]struct{}{
	"u_10000":    {},
	"botfather":  {},
	"fileHelper": {},
}

// ETL 看板每日预聚合任务(T+1，幂等)。
type ETL struct {
	log.Log
	ctx *config.Context
	db  *etlDB
}

// NewETL 创建 ETL。
func NewETL(ctx *config.Context) *ETL {
	return &ETL{
		Log: log.NewTLog("OpanalyticsETL"),
		ctx: ctx,
		db:  newETLDB(ctx),
	}
}

// RunYesterday 跑前一报告时区自然日(供调度器调用)。
func (e *ETL) RunYesterday() error {
	yesterday := time.Now().In(reportLocation()).AddDate(0, 0, -1)
	return e.RunDay(yesterday.Format("2006-01-02"))
}

// RunBackfill 按报告时区日期回填 [fromDate, toDate](含两端，YYYY-MM-DD)，逐日幂等。
func (e *ETL) RunBackfill(fromDate, toDate string) error {
	from, err := time.ParseInLocation("2006-01-02", fromDate, reportLocation())
	if err != nil {
		return err
	}
	to, err := time.ParseInLocation("2006-01-02", toDate, reportLocation())
	if err != nil {
		return err
	}
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		if err := e.RunDay(d.Format("2006-01-02")); err != nil {
			return err
		}
	}
	return nil
}

// chanAgg 单会话当日聚合(用于活跃时间与私聊首条时间)。
type chanAgg struct {
	channelType uint8
	dayMaxTs    int64
	dayMinTs    int64
}

// senderAgg 单(会话,成员)当日聚合。
type senderAgg struct {
	msgCount  int
	lastMsgAt int64
}

// channelMeta 会话的归属/分类(ETL 当日打标)。
type channelMeta struct {
	spaceID     string
	convType    uint8
	channelType uint8
	skip        bool // 不可解析的私聊(uid 含 @ 等) → 跳过其事实行
}

// RunDay 处理某报告时区自然日(YYYY-MM-DD)：刷新维表 → 抽取消息 → ③ → 上卷 ④ → 幂等写。
func (e *ETL) RunDay(date string) error {
	start, end, err := dayWindowUnix(date)
	if err != nil {
		return err
	}

	// 1. 刷新成员维表，得到 uid→member_type 内存映射。
	memberType, err := e.refreshDimMembers()
	if err != nil {
		return err
	}

	// 2. 刷新群会话维表，得到 group_no→{space_id,conv_type}。
	groupMeta, err := e.refreshDimChannelGroups()
	if err != nil {
		return err
	}

	// 3. 跨分片抽取当日消息并内存聚合。
	senders := map[string]*senderAgg{} // key = channelID + "\x00" + senderUID
	channels := map[string]*chanAgg{}  // key = channelID
	for _, table := range e.db.messageTables() {
		rows, qerr := e.db.queryDayMessages(table, start, end)
		if qerr != nil {
			return qerr
		}
		for _, m := range rows {
			ca := channels[m.ChannelID]
			if ca == nil {
				ca = &chanAgg{channelType: m.ChannelType, dayMinTs: m.Timestamp, dayMaxTs: m.Timestamp}
				channels[m.ChannelID] = ca
			}
			if m.Timestamp > ca.dayMaxTs {
				ca.dayMaxTs = m.Timestamp
			}
			if m.Timestamp < ca.dayMinTs {
				ca.dayMinTs = m.Timestamp
			}
			key := m.ChannelID + "\x00" + m.FromUID
			sa := senders[key]
			if sa == nil {
				sa = &senderAgg{}
				senders[key] = sa
			}
			sa.msgCount++
			if m.Timestamp > sa.lastMsgAt {
				sa.lastMsgAt = m.Timestamp
			}
		}
	}

	// 4. 发现并 upsert 私聊会话；合成全量会话归属表。
	meta := map[string]*channelMeta{}
	privateRows := make([][]interface{}, 0)
	for channelID, ca := range channels {
		if ca.channelType == channelTypeGroup {
			if gm, ok := groupMeta[channelID]; ok {
				meta[channelID] = &channelMeta{spaceID: gm.spaceID, convType: gm.convType, channelType: channelTypeGroup}
			} else {
				// 已从 group 表消失的孤儿群：仍计消息，但不归 space、类型未知。
				meta[channelID] = &channelMeta{spaceID: "", convType: 0, channelType: channelTypeGroup}
			}
			continue
		}
		// 私聊(person)
		a, b, ok := normalizePrivatePair(channelID)
		if !ok {
			e.Warn("skip unparseable private channel (uid may contain '@')", zap.String("channel_id", channelID))
			meta[channelID] = &channelMeta{channelType: channelTypePerson, skip: true}
			continue
		}
		conv := privateConvType(memberType[a], memberType[b])
		meta[channelID] = &channelMeta{spaceID: "", convType: conv, channelType: channelTypePerson}
		human, agent := 0, 0
		if memberType[a] == memberTypeAgent {
			agent++
		} else {
			human++
		}
		if memberType[b] == memberTypeAgent {
			agent++
		} else {
			human++
		}
		privateRows = append(privateRows, []interface{}{
			channelID, channelTypePerson, "", conv, "", a, b, privateMemberSize, human, agent, dimStatusNormal, ca.dayMinTs,
		})
	}
	if err = e.db.upsertDimChannelPrivate(privateRows); err != nil {
		return err
	}

	// 5. 组装 ③(成员×会话×日)。
	fact3 := make([]*factMemberChannelDailyModel, 0, len(senders))
	for key, sa := range senders {
		sep := strings.IndexByte(key, '\x00')
		channelID, senderUID := key[:sep], key[sep+1:]
		cm := meta[channelID]
		if cm == nil || cm.skip {
			continue
		}
		st := memberType[senderUID]
		if st == 0 {
			st = memberTypeHuman
		}
		fact3 = append(fact3, &factMemberChannelDailyModel{
			StatDate:    date,
			ChannelID:   channelID,
			ChannelType: cm.channelType,
			SpaceID:     cm.spaceID,
			ConvType:    cm.convType,
			ContentType: 0,
			SenderUID:   senderUID,
			SenderType:  st,
			MsgCount:    sa.msgCount,
			LastMsgAt:   sa.lastMsgAt,
		})
	}

	// 6. 上卷 ④(会话×日)。
	fact4 := rollupChannelDaily(date, fact3, meta)

	// 7. 幂等写两张事实表。
	if err = e.db.replaceFactDay(date, fact3, fact4); err != nil {
		return err
	}

	// 8. 单调更新会话最后活跃时间。
	activityRows := make([][]interface{}, 0, len(channels))
	for channelID, ca := range channels {
		if cm := meta[channelID]; cm != nil && cm.skip {
			continue
		}
		activityRows = append(activityRows, []interface{}{channelID, ca.channelType, ca.dayMaxTs})
	}
	if err = e.db.updateChannelActivity(activityRows); err != nil {
		return err
	}

	e.Info("opanalytics ETL day done",
		zap.String("date", date),
		zap.Int("channels", len(channels)),
		zap.Int("fact_member_rows", len(fact3)),
		zap.Int("fact_channel_rows", len(fact4)))
	return nil
}

// groupMetaInfo 群的归属与类型。
type groupMetaInfo struct {
	spaceID  string
	convType uint8
}

// refreshDimMembers 全量刷新成员维表，返回 uid→member_type 映射。
func (e *ETL) refreshDimMembers() (map[string]uint8, error) {
	users, err := e.db.queryUsersForDim()
	if err != nil {
		return nil, err
	}
	memberType := make(map[string]uint8, len(users))
	rows := make([][]interface{}, 0, len(users))
	for _, u := range users {
		mt := memberTypeHuman
		if u.Robot == 1 {
			mt = memberTypeAgent
		}
		memberType[u.UID] = mt
		excluded := 0
		if isExcludedMember(u.UID, u.Category) {
			excluded = 1
		}
		rows = append(rows, []interface{}{u.UID, u.Name, u.Email, u.Phone, u.Zone, mt, excluded})
	}
	if err = e.db.upsertDimMembers(rows); err != nil {
		return nil, err
	}
	return memberType, nil
}

// refreshDimChannelGroups 全量刷新群会话维表，返回 group_no→{space_id,conv_type}。
func (e *ETL) refreshDimChannelGroups() (map[string]groupMetaInfo, error) {
	groups, err := e.db.queryGroupsForDim()
	if err != nil {
		return nil, err
	}
	counts, err := e.db.queryGroupMemberCounts()
	if err != nil {
		return nil, err
	}
	countByGroup := make(map[string]*groupMemberCountRow, len(counts))
	for _, c := range counts {
		countByGroup[c.GroupNo] = c
	}

	meta := make(map[string]groupMetaInfo, len(groups))
	rows := make([][]interface{}, 0, len(groups))
	for _, g := range groups {
		agent, total := 0, 0
		if c := countByGroup[g.GroupNo]; c != nil {
			agent, total = c.AgentCnt, c.TotalCnt
		}
		human := total - agent
		conv := groupConvType(agent)
		status := dimStatusNormal
		if g.Status != 1 {
			status = dimStatusDisband
		}
		meta[g.GroupNo] = groupMetaInfo{spaceID: g.SpaceID, convType: conv}
		rows = append(rows, []interface{}{
			g.GroupNo, channelTypeGroup, g.SpaceID, conv, g.Name, total, human, agent, status, g.CreatedAtSec,
		})
	}
	if err = e.db.upsertDimChannelGroups(rows); err != nil {
		return nil, err
	}
	return meta, nil
}

// rollupChannelDaily 由 ③ 上卷出 ④(会话×日)。
func rollupChannelDaily(date string, fact3 []*factMemberChannelDailyModel, meta map[string]*channelMeta) []*factChannelDailyModel {
	type acc struct {
		humanMsg, agentMsg       int
		lastMsgAt                int64
		activeHuman, activeAgent map[string]struct{}
	}
	byChannel := map[string]*acc{}
	for _, f := range fact3 {
		a := byChannel[f.ChannelID]
		if a == nil {
			a = &acc{activeHuman: map[string]struct{}{}, activeAgent: map[string]struct{}{}}
			byChannel[f.ChannelID] = a
		}
		if f.SenderType == memberTypeAgent {
			a.agentMsg += f.MsgCount
			a.activeAgent[f.SenderUID] = struct{}{}
		} else {
			a.humanMsg += f.MsgCount
			a.activeHuman[f.SenderUID] = struct{}{}
		}
		if f.LastMsgAt > a.lastMsgAt {
			a.lastMsgAt = f.LastMsgAt
		}
	}

	// 稳定输出顺序(便于幂等校验/测试)。
	channelIDs := make([]string, 0, len(byChannel))
	for id := range byChannel {
		channelIDs = append(channelIDs, id)
	}
	sort.Strings(channelIDs)

	out := make([]*factChannelDailyModel, 0, len(byChannel))
	for _, id := range channelIDs {
		a := byChannel[id]
		cm := meta[id]
		var spaceID string
		var convType, channelType uint8
		if cm != nil {
			spaceID, convType, channelType = cm.spaceID, cm.convType, cm.channelType
		}
		out = append(out, &factChannelDailyModel{
			StatDate:           date,
			ChannelID:          id,
			ChannelType:        channelType,
			SpaceID:            spaceID,
			ConvType:           convType,
			HumanMsgCount:      a.humanMsg,
			AgentMsgCount:      a.agentMsg,
			ActiveHumanMembers: len(a.activeHuman),
			ActiveAgentMembers: len(a.activeAgent),
			LastMsgAt:          a.lastMsgAt,
		})
	}
	return out
}

// ===== 纯函数(便于单测) =====

// dayWindowUnix 返回报告时区某自然日的纪元秒窗口 [start, end)。
func dayWindowUnix(date string) (int64, int64, error) {
	t, err := time.ParseInLocation("2006-01-02", date, reportLocation())
	if err != nil {
		return 0, 0, err
	}
	return t.Unix(), t.AddDate(0, 0, 1).Unix(), nil
}

// isExcludedMember 判定是否系统/测试账号(种子名单 ∪ category=='system')。
func isExcludedMember(uid, category string) bool {
	if _, ok := excludedSeedUIDs[uid]; ok {
		return true
	}
	return category == "system"
}

// normalizePrivatePair 反解 fakeChannelID 双方并按字典序规范化(成员对去重一致)。
// 任一段为空或不是两段(uid 含 @)→ ok=false。
func normalizePrivatePair(channelID string) (string, string, bool) {
	parts := strings.Split(channelID, "@")
	if len(parts) != privateMemberSize || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	a, b := parts[0], parts[1]
	if a > b {
		a, b = b, a
	}
	return a, b, true
}

// groupConvType 群会话类型：含 agent → HA群，否则 HH群。
func groupConvType(agentCount int) uint8 {
	if agentCount > 0 {
		return convTypeHAGroup
	}
	return convTypeHHGroup
}

// privateConvType 私聊会话类型：任一为 agent → HA私聊，否则 HH私聊。
func privateConvType(aType, bType uint8) uint8 {
	if aType == memberTypeAgent || bType == memberTypeAgent {
		return convTypeHAPrivate
	}
	return convTypeHHPrivate
}
