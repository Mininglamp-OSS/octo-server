// Package richtext owns the server-side authoritative handling of
// RichText (ContentType=14) message payloads.
//
// 图文混排（Phase 1）: RichText=14 复用既有 ContentType（见 octo-lib
// common/msg.go），正文以有序 content block 数组承载，顶层 plain 为冗余纯
// 文本。契约规定 plain 由 server 在派发/入库出口权威生成（不信任端上送的
// plain），供 search / 推送 / 摘要 / 复制 / 下游 LLM 复用。
//
// 本包把「检测 type=14 → 用 content 重算 plain → 回写 payload」这一步收敛到
// 一个 helper，供每个 RichText 发送出口（user /v1/message/send、robot
// /message/send）共用，保证各出口对 plain 的生成口径一致。底层重算与 1MB
// 复检由 octo-lib common.(*RichTextPayload).FillPlainBounded 提供。
package richtext

import (
	"encoding/json"

	"github.com/Mininglamp-OSS/octo-lib/common"
)

// IsRichTextPayload 判断 payload map 的 type 字段是否为 RichText(=14)。
// 兼容 json.Number / float64 / int 几种反序列化结果（gin BindJSON 出 float64，
// json.Decoder.UseNumber 出 json.Number）。string 类型的 "14" 不识别，避免误命中。
func IsRichTextPayload(payload map[string]interface{}) bool {
	switch v := payload["type"].(type) {
	case float64:
		return int(v) == common.RichText.Int()
	case int:
		return v == common.RichText.Int()
	case json.Number:
		i, err := v.Int64()
		return err == nil && int(i) == common.RichText.Int()
	}
	return false
}

// EnsurePlain 是 RichText(=14) 派发出口的权威 plain 生成入口：
//   - 非 type=14 的 payload 原样返回（no-op），保证老消息路径不变；
//   - type=14 时用 content 重算顶层 plain 覆盖客户端不可信的 plain，并对回填后
//     的整条 payload 复检 1MB 上限（FillPlainBounded），超限返回其 error。
//
// 就地修改传入的 payload map（写入 payload["plain"]），调用方拿到的即同一个 map。
// 这是下游 summary / matter / search / 复制 / 推送 全部依赖的前置：server 在派发
// 前把权威 plain 写进随消息一起落库 / 进 IM 搜索索引的 payload 字节。
func EnsurePlain(payload map[string]interface{}) error {
	if !IsRichTextPayload(payload) {
		return nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var p common.RichTextPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	plain, err := p.FillPlainBounded()
	if err != nil {
		return err
	}
	payload["plain"] = plain
	return nil
}
