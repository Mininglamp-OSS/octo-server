package searchetl

// srcMessageRow 是 message 分片表读出的单条消息（searchetl 取索引正文 + 鉴权可见性 +
// 稳定性闸门所需列）。
//
// 阶段 1（本骨架）：仅 ID/CreatedUnix 投入使用（空跑游标 + 稳定性闸门）。MessageID/Payload
// 等正文字段在阶段 2 接 Kafka 时随源 SELECT 改造一并启用，契约见 octo-lib contract/searchmsg。
type srcMessageRow struct {
	ID          int64
	MessageID   string
	FromUID     string
	ChannelID   string
	ChannelType uint8
	Timestamp   int64 // 发送时间（纪元秒）
	CreatedUnix int64 // 落库时间（纪元秒, = UNIX_TIMESTAMP(created_at)），稳定性闸门用
	Payload     []byte
}
