package oidc

// own_credential.go — 判断一个 bearer 值是否是**本服务自己签发**的凭据。
//
// 为什么需要:两个端点在同一个 Authorization 头上接受"上游凭据",而 plain-OAuth2
// 的 /userinfo 把凭据放在 **URL query** 上。任何被误当成上游凭据转发出去的东西,
// 都会连值带载荷落进第三方的访问日志。
//
// 已有的 IsForeignToken 回答不了这个问题,而且**按设计**回答不了:它判定的是
// "这张 token 是不是我们用 HS256 签的",判据是 HMAC。会话 token 和 uk_/bf_ 这些
// 同样是我方签发的凭据根本不是 JWT,一进 strings.Split 就出局,于是被归入
// "不是我们的" —— 一个真值判断,只是问错了问题。"我们签的 JWT" 是 "我方凭据"
// 的真子集,而需要拦的是后者。
//
// 判定方式全部是**本地确定性检验**,不外呼、无副作用:
//   - uk_ / bf_ / app_ 是我方签发凭据的规范前缀(userAPIKeyAuth、bot_api 的
//     extractBotToken 就用它们做判据);覆盖完整性由 own_credential_coverage_test.go
//     扫源码钉住 —— 前缀清单是"想起来了的类型"的名单,漏项必须变成一次 CI 失败;
//   - 会话 token 查我方会话存储 —— 查得到就是我们签的。
//
// 已知缺口:modules/bot_api/auth.go 至今仍接受**无前缀的历史 bot token**
// (那里的注释写着 "bf_ 前缀或 legacy token")。这类 token 没有可判定的形态,
// 也不在会话存储里,因此这里认不出来。查它需要打 robot 表 —— 一次 DB round-trip
// 挂在每个未认证请求上,而且会把这两个端点变成 bot token 的存在性预言机。
// 现状是:如果生产里还有这种 token,它仍可能被转发。是否清零由运维确认,
// 记在 guard-matrix.md 的"未闭合格子"里,不在这里靠猜。
//
// 不按形态猜:上游的不透明 access_token 完全可能长得像任何东西,猜错的代价是
// 把一个未经我方验证的载荷当成身份来源。

import (
	"context"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"

	"github.com/Mininglamp-OSS/octo-server/modules/app_bot"
	"github.com/Mininglamp-OSS/octo-server/modules/botfather"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
)

// OwnCredentialKind 本服务签发的凭据类别。空值表示"不是我方凭据"。
type OwnCredentialKind string

const (
	// OwnCredentialNone 判定为非我方凭据(可以走上游路径)。
	OwnCredentialNone OwnCredentialKind = ""
	// OwnCredentialUserAPIKey uk_ 用户 API key。
	OwnCredentialUserAPIKey OwnCredentialKind = "user_api_key"
	// OwnCredentialBotToken bf_ 机器人 token。
	OwnCredentialBotToken OwnCredentialKind = "bot_token"
	// OwnCredentialAppBotToken app_ App Bot token。存 app_bot 表,不在会话存储里,
	// 所以只能靠前缀认出来 —— 长期有效且 DB 背书,泄漏后果比 bf_ 更重。
	OwnCredentialAppBotToken OwnCredentialKind = "app_bot_token"
	// OwnCredentialSessionToken 我方登录会话 token。
	OwnCredentialSessionToken OwnCredentialKind = "session_token"
)

// OwnCredentialDetector 本地判定凭据归属。零值不可用,必须经构造函数。
type OwnCredentialDetector struct {
	reader        auth.TokenRecordReader
	sessionPrefix string
}

// NewOwnCredentialDetector 装配 detector。
//
// 会话查询复用 auth.SessionStoreForContext —— 与 AuthMiddleware 读的是同一份存储,
// 所以"能不能解析成会话"这个判断在两处必然一致。自己另开一个 Redis 连接或另拼一套
// key 规则,就又是一份会漂移的副本,而这里漂移的后果是凭据泄漏。
func NewOwnCredentialDetector(ctx *config.Context) *OwnCredentialDetector {
	if ctx == nil {
		return nil
	}
	return &OwnCredentialDetector{
		reader:        auth.SessionStoreForContext(ctx),
		sessionPrefix: sessionTokenCachePrefix(ctx),
	}
}

// Classify 判定 raw 属于哪一类我方凭据。
//
// 返回 (OwnCredentialNone, nil) 表示"确定不是我方凭据",调用方可以走上游路径。
// 返回 error 表示**判定不出来**(会话存储不可用)—— 调用方必须按"可能是我们的"
// 处理并就地拒绝,不能回落上游:判不出归属时转发,等于把这道守卫的失败方向
// 设成了泄漏。
//
// 前缀判定是纯本地的、不会失败;只有会话查询会返回 error。
func (d *OwnCredentialDetector) Classify(ctx context.Context, raw string) (OwnCredentialKind, error) {
	raw = strings.TrimSpace(raw)
	if d == nil || raw == "" {
		return OwnCredentialNone, nil
	}
	switch {
	case strings.HasPrefix(raw, botfather.UserAPIKeyPrefix):
		return OwnCredentialUserAPIKey, nil
	case strings.HasPrefix(raw, botfather.BotTokenPrefix):
		return OwnCredentialBotToken, nil
	case strings.HasPrefix(raw, app_bot.AppBotTokenPrefix):
		return OwnCredentialAppBotToken, nil
	}
	if d.reader == nil {
		return OwnCredentialNone, nil
	}
	rec, err := d.reader.ReadToken(ctx, d.sessionPrefix+raw)
	if err != nil {
		// 未命中不是 error(存储返回 TTL=-2 + nil),所以走到这里就是存储本身
		// 出了问题。判定不出归属 —— 交给调用方 fail-closed。
		return OwnCredentialNone, err
	}
	if strings.TrimSpace(rec.Payload) == "" {
		return OwnCredentialNone, nil
	}
	return OwnCredentialSessionToken, nil
}
