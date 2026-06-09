package opanalytics

import (
	"sort"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
)

// service 看板读侧业务编排。
type service struct {
	log.Log
	db *opanalyticsDB
}

func newService(ctx *config.Context) *service {
	return &service{
		Log: log.NewTLog("OpanalyticsService"),
		db:  newOpanalyticsDB(ctx),
	}
}

// overview 组装模块A 概览卡片。总数与活跃/消息量均随时间范围与可选 space 筛选收敛：选中某 Space
// 时，总数(space/group/member)也限定到该 Space，前端用"总数+活跃数"算比例才不会失真。
// 活跃成员只算当前在册者(见 overviewActiveMembers)，私聊数在 space 筛选下置 0(私聊无 space 归属)。
func (s *service) overview(start, end string, spaceIDs []string) (*overviewResp, error) {
	spaceTotal, err := s.db.countSpacesTotal(spaceIDs)
	if err != nil {
		return nil, err
	}
	groupTotal, err := s.db.countGroupsTotal(spaceIDs)
	if err != nil {
		return nil, err
	}
	humanTotal, agentTotal, err := s.db.countMembersByType(spaceIDs)
	if err != nil {
		return nil, err
	}
	humanMsg, agentMsg, activeGroups, err := s.db.overviewMsgAndGroups(start, end, spaceIDs)
	if err != nil {
		return nil, err
	}
	activeHuman, activeAgent, err := s.db.overviewActiveMembers(start, end, spaceIDs)
	if err != nil {
		return nil, err
	}
	// 私聊无 space 归属：选中某 Space 时置 0(否则会把"全公司私聊数"混进按空间收敛的卡片，误导)。
	var privateActive int64
	if len(spaceIDs) == 0 {
		if privateActive, err = s.db.privateActiveCount(start, end); err != nil {
			return nil, err
		}
	}
	composition, err := s.db.queryMessageComposition(start, end, spaceIDs)
	if err != nil {
		return nil, err
	}
	messageComposition := normalizeMessageComposition(composition)
	if len(spaceIDs) > 0 {
		humanMsg, agentMsg = stripPrivateCompositionTotals(humanMsg, agentMsg, messageComposition)
		zeroPrivateMessageComposition(messageComposition)
	}
	return &overviewResp{
		SpaceTotal:         spaceTotal,
		GroupTotal:         groupTotal,
		HumanMemberTotal:   humanTotal,
		AgentTotal:         agentTotal,
		ActiveGroups:       activeGroups,
		ActiveHumanMembers: activeHuman,
		ActiveAgentMembers: activeAgent,
		HumanMsgCount:      humanMsg,
		AgentMsgCount:      agentMsg,
		PrivateActiveCount: privateActive,
		MessageComposition: messageComposition,
	}, nil
}

// trend 组装模块C 趋势。消息/活跃会话走 ④，活跃成员走 ③ join 当前 dim_member；
// week 桶内活跃成员按 distinct 计算，不能把每日活跃数相加。
func (s *service) trend(start, end, granularity string, spaceIDs []string) (*trendResp, error) {
	buckets, err := buildTrendBuckets(start, end, granularity)
	if err != nil {
		return nil, err
	}
	channelAgg, convAgg, err := s.db.queryTrendChannelAgg(start, end, granularity, spaceIDs)
	if err != nil {
		return nil, err
	}
	memberAgg, err := s.db.queryTrendActiveMembers(start, end, granularity, spaceIDs)
	if err != nil {
		return nil, err
	}

	items := make([]*trendItem, 0, len(buckets))
	for _, b := range buckets {
		ca := channelAgg[b.Bucket]
		ma := memberAgg[b.Bucket]
		convTypes := normalizeTrendConvTypes(convAgg[b.Bucket])
		if len(spaceIDs) > 0 {
			ca.HumanMsg, ca.AgentMsg = stripPrivateTrendTotals(ca.HumanMsg, ca.AgentMsg, convTypes)
			ca.PrivateActive = 0
			zeroPrivateTrendConvTypes(convTypes)
		}
		item := &trendItem{
			Bucket:             b.Bucket,
			StartDate:          b.StartDate,
			EndDate:            b.EndDate,
			HumanMsgCount:      ca.HumanMsg,
			AgentMsgCount:      ca.AgentMsg,
			TotalMsgCount:      ca.HumanMsg + ca.AgentMsg,
			ActiveHumanMembers: ma.ActiveHuman,
			ActiveAgentMembers: ma.ActiveAgent,
			ActiveGroups:       ca.ActiveGroups,
			PrivateActiveCount: ca.PrivateActive,
			ConvTypeMsgCounts:  convTypes,
		}
		items = append(items, item)
	}
	return &trendResp{Granularity: granularity, List: items}, nil
}

// spaceList 表一：内存合并维表/活跃聚合 → 过滤(活跃状态) → 排序 → 分页。
func (s *service) spaceList(start, end, name, activeStatus, sortBy, order string, offset, limit int) ([]*spaceListItem, int64, error) {
	bases, err := s.db.querySpaceBase(name)
	if err != nil {
		return nil, 0, err
	}
	groupCnt, err := s.db.queryGroupCountBySpace()
	if err != nil {
		return nil, 0, err
	}
	memberTotals, err := s.db.queryMemberTotalsBySpace()
	if err != nil {
		return nil, 0, err
	}
	activeAgg, err := s.db.queryActiveAggBySpace(start, end)
	if err != nil {
		return nil, 0, err
	}

	items := make([]*spaceListItem, 0, len(bases))
	for _, b := range bases {
		agg, isActive := activeAgg[b.SpaceID]
		switch activeStatus {
		case "active":
			if !isActive {
				continue
			}
		case "inactive":
			if isActive {
				continue
			}
		}
		mt := memberTotals[b.SpaceID]
		items = append(items, &spaceListItem{
			SpaceID:          b.SpaceID,
			Name:             b.Name,
			GroupTotal:       groupCnt[b.SpaceID],
			HumanMemberTotal: mt.Human,
			AgentTotal:       mt.Agent,
			HumanMsgCount:    agg.HumanMsg,
			AgentMsgCount:    agg.AgentMsg,
			LastActive:       agg.LastActive,
			IsActive:         isActive,
		})
	}

	sortSpaceItems(items, sortBy, order)

	total := int64(len(items))
	if offset >= len(items) {
		return []*spaceListItem{}, total, nil
	}
	end2 := offset + limit
	if end2 > len(items) {
		end2 = len(items)
	}
	return items[offset:end2], total, nil
}

func normalizeMessageComposition(rows []*messageCompositionItem) []*messageCompositionItem {
	byType := make(map[uint8]*messageCompositionItem, len(rows))
	for _, row := range rows {
		cp := *row
		cp.TotalMsgCount = cp.HumanMsgCount + cp.AgentMsgCount
		byType[cp.ConvType] = &cp
	}
	out := make([]*messageCompositionItem, 0, 4)
	for _, convType := range []uint8{convTypeHHGroup, convTypeHAGroup, convTypeHHPrivate, convTypeHAPrivate} {
		if row := byType[convType]; row != nil {
			out = append(out, row)
			continue
		}
		out = append(out, &messageCompositionItem{ConvType: convType})
	}
	return out
}

func stripPrivateCompositionTotals(humanMsg, agentMsg int64, rows []*messageCompositionItem) (int64, int64) {
	var privateHuman, privateAgent int64
	for _, row := range rows {
		if !isPrivateConvType(row.ConvType) {
			continue
		}
		privateHuman += row.HumanMsgCount
		privateAgent += row.AgentMsgCount
	}
	return subtractFloor(humanMsg, privateHuman), subtractFloor(agentMsg, privateAgent)
}

func zeroPrivateMessageComposition(rows []*messageCompositionItem) {
	for _, row := range rows {
		if !isPrivateConvType(row.ConvType) {
			continue
		}
		row.HumanMsgCount = 0
		row.AgentMsgCount = 0
		row.TotalMsgCount = 0
		row.ActiveChannelCount = 0
	}
}

func normalizeTrendConvTypes(rows map[uint8]trendConvTypeAgg) []*trendConvTypeMsgItem {
	out := make([]*trendConvTypeMsgItem, 0, 4)
	for _, convType := range []uint8{convTypeHHGroup, convTypeHAGroup, convTypeHHPrivate, convTypeHAPrivate} {
		row := rows[convType]
		out = append(out, &trendConvTypeMsgItem{
			ConvType:      convType,
			HumanMsgCount: row.HumanMsg,
			AgentMsgCount: row.AgentMsg,
			TotalMsgCount: row.HumanMsg + row.AgentMsg,
		})
	}
	return out
}

func stripPrivateTrendTotals(humanMsg, agentMsg int64, rows []*trendConvTypeMsgItem) (int64, int64) {
	var privateHuman, privateAgent int64
	for _, row := range rows {
		if !isPrivateConvType(row.ConvType) {
			continue
		}
		privateHuman += row.HumanMsgCount
		privateAgent += row.AgentMsgCount
	}
	return subtractFloor(humanMsg, privateHuman), subtractFloor(agentMsg, privateAgent)
}

func zeroPrivateTrendConvTypes(rows []*trendConvTypeMsgItem) {
	for _, row := range rows {
		if !isPrivateConvType(row.ConvType) {
			continue
		}
		row.HumanMsgCount = 0
		row.AgentMsgCount = 0
		row.TotalMsgCount = 0
	}
}

func isPrivateConvType(convType uint8) bool {
	return convType == convTypeHHPrivate || convType == convTypeHAPrivate
}

func subtractFloor(total, n int64) int64 {
	if n >= total {
		return 0
	}
	return total - n
}

type trendBucket struct {
	Bucket    string
	StartDate string
	EndDate   string
}

func buildTrendBuckets(start, end, granularity string) ([]trendBucket, error) {
	loc := reportLocation()
	const layout = "2006-01-02"
	startDate, err := time.ParseInLocation(layout, start, loc)
	if err != nil {
		return nil, err
	}
	endDate, err := time.ParseInLocation(layout, end, loc)
	if err != nil {
		return nil, err
	}
	if granularity == "week" {
		return buildWeekBuckets(startDate, endDate), nil
	}
	out := make([]trendBucket, 0, int(endDate.Sub(startDate)/(24*time.Hour))+1)
	for d := startDate; !d.After(endDate); d = d.AddDate(0, 0, 1) {
		day := d.Format(layout)
		out = append(out, trendBucket{Bucket: day, StartDate: day, EndDate: day})
	}
	return out, nil
}

func buildWeekBuckets(start, end time.Time) []trendBucket {
	const layout = "2006-01-02"
	weekStart := mondayOf(start)
	out := make([]trendBucket, 0)
	for d := weekStart; !d.After(end); d = d.AddDate(0, 0, 7) {
		bs := d
		if bs.Before(start) {
			bs = start
		}
		be := d.AddDate(0, 0, 6)
		if be.After(end) {
			be = end
		}
		out = append(out, trendBucket{
			Bucket:    d.Format(layout),
			StartDate: bs.Format(layout),
			EndDate:   be.Format(layout),
		})
	}
	return out
}

func mondayOf(d time.Time) time.Time {
	offset := (int(d.Weekday()) + 6) % 7 // Go: Sunday=0; desired Monday=0.
	return d.AddDate(0, 0, -offset)
}

// channelList 表二(仅群组)：SQL 侧 LEFT JOIN + 分页。
func (s *service) channelList(spaceID, start, end, activeStatus, sortBy, order string, offset, limit int) ([]*channelListItem, int64, error) {
	return s.db.queryChannelList(spaceID, start, end, activeStatus, sortBy, order, offset, limit)
}

// spaceExists 判断 Space 是否存在(用于表二 404)。
func (s *service) spaceExists(spaceID string) (bool, error) {
	return s.db.spaceExists(spaceID)
}

// directChatList 全局私聊活跃列表 + 解析双方展示名。
func (s *service) directChatList(start, end, sortBy, order string, offset, limit int) ([]*directChatItem, int64, error) {
	items, total, err := s.db.queryDirectChatList(start, end, sortBy, order, offset, limit)
	if err != nil {
		return nil, 0, err
	}
	uidSet := make(map[string]struct{}, len(items)*2)
	for _, it := range items {
		uidSet[it.MemberAUID] = struct{}{}
		uidSet[it.MemberBUID] = struct{}{}
	}
	uids := make([]string, 0, len(uidSet))
	for u := range uidSet {
		uids = append(uids, u)
	}
	names, err := s.db.queryMemberNames(uids)
	if err != nil {
		return nil, 0, err
	}
	for _, it := range items {
		it.MemberAName = names[it.MemberAUID]
		it.MemberBName = names[it.MemberBUID]
	}
	return items, total, nil
}

// spaceSortValue 取某列排序值(int64)。
func sortSpaceItems(items []*spaceListItem, sortBy, order string) {
	val := func(it *spaceListItem) int64 {
		switch sortBy {
		case "human_msg_count":
			return it.HumanMsgCount
		case "agent_msg_count":
			return it.AgentMsgCount
		case "total_msg":
			return it.HumanMsgCount + it.AgentMsgCount
		case "group_total":
			return it.GroupTotal
		case "human_member_total":
			return it.HumanMemberTotal
		default: // last_active
			return it.LastActive
		}
	}
	asc := order == "asc"
	sort.SliceStable(items, func(i, j int) bool {
		vi, vj := val(items[i]), val(items[j])
		if vi == vj {
			return items[i].SpaceID < items[j].SpaceID // 稳定次序
		}
		if asc {
			return vi < vj
		}
		return vi > vj
	})
}
