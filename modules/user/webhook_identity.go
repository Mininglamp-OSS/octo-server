package user

import (
	"net/http"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
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

// resolveWebhookChannel 通过 BussDataSource.ChannelGet 链解析 webhook 合成身份的展示
// 信息（名称/头像）。未命中（含 webhook 已删除）返回 nil。
func (u *User) resolveWebhookChannel(uid, loginUID string) *model.ChannelResp {
	for _, m := range register.GetModules(u.ctx) {
		if m.BussDataSource.ChannelGet == nil {
			continue
		}
		resp, err := m.BussDataSource.ChannelGet(uid, common.ChannelTypePerson.Uint8(), loginUID)
		if err != nil {
			// ErrDatasourceNotProcess 等：该模块不处理，继续下一个。
			continue
		}
		if resp != nil {
			return resp
		}
	}
	return nil
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
	ch := u.resolveWebhookChannel(uid, "")
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
	imageData, genErr := generateDefaultAvatar(uid)
	if genErr != nil {
		u.Error("生成 webhook 默认头像失败", zap.Error(genErr), zap.String("uid", uid))
		c.Writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	c.Header("Content-Type", "image/png")
	c.Header("Content-Disposition", "inline; filename=avatar.png")
	c.Header("Cache-Control", "public, max-age=86400")
	c.Data(http.StatusOK, "image/png", imageData)
}
