package oidc

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/oidcboot"
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

	// Kind 上游协议种类,决定用哪个 AuthProvider 实现(OCTO_OIDC_PROVIDER_KIND)。
	// 缺省 KindOIDC —— 存量部署没有这个 env,不能因为引入它而改变行为。
	//
	// 注意:业务分支一律读 ProviderCapabilities,不读 Kind。Kind 只用于选实现、
	// 打 metric label 和启动期配置分叉校验。
	Kind ProviderKind

	// BaseURL 仅 KindOAuth2 使用:IdP 站点根,authorize / token / userinfo /
	// 登出路径都拼在其后(OCTO_OIDC_PROVIDER_BASE_URL)。
	// 留空时回落 Issuer 值 —— 这样接入新 kind 不需要新增必填 env(见 loadProvider
	// 里 required 列表的镜像副本说明)。
	BaseURL string

	// AppID 仅 KindOAuth2 使用:单点登出的应用标识(OCTO_OIDC_PROVIDER_APP_ID)。
	//
	// 它**不是** ClientID —— IdP 侧是两个独立的注册值,由同一批管理员分别下发,
	// 且可能每环境不同。申请凭据时只要了 client id/secret/redirect uri 的话,
	// 登出功能会做不出来。
	AppID string

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

	// ---- RP-Initiated Logout(可选,#215)----

	// PostLogoutRedirectURI logout 成功后让 IdP 回跳的地址(写死的登录页)。
	// 空时 logout 不生成 end_session_url,前端退回"仅清本地"。安全考量:此值由
	// 运维写死、不接受前端传入,因此无需在服务端再做 redirect 白名单 —— 单值即白名单。
	// 上线前需在 IdP 侧注册该回跳地址。
	PostLogoutRedirectURI string

	// EndSessionURL 覆盖/兜底 IdP 的 end_session 端点。优先级高于 Discovery 解析值,
	// 仅在 Discovery 未暴露 end_session_endpoint 时才需要配置。
	EndSessionURL string

	// IDTokenTTL callback 成功后缓存 id_token(供 logout 当 id_token_hint)的 TTL。
	// 默认对齐 RT 生命周期(7 天 = 168h),覆盖用户登录后较长时间才登出的场景。
	// 注意 env 值用 time.ParseDuration 解析,只认 h/m/s —— 写 "7d" 会解析失败并静默
	// 回落默认,要 7 天请填 "168h"。
	IDTokenTTL time.Duration
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
		ID:   getStringWithAlias("DM_OIDC_PROVIDER_ID", "", "oidc"),
		Name: getStringWithAlias("DM_OIDC_PROVIDER_NAME", "", "SSO"),
		// 缺省 oidc:存量部署无此 env,行为必须不变。
		Kind:         ProviderKind(getString("OCTO_OIDC_PROVIDER_KIND", string(KindOIDC))),
		BaseURL:      getString("OCTO_OIDC_PROVIDER_BASE_URL", ""),
		AppID:        getString("OCTO_OIDC_PROVIDER_APP_ID", ""),
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

		// RP-Initiated Logout(可选):缺省即禁用 end_session 跳转,纯增量不影响存量部署。
		PostLogoutRedirectURI: getString("OCTO_OIDC_POST_LOGOUT_REDIRECT_URI", ""),
		EndSessionURL:         getString("OCTO_OIDC_PROVIDER_END_SESSION_URL", ""),
		IDTokenTTL:            getDurationWithAlias("OCTO_OIDC_PROVIDER_ID_TOKEN_TTL", "", 7*24*time.Hour),
	}

	// 用 slice 保证检查顺序稳定,缺多个字段时报第一项固定,排查体验更好。
	// 报错消息用新名,引导运维迁移到 PROVIDER_*。
	//
	// NOTE: 此 required 列表在 modules/common/system_settings.go 的
	// isOIDCFullyConfigured() 有一份镜像副本(避免 common→oidc→user→common
	// import 循环)。新增/删除必填项时,两处必须同步修改。
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

	// IDTokenTTL<=0 会让 Redis SET 的过期变成"永不过期"(go-redis 语义),id_token
	// 密文将永久驻留。误配 0 / 负值时钳回默认 7d,杜绝该 footgun。
	if p.IDTokenTTL <= 0 {
		p.IDTokenTTL = 7 * 24 * time.Hour
	}

	// RP-Initiated Logout 的两个 URL 都会进浏览器顶层跳转(end_session 还携带 id_token),
	// 启动期 fail-loud 校验为绝对 https,拦相对地址 / javascript: 等,杜绝误配把 token
	// 发去任意域或在导航时执行脚本。与 validateBindRedirectBase 同模式。空值=功能未开,跳过。
	if err := validateLogoutURL("OCTO_OIDC_POST_LOGOUT_REDIRECT_URI", p.PostLogoutRedirectURI); err != nil {
		return p, err
	}
	if err := validateLogoutURL("OCTO_OIDC_PROVIDER_END_SESSION_URL", p.EndSessionURL); err != nil {
		return p, err
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

	// kind 分叉放在最后:oauth2 分支的 BaseURL 回落依赖 Issuer 已校验非空。
	if err := applyKindConstraints(&p); err != nil {
		return p, err
	}
	return p, nil
}

// applyKindConstraints 按 provider kind 收窄配置,并拒绝该 kind 下语义不成立的项。
//
// 设计原则:对不适用的配置**启动期报错**而不是静默忽略。运维照抄另一个 kind 的
// 配置是常态,静默忽略会让人以为某项已生效(例如登出),等到线上才发现没配上。
//
// 本函数不新增任何必填 env。required 列表在
// modules/common/system_settings.go 的 isOIDCFullyConfigured() 有镜像副本
// (为避开 common→oidc→user→common 的 import 循环),两处漂移会让
// isOIDCFullyConfigured 误答 → anyThirdPartyLoginConfigured 误判 →
// login.local_off 的兜底静默翻回 false → 全站恢复密码登录。
func applyKindConstraints(p *ProviderConfig) error {
	switch p.Kind {
	case KindOIDC:
		// 标准路线:配置形状不变。拒绝决定交给 oidcboot ——
		// 它会挡住那些"配了但在这个 kind 下不会生效"的键。
		return kindRefusal(p)

	case KindOAuth2:
		// BaseURL 回落 Issuer:该 kind 下 Issuer 的语义是"身份命名空间",
		// 而多数部署里它同时就是站点根,所以可以兼作缺省值。
		// 回落必须在校验之前 —— 规则看的是最终会被使用的值。
		if p.BaseURL == "" {
			p.BaseURL = p.Issuer
		}
		if err := kindRefusal(p); err != nil {
			return err
		}

		// 以下是配置**收窄**(不是拒绝),留在本模块:

		// 该 IdP 的 authorize 端点只认 scope=read。运维照抄 OIDC 的 scope 列表
		// 会被上游直接拒,所以在这里收窄而不是原样透传。
		p.Scopes = []string{"read"}

		// 协议里没有 code_challenge 参数。抽象后由 ProviderCapabilities 消费。
		p.RequirePKCE = false

		// 该 IdP 返回 refresh_token,但文档从未给出刷新端点。留非零间隔只会让
		// sync worker 空转,并让运维误以为"IdP 侧封号 → 我方踢线"在工作。
		p.SyncInterval = 0
		return nil

	default:
		return kindRefusal(p)
	}
}

// kindRefusal 把"这份配置能不能起来"的判断委托给 pkg/oidcboot。
//
// 规则放在那个叶子包里而不是这里,是因为 modules/common 的
// isOIDCFullyConfigured() 需要同一个答案,而它不能 import 本包(本包传递依赖
// 它)。两边各写一份就是上一版的 bug:5 个新的致命条件只加在了这一侧,
// 于是一个 KIND 拼写错误会让端点全部 404、同时 login.local_off 仍被采信,
// SSO-only 部署因此没有任何可用登录方式。
func kindRefusal(p *ProviderConfig) error {
	return oidcboot.ValidateKind(oidcboot.KindInput{
		Kind:                  string(p.Kind),
		BaseURL:               p.BaseURL,
		AppID:                 p.AppID,
		EndSessionURL:         p.EndSessionURL,
		PostLogoutRedirectURI: p.PostLogoutRedirectURI,
		AutoLinkByEmail:       p.AutoLinkByEmail,
		RequireEmailVerified:  p.RequireEmailVerified,
		AllowInsecureUpstream: getBool("OCTO_OIDC_ALLOW_INSECURE_UPSTREAM", false),
	})
}

// validateLogoutURL 启动期 fail-loud 校验 RP-Initiated Logout 相关 URL 为绝对 https。
//
// 空值视作"功能未开",直接放行(可选配置)。非空时必须是绝对地址且 https,拦
// 相对地址 / javascript: / data: 等 —— 这两个值最终都会进浏览器顶层跳转,
// EndSessionURL 还携带 id_token,误配会把 token 发去任意域或在导航时执行脚本。
// 开发环境可用 OCTO_OIDC_LOGOUT_ALLOW_INSECURE=1 放宽到 http(与 bind 的同名机制对齐)。
func validateLogoutURL(envName, raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("oidc: invalid %s %q: %w", envName, raw, err)
	}
	if u.Host == "" {
		return fmt.Errorf("oidc: %s %q must be absolute (scheme://host/path)", envName, raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && getBool("OCTO_OIDC_LOGOUT_ALLOW_INSECURE", false) {
		return nil
	}
	return fmt.Errorf("oidc: %s %q must use https scheme "+
		"(set OCTO_OIDC_LOGOUT_ALLOW_INSECURE=1 to allow http for dev)", envName, raw)
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

// getBoolWithAlias 委托 pkg/oidcboot.EnvBool —— 单一定义。
//
// 曾经这里和 modules/common 各有一份,后者的注释还写着 "matching
// modules/oidc.getBoolWithAlias"。它们在"主键存在但解析失败"这一点上不一致,
// 而那一个分歧足以让 SSO-only 部署整体失去登录入口(见 EnvBool 的说明)。
// 一句声称同步的注释不是机制。
func getBoolWithAlias(primary, alias string, def bool) bool {
	return oidcboot.EnvBool(primary, alias, def)
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
