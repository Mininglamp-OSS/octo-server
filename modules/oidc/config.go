package oidc

import (
	"encoding/base64"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// providerIDRe 限定 provider ID 只能用 URL-safe 的小写字母+数字+'-'/'_'。
// 该值会拼进路由 /v1/auth/oidc/<id>/authorize 与 appconfig 的 authorize_path,
// 不做约束的话 ops 误填(如 "foo/bar"、空格)会让 Gin 注册阶段 panic 或下发畸形 URL。
var providerIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// 环境变量命名约定:
//
//   TS_*  — Viper 管理的核心配置(MySQL / Redis / WuKongIM 等),由 dmwork-lib
//           的 Config 结构体反序列化,与 YAML 字段一一对应。
//   DM_*  — 模块自管的功能开关与第三方对接配置(thread / space / oidc 等),
//           由模块直接 os.Getenv 读取,不经 Viper。
//
// OIDC 走 DM_ 是因为 dmwork-lib 暂未支持 OIDC 配置块;dmwork-lib 后续补齐 OIDC
// 字段后,本模块迁移到 cfg.OIDC.* 即可,env 仍可作为运行期 override 保留。
//
// 单 provider 设计:本期仅接入一个 OIDC IdP(可任意:Aegis / Google / Okta / Keycloak),
// IdP 名称由 DM_OIDC_PROVIDER_ID/NAME 配置驱动,代码层不绑定具体厂商。
// 接第二个 IdP 时再扩展为 map,届时本结构作为 default provider 保持不变。

// Config OIDC 模块完整配置
type Config struct {
	Enabled  bool
	Provider ProviderConfig
	// Bind 自助绑定子配置(P0)。Bind.Enabled 独立于 Config.Enabled,允许
	// "OIDC 主流程开但 bind 灰度未开" 的中间态(NFR-5)。
	Bind BindConfig
}

// ProviderConfig 单个 OIDC Provider 配置
type ProviderConfig struct {
	// ID/Name 标识本 provider, 用于路由路径段、审计日志、appconfig 下发给前端做按钮文案与跳转。
	// 未配置时分别默认 "oidc" / "SSO", 保证基础部署不强制运维填这两个字段。
	ID   string
	Name string

	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURI  string
	Scopes       []string

	RequireEmailVerified bool
	RequirePKCE          bool
	AutoLinkByEmail      bool
	// AutoLinkByPhone phone_number_verified=true 时按手机号自动绑历史账号。
	// 单独开关因为部分场景里"邮箱可信"但"手机号未必",分开控制更精细。
	AutoLinkByPhone bool
	AllowNewUser    bool

	ClockSkew   time.Duration
	HTTPTimeout time.Duration

	SyncInterval    time.Duration
	SyncConcurrency int

	// AES-256-GCM 主密钥,用于加密 refresh_token,从 base64 字符串解码
	RefreshTokenEncryptionKey []byte

	// ReturnToHosts callback 完成后允许的 return_to 跳转 host 白名单
	// (DM_OIDC_RETURN_TO_HOSTS,逗号分隔)。空列表表示禁用 return_to,
	// 防开放重定向是 P1.2 必须做的硬约束。
	ReturnToHosts []string
}

// LoadConfig 从环境变量加载 OIDC 配置
//
// Enabled=false 时不校验 provider 字段,允许编译期配置但运行期关闭。
// dmwork-lib 暂未支持 OIDC 配置块,因此走环境变量;后续 dmwork-lib 加完字段
// 再迁移到 YAML,接口签名保持稳定即可。
func LoadConfig() (*Config, error) {
	cfg := &Config{
		Enabled: getBool("DM_OIDC_ENABLED", false),
	}
	if !cfg.Enabled {
		return cfg, nil
	}

	p, err := loadProvider()
	if err != nil {
		return nil, fmt.Errorf("oidc: load provider: %w", err)
	}
	cfg.Provider = p
	// Bind 子配置纯 env,无 required 校验;Enabled=false 时其他字段不参与
	// 任何 runtime 决策(由 oidc/api.go 的 cfg.Bind.Enabled 分支兜底)。
	cfg.Bind = loadBindConfig()
	return cfg, nil
}

// loadProvider 读取 provider 配置。
//
// env 优先级:DM_OIDC_PROVIDER_*  >  DM_OIDC_AEGIS_*(过渡 alias,迁移完成后移除)。
// alias 仅为减小重命名 PR 对部署的冲击,不持久维护。
func loadProvider() (ProviderConfig, error) {
	p := ProviderConfig{
		ID:           getStringWithAlias("DM_OIDC_PROVIDER_ID", "", "oidc"),
		Name:         getStringWithAlias("DM_OIDC_PROVIDER_NAME", "", "SSO"),
		Issuer:       getStringWithAlias("DM_OIDC_PROVIDER_ISSUER", "DM_OIDC_AEGIS_ISSUER", ""),
		ClientID:     getStringWithAlias("DM_OIDC_PROVIDER_CLIENT_ID", "DM_OIDC_AEGIS_CLIENT_ID", ""),
		ClientSecret: getStringWithAlias("DM_OIDC_PROVIDER_CLIENT_SECRET", "DM_OIDC_AEGIS_CLIENT_SECRET", ""),
		RedirectURI:  getStringWithAlias("DM_OIDC_PROVIDER_REDIRECT_URI", "DM_OIDC_AEGIS_REDIRECT_URI", ""),
		// 默认回归通用 OIDC core scopes,不含 Aegis 私有 scope。
		// 历史上这里硬编码了 "identity_verification" —— 对 Aegis 好使,
		// 但 Keycloak / Auth0 / Okta 等严格 IdP 看到未注册的 scope 会直接
		// `/authorize?error=invalid_scope` 拒绝授权,全站 SSO 登录挂掉。
		// Aegis 部署必须在 env (DM_OIDC_PROVIDER_SCOPES 或 DM_OIDC_AEGIS_SCOPES)
		// 里显式配置 "openid profile email phone offline_access identity_verification"。
		// 缺失 identity_verification 时 is_verified 等 claim 不会返回,callback 静默
		// 跳过 upsert(已在 claims.IsVerified=false 分支保护),不影响登录。
		Scopes: getStringSliceWithAlias("DM_OIDC_PROVIDER_SCOPES", "DM_OIDC_AEGIS_SCOPES",
			[]string{"openid", "profile", "email", "phone", "offline_access"}),

		RequireEmailVerified: getBoolWithAlias("DM_OIDC_PROVIDER_REQUIRE_EMAIL_VERIFIED", "DM_OIDC_AEGIS_REQUIRE_EMAIL_VERIFIED", true),
		RequirePKCE:          getBoolWithAlias("DM_OIDC_PROVIDER_REQUIRE_PKCE", "DM_OIDC_AEGIS_REQUIRE_PKCE", true),
		AutoLinkByEmail:      getBoolWithAlias("DM_OIDC_PROVIDER_AUTO_LINK_BY_EMAIL", "DM_OIDC_AEGIS_AUTO_LINK_BY_EMAIL", true),
		AutoLinkByPhone:      getBoolWithAlias("DM_OIDC_PROVIDER_AUTO_LINK_BY_PHONE", "DM_OIDC_AEGIS_AUTO_LINK_BY_PHONE", true),
		AllowNewUser:         getBoolWithAlias("DM_OIDC_PROVIDER_ALLOW_NEW_USER", "DM_OIDC_AEGIS_ALLOW_NEW_USER", true),

		ClockSkew:   getDurationWithAlias("DM_OIDC_PROVIDER_CLOCK_SKEW", "DM_OIDC_AEGIS_CLOCK_SKEW", 60*time.Second),
		HTTPTimeout: getDurationWithAlias("DM_OIDC_PROVIDER_HTTP_TIMEOUT", "DM_OIDC_AEGIS_HTTP_TIMEOUT", 10*time.Second),

		SyncInterval:    getDurationWithAlias("DM_OIDC_PROVIDER_SYNC_INTERVAL", "DM_OIDC_AEGIS_SYNC_INTERVAL", 15*time.Minute),
		SyncConcurrency: getIntWithAlias("DM_OIDC_PROVIDER_SYNC_CONCURRENCY", "DM_OIDC_AEGIS_SYNC_CONCURRENCY", 10),

		ReturnToHosts: getStringSlice("DM_OIDC_RETURN_TO_HOSTS", nil),
	}

	// 用 slice 保证检查顺序稳定,缺多个字段时报第一项固定,排查体验更好。
	// 报错消息用新名,引导运维迁移到 PROVIDER_*。
	required := []struct {
		name string
		val  string
	}{
		{"DM_OIDC_PROVIDER_ISSUER", p.Issuer},
		{"DM_OIDC_PROVIDER_CLIENT_ID", p.ClientID},
		{"DM_OIDC_PROVIDER_CLIENT_SECRET", p.ClientSecret},
		{"DM_OIDC_PROVIDER_REDIRECT_URI", p.RedirectURI},
	}
	for _, r := range required {
		if r.val == "" {
			return p, fmt.Errorf("required env %s is empty", r.name)
		}
	}

	if !providerIDRe.MatchString(p.ID) {
		return p, fmt.Errorf("DM_OIDC_PROVIDER_ID %q invalid: must match %s", p.ID, providerIDRe)
	}

	keyB64 := getString("DM_OIDC_RT_ENC_KEY", "")
	if keyB64 == "" {
		return p, fmt.Errorf("required env DM_OIDC_RT_ENC_KEY is empty")
	}
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return p, fmt.Errorf("DM_OIDC_RT_ENC_KEY base64 decode: %w", err)
	}
	if len(key) != 32 {
		return p, fmt.Errorf("DM_OIDC_RT_ENC_KEY must be 32 bytes after base64 decode, got %d", len(key))
	}
	p.RefreshTokenEncryptionKey = key
	return p, nil
}

func getString(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// getStringWithAlias 优先 primary,缺省回退 alias,再回退 def。alias="" 表示无 alias。
func getStringWithAlias(primary, alias, def string) string {
	if v, ok := os.LookupEnv(primary); ok && v != "" {
		return v
	}
	if alias != "" {
		if v, ok := os.LookupEnv(alias); ok && v != "" {
			return v
		}
	}
	return def
}

func getBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// 解析失败时 fall through 到 alias,避免迁移期 ops 把新 key 写错值反而吞掉
// 老 key 上仍有效的配置。所有 alias 用尽后才返回 def。
func getBoolWithAlias(primary, alias string, def bool) bool {
	if v, ok := os.LookupEnv(primary); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	if alias != "" {
		if v, ok := os.LookupEnv(alias); ok && v != "" {
			if b, err := strconv.ParseBool(v); err == nil {
				return b
			}
		}
	}
	return def
}

func getIntWithAlias(primary, alias string, def int) int {
	if v, ok := os.LookupEnv(primary); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	if alias != "" {
		if v, ok := os.LookupEnv(alias); ok && v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
	}
	return def
}

func getDurationWithAlias(primary, alias string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(primary); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	if alias != "" {
		if v, ok := os.LookupEnv(alias); ok && v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				return d
			}
		}
	}
	return def
}

func getStringSlice(key string, def []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

func getStringSliceWithAlias(primary, alias string, def []string) []string {
	if v, ok := os.LookupEnv(primary); ok && v != "" {
		return parseSlice(v, def)
	}
	if alias != "" {
		if v, ok := os.LookupEnv(alias); ok && v != "" {
			return parseSlice(v, def)
		}
	}
	return def
}

func parseSlice(v string, def []string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}
