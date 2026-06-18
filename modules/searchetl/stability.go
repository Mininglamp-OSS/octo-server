package searchetl

// stablePrefix 截取一批按 id 升序读出的行中「落库已超过 lag」的无空洞稳定前缀（硬条件 C1）。
//
// 语义与 opanalytics runChunk 的稳定性闸门完全一致：message.id 在 INSERT 时分配、COMMIT 时
// 才可见，提交顺序≠id 顺序。id 与 created_at 同在 insert 时刻分配、近似同序，故首个未稳定行
// （CreatedUnix > cutoff）之后（更高 id）均视为未稳定，从该处截断即为无空洞稳定前缀。
//
// 游标只能推进到该前缀末尾的 id，绝不能推进到 batch 末尾——否则会越过「低 id、晚提交、尚未
// 落库满 lag」的行造成永久漏扫（message_id 幂等也修不回，因为这些消息从未被 produce）。
//
// cutoff = DB_NOW - lag。返回稳定前缀（rows 的前缀切片）；队首即未稳定时返回空切片。
func stablePrefix(rows []*srcMessageRow, cutoff int64) []*srcMessageRow {
	for i, r := range rows {
		if r.CreatedUnix > cutoff {
			return rows[:i]
		}
	}
	return rows
}

// firstNonAscendingByID 返回首个**未严格按 id 升序**的行下标（即 rows[i].ID <= rows[i-1].ID），
// 无违规返回 -1。
//
// 期序 debug 断言（YUJ-5012 票4a / ReviewBot §4 额外发现）：stablePrefix 与游标推进的全部
// 无空洞/无漏读保证都建立在「读出的批严格按 id 升序」这一前提上（readStableBatchTx 的
// `ORDER BY id ASC`）。若索引/DB 版本变更或 SQL 改写悄悄破坏了返回序，stablePrefix 会在错误
// 位置截断 → 静默漏读，且 message_id 幂等也补不回（这些行从未被 produce）。本函数是把该前提
// 从「隐式假设」变成「可观测的运行期 tripwire」：调用方在 debug 期校验并大声告警，不改正确性
// 路径（纯检测）。O(n) 线性扫描，开销可忽略。
func firstNonAscendingByID(rows []*srcMessageRow) int {
	for i := 1; i < len(rows); i++ {
		if rows[i].ID <= rows[i-1].ID {
			return i
		}
	}
	return -1
}
