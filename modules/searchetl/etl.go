package searchetl

import (
	"context"
	"sync/atomic"
	"time"

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
// 阶段 2：接 Kafka producer + 事务拆分（C2）+ payload 抽取（P1-d）。
// 阶段 3（本次）：顺序提交（游标只单调推进到已确认成功投递的稳定连续前缀末）+ 跨副本互斥
// （C3）——RunIncremental 整轮在 Redis run-lock 保护下执行（横跨「读批 → 投 Kafka → 推游标」），
// 续租失败尽早 abort 在飞批次。**正确性的最终保证不是 Redis 锁本身，而是「先投递后推进」的
// 顺序 + 游标行 CAS（advanceCursor 的 WHERE last_id=expected，由 FOR UPDATE 串行化）作 fence
// + ES _id 幂等 upsert**：即便失锁竞态，CAS 也挡住越权推进，重复投递由幂等 sink 去重，绝无
// 空洞/丢失/双算（详见 runChunk 注释）。Redis 锁是降低重复、保证「正常情况下单副本跑」的
// 第一道防线；CAS fence 是底线。进程内 running CAS 挡同进程重入。Kafka.On=off 时连 producer/
// lock 都不构造（保持惰性）。
type ETL struct {
	log.Log
	ctx   *config.Context
	db    *etlDB
	batch int
	lag   int64
	// running 是**进程内**重入护栏（CAS 0→1）：挡同进程并发跑两轮 RunIncremental。
	// **跨进程/多副本**互斥由 C3 的 Redis run-lock（runLockedTick：全程持有 + 续租失败 abort）负责。
	running atomic.Bool
	// newLock 构造 Redis run-lock（C3）。默认 productionNewLock；测试可替换为假锁验证锁语义。
	newLock func(*config.Context) runLock
	// renewInterval 续租周期（默认 lockRenewInterval()）。测试可调短以快速触发续租路径。
	renewInterval time.Duration
}

// NewETL 创建 ETL。
func NewETL(ctx *config.Context) *ETL {
	return &ETL{
		Log:           log.NewTLog("SearchETL"),
		ctx:           ctx,
		db:            newETLDB(ctx),
		batch:         batchSize(),
		lag:           lagSeconds(),
		newLock:       productionNewLock,
		renewInterval: lockRenewInterval(),
	}
}

// productionNewLock 用真实 Redis 构造 run-lock（*etlLock 实现 runLock 接口）。
func productionNewLock(ctx *config.Context) runLock {
	return newETLLock(ctx)
}

// RunIncremental 跑一轮真实增量抽取（阶段 2/3）：在 Redis run-lock 保护下，逐分片以事务拆分
// 投递正文到 Kafka，游标顺序提交到已确认成功投递的稳定连续前缀末。
//
// 前置：仅当 Kafka.On 时才有意义——off 时直接返回（不连 Kafka/Redis、不推进游标，保持惰性）。
//
// 互斥两道防线：
//   - 进程内 running CAS：挡同进程并发重入。
//   - 🔴 C3 Redis run-lock（runLockedTick）：挡跨副本并发——锁横跨整 tick 持有，续租失败立即
//     abort 在飞批次（lockCtx 取消 → 投递/推进游标都停），失锁不双算。抢不到锁直接跳过本轮。
func (e *ETL) RunIncremental(ctx context.Context) error {
	cfg := e.ctx.GetConfig()
	if !cfg.Kafka.On {
		e.Info("searchetl: Kafka.On=false, skip incremental (lazy, no producer)")
		return nil
	}

	// 进程内重入护栏：已有一轮在跑则直接跳过本轮（不报错，等下个 tick）。
	if !e.running.CompareAndSwap(false, true) {
		e.Info("searchetl: another incremental run in progress (same process), skip")
		return nil
	}
	defer e.running.Store(false)

	// 🔴 C3：整轮在 Redis run-lock 保护下执行。lockCtx 在续租失败时被取消，runTick 内尊重取消。
	lock := e.newLock(e.ctx)
	if closer, ok := lock.(interface{ Close() error }); ok {
		defer func() {
			if cerr := closer.Close(); cerr != nil {
				e.Error("searchetl: close run-lock failed", zap.Error(cerr))
			}
		}()
	}
	return runLockedTick(ctx, lock, e.renewInterval, e.Log, e.runTick)
}

// runTick 是「持有 run-lock 的一轮」实际工作：构造 producer，逐分片 runChunk 投递 + 顺序推进游标。
// ctx 即 lockCtx——续租失败被取消后，每个 chunk 前的 ctx.Err() 检查令在飞循环立即 abort，
// 绝不在失锁后继续投递或推进游标（C3 验证门：失锁路径不双算）。
func (e *ETL) runTick(ctx context.Context) error {
	cfg := e.ctx.GetConfig()
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
			// 🔴 C3：每个 chunk 前检查锁是否仍持有（lockCtx 未取消）。失锁立即停，不再投递/推进。
			if cerr := ctx.Err(); cerr != nil {
				e.Warn("searchetl: lock lost / ctx cancelled, abort in-flight tick",
					zap.String("table", table), zap.Error(cerr))
				return cerr
			}
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

	// 🔴 C3 安全性的真正保证不是下面的 ctx 检查，而是「先投递后推进」的顺序 + 游标行 CAS
	// （advanceCursor 的 WHERE last_id=expected，由 readStableBatchTx 的 FOR UPDATE 串行化）
	// 充当 fencing token，外加 ES _id 幂等 upsert。推演任意交错：
	//   - 本 owner 总是在「投递全部 [cursor+1, maxID] 成功」之后才尝试推进，故游标永不领先于
	//     已投递消息——绝无空洞/丢失。
	//   - 若本 owner 已失锁、另一副本 B 抢到并先推进了游标：本 owner 的 CAS（last_id=cursor）
	//     失配 → 不推进（落入下方 !advanced 分支）。
	//   - 若本 owner 先推进、B 后读：B 的 FOR UPDATE 读到新水位、从新位置续投。
	//   - 两副本重叠投递的重复条由 ES _id upsert 去重（at-least-once 本就允许重复）。
	// 因此「失锁推进」在本设计下无害：CAS 是 fence，Redis 锁失效不会造成双算/空洞。
	//
	// 下面的 ctx.Err() 仅是**尽力而为的早退优化**：失锁后尽早停手、少做无谓的重复推进，
	// 但即便它漏判（检查通过后才失锁），CAS + 顺序 + 幂等仍保证正确性。
	if cerr := ctx.Err(); cerr != nil {
		lg.Warn("searchetl: lock lost after produce, skip cursor advance (re-produce next tick)",
			zap.String("table", table), zap.Int64("to", plan.maxID), zap.Error(cerr))
		return plan, 0, cerr
	}

	// 投递确认成功 → 短事务以游标行 CAS 推进到稳定前缀末（绝不到 batch 末，C1）。
	advanced, err := store.advanceCursor(table, cursor, plan.maxID)
	if err != nil {
		return plan, 0, err
	}
	if !advanced {
		// CAS 失配：游标已被另一持锁副本推进（失锁竞态的安全收口）。本轮不计已投，
		// 已投消息靠 message_id 幂等 + ES _id upsert 覆盖，下轮从新水位续跑——无空洞。
		lg.Warn("searchetl: cursor moved by another writer (CAS miss), skip advance",
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
