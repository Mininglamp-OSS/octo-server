package db

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/pkg/redis"
)

// NewRedis 兼容旧调用方，不会启用 TLS。
// 新代码应优先用 NewRedisFromConfig 以自动应用 TLS 等公共配置。
func NewRedis(addr string, password string) *redis.Conn {
	return redis.New(addr, password)
}

// NewRedisFromConfig 从 cfg 构造 Redis 连接，自动应用 RedisTLS / CA 等设置。
func NewRedisFromConfig(cfg *config.Config) *redis.Conn {
	return redis.NewWithOptions(redis.MustBuildOptions(cfg))
}
