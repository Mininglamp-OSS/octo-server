package searchetl

import (
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
)

// etlDB 是 searchetl 的数据访问层（读 message 分片 + 独立游标表 octo_etl_es_cursor）。
//
// 阶段 1（本骨架）：只读——ensureCursor / readStablePrefix / dbNowUnix。游标推进（advanceCursor）
// 在阶段 2 接 Kafka、确认投递成功后才调用（事务拆分见 plan §3.5）；阶段 1 空跑不推进。
type etlDB struct {
	ctx     *config.Context
	session *dbr.Session
}

func newETLDB(ctx *config.Context) *etlDB {
	return &etlDB{ctx: ctx, session: ctx.DB()}
}

// messageTables 枚举全部 message 分片表（与 opanalytics/message 模块分片集一致）。
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

// ensureCursor 确保分片水位行存在（首次为 0），使后续 FOR UPDATE 总能命中行串行化。
func (d *etlDB) ensureCursor(table string) error {
	_, err := d.session.InsertBySql(
		"INSERT IGNORE INTO octo_etl_es_cursor (shard_table, last_id) VALUES (?, 0)", table).Exec()
	return err
}

// dbNowUnix 返回数据库当前时间（纪元秒），作为稳定性闸门统一时基（避免应用/DB 时钟偏差）。
func (d *etlDB) dbNowUnix() (int64, error) {
	var now int64
	err := d.session.SelectBySql("SELECT UNIX_TIMESTAMP()").LoadOne(&now)
	return now, err
}

// loadCursor 读取某分片当前水位（只读，不加锁；阶段 1 空跑用）。
func (d *etlDB) loadCursor(table string) (int64, error) {
	var cursor int64
	err := d.session.SelectBySql(
		"SELECT last_id FROM octo_etl_es_cursor WHERE shard_table=?", table).LoadOne(&cursor)
	return cursor, err
}

// maxID 返回某分片当前最大主键 id（用于积压量 max(id)-cursor 监控；阶段 1 空跑用）。
func (d *etlDB) maxID(table string) (int64, error) {
	var maxID int64
	// COALESCE 兜底空表返回 0。
	err := d.session.SelectBySql(
		fmt.Sprintf("SELECT COALESCE(MAX(id),0) FROM `%s`", table)).LoadOne(&maxID)
	return maxID, err
}

// readBatch 从某分片按 keyset 读一批（id>cursor 升序 LIMIT batch）。
//
// 阶段 1：不开事务、不加 FOR UPDATE（仅空跑读取，不推进游标）。源 SELECT 已含 message_id /
// payload 与稳定性闸门所需 created_unix——阶段 2 接 Kafka 时直接复用同一行结构，无需再改 SQL。
func (d *etlDB) readBatch(table string, cursor int64, batch int) ([]*srcMessageRow, error) {
	var rows []*srcMessageRow
	_, err := d.session.SelectBySql(
		fmt.Sprintf("SELECT id, message_id, from_uid, channel_id, channel_type, `timestamp`, "+
			"UNIX_TIMESTAMP(created_at) AS created_unix, payload "+
			"FROM `%s` WHERE id>? ORDER BY id ASC LIMIT ?", table),
		cursor, batch).Load(&rows)
	return rows, err
}
