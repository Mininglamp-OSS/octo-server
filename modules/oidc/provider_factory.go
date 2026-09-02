package oidc

// provider_factory.go — 按 provider kind 构造 AuthProvider 的唯一入口。
//
// 为什么要导出:有两处需要"从这份配置得到一个能用的 provider" ——
// 本模块的 New(),以及 modules/integration 的 New()。后者原本无条件调
// NewClient() 做 Discovery,于是切到没有 Discovery 的 kind 之后它的两个对外
// 端点整体返回 500;而修法如果是在那边再抄一份 switch,就又造出一份会漂移的
// 副本(参见 pkg/oidcboot 因同类漂移导致全站登录锁死那件事)。
//
// 所以分派只留一份,放在这里。

import (
	"context"
	"fmt"
)

// AuthProviderResult 构造结果。
//
// Client 只在标准 OIDC kind 下非 nil。把它单独返回而不是藏进 provider,是因为
// SyncWorker 仍直接依赖 *Client 的刷新能力(那部分尚未迁到抽象后面);plain-OAuth2
// kind 下它恒为 nil,调用方必须按 nil 处理而不能假设总有一个 client。
type AuthProviderResult struct {
	Provider AuthProvider
	Client   *Client
}

// NewAuthProvider 按 cfg.Kind 构造 AuthProvider。
//
// onWarn 可为 nil;非 nil 时用于上报那些"能继续但值得知道"的情况(例如标准 OIDC
// 下 /userinfo 拉取失败但 id_token 已足够确立身份)。
//
// 失败即返回 error,调用方必须让相关端点 fail-closed —— 一个构造失败的 provider
// 意味着我方无法确立任何身份,此时放行任何请求都是无认证放行。
func NewAuthProvider(ctx context.Context, cfg ProviderConfig, onWarn func(msg string, err error)) (*AuthProviderResult, error) {
	switch cfg.Kind {
	case KindOAuth2:
		// 无 Discovery、无 JWKS:端点全部由配置拼出,构造过程不发任何网络请求。
		prov, err := newOAuth2Provider(oauth2ProviderConfig{
			Issuer:                cfg.Issuer,
			BaseURL:               cfg.BaseURL,
			ClientID:              cfg.ClientID,
			ClientSecret:          cfg.ClientSecret,
			RedirectURI:           cfg.RedirectURI,
			Scopes:                cfg.Scopes,
			AppID:                 cfg.AppID,
			PostLogoutRedirectURI: cfg.PostLogoutRedirectURI,
		})
		if err != nil {
			return nil, fmt.Errorf("oidc: build oauth2 provider: %w", err)
		}
		return &AuthProviderResult{Provider: prov}, nil

	default:
		// KindOIDC,以及存量未配 KIND 的部署:Discovery → JWKS → 包装。
		// 这一步会发网络请求,所以调用方要给 ctx 带超时。
		client, err := NewClient(ctx, ClientConfig{
			Issuer:       cfg.Issuer,
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURI:  cfg.RedirectURI,
			Scopes:       cfg.Scopes,
			HTTPTimeout:  cfg.HTTPTimeout,
			ClockSkew:    cfg.ClockSkew,
		})
		if err != nil {
			return nil, fmt.Errorf("oidc: discovery: %w", err)
		}
		prov, err := newOIDCProvider(oidcProviderConfig{
			Client:                client,
			Scopes:                cfg.Scopes,
			PostLogoutRedirectURI: cfg.PostLogoutRedirectURI,
			EndSessionURLOverride: cfg.EndSessionURL,
			OnWarn:                onWarn,
		})
		if err != nil {
			return nil, fmt.Errorf("oidc: wrap oidc provider: %w", err)
		}
		return &AuthProviderResult{Provider: prov, Client: client}, nil
	}
}
