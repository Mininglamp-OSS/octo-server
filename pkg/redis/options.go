package redis

import (
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/config"
	liboredis "github.com/Mininglamp-OSS/octo-lib/pkg/redis"
	rd "github.com/go-redis/redis"
)

// OptionsOverride 允许调用方覆盖 BuildOptions 默认值。
// Addr / Password / TLSConfig 由 cfg 决定，不在此处暴露，避免绕过 TLS 设置。
type OptionsOverride func(*rd.Options)

// BuildOptions 根据 cfg.DB 中的 Redis 相关字段构造 *redis.Options。
//
// 统一处理 Addr / Password / TLSConfig，所有在 octo-server 内直接使用
// rd.NewClient 的位置（限流、OIDC、模块级 NewClient 等）都应通过本函数构造
// 参数，确保 TLS 配置不会被遗漏。其它字段（PoolSize / Timeout 等）通过
// override 函数传入。
//
// TLS 构造逻辑复用 octo-lib/pkg/redis.BuildTLSConfig，与 ctx.GetRedisConn()
// 链路保持一致。
func BuildOptions(cfg *config.Config, overrides ...OptionsOverride) (*rd.Options, error) {
	opts := &rd.Options{
		Addr:     cfg.DB.RedisAddr,
		Password: cfg.DB.RedisPass,
	}
	if cfg.DB.RedisTLS {
		tlsCfg, err := liboredis.BuildTLSConfig(
			cfg.DB.RedisTLSInsecureSkipVerify,
			cfg.DB.RedisTLSCAFile,
		)
		if err != nil {
			return nil, fmt.Errorf("redis: build tls config: %w", err)
		}
		opts.TLSConfig = tlsCfg
	}
	for _, o := range overrides {
		if o != nil {
			o(opts)
		}
	}
	return opts, nil
}

// MustBuildOptions 在 BuildOptions 失败时 panic。
// 仅用于启动期初始化场景 —— TLS CA 文件读取 / 解析失败属于配置错误，
// 进程应立即终止而非带病运行。
func MustBuildOptions(cfg *config.Config, overrides ...OptionsOverride) *rd.Options {
	opts, err := BuildOptions(cfg, overrides...)
	if err != nil {
		panic(err)
	}
	return opts
}
