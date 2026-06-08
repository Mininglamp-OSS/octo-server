package opanalytics

import (
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
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

// ETL 看板预聚合任务(增量水位，幂等)。
//
// 抽取模型：不再按 timestamp 全表扫 message 分片(无该索引)，改为按主键 id keyset 分页
// 流式增量读取，每条消息精确一次累加进 ③；首次运行从 id=0 自动全量回填，之后每次只读新增。
// 稳定性闸门(created_at ≤ DB_NOW-lag)杜绝"低 id 晚提交被游标越过"的并发漏扫(见 etlLagSeconds)。
//
// 口径语义(事件处理时口径 / event-time semantics)：③/④ 是按消息处理当时的维表(成员类型、
// 排除名单、群成员构成)累加的不可变事实。事后变更维表(如 user.category 改 system、系统 bot
// 名单扩展、robot 更正、群成员增减)只影响**之后**处理的新消息，不回溯已写入的历史行。如需让
// 维表变更追溯既往，调用 Rebuild() 安全重建(清事实+水位，下轮用当前维表全量重算)。
type ETL struct {
	log.Log
	ctx   *config.Context
	db    *etlDB
	batch int
	lag   int64
}

// NewETL 创建 ETL。
func NewETL(ctx *config.Context) *ETL {
	return &ETL{
		Log:   log.NewTLog("OpanalyticsETL"),
		ctx:   ctx,
		db:    newETLDB(ctx),
		batch: etlBatchSize(),
		lag:   etlLagSeconds(),
	}
}

// RunIncremental 增量跑一轮：刷新维表 → 逐分片按水位流式抽取并累加 ③ → 标脏日 → 由 ③ 重算 ④。
//
// 幂等：游标保证已处理消息不重复计入(精确一次)；重复调用在无新消息时为空操作。
// 崩溃安全：③ 累加与游标推进同事务提交；④ 经持久化脏日队列由本轮或下轮重算，最终一致。
func (e *ETL) RunIncremental() error {
	// 1. 全量刷新成员维表，得到 uid→member_type 与排除集(系统/测试账号)。
	memberType, excluded, err := e.refreshDimMembers()
	if err != nil {
		return err
	}

	// 2. 全量刷新群会话维表，得到 group_no→{space_id,conv_type}。
	groupMeta, err := e.refreshDimChannelGroups()
	if err != nil {
		return err
	}

	// 3. 逐分片增量抽取并累加 ③(每 chunk 一个事务，FOR UPDATE 串行化多实例)。
	//    nowUnix 取自 DB，配合 lag 构成稳定性闸门，统一时基避免应用/DB 时钟偏差。
	nowUnix, err := e.db.dbNowUnix()
	if err != nil {
		return err
	}
	excludedUID := func(uid string) bool { return spacepkg.IsSystemBot(uid) || excluded[uid] }
	var totalRows int64
	for _, table := range e.db.messageTables() {
		if err = e.db.ensureCursor(table); err != nil {
			return err
		}
		for {
			n, cerr := e.db.runChunk(table, e.batch, nowUnix, e.lag, func(rows []*srcMessageRow) *chunkResult {
				return aggregateChunk(rows, memberType, excludedUID, groupMeta)
			})
			if cerr != nil {
				return cerr
			}
			totalRows += n
			if int(n) < e.batch {
				break
			}
		}
	}

	// 4. 由 ③ 重算所有脏日的 ④，成功后出队(对③最终一致；下轮自愈残留脏日)。
	days, err := e.db.loadDirtyDays()
	if err != nil {
		return err
	}
	for _, day := range days {
		if err = e.db.recomputeChannelDay(day); err != nil {
			return err
		}
	}

	e.Info("opanalytics ETL incremental done",
		zap.Int64("messages_scanned", totalRows),
		zap.Int("recomputed_days", len(days)))
	return nil
}

// Rebuild 安全重建：清空事实表(③/④)、抽取水位与脏日队列，再用**当前**维表从 id=0 全量重算。
//
// 用途——口径回溯(event-time 语义的逃生门)：当成员类型/排除名单/群成员构成等维度发生历史性
// 修正，需要让既往统计也按新口径重算时，运行此入口。注意这是一次性重读全部 message 历史的重操作，
// 应在低峰由运维显式触发(非定时路径)。
//
// 运维亦可等价地手工执行：TRUNCATE octo_fact_member_channel_daily / octo_fact_channel_daily /
// octo_etl_message_cursor / octo_etl_dirty_day，下一次定时 RunIncremental 会自动全量回填。
//
// 注意：本方法**不持** scheduler 的 Redis ETL 锁。当前未接任何入口(仅供运维/测试直接调用)，
// 故安全。将来若挂到 ops 端点，必须先获取同一把锁(并暂停 cron)，否则与定时跑并发会在清空游标
// 与增量重算之间互相踩踏。
func (e *ETL) Rebuild() error {
	if err := e.db.truncateForRebuild(); err != nil {
		return err
	}
	e.Info("opanalytics ETL rebuild: facts/cursor cleared, full recompute starting")
	return e.RunIncremental()
}

// groupMetaInfo 群的归属与类型。
type groupMetaInfo struct {
	spaceID  string
	convType uint8
}

// chunkResult 单个抽取 chunk 聚合后的待写数据(纯数据，由 etlDB 在事务内落库)。
type chunkResult struct {
	fact3        []*factMemberChannelDailyModel // ③ 增量(msg_count=本 chunk 计数，累加 upsert)
	privateRows  [][]interface{}                // dim_channel 私聊 upsert 行
	activityRows [][]interface{}                // dim_channel 活跃时间 upsert 行 [channel_id,channel_type,last_active_at]
	dirtyDays    []string                       // 本 chunk 触达的 stat_date(去重)
}

// chanDayAgg 单(日,会话)聚合(活跃时间与私聊首条时间)。
type chanDayAgg struct {
	day         string
	channelID   string
	channelType uint8
	dayMaxTs    int64
	dayMinTs    int64
}

// senderDayAgg 单(日,会话,成员)聚合。
type senderDayAgg struct {
	day       string
	channelID string
	senderUID string
	msgCount  int
	lastMsgAt int64
}

// channelMeta 会话的归属/分类(ETL 当日打标)。
type channelMeta struct {
	spaceID     string
	convType    uint8
	channelType uint8
	skip        bool // 不可解析的私聊(uid 含 @)或任一方为系统/测试账号 → 丢弃其事实行
}

// aggregateChunk 把一个 chunk 的消息按(日,会话,成员)聚合成待写数据(纯函数)。
//
// 排除口径(验收①)：excludedUID 命中的 sender(系统机器人/测试账号)其消息直接跳过，
// 自然不进 ③/④/活跃数/活跃时间；私聊任一方被排除则整条会话丢弃。
func aggregateChunk(rows []*srcMessageRow, memberType map[string]uint8, excludedUID func(string) bool, groupMeta map[string]groupMetaInfo) *chunkResult {
	loc := reportLocation()
	dayOf := func(ts int64) string { return time.Unix(ts, 0).In(loc).Format("2006-01-02") }

	channels := map[string]*chanDayAgg{}  // key = day\x00channelID
	senders := map[string]*senderDayAgg{} // key = day\x00channelID\x00senderUID
	for _, m := range rows {
		if excludedUID(m.FromUID) {
			continue
		}
		day := dayOf(m.Timestamp)
		ck := day + "\x00" + m.ChannelID
		ca := channels[ck]
		if ca == nil {
			ca = &chanDayAgg{day: day, channelID: m.ChannelID, channelType: m.ChannelType, dayMinTs: m.Timestamp, dayMaxTs: m.Timestamp}
			channels[ck] = ca
		}
		if m.Timestamp > ca.dayMaxTs {
			ca.dayMaxTs = m.Timestamp
		}
		if m.Timestamp < ca.dayMinTs {
			ca.dayMinTs = m.Timestamp
		}
		sk := ck + "\x00" + m.FromUID
		sa := senders[sk]
		if sa == nil {
			sa = &senderDayAgg{day: day, channelID: m.ChannelID, senderUID: m.FromUID}
			senders[sk] = sa
		}
		sa.msgCount++
		if m.Timestamp > sa.lastMsgAt {
			sa.lastMsgAt = m.Timestamp
		}
	}

	// 会话归属/分类按 channelID 解析一次(缓存)。
	metaCache := map[string]*channelMeta{}
	getMeta := func(channelID string, channelType uint8) *channelMeta {
		if cm, ok := metaCache[channelID]; ok {
			return cm
		}
		cm := resolveChannelMeta(channelID, channelType, memberType, excludedUID, groupMeta)
		metaCache[channelID] = cm
		return cm
	}

	res := &chunkResult{}
	dirtySet := map[string]struct{}{}

	for _, ca := range channels {
		cm := getMeta(ca.channelID, ca.channelType)
		if cm.skip {
			continue
		}
		dirtySet[ca.day] = struct{}{}
		res.activityRows = append(res.activityRows, []interface{}{ca.channelID, ca.channelType, ca.dayMaxTs})
		if cm.channelType == channelTypePerson {
			a, b, _ := normalizePrivatePair(ca.channelID) // skip=false 保证可解析
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
			res.privateRows = append(res.privateRows, []interface{}{
				ca.channelID, channelTypePerson, "", cm.convType, "", a, b, privateMemberSize, human, agent, dimStatusNormal, ca.dayMinTs,
			})
		}
	}

	for _, sa := range senders {
		cm := getMeta(sa.channelID, channelTypeFromChannels(channels, sa.day, sa.channelID))
		if cm == nil || cm.skip {
			continue
		}
		st := memberType[sa.senderUID]
		if st == 0 {
			st = memberTypeHuman
		}
		res.fact3 = append(res.fact3, &factMemberChannelDailyModel{
			StatDate:    sa.day,
			ChannelID:   sa.channelID,
			ChannelType: cm.channelType,
			SpaceID:     cm.spaceID,
			ConvType:    cm.convType,
			ContentType: 0,
			SenderUID:   sa.senderUID,
			SenderType:  st,
			MsgCount:    sa.msgCount,
			LastMsgAt:   sa.lastMsgAt,
		})
	}

	res.dirtyDays = make([]string, 0, len(dirtySet))
	for d := range dirtySet {
		res.dirtyDays = append(res.dirtyDays, d)
	}
	return res
}

// channelTypeFromChannels 取(日,会话)聚合里记录的 channel_type(供 sender 行解析 meta)。
func channelTypeFromChannels(channels map[string]*chanDayAgg, day, channelID string) uint8 {
	if ca := channels[day+"\x00"+channelID]; ca != nil {
		return ca.channelType
	}
	return 0
}

// resolveChannelMeta 解析会话归属/分类(群查 group 维表；私聊反解 fakeChannelID)。
func resolveChannelMeta(channelID string, channelType uint8, memberType map[string]uint8, excludedUID func(string) bool, groupMeta map[string]groupMetaInfo) *channelMeta {
	if channelType == channelTypeGroup {
		if gm, ok := groupMeta[channelID]; ok {
			return &channelMeta{spaceID: gm.spaceID, convType: gm.convType, channelType: channelTypeGroup}
		}
		// 已从 group 表消失的孤儿群：仍计消息，但不归 space、类型未知。
		return &channelMeta{spaceID: "", convType: 0, channelType: channelTypeGroup}
	}
	// 私聊(person)
	a, b, ok := normalizePrivatePair(channelID)
	if !ok || excludedUID(a) || excludedUID(b) {
		// 不可解析，或任一方系统/测试账号 → 丢弃。
		return &channelMeta{channelType: channelTypePerson, skip: true}
	}
	return &channelMeta{spaceID: "", convType: privateConvType(memberType[a], memberType[b]), channelType: channelTypePerson}
}

// refreshDimMembers 全量替换成员维表，返回 uid→member_type 映射与**消息路径**排除集。
//
// 两类排除语义分离：
//   - dim_member.is_excluded(用于成员总数)= 系统/测试账号 ∪ 禁用/注销(status≠1)。
//     总数反映"当前在册成员"，故禁用用户应剔除。
//   - 返回的 excludedForMessages(用于消息计数)= 仅系统/测试账号，**不含** status。
//     消息计数是 event-time 语义：用户活跃期发的消息照算，事后禁用不回溯。
func (e *ETL) refreshDimMembers() (map[string]uint8, map[string]bool, error) {
	users, err := e.db.queryUsersForDim()
	if err != nil {
		return nil, nil, err
	}
	memberType := make(map[string]uint8, len(users))
	excludedForMessages := make(map[string]bool)
	rows := make([][]interface{}, 0, len(users))
	for _, u := range users {
		mt := memberTypeHuman
		if u.Robot == 1 {
			mt = memberTypeAgent
		}
		memberType[u.UID] = mt
		noise := isExcludedMember(u.UID, u.Category)
		if noise {
			excludedForMessages[u.UID] = true
		}
		ex := 0
		if noise || u.Status != 1 {
			ex = 1
		}
		rows = append(rows, []interface{}{u.UID, u.Name, u.Email, u.Phone, u.Zone, mt, ex})
	}
	if err = e.db.replaceDimMembers(rows); err != nil {
		return nil, nil, err
	}
	return memberType, excludedForMessages, nil
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

// ===== 纯函数(便于单测) =====

// isExcludedMember 判定是否系统/测试账号。单一真源 pkg/space.SystemBots
// (botfather/u_10000/fileHelper/notification) ∪ user.category=='system'。
func isExcludedMember(uid, category string) bool {
	return spacepkg.IsSystemBot(uid) || category == "system"
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

// dayWindowUnix 返回报告时区某自然日的纪元秒窗口 [start, end)(供测试与日期解析复用)。
func dayWindowUnix(date string) (int64, int64, error) {
	t, err := time.ParseInLocation("2006-01-02", date, reportLocation())
	if err != nil {
		return 0, 0, err
	}
	return t.Unix(), t.AddDate(0, 0, 1).Unix(), nil
}
