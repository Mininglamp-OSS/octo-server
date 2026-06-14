package searchetl

// srcMessageRow 是 message 分片表读出的单条消息（searchetl 取索引正文 + 鉴权可见性 +
// 稳定性闸门所需列）。
//
// 阶段 2：MessageID/Setting/Signal/Payload 随真实投递启用——payload 抽取（P1-d）据
// Setting/Signal 判 Signal 加密、据 payload.type 三态判正文类型，契约见 octo-lib
// contract/searchmsg。ID/CreatedUnix 仍承担空跑游标 + 稳定性闸门（C1）。
type srcMessageRow struct {
	ID          int64
	MessageID   string
	FromUID     string
	ChannelID   string
	ChannelType uint8
	Setting     uint8 // 消息 setting 位（含 Signal 加密位，见 config.SettingFromUint8）
	Signal      int   // 专用 signal 加密列（webhook 落库时与 setting 位一并写入）
	Timestamp   int64 // 发送时间（纪元秒）
	CreatedUnix int64 // 落库时间（纪元秒, = UNIX_TIMESTAMP(created_at)），稳定性闸门用
	Payload     []byte
}
