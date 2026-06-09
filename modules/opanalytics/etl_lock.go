package opanalytics

import (
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
)

// etlRunLockKey 每日 ETL 的分布式互斥锁 key。多副本部署时同一时刻只允许一个实例
// 真正执行 RunIncremental，避免 N 实例同跑同一轮造成的重复压力/锁竞争(验收④)。
//
// 注:correctness 还有第二道保险——runChunk 内对水位行 `SELECT ... FOR UPDATE`
// 串行化，故即便锁因 TTL 过期出现短暂并发，消息仍精确一次累加。
const etlRunLockKey = "opanalytics:etl:run"

// etlRunLockTTL 锁租约。执行期间由 scheduler/手动触发路径定期续租，用于覆盖首次全量回填
// 这类可能超过单个 TTL 的长任务；若进程崩溃，TTL 仍会自动释放锁。
const etlRunLockTTL = 30 * time.Minute

// luaReleaseETLLock CAS-DEL:仅当 token 匹配时才释放(规避 lease 边界误删后继 owner 锁)。
var luaReleaseETLLock = rd.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
else
  return 0
end
`)

// luaRenewETLLock 仅当 token 匹配当前 owner 时续租，避免误延长后继 owner 的锁。
var luaRenewETLLock = rd.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("PEXPIRE", KEYS[1], ARGV[2])
else
  return 0
end
`)

// etlLock 用 Redis SET NX EX + Lua CAS-DEL 实现的单实例 ETL 互斥锁(仿 modules/oidc 的 RedisTickLock)。
type etlLock struct {
	client *rd.Client
}

func newETLLock(ctx *config.Context) *etlLock {
	client := rd.NewClient(octoredis.MustBuildOptions(ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 3
		o.ReadTimeout = 3 * time.Second
		o.WriteTimeout = 3 * time.Second
		o.DialTimeout = 3 * time.Second
	}))
	return &etlLock{client: client}
}

// Acquire 用 SET NX EX 原子抢锁。返回 (true,nil)=抢到, (false,nil)=别人持锁, (_,err)=Redis 故障。
func (l *etlLock) Acquire(token string) (bool, error) {
	ok, err := l.client.SetNX(etlRunLockKey, token, etlRunLockTTL).Result()
	if err != nil {
		return false, fmt.Errorf("opanalytics: etl lock acquire: %w", err)
	}
	return ok, nil
}

// Release 走 Lua CAS-DEL，只在 token 匹配时释放(token 不匹配/已过期均视为正常，不报错)。
func (l *etlLock) Release(token string) error {
	_, err := luaReleaseETLLock.Run(l.client, []string{etlRunLockKey}, token).Result()
	if err != nil && !errors.Is(err, rd.Nil) {
		return fmt.Errorf("opanalytics: etl lock release: %w", err)
	}
	return nil
}

// Renew 在当前 token 仍持有锁时延长租约。返回 false 表示锁已过期或 owner 已变化。
func (l *etlLock) Renew(token string) (bool, error) {
	res, err := luaRenewETLLock.Run(l.client, []string{etlRunLockKey}, token, etlRunLockTTL.Milliseconds()).Result()
	if err != nil && !errors.Is(err, rd.Nil) {
		return false, fmt.Errorf("opanalytics: etl lock renew: %w", err)
	}
	n, ok := res.(int64)
	return ok && n == 1, nil
}

// Close 释放底层连接池。
func (l *etlLock) Close() error {
	if l.client == nil {
		return nil
	}
	return l.client.Close()
}
