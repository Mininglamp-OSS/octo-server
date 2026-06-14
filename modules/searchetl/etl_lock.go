package searchetl

import (
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
)

// etlRunLockKey 是 searchetl 增量抽取的分布式互斥锁 key。与 opanalytics 的
// `opanalytics:etl:run` 独立——两条 ETL 各跑各的游标，绝不共用一把锁。
//
// 🔴 单副本互斥（C3）：plan §3.5 把 opanalytics 的「读游标 + 推进同一 FOR UPDATE 事务」
// 拆成三段后，FOR UPDATE 行级第二防线消失。须靠本 Redis 锁全程持有 + 续租，且续租失败必须
// 立即 abort 在飞批次（阶段 3 落地）。阶段 1 骨架先就位锁原语。
const etlRunLockKey = "searchetl:etl:run"

// etlRunLockTTL 锁租约。执行期间定期续租以覆盖可能超过单 TTL 的长任务（如全量 backfill）；
// 进程崩溃则 TTL 自动释放。
const etlRunLockTTL = 30 * time.Minute

// luaReleaseETLLock CAS-DEL：仅当 token 匹配时才释放（规避 lease 边界误删后继 owner 锁）。
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

// etlLock 用 Redis SET NX EX + Lua CAS-DEL/CAS-PEXPIRE 实现的单实例 ETL 互斥锁
// （仿 opanalytics etlLock）。
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
		return false, fmt.Errorf("searchetl: etl lock acquire: %w", err)
	}
	return ok, nil
}

// Release 走 Lua CAS-DEL，只在 token 匹配时释放（token 不匹配/已过期均视为正常，不报错）。
func (l *etlLock) Release(token string) error {
	_, err := luaReleaseETLLock.Run(l.client, []string{etlRunLockKey}, token).Result()
	if err != nil && !errors.Is(err, rd.Nil) {
		return fmt.Errorf("searchetl: etl lock release: %w", err)
	}
	return nil
}

// Renew 在当前 token 仍持有锁时延长租约。返回 false 表示锁已过期或 owner 已变化
// （阶段 3：续租失败 → 立即 abort 在飞批次，C3）。
func (l *etlLock) Renew(token string) (bool, error) {
	res, err := luaRenewETLLock.Run(l.client, []string{etlRunLockKey}, token, etlRunLockTTL.Milliseconds()).Result()
	if err != nil && !errors.Is(err, rd.Nil) {
		return false, fmt.Errorf("searchetl: etl lock renew: %w", err)
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
