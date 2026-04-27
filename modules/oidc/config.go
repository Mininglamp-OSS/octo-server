package oidc

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// 环境变量命名约定:
//
//   TS_*  — Viper 管理的核心配置(MySQL / Redis / WuKongIM 等),由 dmwork-lib
//           的 Config 结构体反序列化,与 YAML 字段一一对应。
//   DM_*  — 模块自管的功能开关与第三方对接配置(thread / space / oidc 等),
//           由模块直接 os.Getenv 读取,不经 Viper。
//
// OIDC 走 DM_ 是因为 dmwork-lib 暂未支持 OIDC 配置块;dmwork-lib 后续补齐 OIDC
// 字段后,本模块迁移到 cfg.OIDC.* 即可,env 仍可作为运行期 override 保留。

// Config OIDC 模块完整配置
type Config struct {
	Enabled bool
	Aegis   ProviderConfig
}

// ProviderConfig 单个 OIDC Provider 配置(本期只用 Aegis,字段为 Google/Okta 等扩展预留)
type ProviderConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURI  string
	Scopes       []string

	RequireEmailVerified bool
	RequirePKCE          bool
	AutoLinkByEmail      bool
	AllowNewUser         bool

	ClockSkew   time.Duration
	HTTPTimeout time.Duration

	SyncInterval    time.Duration
	SyncConcurrency int

	// AES-256-GCM 主密钥,用于加密 refresh_token,从 base64 字符串解码
	RefreshTokenEncryptionKey []byte
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

	p, err := loadAegis()
	if err != nil {
		return nil, fmt.Errorf("oidc: load aegis: %w", err)
	}
	cfg.Aegis = p
	return cfg, nil
}

func loadAegis() (ProviderConfig, error) {
	p := ProviderConfig{
		Issuer:       getString("DM_OIDC_AEGIS_ISSUER", ""),
		ClientID:     getString("DM_OIDC_AEGIS_CLIENT_ID", ""),
		ClientSecret: getString("DM_OIDC_AEGIS_CLIENT_SECRET", ""),
		RedirectURI:  getString("DM_OIDC_AEGIS_REDIRECT_URI", ""),
		Scopes: getStringSlice("DM_OIDC_AEGIS_SCOPES",
			[]string{"openid", "profile", "email", "phone", "offline_access"}),

		RequireEmailVerified: getBool("DM_OIDC_AEGIS_REQUIRE_EMAIL_VERIFIED", true),
		RequirePKCE:          getBool("DM_OIDC_AEGIS_REQUIRE_PKCE", true),
		AutoLinkByEmail:      getBool("DM_OIDC_AEGIS_AUTO_LINK_BY_EMAIL", true),
		AllowNewUser:         getBool("DM_OIDC_AEGIS_ALLOW_NEW_USER", true),

		ClockSkew:   getDuration("DM_OIDC_AEGIS_CLOCK_SKEW", 60*time.Second),
		HTTPTimeout: getDuration("DM_OIDC_AEGIS_HTTP_TIMEOUT", 10*time.Second),

		SyncInterval:    getDuration("DM_OIDC_AEGIS_SYNC_INTERVAL", 15*time.Minute),
		SyncConcurrency: getInt("DM_OIDC_AEGIS_SYNC_CONCURRENCY", 10),
	}

	// 用 slice 保证检查顺序稳定,缺多个字段时报第一项固定,排查体验更好
	required := []struct {
		name string
		val  string
	}{
		{"DM_OIDC_AEGIS_ISSUER", p.Issuer},
		{"DM_OIDC_AEGIS_CLIENT_ID", p.ClientID},
		{"DM_OIDC_AEGIS_CLIENT_SECRET", p.ClientSecret},
		{"DM_OIDC_AEGIS_REDIRECT_URI", p.RedirectURI},
	}
	for _, r := range required {
		if r.val == "" {
			return p, fmt.Errorf("required env %s is empty", r.name)
		}
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

func getInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func getDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
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
