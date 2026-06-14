package searchetl

import (
	"encoding/json"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/contract/searchmsg"
)

// extractOutcome 是 payload 抽取的三态结果（P1-d 规则，硬条件 C2 语义口径）：
//   - outcomeOK：正常解析出可检索正文，进正常流投 Kafka。
//   - outcomeRawExcluded：已知不可索引类（Signal 加密 DM / 非文本结构化内容）——
//     content=nil、raw_excluded=true，**走正常流不进 DLQ**（不算丢消息）。
//   - outcomeDLQ：本应可解析却解析失败的真异常——进 DLQ topic，游标仍推进。
//
// raw_excluded 与 DLQ 的严格区分是 C2 的核心：把「已知不可索引」误判成「解析失败」
// 会把正常加密消息灌满 DLQ；反之把真异常当 raw_excluded 会静默吞掉本可修复的错误。
type extractOutcome int

const (
	outcomeOK extractOutcome = iota
	outcomeRawExcluded
	outcomeDLQ
)

// extractMessage 把一行源消息抽取为 Kafka 正文契约（searchmsg.Message）。
//
// P1-d 规则：
//   - Signal 加密（setting 的 Signal 位 或 signal 列为真）→ payload 非明文 JSON，
//     直接 raw_excluded（不尝试解析，避免把密文当损坏 JSON 误判进 DLQ）。
//   - 非 Signal → payload 应是明文 JSON。解析失败（本应可解析却失败）→ DLQ。
//   - 解析成功后按 type 三态（float64/int/json.Number，复用 message.CoerceTextPayloadContent
//     的兼容口径）取 content：
//       · type=Text 且 content 为 string → 取该 string 作正文。
//       · 非 Text 或 content 非 string（媒体/富文本/结构化对象）→ 本期保守 raw_excluded
//         （阶段 4+ 可细化为序列化可检索文本，契约字段不变）。
//
// 返回的 Message 已填 SchemaVersion / Source / 可见性字段；outcome 决定投正文 topic 还是 DLQ。
func extractMessage(row *srcMessageRow) (searchmsg.Message, extractOutcome) {
	msg := searchmsg.Message{
		SchemaVersion: searchmsg.SchemaVersion,
		MessageID:     row.MessageID,
		ChannelID:     row.ChannelID,
		ChannelType:   int(row.ChannelType),
		FromUID:       row.FromUID,
		MsgTimestamp:  row.Timestamp,
		CreatedAt:     row.CreatedUnix,
		Source:        searchmsg.SourceETLMessageTable,
	}

	if isSignalEncrypted(row) {
		// Signal 加密 DM：payload 是密文，解不出明文是预期行为，非异常。
		msg.RawExcluded = true
		msg.Content = nil
		return msg, outcomeRawExcluded
	}

	var m map[string]interface{}
	if err := json.Unmarshal(row.Payload, &m); err != nil || len(m) == 0 {
		// 非加密消息本应是明文 JSON，解析失败/空 map 属真异常 → DLQ（游标仍推进）。
		return msg, outcomeDLQ
	}

	contentType, isText := payloadType(m)
	msg.ContentType = contentType

	if !isText {
		// 非文本（媒体/系统/富文本等结构化内容）：本期不索引，raw_excluded。
		msg.RawExcluded = true
		msg.Content = nil
		return msg, outcomeRawExcluded
	}

	c, ok := m["content"].(string)
	if !ok {
		// type=Text 但 content 非 string（如 bot 误塞 object，见 issue #1097）：
		// 本期保守 raw_excluded，不强转，避免把结构化内容当正文索引。
		msg.RawExcluded = true
		msg.Content = nil
		return msg, outcomeRawExcluded
	}

	content := c
	msg.Content = &content
	return msg, outcomeOK
}

// isSignalEncrypted 判定消息是否 Signal 加密：setting 的 Signal 位 或 专用 signal 列。
// 两个来源都查是因为历史落库既写 setting 位也写独立 signal 列（webhook/db_message.go），
// 任一为真即视为加密。
func isSignalEncrypted(row *srcMessageRow) bool {
	if row.Signal != 0 {
		return true
	}
	return config.SettingFromUint8(row.Setting).Signal
}

// payloadType 从 payload map 解出消息类型（兼容 float64/int/json.Number 三种反序列化结果，
// 与 message.CoerceTextPayloadContent / isTextType 口径一致），并返回是否为 Text(=1)。
// 无法识别类型时 contentType 返回 0、isText=false。
func payloadType(m map[string]interface{}) (contentType int, isText bool) {
	switch v := m["type"].(type) {
	case float64:
		contentType = int(v)
	case int:
		contentType = v
	case json.Number:
		if i, err := v.Int64(); err == nil {
			contentType = int(i)
		}
	}
	return contentType, contentType == common.Text.Int()
}
