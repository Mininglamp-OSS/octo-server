package oidc

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
)

// OIDC OIDC 登录模块
//
// P1.2 将在此结构上补充以下依赖:
//   cfg         *Config         // 通过 LoadConfig() 在 New 时加载,Enabled=false 时 Route 早返回
//   encryptor   *Encryptor      // refresh_token at-rest 加密;构造 Encryptor 后立即把
//                              //   cfg.Aegis.RefreshTokenEncryptionKey 置 nil,缩短主密钥
//                              //   驻留内存的时间窗口(防御纵深,Go 无法保证擦除)
//   stateStore  StateStore      // CSRF state + PKCE 验证存储(Redis 实例需要在 graceful shutdown 时 Close)
//   oidcClient  *oidcClient     // coreos/go-oidc + oauth2 封装
//   userSvc     user.IService   // 复用 loginCommon 走 IM Token 签发
type OIDC struct {
	ctx *config.Context
	log.Log
	db *DB
}

// New 构造 OIDC 模块
//
// 当前仅初始化 db / log,scaffold 阶段不加载 config 与外部 IdP 客户端;
// P1.2 接入 handler 时统一在此处装配并实现 Enabled=false 时的早返回。
func New(ctx *config.Context) *OIDC {
	return &OIDC{
		ctx: ctx,
		Log: log.NewTLog("OIDC"),
		db:  NewDB(ctx),
	}
}

// Route 路由注册 — authorize / callback / logout 将在 P1.2 实现
func (o *OIDC) Route(r *wkhttp.WKHttp) {
	_ = r.Group("/v1/auth/oidc")
}
