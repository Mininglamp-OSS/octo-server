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
