package opanalytics

import (
	"sort"

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
	}, nil
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
