package searchetl

import (
	"context"
	"sync/atomic"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/contract/searchmsg"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

// chunkStore 抽象 chunk 的 DB 读/推进（由 *etlDB 实现），便于 runChunk 单测注入假实现，
// 在不连真实 MySQL 的前提下验证事务边界与游标推进语义（C2/C3 护栏）。
type chunkStore interface {
	readStableBatchTx(table string, batch int) (cursor int64, rows []*srcMessageRow, err error)
	advanceCursor(table string, expected, newID int64) (bool, error)
}

// chunkSink 抽象 Kafka 投递（由 *producer 实现），便于单测注入假实现验证：
// 整批投递在 DB 读事务之外发生、任一失败整 chunk 不推进游标（C2）。
type chunkSink interface {
	produceBatch(ctx context.Context, msgs []searchmsg.Message) error
	produceDLQ(ctx context.Context, msgs []searchmsg.Message) error
}

// ETL 是 searchetl 消息检索增量抽取器（YUJ-4530，克隆 opanalytics 游标范式）。
//
// 目标架构：读 message 5 分表 → 投 Kafka topic octo.message.v1 → es-indexer 消费 → OpenSearch。
// 撤回/删除走读时查询侧 join（路线甲），producer 只跑正文一条流。
//
// 阶段 2（本次）：接 Kafka producer + 事务拆分（C2）+ payload 抽取（P1-d）。RunIncremental
// 走「短读事务取批 → 事务外整批投 Kafka 确认 → 短事务推进游标到稳定前缀末」，并带**进程内**
// 重入护栏（running CAS）挡住同进程并发重复投递。跨副本互斥（C3：Redis 锁全程持有 + 续租失败
// abort）与 scheduler 接入留阶段 3——本阶段不自动启 tick，Kafka.On=off 时连 producer 都不
// 构造（保持阶段 1 惰性，部署测试环境零运行期变更）。
type ETL struct {
	log.Log
	ctx   *config.Context
	db    *etlDB
	batch int
	lag   int64
	// running 是**进程内**重入护栏（CAS 0→1）：拆事务后单 chunk 内不再有跨读-投-推进的
	// FOR UPDATE 第二防线，若同进程并发跑两轮 RunIncremental，会在同批 readStableBatchTx 后
	// 各自向 Kafka 重复投递（ES _id upsert 去重故不丢/不脏，但浪费且无谓放大）。本护栏挡住
	// 同进程并发；**跨进程/多副本**互斥仍由阶段 3 的 Redis 锁（全程持有 + 续租失败 abort）负责。
	running atomic.Bool
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

// RunIncremental 跑一轮真实增量抽取（阶段 2）：逐分片以事务拆分（C2）投递正文到 Kafka。
//
// 前置：仅当 Kafka.On 时才有意义——off 时直接返回（不连 Kafka、不推进游标，保持惰性）。
// 单实例互斥（C3）由调用方（阶段 3 的 scheduler，持 Redis 锁 + 续租）保证；本方法本身不抢锁，
// 当前仅供测试/未来 scheduler 调用，未挂自动 tick。
//
// 每分片循环 runChunk 直到触达未稳定尾部（稳定前缀 < batch）。任一 chunk 投递失败即整轮
// 返回 error、该分片游标不推进，下轮整批重投（message_id 幂等 + ES _id upsert 去重，C2）。
func (e *ETL) RunIncremental(ctx context.Context) error {
	cfg := e.ctx.GetConfig()
	if !cfg.Kafka.On {
		e.Info("searchetl: Kafka.On=false, skip incremental (lazy, no producer)")
		return nil
	}

	// 进程内重入护栏：已有一轮在跑则直接跳过本轮（不报错，等下个 tick）。
	// 跨副本互斥由阶段 3 Redis 锁补上；这里挡住同进程并发重复投递。
	if !e.running.CompareAndSwap(false, true) {
		e.Info("searchetl: another incremental run in progress (same process), skip")
		return nil
	}
	defer e.running.Store(false)

	prod := newProducer(cfg)
	defer func() {
		if cerr := prod.Close(); cerr != nil {
			e.Error("searchetl: close producer failed", zap.Error(cerr))
		}
	}()

	nowUnix, err := e.db.dbNowUnix()
	if err != nil {
		return err
	}
	cutoff := nowUnix - e.lag

	var totalMain, totalDLQ int64
	for _, table := range e.db.messageTables() {
		if err = e.db.ensureCursor(table); err != nil {
			return err
		}
		for {
			plan, n, cerr := runChunk(ctx, e.db, prod, table, cutoff, e.batch, e.Log)
			if cerr != nil {
				return cerr
			}
			totalMain += int64(len(plan.main))
			totalDLQ += int64(len(plan.dlq))
			// 触达未稳定尾部（稳定前缀不足一整批）→ 本分片本轮结束。
			if n < e.batch {
				break
			}
		}
	}

	e.Info("searchetl incremental done",
		zap.Int64("main_produced", totalMain),
		zap.Int64("dlq_produced", totalDLQ),
		zap.Int64("lag_seconds", e.lag))
	return nil
}

// runChunk 处理某分片一个 chunk（C2 三段事务边界）：
//  1. 短读事务取游标 + 一批源行（readStableBatchTx，立即释锁，事务内无 Kafka IO）；
//  2. 事务外做稳定性闸门截断 + payload 抽取（planChunk，纯计算）；
//  3. 事务外整批投 Kafka（main + DLQ 全部确认成功）；
//  4. 全部确认后另开短事务把游标推进到稳定前缀末（advanceCursor）。
//
// 返回稳定前缀行数（用于「是否触达未稳定尾部」判定）。任一步失败即返回 error、游标不推进。
// 取 store/sink 接口入参便于单测注入假实现验证事务边界与原子重投语义。
func runChunk(ctx context.Context, store chunkStore, sink chunkSink, table string, cutoff int64, batch int, lg log.Log) (chunkPlan, int, error) {
	cursor, rows, err := store.readStableBatchTx(table, batch)
	if err != nil {
		return chunkPlan{}, 0, err
	}
	if len(rows) == 0 {
		return chunkPlan{}, 0, nil
	}

	plan := planChunk(rows, cutoff)
	if !plan.advanced {
		// 队首即未稳定：本轮不推进，等其落库满 lag。返回 0 让调用方停止本分片。
		return plan, 0, nil
	}

	// 🔴 C2：事务外整批投递。先 main 再 DLQ，任一失败整 chunk 不推进、下轮整批重投。
	if err = sink.produceBatch(ctx, plan.main); err != nil {
		return plan, 0, err
	}
	if err = sink.produceDLQ(ctx, plan.dlq); err != nil {
		return plan, 0, err
	}

	// 全部投递确认成功 → 短事务推进游标到稳定前缀末（绝不到 batch 末，C1）。
	advanced, err := store.advanceCursor(table, cursor, plan.maxID)
	if err != nil {
		return plan, 0, err
	}
	if !advanced {
		// 游标已被他者改动（理论上 C3 锁保证不会发生）：本轮不计已投，下轮乐观重试。
		lg.Warn("searchetl: cursor moved by another writer, skip advance",
			zap.String("table", table), zap.Int64("expected", cursor), zap.Int64("new", plan.maxID))
		return plan, 0, nil
	}
	lg.Debug("searchetl chunk produced + cursor advanced",
		zap.String("table", table),
		zap.Int64("from", cursor),
		zap.Int64("to", plan.maxID),
		zap.Int("main", len(plan.main)),
		zap.Int("dlq", len(plan.dlq)))
	return plan, plan.stableCount, nil
}

// RunIncrementalDryRun 跑一轮「空跑游标」（观测）：逐分片读稳定前缀、统计稳定行数与积压，
// **不投 Kafka、不推进游标**。用于上线前观察源读取/稳定性闸门是否符合预期（与 Kafka.On 无关，
// 永不产生运行期副作用）。
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
