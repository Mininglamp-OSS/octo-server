// Package richtext owns the server-side authoritative handling of
// RichText (ContentType=14) message payloads.
//
// 图文混排（Phase 1）: RichText=14 复用既有 ContentType（见 octo-lib
// common/msg.go），正文以有序 content block 数组承载，顶层 plain 为冗余纯
// 文本。契约规定 plain 由 server 在派发/入库出口权威生成（不信任端上送的
// plain），供 search / 推送 / 摘要 / 复制 / 下游 LLM 复用。
//
// 本包把 RichText(=14) 发送出口的两步收敛成共用 helper，供每个出口
// （user /v1/message/send、robot /message/send）对称调用：
//
//  1. Validate —— 入站 write-strict 校验（与 robot payloadIsVail 对称）：
//     拒 缺/空 content、空 text、data: 图片 URL、缺图片宽高、未知 block type；
//     大小上限作用在原始「完整」payload 字节（含未知顶层字段），不是裁剪后的
//     content+plain 子集。
//  2. Finalize —— 所有 server 端 enrich（space_id 注入 / mention 改写 / 展开
//     等）之后调用：用 content 重算权威顶层 plain 覆盖端上不可信 plain，并对
//     真正出站的「完整」payload（含 server 注入的顶层字段）复检 1MB 上限。
//
// 设计要点（Jerry-Xin PR#232 review 两条 Critical 的修复口径）：
//   - 大小检查必须落在真实完整 payload 上。octo-lib 的
//     (*RichTextPayload).FillPlainBounded 只 marshal content+plain 子集，原始
//     map 的未知/ server 顶层字段在 size check 前已被丢弃——会漏检。故本包统一
//     marshal 整个 payload map 来判定大小。
//   - Finalize 必须排在所有 enrich 之后：server 注入 space_id / 展开 mention.uids
//     都会把 payload 撑大，入站只看原始字节会放过「enrich 后超限」的 payload。
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

// Validate 是 RichText(=14) 发送入口的 write-strict 校验 gate（与 robot 路径
// 的 payloadIsVail→common.ValidateRichTextPayload 对称）：
//   - 非 type=14 的 payload 直接通过（no-op），保证老消息路径不变；
//   - type=14 时对「完整」payload 跑 common.ValidateRichTextPayload：拒 缺/空
//     content、空 text 块、data:/javascript:/file: 等非 http(s) 图片 URL、缺图片
//     宽高、未知 block type，并把 1MB 大小上限作用在原始完整 payload 字节上
//     （含未知顶层字段）。
//
// ⚠️ 与「裁剪后子集校验」的关键差异（PR#232 Jerry-Xin Critical#2）：这里 marshal
// 的是 *整个* payload map，不是 content+plain 子集。octo-lib 内部的
// (*RichTextPayload).UnmarshalJSON 会丢弃未知顶层字段，若先 marshal 子集再判定
// 大小，端塞的未知顶层巨字段会逃过 size check。本函数从完整 map 取字节，size
// 检查与真实入站体一致。
//
// Validate 不修改 payload，只做 gate；plain 的权威生成在所有 enrich 之后由
// Finalize 完成。
func Validate(payload map[string]interface{}) error {
	if !IsRichTextPayload(payload) {
		return nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := common.ValidateRichTextPayload(raw); err != nil {
		return err
	}
	return nil
}

// Finalize 是 RichText(=14) 派发出口的权威收尾：必须在所有 server 端 enrich
// （space_id 注入 / mention 改写 / mention.uids 展开等）之后调用。
//   - 非 type=14 的 payload 原样返回（no-op），保证老消息路径不变；
//   - type=14 时用 content 重算顶层 plain 覆盖客户端不可信的 plain，并对真正出站
//     的「完整」payload（含 server 注入的顶层字段，如 space_id）复检 1MB 上限，
//     超限返回 common.ErrRichTextPayloadTooLarge。
//
// 就地修改传入的 payload map（写入 payload["plain"]），调用方拿到的即同一个 map。
// 这是下游 summary / matter / search / 复制 / 推送 全部依赖的前置：server 在派发
// 前把权威 plain 写进随消息一起落库 / 进 IM 搜索索引的 payload 字节。
//
// ⚠️ 大小复检落在完整 payload（PR#232 Jerry-Xin Critical#2）：先 FillPlain 重算
// plain 写回 map，再 marshal *整个* map 判定大小，确保 server enrich 把 payload
// 撑过 1MB 的情况会被这里拦下——而不是只看 content+plain 子集而漏检。
func Finalize(payload map[string]interface{}) error {
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
	// server 权威重算 plain（不复检子集大小，大小复检在下方对完整 payload 做）。
	payload["plain"] = p.FillPlain()
	// 对真正出站的完整 payload（含未知/ server 注入顶层字段）复检 1MB 上限。
	out, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if len(out) > common.RichTextMaxPayloadBytes {
		return common.ErrRichTextPayloadTooLarge
	}
	return nil
}

// EnsurePlain 是旧的「校验+回填 plain」单步入口，保留以兼容仍只需要回填
// plain 的调用方。
//
// Deprecated: 新出口请改用 Validate（入站 gate）+ Finalize（enrich 后收尾）的
// 两步组合，使 1MB 复检作用在真实最终 payload 上、入站强校验与 robot 路径对称。
// 本函数等价于 Validate 后立即 Finalize（中间无 enrich），仅用于无 enrich 的
// 简单路径。
func EnsurePlain(payload map[string]interface{}) error {
	if err := Validate(payload); err != nil {
		return err
	}
	return Finalize(payload)
}
