package opanalytics

import (
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
)

// opanalyticsDB 看板读侧数据访问层(只查预聚合表 + space/group/dim 维表)。
type opanalyticsDB struct {
	ctx     *config.Context
	session *dbr.Session
}

func newOpanalyticsDB(ctx *config.Context) *opanalyticsDB {
	return &opanalyticsDB{ctx: ctx, session: ctx.DB()}
}

// ===== 概览(模块A) =====

func (d *opanalyticsDB) countSpacesTotal() (int64, error) {
	var n int64
	_, err := d.session.Select("count(*)").From("space").Where("status=1").Load(&n)
	return n, err
}

func (d *opanalyticsDB) countGroupsTotal() (int64, error) {
	var n int64
	_, err := d.session.Select("count(*)").From("`group`").Where("status=1").Load(&n)
	return n, err
}

// countMembersByType 全局 human/agent 成员总数(源 dim_member；P0 不按 is_excluded 过滤)。
func (d *opanalyticsDB) countMembersByType() (human int64, agent int64, err error) {
	var rows []struct {
		MemberType uint8 `db:"member_type"`
		Cnt        int64 `db:"cnt"`
	}
	_, err = d.session.SelectBySql(
		"SELECT member_type, COUNT(*) AS cnt FROM octo_dim_member GROUP BY member_type",
	).Load(&rows)
	if err != nil {
		return 0, 0, err
	}
	for _, r := range rows {
		if r.MemberType == memberTypeAgent {
			agent = r.Cnt
		} else {
			human = r.Cnt
		}
	}
	return human, agent, nil
}

// overviewMsgAndGroups 范围内人/agent 消息量与活跃群数(可选 space 过滤)。
func (d *opanalyticsDB) overviewMsgAndGroups(start, end string, spaceIDs []string) (humanMsg, agentMsg, activeGroups int64, err error) {
	var res struct {
		HumanMsg     int64 `db:"human_msg"`
		AgentMsg     int64 `db:"agent_msg"`
		ActiveGroups int64 `db:"active_groups"`
	}
	stmt := d.session.Select(
		"IFNULL(SUM(human_msg_count),0) AS human_msg",
		"IFNULL(SUM(agent_msg_count),0) AS agent_msg",
		"COUNT(DISTINCT CASE WHEN channel_type=2 THEN channel_id END) AS active_groups",
	).From("octo_fact_channel_daily").Where("stat_date BETWEEN ? AND ?", start, end)
	stmt = applySpaceFilter(stmt, spaceIDs)
	_, err = stmt.Load(&res)
	return res.HumanMsg, res.AgentMsg, res.ActiveGroups, err
}

// overviewActiveMembers 范围内活跃 human/agent 成员去重数(可选 space 过滤)。
func (d *opanalyticsDB) overviewActiveMembers(start, end string, spaceIDs []string) (human, agent int64, err error) {
	var res struct {
		ActiveHuman int64 `db:"active_human"`
		ActiveAgent int64 `db:"active_agent"`
	}
	stmt := d.session.Select(
		"COUNT(DISTINCT CASE WHEN sender_type=1 THEN sender_uid END) AS active_human",
		"COUNT(DISTINCT CASE WHEN sender_type=2 THEN sender_uid END) AS active_agent",
	).From("octo_fact_member_channel_daily").Where("stat_date BETWEEN ? AND ?", start, end)
	stmt = applySpaceFilter(stmt, spaceIDs)
	_, err = stmt.Load(&res)
	return res.ActiveHuman, res.ActiveAgent, err
}

// privateActiveCount 范围内活跃私聊数(口径1：只出活跃数；全局，永不按 space 过滤)。
func (d *opanalyticsDB) privateActiveCount(start, end string) (int64, error) {
	var n int64
	_, err := d.session.Select("COUNT(DISTINCT channel_id)").
		From("octo_fact_channel_daily").
		Where("channel_type=1 AND stat_date BETWEEN ? AND ?", start, end).
		Load(&n)
	return n, err
}

// ===== 表一 Space 列表(在内存合并/排序/分页，spaces 数量适中) =====

type spaceBaseRow struct {
	SpaceID string `db:"space_id"`
	Name    string `db:"name"`
}

func (d *opanalyticsDB) querySpaceBase(nameLike string) ([]*spaceBaseRow, error) {
	var rows []*spaceBaseRow
	stmt := d.session.Select("space_id", "name").From("space").Where("status=1")
	if nameLike != "" {
		stmt = stmt.Where("name LIKE ?", "%"+nameLike+"%")
	}
	_, err := stmt.Load(&rows)
	return rows, err
}

func (d *opanalyticsDB) queryGroupCountBySpace() (map[string]int64, error) {
	var rows []struct {
		SpaceID string `db:"space_id"`
		Cnt     int64  `db:"cnt"`
	}
	_, err := d.session.SelectBySql(
		"SELECT space_id, COUNT(*) AS cnt FROM `group` WHERE status=1 GROUP BY space_id",
	).Load(&rows)
	if err != nil {
		return nil, err
	}
	m := make(map[string]int64, len(rows))
	for _, r := range rows {
		m[r.SpaceID] = r.Cnt
	}
	return m, nil
}

type spaceMemberTotals struct {
	Human int64
	Agent int64
}

func (d *opanalyticsDB) queryMemberTotalsBySpace() (map[string]spaceMemberTotals, error) {
	var rows []struct {
		SpaceID string `db:"space_id"`
		Agent   int64  `db:"agent"`
		Total   int64  `db:"total"`
	}
	_, err := d.session.SelectBySql(
		"SELECT sm.space_id AS space_id, " +
			"SUM(CASE WHEN m.member_type=2 THEN 1 ELSE 0 END) AS agent, COUNT(*) AS total " +
			"FROM space_member sm LEFT JOIN octo_dim_member m ON m.uid = sm.uid " +
			"WHERE sm.status=1 GROUP BY sm.space_id",
	).Load(&rows)
	if err != nil {
		return nil, err
	}
	m := make(map[string]spaceMemberTotals, len(rows))
	for _, r := range rows {
		m[r.SpaceID] = spaceMemberTotals{Human: r.Total - r.Agent, Agent: r.Agent}
	}
	return m, nil
}

type spaceActiveAgg struct {
	HumanMsg   int64
	AgentMsg   int64
	LastActive int64
}

func (d *opanalyticsDB) queryActiveAggBySpace(start, end string) (map[string]spaceActiveAgg, error) {
	var rows []struct {
		SpaceID    string `db:"space_id"`
		HumanMsg   int64  `db:"human_msg"`
		AgentMsg   int64  `db:"agent_msg"`
		LastActive int64  `db:"last_active"`
	}
	_, err := d.session.Select(
		"space_id",
		"IFNULL(SUM(human_msg_count),0) AS human_msg",
		"IFNULL(SUM(agent_msg_count),0) AS agent_msg",
		"IFNULL(MAX(last_msg_at),0) AS last_active",
	).From("octo_fact_channel_daily").
		Where("stat_date BETWEEN ? AND ? AND channel_type=2 AND space_id<>''", start, end).
		GroupBy("space_id").Load(&rows)
	if err != nil {
		return nil, err
	}
	m := make(map[string]spaceActiveAgg, len(rows))
	for _, r := range rows {
		m[r.SpaceID] = spaceActiveAgg{HumanMsg: r.HumanMsg, AgentMsg: r.AgentMsg, LastActive: r.LastActive}
	}
	return m, nil
}

// ===== 表二 群组列表(SQL 侧 LEFT JOIN + 分页) =====

// channelSortColumns 排序白名单(防注入)。
var channelSortColumns = map[string]string{
	"human_msg_count": "human_msg",
	"agent_msg_count": "agent_msg",
	"total_msg":       "(human_msg+agent_msg)",
	"member_count":    "c.member_count",
	"last_active":     "c.last_active_at",
}

// queryChannelList 表二群组列表(仅 channel_type=2)。activeStatus ∈ {all,active,inactive}。
func (d *opanalyticsDB) queryChannelList(spaceID, start, end, activeStatus, sortBy, order string, offset, limit int) ([]*channelListItem, int64, error) {
	sortExpr, ok := channelSortColumns[sortBy]
	if !ok {
		sortExpr = "c.last_active_at"
	}
	dir := "DESC"
	if strings.EqualFold(order, "asc") {
		dir = "ASC"
	}
	activeCond := channelActiveCond(activeStatus)

	base := "FROM octo_dim_channel c " +
		"LEFT JOIN (SELECT channel_id, SUM(human_msg_count) AS hm, SUM(agent_msg_count) AS am " +
		"FROM octo_fact_channel_daily WHERE stat_date BETWEEN ? AND ? AND space_id=? AND channel_type=2 " +
		"GROUP BY channel_id) f ON f.channel_id=c.channel_id " +
		"WHERE c.space_id=? AND c.channel_type=2" + activeCond

	// 计数
	var total int64
	cntArgs := []interface{}{start, end, spaceID, spaceID}
	_, err := d.session.SelectBySql("SELECT COUNT(*) "+base, cntArgs...).Load(&total)
	if err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []*channelListItem{}, 0, nil
	}

	sel := "SELECT c.channel_id, c.name, c.conv_type, c.member_count, c.human_member_count, c.agent_member_count, " +
		"c.status, c.last_active_at, IFNULL(f.hm,0) AS human_msg, IFNULL(f.am,0) AS agent_msg, " +
		"(f.channel_id IS NOT NULL) AS is_active " + base +
		fmt.Sprintf(" ORDER BY %s %s, c.channel_id ASC LIMIT ? OFFSET ?", sortExpr, dir)
	listArgs := []interface{}{start, end, spaceID, spaceID, limit, offset}

	var rows []struct {
		ChannelID        string `db:"channel_id"`
		Name             string `db:"name"`
		ConvType         uint8  `db:"conv_type"`
		MemberCount      int    `db:"member_count"`
		HumanMemberCount int    `db:"human_member_count"`
		AgentMemberCount int    `db:"agent_member_count"`
		Status           uint8  `db:"status"`
		LastActiveAt     int64  `db:"last_active_at"`
		HumanMsg         int64  `db:"human_msg"`
		AgentMsg         int64  `db:"agent_msg"`
		IsActive         bool   `db:"is_active"`
	}
	if _, err = d.session.SelectBySql(sel, listArgs...).Load(&rows); err != nil {
		return nil, 0, err
	}
	out := make([]*channelListItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, &channelListItem{
			ChannelID: r.ChannelID, Name: r.Name, ConvType: r.ConvType,
			MemberCount: r.MemberCount, HumanMemberCount: r.HumanMemberCount, AgentMemberCount: r.AgentMemberCount,
			HumanMsgCount: r.HumanMsg, AgentMsgCount: r.AgentMsg,
			LastActiveAt: r.LastActiveAt, Status: r.Status, IsActive: r.IsActive,
		})
	}
	return out, total, nil
}

func (d *opanalyticsDB) spaceExists(spaceID string) (bool, error) {
	var n int64
	_, err := d.session.Select("count(*)").From("space").Where("space_id=?", spaceID).Load(&n)
	return n > 0, err
}

// ===== 全局私聊活跃列表(SQL 侧聚合 + 分页) =====

var directSortColumns = map[string]string{
	"msg_count":   "msg_count",
	"last_active": "last_active",
}

func (d *opanalyticsDB) queryDirectChatList(start, end, sortBy, order string, offset, limit int) ([]*directChatItem, int64, error) {
	sortExpr, ok := directSortColumns[sortBy]
	if !ok {
		sortExpr = "last_active"
	}
	dir := "DESC"
	if strings.EqualFold(order, "asc") {
		dir = "ASC"
	}

	var total int64
	_, err := d.session.Select("COUNT(DISTINCT channel_id)").
		From("octo_fact_channel_daily").
		Where("channel_type=1 AND stat_date BETWEEN ? AND ?", start, end).
		Load(&total)
	if err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []*directChatItem{}, 0, nil
	}

	sel := "SELECT f.channel_id AS channel_id, c.member_a_uid AS member_a_uid, c.member_b_uid AS member_b_uid, " +
		"c.conv_type AS conv_type, SUM(f.human_msg_count + f.agent_msg_count) AS msg_count, MAX(f.last_msg_at) AS last_active " +
		"FROM octo_fact_channel_daily f JOIN octo_dim_channel c ON c.channel_id=f.channel_id " +
		"WHERE f.channel_type=1 AND f.stat_date BETWEEN ? AND ? " +
		"GROUP BY f.channel_id, c.member_a_uid, c.member_b_uid, c.conv_type " +
		fmt.Sprintf("ORDER BY %s %s, f.channel_id ASC LIMIT ? OFFSET ?", sortExpr, dir)

	var rows []struct {
		ChannelID  string `db:"channel_id"`
		MemberAUID string `db:"member_a_uid"`
		MemberBUID string `db:"member_b_uid"`
		ConvType   uint8  `db:"conv_type"`
		MsgCount   int64  `db:"msg_count"`
		LastActive int64  `db:"last_active"`
	}
	if _, err = d.session.SelectBySql(sel, start, end, limit, offset).Load(&rows); err != nil {
		return nil, 0, err
	}

	out := make([]*directChatItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, &directChatItem{
			ChannelID: r.ChannelID, MemberAUID: r.MemberAUID, MemberBUID: r.MemberBUID,
			ConvType: r.ConvType, MsgCount: r.MsgCount, LastActive: r.LastActive,
		})
	}
	return out, total, nil
}

// queryMemberNames 批量取 uid→展示名(用于私聊"A & B"展示)。
func (d *opanalyticsDB) queryMemberNames(uids []string) (map[string]string, error) {
	if len(uids) == 0 {
		return map[string]string{}, nil
	}
	var rows []struct {
		UID  string `db:"uid"`
		Name string `db:"name"`
	}
	_, err := d.session.Select("uid", "name").From("octo_dim_member").Where("uid IN ?", uids).Load(&rows)
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		m[r.UID] = r.Name
	}
	return m, nil
}

// ===== 内部辅助 =====

func applySpaceFilter(stmt *dbr.SelectStmt, spaceIDs []string) *dbr.SelectStmt {
	if len(spaceIDs) > 0 {
		stmt = stmt.Where("space_id IN ?", spaceIDs)
	}
	return stmt
}

// channelActiveCond 返回拼接到 WHERE 的活跃过滤片段。
func channelActiveCond(activeStatus string) string {
	switch activeStatus {
	case "active":
		return " AND f.channel_id IS NOT NULL"
	case "inactive":
		return " AND f.channel_id IS NULL"
	default:
		return ""
	}
}
