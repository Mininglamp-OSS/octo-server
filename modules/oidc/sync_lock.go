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

// tickSyncLockKey 单实例 SyncWorker tick 互斥的 Redis key。
//
// 同一时刻只允许一个 SyncWorker 实例在跑 RunOnce,避免 N 实例 = N×IdP 流量。
const tickSyncLockKey = "oidc:sync:tick"

// tickLock 分布式互斥锁的最小接口。
//
// 设计原则(对应 Redlock 的常见错误规避):
//
//   - Acquire 必须原子(SET NX EX),不能 GET-then-SET。
//   - Release 必须 token-aware:只有 token 与持有者匹配才能 DEL,否则
//     "lease 续约失败 / 进程暂停" 场景下原 owner 在新 owner 持锁期间
//     调 Release 会误删别人的锁,触发"两实例同时进入临界区"。
//   - 不持有锁时 Release 返回 (false, nil),不当作错误。
type tickLock interface {
	Acquire(ctx context.Context, key, token string, ttl time.Duration) (bool, error)
	Release(ctx context.Context, key, token string) (bool, error)
}

// luaReleaseLock CAS-DEL:仅当 KEYS[1] 的 value == ARGV[1] 时才 DEL。
//
// 必须原子,否则 GET → 比较 → DEL 三步会形成 TOCTOU,在 lease 边界
// 上误删后续 owner 的锁。脚本返回 1 表示成功 DEL,0 表示 token 不匹配
// 或 key 已不存在。
var luaReleaseLock = rd.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
else
  return 0
end
`)

// RedisTickLock 用 Redis SET NX EX + Lua CAS-DEL 实现 tickLock。
//
// 持有独立 *redis.Client 与 redisStateStore 同样的理由(详见该文件):
// dmwork-lib 的 Redis wrapper 不暴露 Eval/SETNX 等高级原语。
type RedisTickLock struct {
	client *rd.Client
}

func newRedisTickLock(ctx *config.Context) *RedisTickLock {
	client := rd.NewClient(octoredis.MustBuildOptions(ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 3
		o.ReadTimeout = 3 * time.Second
		o.WriteTimeout = 3 * time.Second
		o.DialTimeout = 3 * time.Second
	}))
	return &RedisTickLock{client: client}
}

// Acquire 用 SET NX EX 原子抢锁。token 后续 Release 校验用,必须每次新生成。
//
// 返回 (true, nil)=抢到, (false, nil)=别人持锁(正常竞争), (_, err)=Redis 故障。
func (l *RedisTickLock) Acquire(_ context.Context, key, token string, ttl time.Duration) (bool, error) {
	if key == "" || token == "" || ttl <= 0 {
		return false, fmt.Errorf("oidc: tickLock Acquire: key/token/ttl required")
	}
	// go-redis v6 的 SetNX 直接对应 SET NX,带 expire 参数即原子 SET NX EX。
	ok, err := l.client.SetNX(key, token, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("oidc: tickLock Acquire %q: %w", key, err)
	}
	return ok, nil
}

// Release 走 Lua CAS-DEL,只在 token 匹配时释放。
//
// 返回 (true, nil)=成功释放, (false, nil)=token 不匹配 / key 已过期(都视为
// 正常,不报错), (_, err)=Redis 故障。
func (l *RedisTickLock) Release(_ context.Context, key, token string) (bool, error) {
	if key == "" || token == "" {
		return false, fmt.Errorf("oidc: tickLock Release: key/token required")
	}
	res, err := luaReleaseLock.Run(l.client, []string{key}, token).Result()
	if err != nil {
		if errors.Is(err, rd.Nil) {
			return false, nil
		}
		return false, fmt.Errorf("oidc: tickLock Release %q: %w", key, err)
	}
	// Redis Lua 返回 number 在 go-redis 反射成 int64;若哪天行为改了或被中间件
	// 包装成别的类型,silently 退到 0 会让"以为释放了实际没有"的 bug 难查。
	n, ok := res.(int64)
	if !ok {
		return false, fmt.Errorf("oidc: tickLock Release %q: unexpected lua result type %T", key, res)
	}
	return n == 1, nil
}

// Close 释放底层连接池。
func (l *RedisTickLock) Close() error {
	if l.client == nil {
		return nil
	}
	if err := l.client.Close(); err != nil {
		return fmt.Errorf("oidc: tickLock close: %w", err)
	}
	return nil
}
