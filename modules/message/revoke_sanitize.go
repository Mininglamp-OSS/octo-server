package message

import (
	"encoding/json"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
)

// 撤回消息脱敏的唯一实现来源。
//
// Octo 撤回是 message_extra.revoke=1 的软删除，WuKongIM 里原始 payload 仍在；
// 任何返回 message body 的拉取路径都必须在下发前把撤回消息的正文剥离，只保留占位
// 态（type + revoke 标志），否则就绕过了发送方的撤回意图（信息治理漏洞）。历史上
// /message/channel/sync 与单条直读各自实现过一次，bot 拉历史路径 /v1/bot/messages/sync
// 却漏做（octo-server#777 / WS-168）。为避免每新增一个拉取接口就重漏一次，脱敏的
// 「占位态长什么样」在此集中定义，各路径按自己的响应结构调用对应 wrapper，不再各自
// 拼装 payload。

// revokedPayload 返回撤回消息对外下发的最小 payload：仅保留原始 type，剥离 content
// 及一切内容承载字段（url / name / reply / content.users …）。
//
// 保留 type 而不整条清空：撤回对所有人生效，前端仅凭 revoke=1 渲染撤回提示、不读
// payload（撤回者名由 revoker UID 单独查，见 octo-web RevokeCell）；保留 type 只为
// 兼容按 type 分支的老客户端渲染路径，type 本身不含消息正文。
//
// type 必须规范化为数字标量（scalarContentType）：payload 是不可信的调用方 JSON，
// send 路径不约束 type 必须是数字（只剥 __obo_* / 处理 mention/space_id/richtext），
// 若原样透传，攻击者可把正文藏进 type（字符串 / 对象 / 数组）绕过脱敏原样下发。
// 服务端所有 type 消费方（isTextType / payloadMsgType）都严格按数字读取、非数字一律
// 视为未知，故强制数字标量、非数字 fallback ContentError 不影响任何合法渲染（D23 安全整改）。
func revokedPayload(original map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"type": scalarContentType(original["type"]),
	}
}

// scalarContentType 把 payload 的 type 归一为已识别的数字 content-type：
// float64 / int / json.Number 三种反序列化结果转 int，其余（string / map / array / 缺失）
// 一律 fallback common.ContentError，杜绝非标量 type 承载正文逃逸。
func scalarContentType(v interface{}) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return int(i)
		}
	}
	return common.ContentError.Int()
}

// sanitizeRevokedMsgSyncResp 就地把一条撤回消息的正文从 MsgSyncResp 中剥离，只留
// 占位 payload + revoke=1/revoker 供前端渲染撤回提示。/message/channel/sync 与
// /conversation/sync（共用 from()）以及单条直读的响应都是 MsgSyncResp 形状，走这里。
// 必须在 Payload / SignalPayload / Streams / MessageExtra 全部赋值之后调用。
func sanitizeRevokedMsgSyncResp(m *MsgSyncResp) {
	m.Payload = revokedPayload(m.Payload)
	m.SignalPayload = ""
	m.Streams = nil
	// message_extra.content_edit 是「编辑后的正文」，同样是原文载体：撤回后必须
	// 一并剥离，否则编辑过的消息被撤回时仍会经 content_edit 把原文下发。
	if m.MessageExtra != nil {
		m.MessageExtra.ContentEdit = nil
		m.MessageExtra.EditedAt = 0
	}
}

// SanitizeRevokedPayloadBytes 接收 WuKongIM 原样存储 / IMSyncChannelMessage 直出的
// 消息 payload（JSON bytes），返回撤回占位态的 payload bytes。供 bot 拉历史路径
// (/v1/bot/messages/sync) 这类持有原始 []byte payload、不经过 from() 的接口复用同一
// 套占位定义，避免重复实现导致口径漂移。
//
// fail-closed：payload 解析失败（截断 / 非 JSON）时绝不能原样透传撤回原文，退化为
// 空 original，revokedPayload 会 fallback 到 ContentError 占位。
//
// 注意：本函数只处理 payload 字节，不触碰 MessageExtra/ContentEdit。当前 bot 拉历史
// 路径的 config.MessageResp 结构本身不含 MessageExtra，故无泄漏面。若响应结构未来
// 引入 MessageExtra/ContentEdit（如为对齐 web 端），调用方必须自行剥离 content_edit
// ——参见 sanitizeRevokedMsgSyncResp 对 m.MessageExtra.ContentEdit/EditedAt 的清理，
// content_edit 是「编辑后的正文」，同样是原文载体，撤回后必须一并剥离。
func SanitizeRevokedPayloadBytes(payload []byte) []byte {
	var original map[string]interface{}
	if err := util.ReadJsonByByte(payload, &original); err != nil {
		original = nil
	}
	return []byte(util.ToJson(revokedPayload(original)))
}
