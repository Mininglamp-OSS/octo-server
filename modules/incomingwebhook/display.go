package incomingwebhook

import (
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/model"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
)

// webhookIDPrefix 是 incoming webhook 合成发送者 UID 的固定前缀（形如 iwh_xxx）。
// 它既是 webhook 的公开 ID，也作为消息 FromUID。客户端把它当普通用户去查
// /v1/channels、/v1/users、/v1/users/:uid/avatar 时，服务端据此前缀识别并兜底，
// 避免"用户信息不存在/500/头像裂图"。
const webhookIDPrefix = "iwh_"

// WebhookIDPrefix / ExtraAvatarKey 是导出的契约常量别名，仅供其它模块的【测试】校验
// 跨包常量一致性（见 modules/user 的一致性测试）。生产代码请勿跨层 import 本（上层）
// 模块——user 侧本地复制同值常量以保持分层方向，编译期不可见的漂移由该测试兜底。
const (
	WebhookIDPrefix = webhookIDPrefix
	ExtraAvatarKey  = extraAvatarKey
)

// ChannelResp.Extra 里下发的 key 约定。user 模块的头像端点据 extraAvatarKey 取原始
// avatar URL 做重定向；契约在两侧各自定义同名常量（user 模块不 import 本上层模块）。
// 刻意不下发 group_no：渲染发送者名/头像不需要它，且避免任意登录用户据 iwh_ 反查到
// webhook 归属群（租户信息最小暴露，PR #250 review）。
const (
	extraKindKey   = "kind"           // 固定为 "webhook"，便于客户端识别非真实用户
	extraAvatarKey = "webhook_avatar" // webhook 创建时填写的原始头像 URL（可能为空）
	extraKindValue = "webhook"
)

// defaultWebhookDisplayName 当 webhook 未设置名称时的兜底展示名。
const defaultWebhookDisplayName = "Webhook"

// isWebhookUID 判断 uid 是否为 incoming webhook 合成身份。
func isWebhookUID(uid string) bool {
	return strings.HasPrefix(uid, webhookIDPrefix)
}

// webhookDisplayName 返回 webhook 的展示名，空名兜底到 defaultWebhookDisplayName。
func webhookDisplayName(m *incomingWebhookModel) string {
	if name := strings.TrimSpace(m.Name); name != "" {
		return name
	}
	return defaultWebhookDisplayName
}

// newWebhookChannelResp 把 webhook 记录合成为单聊频道详情，供 /v1/channels/:id/:type
// 与 /v1/users/:uid 渲染发送者名/头像。
//
// Logo 故意采用与真实用户一致的相对路径 users/{uid}/avatar，让客户端继续走头像端点；
// 原始 avatar URL 放进 Extra[extraAvatarKey]，由头像端点决定重定向或回退默认图。
// 绝不下发 token / token_hash。
func newWebhookChannelResp(m *incomingWebhookModel) *model.ChannelResp {
	resp := &model.ChannelResp{}
	resp.Channel.ChannelID = m.WebhookID
	resp.Channel.ChannelType = common.ChannelTypePerson.Uint8()
	resp.Name = webhookDisplayName(m)
	resp.Logo = fmt.Sprintf("users/%s/avatar", m.WebhookID)
	resp.Category = extraKindValue
	// Status 固定 1（ChannelResp 语义 0/1 均为"正常"，2 才是黑名单）。webhook 的展示
	// 身份刻意不反映 enabled/disabled：历史消息无论 webhook 当前是否被禁用（群解散会
	// disable 而非 delete）都需渲染出发送者名/头像；推送鉴权在 push 路径独立 gate
	// m.Status，与展示互不影响（PR #250 review，deliberate divergence）。
	resp.Status = 1
	resp.Extra = map[string]interface{}{
		extraKindKey:   extraKindValue,
		extraAvatarKey: m.Avatar,
	}
	return resp
}

// newChannelGetDatasource 构造 incomingwebhook 模块的 BussDataSource.ChannelGet：
//   - 仅处理"单聊 + iwh_ 前缀"，其余一律返回 ErrDatasourceNotProcess 交还链路；
//   - 软删除（status=statusDeleted）后记录仍保留，且此处刻意不按 status 过滤，因此
//     已删除 webhook 的历史消息仍能解析出真实发送者名/头像（#254，这正是软删除的目的）；
//   - 仅当记录真正不存在（伪造或历史硬删除遗留的 iwh_）时返回 ErrDatasourceNotProcess，
//     由上层优雅回落到"频道不存在/未知发送者"，不报 500。
func newChannelGetDatasource(d *incomingWebhookDB) func(channelID string, channelType uint8, loginUID string) (*model.ChannelResp, error) {
	return func(channelID string, channelType uint8, _ string) (*model.ChannelResp, error) {
		if channelType != common.ChannelTypePerson.Uint8() || !isWebhookUID(channelID) {
			return nil, register.ErrDatasourceNotProcess
		}
		m, err := d.queryByWebhookID(channelID)
		if err != nil {
			return nil, fmt.Errorf("incomingwebhook: query webhook for channel get: %w", err)
		}
		if m == nil {
			return nil, register.ErrDatasourceNotProcess
		}
		return newWebhookChannelResp(m), nil
	}
}
