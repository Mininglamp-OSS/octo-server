package searchetl

import "github.com/Mininglamp-OSS/octo-lib/contract/searchmsg"

// chunkPlan 是对「一批已读源行」做稳定性闸门截断 + payload 抽取后的投递计划（纯函数产物，
// 不含任何 IO，便于单测）。调用方据此：先投 main + dlq 全部确认成功 → 再把游标推进到 maxID。
type chunkPlan struct {
	// main 投正文 topic（outcomeOK 的正文 + outcomeRawExcluded 的 content=null 消息）。
	main []searchmsg.Message
	// dlq 投 DLQ topic（outcomeDLQ：本应可解析却失败的真异常）。
	dlq []searchmsg.Message
	// stableCount 稳定前缀内的行数（= main+dlq 条数；用于「是否触达未稳定尾部」判定）。
	stableCount int
	// maxID 稳定前缀末行的 message 表自增 id；游标只能推进到此（绝不到 batch 末，C1）。
	// 稳定前缀为空时为 0，调用方据此本轮不推进。
	maxID int64
	// advanced 表示稳定前缀非空、产生了可推进的水位。
	advanced bool
}

// planChunk 对一批按 id 升序读出的源行做：① 稳定性闸门截断（C1，cutoff=DB_NOW-lag）→
// ② 逐行 payload 抽取三态分流（C2/P1-d：OK/RawExcluded 进 main，DLQ 进 dlq）。
//
// 纯函数：输入源行 + cutoff，输出投递计划，不触 DB/Kafka。游标推进水位取稳定前缀末行 id，
// 与 outcome 无关——raw_excluded / DLQ 行同样已被消费、其 id 必须计入水位，否则下轮重扫。
func planChunk(rows []*srcMessageRow, cutoff int64) chunkPlan {
	stable := stablePrefix(rows, cutoff)
	plan := chunkPlan{stableCount: len(stable)}
	if len(stable) == 0 {
		return plan
	}
	plan.main = make([]searchmsg.Message, 0, len(stable))
	for _, row := range stable {
		msg, outcome := extractMessage(row)
		switch outcome {
		case outcomeDLQ:
			plan.dlq = append(plan.dlq, msg)
		default: // outcomeOK / outcomeRawExcluded 都走正文流
			plan.main = append(plan.main, msg)
		}
	}
	plan.maxID = stable[len(stable)-1].ID
	plan.advanced = true
	return plan
}
