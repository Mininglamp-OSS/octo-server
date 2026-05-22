package oidc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
)

// stateKeyPrefix Redis key 前缀
const stateKeyPrefix = "oidc:state:"

// luaGetDel 原子地获取并删除 key,保证 state 的一次性消费语义
//
// 用 Lua 而非 GET+DEL 两步,是为了消除 TOCTOU 窗口 — 否则两个并发回调
// 可能同时通过 GET 拿到值再各自 DEL,绕过 CSRF 一次性保护。
var luaGetDel = rd.NewScript(`
local v = redis.call("GET", KEYS[1])
if v then
  redis.call("DEL", KEYS[1])
end
return v
`)

// redisStateStore 生产环境 StateStore
//
// 持有独立 *redis.Client(go-redis v6),与 dmwork-lib 共用 Redis 后端但不共用 Conn 包装,
// 因为后者未暴露 Eval / 事务等高级原语。Read/WriteTimeout 提供命令级超时保护,
// 避免网络分区时阻塞整个 callback 处理路径。
type redisStateStore struct {
	client *rd.Client
}

func newRedisStateStore(ctx *config.Context) *redisStateStore {
	client := rd.NewClient(octoredis.MustBuildOptions(ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 3
		o.ReadTimeout = 3 * time.Second
		o.WriteTimeout = 3 * time.Second
		o.DialTimeout = 3 * time.Second
	}))
	return &redisStateStore{client: client}
}

// Save / Consume 接受 context.Context 是为了满足 StateStore 接口契约,
// 但 go-redis v6 的命令 API 不支持 context 取消;cancellation 由 Read/WriteTimeout
// 替代防住网络阻塞。升级到 go-redis v8+ 时把 context 真正下推到底层 Cmd 即可。
func (s *redisStateStore) Save(_ context.Context, state string, data *StateData, ttl time.Duration) error {
	if state == "" {
		return errors.New("oidc: state key required")
	}
	encoded, err := encodeStateData(data)
	if err != nil {
		return err
	}
	if err := s.client.Set(stateKey(state), encoded, ttl).Err(); err != nil {
		return fmt.Errorf("oidc: redis set state: %w", err)
	}
	return nil
}

func (s *redisStateStore) Consume(_ context.Context, state string) (*StateData, error) {
	if state == "" {
		return nil, ErrStateNotFound
	}
	res, err := luaGetDel.Run(s.client, []string{stateKey(state)}).Result()
	if err != nil {
		if errors.Is(err, rd.Nil) {
			return nil, ErrStateNotFound
		}
		return nil, fmt.Errorf("oidc: redis getdel state: %w", err)
	}
	raw, ok := res.(string)
	if !ok || raw == "" {
		return nil, ErrStateNotFound
	}
	return decodeStateData(raw)
}

// Close 释放底层 Redis 连接池,应在模块/进程优雅关闭时调用。
func (s *redisStateStore) Close() error {
	if s.client == nil {
		return nil
	}
	if err := s.client.Close(); err != nil {
		return fmt.Errorf("oidc: redis state store close: %w", err)
	}
	return nil
}

func stateKey(state string) string {
	return stateKeyPrefix + state
}
