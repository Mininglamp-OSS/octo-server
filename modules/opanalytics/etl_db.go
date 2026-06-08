package opanalytics

import (
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/gocraft/dbr/v2"
)

// upsertChunkRows 单条 upsert 语句最多带的行数，避免超大 SQL / 占位符过多。
const upsertChunkRows = 500

// etlDB 看板 ETL 的数据访问层(读源分片 + 维表 upsert + 事实表幂等替换)。
type etlDB struct {
	ctx     *config.Context
	session *dbr.Session
}

func newETLDB(ctx *config.Context) *etlDB {
	return &etlDB{ctx: ctx, session: ctx.DB()}
}

// messageTables 枚举全部消息分片表(与 modules/message/db.go getTable 的分片集一致)。
func (d *etlDB) messageTables() []string {
	count := d.ctx.GetConfig().TablePartitionConfig.MessageTableCount
	if count <= 0 {
		return []string{"message"}
	}
	tables := make([]string, 0, count)
	tables = append(tables, "message")
	for i := 1; i < count; i++ {
		tables = append(tables, fmt.Sprintf("message%d", i))
	}
	return tables
}

// queryDayMessages 抽取某分片表中 [start,end) 纪元秒窗口内的消息(全量：不过滤 is_deleted/类型)。
func (d *etlDB) queryDayMessages(table string, start, end int64) ([]*srcMessageRow, error) {
	var rows []*srcMessageRow
	_, err := d.session.
		Select("from_uid", "channel_id", "channel_type", "timestamp", "`signal`").
		From(table).
		Where("timestamp >= ? AND timestamp < ?", start, end).
		Load(&rows)
	return rows, err
}

// queryUsersForDim 读 user 表(在册)用于刷新成员维表；robot=1 即 agent。
func (d *etlDB) queryUsersForDim() ([]*userDimRow, error) {
	var rows []*userDimRow
	_, err := d.session.
		Select("uid", "name", "email", "phone", "zone", "robot", "category").
		From("`user`").
		Where("status=1").
		Load(&rows)
	return rows, err
}

// queryGroupsForDim 读 group 表用于刷新会话维表(群)。
func (d *etlDB) queryGroupsForDim() ([]*groupDimRow, error) {
	var rows []*groupDimRow
	_, err := d.session.
		Select("group_no", "name", "space_id", "status", "IFNULL(UNIX_TIMESTAMP(created_at),0) AS created_at_sec").
		From("`group`").
		Load(&rows)
	return rows, err
}

// queryGroupMemberCounts 按群统计在册(status=1)成员数及其中的 agent 数。
// 成员类型优先取 dim_member.member_type，回退 group_member.robot。
func (d *etlDB) queryGroupMemberCounts() ([]*groupMemberCountRow, error) {
	var rows []*groupMemberCountRow
	_, err := d.session.SelectBySql(
		"SELECT gm.group_no AS group_no, " +
			"SUM(CASE WHEN COALESCE(m.member_type, IF(gm.robot=1,2,1))=2 THEN 1 ELSE 0 END) AS agent_cnt, " +
			"COUNT(*) AS total_cnt " +
			"FROM `group_member` gm LEFT JOIN octo_dim_member m ON m.uid = gm.uid " +
			"WHERE gm.status=1 GROUP BY gm.group_no",
	).Load(&rows)
	return rows, err
}

// upsertDimMembers 批量 upsert 成员维表(维表全刷可用 ODKU；禁令只针对会消失 key 的事实表)。
func (d *etlDB) upsertDimMembers(rows [][]interface{}) error {
	const cols = "(`uid`,`name`,`email`,`phone`,`zone`,`member_type`,`is_excluded`)"
	const suffix = " ON DUPLICATE KEY UPDATE " +
		"`name`=VALUES(`name`),`email`=VALUES(`email`),`phone`=VALUES(`phone`)," +
		"`zone`=VALUES(`zone`),`member_type`=VALUES(`member_type`),`is_excluded`=VALUES(`is_excluded`)"
	return d.execValuesUpsert("octo_dim_member", cols, 7, suffix, rows)
}

// upsertDimChannelGroups 批量 upsert 群会话维表。不触碰 last_active_at(由消息活跃单调更新)。
// first_msg_at 取 LEAST 保持单调；群的 created_at 稳定，LEAST 等价。
func (d *etlDB) upsertDimChannelGroups(rows [][]interface{}) error {
	const cols = "(`channel_id`,`channel_type`,`space_id`,`conv_type`,`name`," +
		"`member_count`,`human_member_count`,`agent_member_count`,`status`,`first_msg_at`)"
	const suffix = " ON DUPLICATE KEY UPDATE " +
		"`channel_type`=VALUES(`channel_type`),`space_id`=VALUES(`space_id`),`conv_type`=VALUES(`conv_type`)," +
		"`name`=VALUES(`name`),`member_count`=VALUES(`member_count`)," +
		"`human_member_count`=VALUES(`human_member_count`),`agent_member_count`=VALUES(`agent_member_count`)," +
		"`status`=VALUES(`status`),`first_msg_at`=LEAST(`first_msg_at`,VALUES(`first_msg_at`))"
	return d.execValuesUpsert("octo_dim_channel", cols, 10, suffix, rows)
}

// upsertDimChannelPrivate 批量 upsert 私聊会话维表(space_id=” 不进空间维度)。
func (d *etlDB) upsertDimChannelPrivate(rows [][]interface{}) error {
	const cols = "(`channel_id`,`channel_type`,`space_id`,`conv_type`,`name`," +
		"`member_a_uid`,`member_b_uid`,`member_count`,`human_member_count`,`agent_member_count`,`status`,`first_msg_at`)"
	const suffix = " ON DUPLICATE KEY UPDATE " +
		"`conv_type`=VALUES(`conv_type`),`member_a_uid`=VALUES(`member_a_uid`),`member_b_uid`=VALUES(`member_b_uid`)," +
		"`human_member_count`=VALUES(`human_member_count`),`agent_member_count`=VALUES(`agent_member_count`)," +
		"`first_msg_at`=LEAST(`first_msg_at`,VALUES(`first_msg_at`))"
	return d.execValuesUpsert("octo_dim_channel", cols, 12, suffix, rows)
}

// updateChannelActivity 单调更新会话最后活跃时间(GREATEST)；对消息里出现但维表缺失的
// 孤儿会话顺带插入最小行(channel_type 来自消息)。每行: [channel_id, channel_type, last_active_at]。
func (d *etlDB) updateChannelActivity(rows [][]interface{}) error {
	const cols = "(`channel_id`,`channel_type`,`last_active_at`)"
	const suffix = " ON DUPLICATE KEY UPDATE `last_active_at`=GREATEST(`last_active_at`,VALUES(`last_active_at`))"
	return d.execValuesUpsert("octo_dim_channel", cols, 3, suffix, rows)
}

// replaceFactDay 幂等替换某统计日的两张事实表：单事务内 DELETE WHERE stat_date=X + 批量 INSERT。
// 禁用 ODKU(事实表重算后消失的 key 会留脏行)。
func (d *etlDB) replaceFactDay(statDate string, fact3 []*factMemberChannelDailyModel, fact4 []*factChannelDailyModel) error {
	tx, err := d.session.Begin()
	if err != nil {
		return err
	}
	defer tx.RollbackUnlessCommitted()

	if _, err = tx.DeleteFrom("octo_fact_member_channel_daily").Where("stat_date=?", statDate).Exec(); err != nil {
		return err
	}
	if _, err = tx.DeleteFrom("octo_fact_channel_daily").Where("stat_date=?", statDate).Exec(); err != nil {
		return err
	}

	cols3 := util.AttrToUnderscore(&factMemberChannelDailyModel{})
	for i := 0; i < len(fact3); i += upsertChunkRows {
		end := i + upsertChunkRows
		if end > len(fact3) {
			end = len(fact3)
		}
		ins := tx.InsertInto("octo_fact_member_channel_daily").Columns(cols3...)
		for _, r := range fact3[i:end] {
			ins.Record(r)
		}
		if _, err = ins.Exec(); err != nil {
			return err
		}
	}

	cols4 := util.AttrToUnderscore(&factChannelDailyModel{})
	for i := 0; i < len(fact4); i += upsertChunkRows {
		end := i + upsertChunkRows
		if end > len(fact4) {
			end = len(fact4)
		}
		ins := tx.InsertInto("octo_fact_channel_daily").Columns(cols4...)
		for _, r := range fact4[i:end] {
			ins.Record(r)
		}
		if _, err = ins.Exec(); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// execValuesUpsert 构造并分块执行 `INSERT INTO t cols VALUES (...),(...) <suffix>`。
func (d *etlDB) execValuesUpsert(table, cols string, colCount int, suffix string, rows [][]interface{}) error {
	if len(rows) == 0 {
		return nil
	}
	placeholder := "(" + strings.TrimSuffix(strings.Repeat("?,", colCount), ",") + ")"
	for i := 0; i < len(rows); i += upsertChunkRows {
		end := i + upsertChunkRows
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[i:end]
		var sb strings.Builder
		sb.WriteString("INSERT INTO ")
		sb.WriteString(table)
		sb.WriteString(" ")
		sb.WriteString(cols)
		sb.WriteString(" VALUES ")
		args := make([]interface{}, 0, len(chunk)*colCount)
		for j, row := range chunk {
			if j > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(placeholder)
			args = append(args, row...)
		}
		sb.WriteString(suffix)
		if _, err := d.session.InsertBySql(sb.String(), args...).Exec(); err != nil {
			return err
		}
	}
	return nil
}
