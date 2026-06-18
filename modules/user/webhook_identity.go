package user

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/model"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// incoming webhook 合成身份相关常量。webhook 的发送者 UID 形如 iwh_xxx，不是真实
// 用户，客户端渲染消息时仍会把它当用户去查 /v1/users 与 /v1/users/:uid/avatar。
// 这里做服务端兜底，避免"用户信息不存在/500/头像裂图"。
//
// 注意：user 是底层模块，不能 import 上层 incomingwebhook（会造成分层倒置），因此
// 前缀与 Extra key 在此本地复制；其权威定义在 modules/incomingwebhook/display.go，
// 二者必须保持一致。webhook 展示数据通过 octo-lib 的 BussDataSource.ChannelGet 注册
// 机制跨模块获取，user 模块不直接依赖 incomingwebhook。
const (
	webhookUIDPrefix      = "iwh_"
	webhookExtraAvatarKey = "webhook_avatar"
)

// 导出别名仅供 incomingwebhook 包的测试做跨包契约一致性校验（见其 display_test.go），
// 把"本地复制的常量"与"上层源头常量"在编译期/测试期绑定，防止任一侧改动后悄悄漂移。
// 生产代码不依赖这些导出。
const (
	WebhookUIDPrefix      = webhookUIDPrefix
	WebhookExtraAvatarKey = webhookExtraAvatarKey
)

// resolveWebhookChannel 通过 BussDataSource.ChannelGet 链解析 webhook 合成身份的展示
// 信息（名称/头像）。
//
// 返回值语义（必须区分，否则会把存储故障误判成 not-found）：
//   - (resp, nil)：命中，resp 为合成的频道详情。
//   - (nil, nil)：无任何模块处理此 uid（webhook 真正不存在，含已删除）→ 调用方走
//     not-found / 默认头像降级。
//   - (nil, err)：某模块返回了真实错误（DB/查询失败）→ 调用方必须返回 5xx，不可降级。
//
// datasource 用 register.ErrDatasourceNotProcess 表示"不处理"，用包装后的真实 error
// 表示故障（见 incomingwebhook 的 newChannelGetDatasource），这里据此区分。
func (u *User) resolveWebhookChannel(uid, loginUID string) (*model.ChannelResp, error) {
	return resolveWebhookChannelVia(u.ctx, uid, loginUID)
}

// resolveWebhookChannelVia 是 resolveWebhookChannel 的 ctx 版实现，供 *User 的 HTTP
// 处理器与包级 ResolveWebhookDisplayName（推送链路无 *User 实例，只有 ctx）共用，避免
// 复制 datasource 遍历与 (resp,nil)/(nil,nil)/(nil,err) 的错误语义。语义见
// resolveWebhookChannel 的注释。
func resolveWebhookChannelVia(ctx *config.Context, uid, loginUID string) (*model.ChannelResp, error) {
	for _, m := range register.GetModules(ctx) {
		if m.BussDataSource.ChannelGet == nil {
			continue
		}
		resp, err := m.BussDataSource.ChannelGet(uid, common.ChannelTypePerson.Uint8(), loginUID)
		if err != nil {
			if errors.Is(err, register.ErrDatasourceNotProcess) {
				continue // 该模块不处理此 uid，尝试下一个
			}
			return nil, err // 真实错误向上传播，禁止降级成 not-found / 默认头像
		}
		if resp != nil {
			return resp, nil
		}
	}
	return nil, nil
}

// ResolveWebhookDisplayName 返回合成发送者（如 incoming webhook 的 iwh_xxx，user 表里
// 没有对应行）的展示名，通过 BussDataSource.ChannelGet 注册链解析。离线推送链路用它兜底
// 渲染发送者名，避免 webhook 消息推送出来没有发件人名字。
//
// 返回 ("", nil) 表示没有任何模块处理该 uid（不是 webhook，或已删除）；("", err) 仅在
// datasource 真实故障时返回。调用方应把空名当作"无兜底"，把 err 当作可记录的非致命错误。
func ResolveWebhookDisplayName(ctx *config.Context, uid string) (string, error) {
	ch, err := resolveWebhookChannelVia(ctx, uid, "")
	if err != nil {
		return "", err
	}
	if ch == nil {
		return "", nil
	}
	return ch.Name, nil
}

// newWebhookUserDetailResp 把 webhook 频道详情合成为最小化用户详情，供 /v1/users/:uid
// 渲染发送者名。仅填充展示必需字段，其余保持零值；绝不携带 token。
func newWebhookUserDetailResp(uid string, ch *model.ChannelResp) *UserDetailResp {
	return &UserDetailResp{
		UID:      uid,
		Name:     ch.Name,
		Category: ch.Category,
		Status:   1,
	}
}

// writeWebhookAvatar 处理 webhook 头像请求：有自定义 http(s) 头像 URL 则 302 重定向，
// 否则（未设置头像或 webhook 已删除）回退到基于 uid 的默认头像，避免裂图。
func (u *User) writeWebhookAvatar(c *wkhttp.Context, uid string) {
	ch, err := u.resolveWebhookChannel(uid, "")
	if err != nil {
		// 真实查询故障：返回 500，不可静默回退默认头像（掩盖故障）。
		u.Error("查询 webhook 头像失败", zap.Error(err), zap.String("uid", uid))
		c.Writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	avatarURL := ""
	if ch != nil {
		if v, ok := ch.Extra[webhookExtraAvatarKey].(string); ok {
			avatarURL = strings.TrimSpace(v)
		}
	}
	if strings.HasPrefix(avatarURL, "http://") || strings.HasPrefix(avatarURL, "https://") {
		c.Redirect(http.StatusFound, avatarURL)
		return
	}
	// 仅当 webhook 存在但无自定义头像、或 webhook 已删除（ch==nil）时回退默认头像。
	// 复用 bot 默认头像逻辑（crc32(uid) 确定性选 13 色内置 PNG）：webhook 与 bot 同属
	// "非真实用户"发送者，视觉口径保持一致；成员/bot 自助创建的 webhook 不可自定义
	// 头像（#member-perms），即默认全部落在这条 palette 路径上。
	imageData, genErr := readBotDefaultAvatar(uid)
	if genErr != nil {
		u.Error("读取 webhook 默认头像失败", zap.Error(genErr), zap.String("uid", uid))
		c.Writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	c.Header("Content-Type", "image/png")
	c.Header("Content-Disposition", "inline; filename=avatar.png")
	c.Header("Cache-Control", "public, max-age=86400")
	c.Data(http.StatusOK, "image/png", imageData)
}
