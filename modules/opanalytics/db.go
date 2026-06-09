package opanalytics

import (
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
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

// countSpacesTotal 空间总数；给定 spaceIDs 时只数其中存在(status=1)者，使概览总数随筛选收敛。
func (d *opanalyticsDB) countSpacesTotal(spaceIDs []string) (int64, error) {
	var n int64
	stmt := d.session.Select("count(*)").From("space").Where("status=1")
	stmt = applySpaceFilter(stmt, spaceIDs)
	_, err := stmt.Load(&n)
	return n, err
}

// countGroupsTotal 群组总数；给定 spaceIDs 时只数其中的群。
func (d *opanalyticsDB) countGroupsTotal(spaceIDs []string) (int64, error) {
	var n int64
	stmt := d.session.Select("count(*)").From("`group`").Where("status=1")
	stmt = applySpaceFilter(stmt, spaceIDs)
	_, err := stmt.Load(&n)
	return n, err
}

// countMembersByType human/agent 成员总数。spaceIDs 为空=全局(源 dim_member)；非空=选中
// 空间在册成员去重(space_member ⋈ dim_member)，使总数随空间筛选收敛、活跃比例不失真。
// 两路都剔除 is_excluded(系统/测试/禁用)与 SystemBots。
func (d *opanalyticsDB) countMembersByType(spaceIDs []string) (human int64, agent int64, err error) {
	var rows []struct {
		MemberType uint8 `db:"member_type"`
		Cnt        int64 `db:"cnt"`
	}
	if len(spaceIDs) == 0 {
		_, err = d.session.SelectBySql(
			"SELECT member_type, COUNT(*) AS cnt FROM octo_dim_member WHERE is_excluded=0 GROUP BY member_type",
		).Load(&rows)
	} else {
		botClause, botArgs := systemBotExclusion("sm.uid")
		spaceClause, spaceArgs := inClause("sm.space_id", spaceIDs)
		args := append(botArgs, spaceArgs...)
		_, err = d.session.SelectBySql(
			"SELECT m.member_type AS member_type, COUNT(DISTINCT sm.uid) AS cnt "+
				"FROM space_member sm JOIN octo_dim_member m ON m.uid = sm.uid "+
				"WHERE sm.status=1 AND m.is_excluded=0"+botClause+" AND "+spaceClause+
				" GROUP BY m.member_type",
			args...,
		).Load(&rows)
	}
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
//
// 与总成数**同口径**，保证 active ⊆ total、率 ≤100%：
//   - 按**当前** dim_member.member_type 拆 human/agent(不用 ③ 里冻结的 sender_type，否则成员
//     由 human 转 agent 后会同时撑大 active_human 与 agent_total 而对不齐)。
//   - is_excluded=0 剔除系统/测试/禁用账号。
//   - 选中 Space 时再 JOIN space_member(status=1) 约束为该 Space 的**当前在册**成员
//     (否则"在群里发过言后退出空间"的人会计活跃却不计总数)。
//   - 消息量(volume)仍按 event-time 保留，与成员口径有意不同。
func (d *opanalyticsDB) overviewActiveMembers(start, end string, spaceIDs []string) (human, agent int64, err error) {
	var res struct {
		ActiveHuman int64 `db:"active_human"`
		ActiveAgent int64 `db:"active_agent"`
	}
	sql := "SELECT " +
		"COUNT(DISTINCT CASE WHEN m.member_type=1 THEN f.sender_uid END) AS active_human, " +
		"COUNT(DISTINCT CASE WHEN m.member_type=2 THEN f.sender_uid END) AS active_agent " +
		"FROM octo_fact_member_channel_daily f JOIN octo_dim_member m ON m.uid = f.sender_uid"
	var args []interface{}
	if len(spaceIDs) > 0 {
		smClause, smArgs := inClause("sm.space_id", spaceIDs)
		sql += " JOIN space_member sm ON sm.uid = f.sender_uid AND sm.status=1 AND " + smClause
		args = append(args, smArgs...)
	}
	sql += " WHERE f.stat_date BETWEEN ? AND ? AND m.is_excluded=0"
	args = append(args, start, end)
	if len(spaceIDs) > 0 {
		spaceClause, spaceArgs := inClause("f.space_id", spaceIDs)
		sql += " AND f.channel_type=2 AND " + spaceClause
		args = append(args, spaceArgs...)
	}
	_, err = d.session.SelectBySql(sql, args...).Load(&res)
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

// queryMessageComposition 返回模块B会话类型分布。固定 conv_type 补齐在 service 层完成。
func (d *opanalyticsDB) queryMessageComposition(start, end string, spaceIDs []string) ([]*messageCompositionItem, error) {
	var rows []*messageCompositionItem
	stmt := d.session.Select(
		"conv_type",
		"IFNULL(SUM(human_msg_count),0) AS human_msg_count",
		"IFNULL(SUM(agent_msg_count),0) AS agent_msg_count",
		"COUNT(DISTINCT channel_id) AS active_channel_count",
	).From("octo_fact_channel_daily").
		Where("stat_date BETWEEN ? AND ?", start, end).
		Where("conv_type BETWEEN 1 AND 4").
		GroupBy("conv_type")
	stmt = applySpaceFilter(stmt, spaceIDs)
	_, err := stmt.Load(&rows)
	return rows, err
}

type trendChannelAgg struct {
	HumanMsg      int64
	AgentMsg      int64
	ActiveGroups  int64
	PrivateActive int64
}

type trendMemberAgg struct {
	ActiveHuman int64
	ActiveAgent int64
}

type trendConvTypeAgg struct {
	HumanMsg int64
	AgentMsg int64
}

func (d *opanalyticsDB) queryTrendChannelAgg(start, end, granularity string, spaceIDs []string) (map[string]trendChannelAgg, map[string]map[uint8]trendConvTypeAgg, error) {
	total, err := d.queryTrendChannelTotals(start, end, granularity, spaceIDs)
	if err != nil {
		return nil, nil, err
	}
	conv, err := d.queryTrendConvTypeMsg(start, end, granularity, spaceIDs)
	if err != nil {
		return nil, nil, err
	}
	return total, conv, nil
}

func (d *opanalyticsDB) queryTrendChannelTotals(start, end, granularity string, spaceIDs []string) (map[string]trendChannelAgg, error) {
	bucketExpr := trendBucketSQL("stat_date", granularity)
	sql := "SELECT " + bucketExpr + " AS bucket, " +
		"IFNULL(SUM(human_msg_count),0) AS human_msg, " +
		"IFNULL(SUM(agent_msg_count),0) AS agent_msg, " +
		"COUNT(DISTINCT CASE WHEN channel_type=2 THEN channel_id END) AS active_groups, " +
		"COUNT(DISTINCT CASE WHEN channel_type=1 THEN channel_id END) AS private_active " +
		"FROM octo_fact_channel_daily WHERE stat_date BETWEEN ? AND ?"
	args := []interface{}{start, end}
	if len(spaceIDs) > 0 {
		spaceClause, spaceArgs := inClause("space_id", spaceIDs)
		sql += " AND " + spaceClause
		args = append(args, spaceArgs...)
	}
	sql += " GROUP BY bucket"

	var rows []struct {
		Bucket        string `db:"bucket"`
		HumanMsg      int64  `db:"human_msg"`
		AgentMsg      int64  `db:"agent_msg"`
		ActiveGroups  int64  `db:"active_groups"`
		PrivateActive int64  `db:"private_active"`
	}
	if _, err := d.session.SelectBySql(sql, args...).Load(&rows); err != nil {
		return nil, err
	}
	out := make(map[string]trendChannelAgg, len(rows))
	for _, r := range rows {
		out[r.Bucket] = trendChannelAgg{
			HumanMsg: r.HumanMsg, AgentMsg: r.AgentMsg,
			ActiveGroups: r.ActiveGroups, PrivateActive: r.PrivateActive,
		}
	}
	return out, nil
}

func (d *opanalyticsDB) queryTrendConvTypeMsg(start, end, granularity string, spaceIDs []string) (map[string]map[uint8]trendConvTypeAgg, error) {
	bucketExpr := trendBucketSQL("stat_date", granularity)
	sql := "SELECT " + bucketExpr + " AS bucket, conv_type, " +
		"IFNULL(SUM(human_msg_count),0) AS human_msg, " +
		"IFNULL(SUM(agent_msg_count),0) AS agent_msg " +
		"FROM octo_fact_channel_daily WHERE stat_date BETWEEN ? AND ? AND conv_type BETWEEN 1 AND 4"
	args := []interface{}{start, end}
	if len(spaceIDs) > 0 {
		spaceClause, spaceArgs := inClause("space_id", spaceIDs)
		sql += " AND " + spaceClause
		args = append(args, spaceArgs...)
	}
	sql += " GROUP BY bucket, conv_type"

	var rows []struct {
		Bucket   string `db:"bucket"`
		ConvType uint8  `db:"conv_type"`
		HumanMsg int64  `db:"human_msg"`
		AgentMsg int64  `db:"agent_msg"`
	}
	if _, err := d.session.SelectBySql(sql, args...).Load(&rows); err != nil {
		return nil, err
	}
	out := make(map[string]map[uint8]trendConvTypeAgg, len(rows))
	for _, r := range rows {
		if out[r.Bucket] == nil {
			out[r.Bucket] = map[uint8]trendConvTypeAgg{}
		}
		out[r.Bucket][r.ConvType] = trendConvTypeAgg{HumanMsg: r.HumanMsg, AgentMsg: r.AgentMsg}
	}
	return out, nil
}

func (d *opanalyticsDB) queryTrendActiveMembers(start, end, granularity string, spaceIDs []string) (map[string]trendMemberAgg, error) {
	bucketExpr := trendBucketSQL("f.stat_date", granularity)
	sql := "SELECT " + bucketExpr + " AS bucket, " +
		"COUNT(DISTINCT CASE WHEN m.member_type=1 THEN f.sender_uid END) AS active_human, " +
		"COUNT(DISTINCT CASE WHEN m.member_type=2 THEN f.sender_uid END) AS active_agent " +
		"FROM octo_fact_member_channel_daily f JOIN octo_dim_member m ON m.uid = f.sender_uid"
	args := []interface{}{}
	if len(spaceIDs) > 0 {
		smClause, smArgs := inClause("sm.space_id", spaceIDs)
		sql += " JOIN space_member sm ON sm.uid = f.sender_uid AND sm.status=1 AND " + smClause
		args = append(args, smArgs...)
	}
	sql += " WHERE f.stat_date BETWEEN ? AND ? AND m.is_excluded=0"
	args = append(args, start, end)
	if len(spaceIDs) > 0 {
		spaceClause, spaceArgs := inClause("f.space_id", spaceIDs)
		sql += " AND f.channel_type=2 AND " + spaceClause
		args = append(args, spaceArgs...)
	}
	sql += " GROUP BY bucket"

	var rows []struct {
		Bucket      string `db:"bucket"`
		ActiveHuman int64  `db:"active_human"`
		ActiveAgent int64  `db:"active_agent"`
	}
	if _, err := d.session.SelectBySql(sql, args...).Load(&rows); err != nil {
		return nil, err
	}
	out := make(map[string]trendMemberAgg, len(rows))
	for _, r := range rows {
		out[r.Bucket] = trendMemberAgg{ActiveHuman: r.ActiveHuman, ActiveAgent: r.ActiveAgent}
	}
	return out, nil
}

func trendBucketSQL(col, granularity string) string {
	// SECURITY: col is an internal constant from call sites, and granularity is normalized
	// to the closed enum day/week before service dispatch; no raw user input reaches this SQL fragment.
	if granularity == "week" {
		return "DATE_FORMAT(DATE_SUB(" + col + ", INTERVAL WEEKDAY(" + col + ") DAY), '%Y-%m-%d')"
	}
	return "DATE_FORMAT(" + col + ", '%Y-%m-%d')"
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
		// 转义 LIKE 通配符，'!' 作转义符：用户输入里的 % _ 当字面量匹配，不当通配(也避免病态全表扫)。
		stmt = stmt.Where("name LIKE ? ESCAPE '!'", "%"+escapeLike(nameLike)+"%")
	}
	_, err := stmt.Load(&rows)
	return rows, err
}

// escapeLike 转义 LIKE 模式中的通配符/转义符，配合 `ESCAPE '!'` 使用。
func escapeLike(s string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(s)
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

// queryMemberTotalsBySpace 表一每个 Space 的在册 human/agent 成员数。
// 用 INNER JOIN dim_member(而非 LEFT JOIN+COALESCE)：孤儿 space_member(user 已删、dim 无行)
// 不计入，与概览 Space 路径 countMembersByType 口径完全一致(同一"成员总数"两接口不打架)。
// 一个 uid 在一个 space 至多一行，故 COUNT(*) 即去重数。
func (d *opanalyticsDB) queryMemberTotalsBySpace() (map[string]spaceMemberTotals, error) {
	var rows []struct {
		SpaceID string `db:"space_id"`
		Agent   int64  `db:"agent"`
		Total   int64  `db:"total"`
	}
	botClause, botArgs := systemBotExclusion("sm.uid")
	_, err := d.session.SelectBySql(
		"SELECT sm.space_id AS space_id, "+
			"SUM(CASE WHEN m.member_type=2 THEN 1 ELSE 0 END) AS agent, COUNT(*) AS total "+
			"FROM space_member sm JOIN octo_dim_member m ON m.uid = sm.uid "+
			"WHERE sm.status=1 AND m.is_excluded=0"+botClause+" GROUP BY sm.space_id",
		botArgs...,
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

	// INNER JOIN 活的 group 表(status=1)：硬删除的群(无 group 行)与已解散群(status≠1)不再展示。
	// dim_channel 只 upsert 不删旧群行，故权威存在性/状态以 group 表为准。
	base := "FROM octo_dim_channel c " +
		"JOIN `group` g ON g.group_no = c.channel_id AND g.status=1 " +
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

// spaceExists 仅在 Space 存在**且 status=1** 时返回 true：与 /spaces 列表口径一致，
// 软删除的 Space 视为不存在(表二返回 404，而非 200+数据)。
func (d *opanalyticsDB) spaceExists(spaceID string) (bool, error) {
	var n int64
	_, err := d.session.Select("count(*)").From("space").Where("space_id=? AND status=1", spaceID).Load(&n)
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

// systemBotExclusion 返回 "AND <col> NOT IN (?,?...)" 片段与对应参数，按单一真源
// pkg/space.SystemBots(botfather/u_10000/fileHelper/notification) 在成员计数处结构性
// 剔除系统账号 —— 兜底"系统bot是 space/group 成员但无 user/dim 行致 COALESCE 漏算"的场景。
func systemBotExclusion(col string) (string, []interface{}) {
	bots := spacepkg.SystemBotList()
	ph := strings.TrimSuffix(strings.Repeat("?,", len(bots)), ",")
	args := make([]interface{}, len(bots))
	for i, b := range bots {
		args[i] = b
	}
	return " AND " + col + " NOT IN (" + ph + ")", args
}

// inClause 构造 "<col> IN (?,?...)" 片段与参数(用于 SelectBySql 裸 SQL；调用方需保证非空)。
func inClause(col string, vals []string) (string, []interface{}) {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(vals)), ",")
	args := make([]interface{}, len(vals))
	for i, v := range vals {
		args[i] = v
	}
	return col + " IN (" + ph + ")", args
}

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
