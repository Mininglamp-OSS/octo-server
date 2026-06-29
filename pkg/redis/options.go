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

// NewInstrumentedClient 用 cfg(+overrides) 构造一个裸 *rd.Client，并在返回前挂上
// octo-lib 的每条命令计时 hook（liboredis.Instrument），使其命令进入
// dependency="redis" 指标。
//
// octo-server 内所有需要裸 *rd.Client 的场景（限流令牌桶、OIDC 锁、health 探针等
// 需要 Eval/Script/SetNX、无法用 lib 的 Conn 包装的地方）都应通过本函数构造 —— 既
// 统一了 TLS（经 BuildOptions），又确保插桩不被漏掉。插桩在构造时、client 被共享
// 前完成，满足 octo-lib Instrument 的「共享前插桩」契约。
//
// 与 MustBuildOptions 一样，TLS 配置错误属启动期配置错误，直接 panic。
func NewInstrumentedClient(cfg *config.Config, overrides ...OptionsOverride) *rd.Client {
	return InstrumentedClientFromOptions(MustBuildOptions(cfg, overrides...))
}

// InstrumentedClientFromOptions 用调用方预构造的 *rd.Options 建裸 *rd.Client 并插桩。
// 供少数已自行拼好 Options 的场景（如 health 探针）使用；一般情况优先用
// NewInstrumentedClient。
func InstrumentedClientFromOptions(opts *rd.Options) *rd.Client {
	// 防御性复制：go-redis 会就地写入若干默认值，不复制可能在调用方复用同一
	// *rd.Options 时串改。与 octo-lib redis.NewWithOptions 的处理保持一致。
	local := *opts
	c := rd.NewClient(&local)
	liboredis.Instrument(c)
	return c
}
