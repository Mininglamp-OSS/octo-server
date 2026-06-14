package searchetl

import (
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
)

// etlDB 是 searchetl 的数据访问层（读 message 分片 + 独立游标表 octo_etl_es_cursor）。
//
// 阶段 2（事务拆分 C2）：读路径拆成两个**短**事务，中间留出事务外的 Kafka 投递窗口——
//   1. readStableBatchTx：短读事务（FOR UPDATE 读游标 + keyset 取批 → 立即 Commit 释锁），
//      **绝不持锁做 Kafka 网络 IO**；
//   2. （调用方在事务外整批投 Kafka 确认成功）；
//   3. advanceCursor：另开短事务把游标推进到稳定前缀末。
// 拆事务后失去 opanalytics 跨读-写-推进的 FOR UPDATE 行级第二防线，单副本互斥由 C3
// （阶段 3 的 Redis 锁全程持有 + 续租失败 abort）补回，本阶段先铺好结构。
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
// 阶段 1/空跑：不开事务、不加 FOR UPDATE（仅观测读取，不推进游标）。真实投递走
// readStableBatchTx（短读事务）。源 SELECT 已含 message_id / setting / signal / payload 与
// 稳定性闸门所需 created_unix——两条读路径复用同一行结构。
func (d *etlDB) readBatch(table string, cursor int64, batch int) ([]*srcMessageRow, error) {
	var rows []*srcMessageRow
	_, err := d.session.SelectBySql(
		fmt.Sprintf("SELECT id, message_id, from_uid, channel_id, channel_type, setting, `signal`, "+
			"`timestamp`, UNIX_TIMESTAMP(created_at) AS created_unix, payload "+
			"FROM `%s` WHERE id>? ORDER BY id ASC LIMIT ?", table),
		cursor, batch).Load(&rows)
	return rows, err
}

// readStableBatchTx 是阶段 2 真实投递的**短读事务**（C2 第一段）：单事务内
// FOR UPDATE 锁住该分片游标行 → keyset 读一批 → **立即 Commit 释锁**。
// 返回当前游标 + 读到的批（未截稳定前缀，稳定前缀截断由调用方在事务外用 stablePrefix 做）。
//
// 🔴 C2 硬约束：本方法只做「读游标 + 取批」，**绝不在事务内投 Kafka**——Kafka 网络 IO
// 由调用方在本事务 Commit 之后进行，避免持 DB 行锁期间阻塞在网络往返上放大锁竞争。
// FOR UPDATE 仍保留：它串行化「同一分片游标行」的并发读取（与 Redis 锁 C3 双保险），
// 但锁的持有时间被压缩到「一次本地 keyset 查询」，不含任何外部 IO。
func (d *etlDB) readStableBatchTx(table string, batch int) (cursor int64, rows []*srcMessageRow, err error) {
	tx, err := d.session.Begin()
	if err != nil {
		return 0, nil, err
	}
	defer tx.RollbackUnlessCommitted()

	if err = tx.SelectBySql(
		"SELECT last_id FROM octo_etl_es_cursor WHERE shard_table=? FOR UPDATE", table).LoadOne(&cursor); err != nil {
		return 0, nil, err
	}
	if _, err = tx.SelectBySql(
		fmt.Sprintf("SELECT id, message_id, from_uid, channel_id, channel_type, setting, `signal`, "+
			"`timestamp`, UNIX_TIMESTAMP(created_at) AS created_unix, payload "+
			"FROM `%s` WHERE id>? ORDER BY id ASC LIMIT ?", table),
		cursor, batch).Load(&rows); err != nil {
		return 0, nil, err
	}
	if err = tx.Commit(); err != nil {
		return 0, nil, err
	}
	return cursor, rows, nil
}

// advanceCursor 是阶段 2 真实投递的**短推进事务**（C2 第三段）：在 Kafka 整批投递确认
// 成功后，另开短事务把游标从 expected 推进到 newID。
//
// 用 `WHERE shard_table=? AND last_id=?(expected)` 做乐观校验：仅当游标仍停在本轮读到的
// 位置才推进，防止与其它实例/重入交错导致游标回退或跳跃（C3 之前的轻量护栏；C3 落地后
// Redis 锁保证单副本，此校验作为冗余防线保留）。返回是否实际推进了一行。
func (d *etlDB) advanceCursor(table string, expected, newID int64) (bool, error) {
	res, err := d.session.UpdateBySql(
		"UPDATE octo_etl_es_cursor SET last_id=? WHERE shard_table=? AND last_id=?",
		newID, table, expected).Exec()
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}
