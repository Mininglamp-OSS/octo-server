package opanalytics

import (
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
)

// upsertChunkRows 单条 upsert 语句最多带的行数，避免超大 SQL / 占位符过多。
const upsertChunkRows = 500

// sqlRunner 是 INSERT 语句的执行入口，*dbr.Session 与 *dbr.Tx 均满足，
// 使维表全刷(autocommit)与 chunk 累加(事务内)复用同一批 upsert 构造逻辑。
type sqlRunner interface {
	InsertBySql(query string, value ...interface{}) *dbr.InsertStmt
}

// etlDB 看板 ETL 的数据访问层(读源分片 + 维表 upsert + 事实表累加 + ④ 重算)。
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

// ===== 增量抽取：水位游标 + keyset 流式 chunk =====

// ensureCursor 确保分片水位行存在(首次为 0)，使 chunk 内 FOR UPDATE 总能命中行串行化。
func (d *etlDB) ensureCursor(table string) error {
	_, err := d.session.InsertBySql(
		"INSERT IGNORE INTO octo_etl_message_cursor (shard_table, last_id) VALUES (?, 0)", table).Exec()
	return err
}

// runChunk 处理某分片的一个抽取 chunk：单事务内 SELECT 水位 FOR UPDATE → keyset 读 batch 行
// → aggregate(纯函数) → 累加 ③ / 维表 / 脏日 → 推进水位。返回本 chunk 读到的行数。
//
// FOR UPDATE 锁住该分片水位行，串行化多实例(配合 Redis 锁双保险)；游标与 ③ 累加同事务
// 提交，保证每条消息精确一次(失败回滚则下次重读，不重复计入)。
func (d *etlDB) runChunk(table string, batch int, aggregate func([]*srcMessageRow) *chunkResult) (int64, error) {
	tx, err := d.session.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.RollbackUnlessCommitted()

	var cursor int64
	if err = tx.SelectBySql(
		"SELECT last_id FROM octo_etl_message_cursor WHERE shard_table=? FOR UPDATE", table).LoadOne(&cursor); err != nil {
		return 0, err
	}

	var rows []*srcMessageRow
	if _, err = tx.SelectBySql(
		fmt.Sprintf("SELECT id, from_uid, channel_id, channel_type, `timestamp` FROM `%s` WHERE id>? ORDER BY id ASC LIMIT ?", table),
		cursor, batch).Load(&rows); err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, tx.Commit()
	}

	res := aggregate(rows)
	if err = d.writeChunk(tx, res); err != nil {
		return 0, err
	}

	maxID := rows[len(rows)-1].ID
	if _, err = tx.UpdateBySql(
		"UPDATE octo_etl_message_cursor SET last_id=? WHERE shard_table=?", maxID, table).Exec(); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return int64(len(rows)), nil
}

// writeChunk 在事务内落库一个 chunk 的聚合结果：③ 累加 + 私聊维表 + 活跃时间 + 脏日入队。
func (d *etlDB) writeChunk(tx *dbr.Tx, res *chunkResult) error {
	if err := d.accumulateFact3(tx, res.fact3); err != nil {
		return err
	}
	if err := d.upsertDimChannelPrivate(tx, res.privateRows); err != nil {
		return err
	}
	if err := d.updateChannelActivity(tx, res.activityRows); err != nil {
		return err
	}
	return d.markDirtyDays(tx, res.dirtyDays)
}

// accumulateFact3 累加 upsert ③(msg_count += 本 chunk 增量；last_msg_at 单调增)。
func (d *etlDB) accumulateFact3(tx *dbr.Tx, fact3 []*factMemberChannelDailyModel) error {
	if len(fact3) == 0 {
		return nil
	}
	const cols = "(`stat_date`,`channel_id`,`channel_type`,`space_id`,`conv_type`,`content_type`," +
		"`sender_uid`,`sender_type`,`msg_count`,`last_msg_at`)"
	const suffix = " ON DUPLICATE KEY UPDATE " +
		"`channel_type`=VALUES(`channel_type`),`space_id`=VALUES(`space_id`),`conv_type`=VALUES(`conv_type`)," +
		"`sender_type`=VALUES(`sender_type`)," +
		"`msg_count`=`msg_count`+VALUES(`msg_count`)," +
		"`last_msg_at`=GREATEST(`last_msg_at`,VALUES(`last_msg_at`))"
	rows := make([][]interface{}, 0, len(fact3))
	for _, f := range fact3 {
		rows = append(rows, []interface{}{
			f.StatDate, f.ChannelID, f.ChannelType, f.SpaceID, f.ConvType, f.ContentType,
			f.SenderUID, f.SenderType, f.MsgCount, f.LastMsgAt,
		})
	}
	return execValuesUpsert(tx, "octo_fact_member_channel_daily", cols, 10, suffix, rows)
}

// markDirtyDays 把本 chunk 触达的统计日入队(待全部 chunk 后由 ③ 重算 ④)。
func (d *etlDB) markDirtyDays(tx *dbr.Tx, days []string) error {
	if len(days) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("INSERT IGNORE INTO octo_etl_dirty_day (stat_date) VALUES ")
	args := make([]interface{}, 0, len(days))
	for i, day := range days {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(?)")
		args = append(args, day)
	}
	_, err := tx.InsertBySql(sb.String(), args...).Exec()
	return err
}

// loadDirtyDays 取出全部待重算日(DATE_FORMAT 规避驱动 DATE↔string 解析差异)。
func (d *etlDB) loadDirtyDays() ([]string, error) {
	var days []string
	_, err := d.session.SelectBySql(
		"SELECT DATE_FORMAT(stat_date,'%Y-%m-%d') FROM octo_etl_dirty_day ORDER BY stat_date").Load(&days)
	return days, err
}

// recomputeChannelDay 由 ③ 重算某统计日的 ④，并出队该脏日(单事务，对③最终一致)。
func (d *etlDB) recomputeChannelDay(day string) error {
	tx, err := d.session.Begin()
	if err != nil {
		return err
	}
	defer tx.RollbackUnlessCommitted()

	if _, err = tx.DeleteBySql("DELETE FROM octo_fact_channel_daily WHERE stat_date=?", day).Exec(); err != nil {
		return err
	}
	if _, err = tx.InsertBySql(
		"INSERT INTO octo_fact_channel_daily "+
			"(stat_date,channel_id,channel_type,space_id,conv_type,human_msg_count,agent_msg_count,"+
			"active_human_members,active_agent_members,last_msg_at) "+
			"SELECT stat_date, channel_id, MAX(channel_type), MAX(space_id), MAX(conv_type), "+
			"SUM(CASE WHEN sender_type=1 THEN msg_count ELSE 0 END), "+
			"SUM(CASE WHEN sender_type=2 THEN msg_count ELSE 0 END), "+
			"COUNT(DISTINCT CASE WHEN sender_type=1 THEN sender_uid END), "+
			"COUNT(DISTINCT CASE WHEN sender_type=2 THEN sender_uid END), "+
			"MAX(last_msg_at) "+
			"FROM octo_fact_member_channel_daily WHERE stat_date=? GROUP BY stat_date, channel_id", day).Exec(); err != nil {
		return err
	}
	if _, err = tx.DeleteBySql("DELETE FROM octo_etl_dirty_day WHERE stat_date=?", day).Exec(); err != nil {
		return err
	}
	return tx.Commit()
}

// ===== 维表来源读取 =====

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

// queryGroupMemberCounts 按群统计在册(status=1)成员数及其中的 agent 数，剔除系统/测试账号。
// 成员类型优先取 dim_member.member_type，回退 group_member.robot。
// 排除口径：dim_member.is_excluded=1，再按单一真源结构性兜底剔除 SystemBots(防 dim 缺行)。
func (d *etlDB) queryGroupMemberCounts() ([]*groupMemberCountRow, error) {
	var rows []*groupMemberCountRow
	botClause, botArgs := systemBotExclusion("gm.uid")
	_, err := d.session.SelectBySql(
		"SELECT gm.group_no AS group_no, "+
			"SUM(CASE WHEN COALESCE(m.member_type, IF(gm.robot=1,2,1))=2 THEN 1 ELSE 0 END) AS agent_cnt, "+
			"COUNT(*) AS total_cnt "+
			"FROM `group_member` gm LEFT JOIN octo_dim_member m ON m.uid = gm.uid "+
			"WHERE gm.status=1 AND COALESCE(m.is_excluded,0)=0"+botClause+" GROUP BY gm.group_no",
		botArgs...,
	).Load(&rows)
	return rows, err
}

// ===== 维表 upsert =====

// upsertDimMembers 批量 upsert 成员维表(维表全刷可用 ODKU；禁令只针对会消失 key 的事实表)。
func (d *etlDB) upsertDimMembers(rows [][]interface{}) error {
	const cols = "(`uid`,`name`,`email`,`phone`,`zone`,`member_type`,`is_excluded`)"
	const suffix = " ON DUPLICATE KEY UPDATE " +
		"`name`=VALUES(`name`),`email`=VALUES(`email`),`phone`=VALUES(`phone`)," +
		"`zone`=VALUES(`zone`),`member_type`=VALUES(`member_type`),`is_excluded`=VALUES(`is_excluded`)"
	return execValuesUpsert(d.session, "octo_dim_member", cols, 7, suffix, rows)
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
	return execValuesUpsert(d.session, "octo_dim_channel", cols, 10, suffix, rows)
}

// upsertDimChannelPrivate 批量 upsert 私聊会话维表(space_id=” 不进空间维度)；chunk 内事务执行。
func (d *etlDB) upsertDimChannelPrivate(runner sqlRunner, rows [][]interface{}) error {
	const cols = "(`channel_id`,`channel_type`,`space_id`,`conv_type`,`name`," +
		"`member_a_uid`,`member_b_uid`,`member_count`,`human_member_count`,`agent_member_count`,`status`,`first_msg_at`)"
	const suffix = " ON DUPLICATE KEY UPDATE " +
		"`conv_type`=VALUES(`conv_type`),`member_a_uid`=VALUES(`member_a_uid`),`member_b_uid`=VALUES(`member_b_uid`)," +
		"`human_member_count`=VALUES(`human_member_count`),`agent_member_count`=VALUES(`agent_member_count`)," +
		"`first_msg_at`=LEAST(`first_msg_at`,VALUES(`first_msg_at`))"
	return execValuesUpsert(runner, "octo_dim_channel", cols, 12, suffix, rows)
}

// updateChannelActivity 单调更新会话最后活跃时间(GREATEST)；对消息里出现但维表缺失的
// 孤儿会话顺带插入最小行(channel_type 来自消息)。每行: [channel_id, channel_type, last_active_at]。
func (d *etlDB) updateChannelActivity(runner sqlRunner, rows [][]interface{}) error {
	const cols = "(`channel_id`,`channel_type`,`last_active_at`)"
	const suffix = " ON DUPLICATE KEY UPDATE `last_active_at`=GREATEST(`last_active_at`,VALUES(`last_active_at`))"
	return execValuesUpsert(runner, "octo_dim_channel", cols, 3, suffix, rows)
}

// execValuesUpsert 构造并分块执行 `INSERT INTO t cols VALUES (...),(...) <suffix>`。
func execValuesUpsert(runner sqlRunner, table, cols string, colCount int, suffix string, rows [][]interface{}) error {
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
		if _, err := runner.InsertBySql(sb.String(), args...).Exec(); err != nil {
			return err
		}
	}
	return nil
}
