package searchetl

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

// ETL 是 searchetl 消息检索增量抽取器（YUJ-4530，克隆 opanalytics 游标范式）。
//
// 目标架构：读 message 5 分表 → 投 Kafka topic octo.message.v1 → es-indexer 消费 → OpenSearch。
// 撤回/删除走读时查询侧 join（路线甲），producer 只跑正文一条流。
//
// 🚧 阶段 1（本骨架）：只做「空跑游标」——遍历分片、读稳定前缀、记录将会推进到的水位，
// 但**不接 Kafka、不推进游标**。事务拆分（读事务→事务外投 Kafka→推进事务）+ 单副本重入
// 互斥 + 整 chunk 原子重投在阶段 2/3 落地（plan §3.5）。稳定性闸门（C1）已在此就位。
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
		Log:   log.NewTLog("SearchETL"),
		ctx:   ctx,
		db:    newETLDB(ctx),
		batch: batchSize(),
		lag:   lagSeconds(),
	}
}

// RunIncrementalDryRun 跑一轮「空跑游标」（阶段 1）：逐分片读稳定前缀、统计稳定行数与积压，
// **不投 Kafka、不推进游标**。用于骨架自检与上线前观察源读取/稳定性闸门是否符合预期。
//
// 阶段 2 将以此为基替换为真实 RunIncremental：读事务取稳定前缀 → 事务外整批投 Kafka 确认 →
// 短事务推进游标到稳定前缀末（at-least-once + ES _id 幂等 sink = sink 处 effectively-once）。
func (e *ETL) RunIncrementalDryRun() error {
	nowUnix, err := e.db.dbNowUnix()
	if err != nil {
		return err
	}
	cutoff := nowUnix - e.lag

	var totalStable, totalBacklog int64
	for _, table := range e.db.messageTables() {
		if err = e.db.ensureCursor(table); err != nil {
			return err
		}
		cursor, lerr := e.db.loadCursor(table)
		if lerr != nil {
			return lerr
		}
		maxID, merr := e.db.maxID(table)
		if merr != nil {
			return merr
		}
		rows, rerr := e.db.readBatch(table, cursor, e.batch)
		if rerr != nil {
			return rerr
		}
		stable := stablePrefix(rows, cutoff)
		totalStable += int64(len(stable))
		if maxID > cursor {
			totalBacklog += maxID - cursor
		}
		e.Debug("searchetl dry-run shard scanned",
			zap.String("table", table),
			zap.Int64("cursor", cursor),
			zap.Int64("max_id", maxID),
			zap.Int("read", len(rows)),
			zap.Int("stable", len(stable)))
	}

	e.Info("searchetl incremental dry-run done (no Kafka, no cursor advance)",
		zap.Int64("stable_rows", totalStable),
		zap.Int64("backlog_ids", totalBacklog),
		zap.Int64("lag_seconds", e.lag))
	return nil
}
