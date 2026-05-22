package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
)

const (
	bindSessionKeyPrefix = "oidc:bind:sess:"
	bindCounterKeyPrefix = "oidc:bind:cnt:"
)

// luaBindCASUpdate 原子地校验当前 Status 字段并改写整段 JSON,刷新 TTL。
//
// 逻辑:
//  1. GET key,不存在 → 返 "notfound"
//  2. JSON 解出 "status",与 ARGV[1] 不等 → 返 "conflict"
//  3. 写入 ARGV[2](新 JSON),PEXPIRE ARGV[3](毫秒) → 返 "ok"
//
// 用 Lua 而非客户端读改写,保证 CAS 在 Redis 单线程里完成。
// JSON 解析放 Lua 端不太美,但 Redis 7+ 内置 cjson,工程上比客户端两次 RTT
// 简单。**不依赖 cjson 时**改方案:把 status 与 JSON 拆成两个 key(status 独
// 立 SET, payload 独立 SET),CAS 用 GETSET status。当前实现优先复用单 key。
var luaBindCASUpdate = rd.NewScript(`
local raw = redis.call("GET", KEYS[1])
if not raw then return "notfound" end
local ok, payload = pcall(cjson.decode, raw)
if not ok then return "decode_err" end
if payload.status ~= ARGV[1] then return "conflict" end
redis.call("SET", KEYS[1], ARGV[2])
redis.call("PEXPIRE", KEYS[1], ARGV[3])
return "ok"
`)

// luaBindGetDel 原子地 GET + DEL,与 luaGetDel 同模式,服务 Consume 的单次消费语义。
var luaBindGetDel = rd.NewScript(`
local v = redis.call("GET", KEYS[1])
if v then redis.call("DEL", KEYS[1]) end
return v
`)

// luaBindIncrAndCheck INCR + (首次 EXPIRE) + 与 limit 比对。
//
// 首次 +1 时 TTL 缺失就 PEXPIRE,后续命中不刷新 TTL —— 这是"滑动窗口 vs 固
// 定窗口"的取舍:
//   - 固定窗口(本实现):TTL 在第一次 +1 时确定,窗口结束后整体清零;
//   - 滑动窗口:每次 +1 都续 TTL,攻击者持续尝试 → 永远不解锁。
//
// 选固定窗口是因为 bind_token 自身 5min TTL 已经够小,counter 的窗口应当
// 不超过 token 寿命 + 一点容差,避免 token 已过期但 counter 残留下次绑定
// 误报"超限"。CallbackGuard 用滑动窗口是因为它跨 token 起防扫描作用,语义不同。
var luaBindIncrAndCheck = rd.NewScript(`
local n = redis.call("INCR", KEYS[1])
if n == 1 then redis.call("PEXPIRE", KEYS[1], ARGV[2]) end
local limit = tonumber(ARGV[1])
if n > limit then return {n, 1} end
return {n, 0}
`)

type redisBindStore struct {
	client *rd.Client
}

func newRedisBindStore(ctx *config.Context) *redisBindStore {
	client := rd.NewClient(octoredis.MustBuildOptions(ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 3
		o.ReadTimeout = 3 * time.Second
		o.WriteTimeout = 3 * time.Second
		o.DialTimeout = 3 * time.Second
	}))
	return &redisBindStore{client: client}
}

func bindSessionKey(jti string) string { return bindSessionKeyPrefix + jti }
func bindCounterKey(key string) string { return bindCounterKeyPrefix + key }

func (s *redisBindStore) Save(_ context.Context, sess *BindSession, ttl time.Duration) error {
	if sess == nil || sess.JTI == "" {
		return errors.New("oidc: bind session: jti required")
	}
	if ttl <= 0 {
		return fmt.Errorf("oidc: bind session: ttl must be positive, got %v", ttl)
	}
	encoded, err := json.Marshal(sess)
	if err != nil {
		return fmt.Errorf("oidc: bind session marshal: %w", err)
	}
	if err := s.client.Set(bindSessionKey(sess.JTI), encoded, ttl).Err(); err != nil {
		return fmt.Errorf("oidc: redis save bind session: %w", err)
	}
	return nil
}

func (s *redisBindStore) Get(_ context.Context, jti string) (*BindSession, error) {
	if jti == "" {
		return nil, ErrBindNotFound
	}
	raw, err := s.client.Get(bindSessionKey(jti)).Result()
	if err != nil {
		if errors.Is(err, rd.Nil) {
			return nil, ErrBindNotFound
		}
		return nil, fmt.Errorf("oidc: redis get bind session: %w", err)
	}
	return decodeBindSession([]byte(raw))
}

func (s *redisBindStore) CASSave(_ context.Context, sess *BindSession, expected BindStatus, ttl time.Duration) error {
	if sess == nil || sess.JTI == "" {
		return errors.New("oidc: bind CASSave: sess/jti required")
	}
	if ttl <= 0 {
		return fmt.Errorf("oidc: bind CASSave: ttl must be positive, got %v", ttl)
	}
	encoded, err := json.Marshal(sess)
	if err != nil {
		return fmt.Errorf("oidc: bind session marshal: %w", err)
	}
	// 调用方已经在 sess 上设了 new status + 业务字段(如 CandidateUID /
	// VerifiedMethod)。luaBindCASUpdate 在 Redis 单线程里 GET → 校验当前
	// status == expected → SET 整段 → PEXPIRE。并发场景下两个调用即便都
	// 基于 status=issued 的旧快照,只有一个能把 status 推进到 verified;
	// 另一个看到 status 已是 verified,返 conflict。
	res, err := luaBindCASUpdate.Run(
		s.client,
		[]string{bindSessionKey(sess.JTI)},
		string(expected), string(encoded), ttl.Milliseconds(),
	).Result()
	if err != nil {
		return fmt.Errorf("oidc: redis cas update bind: %w", err)
	}
	switch res {
	case "ok":
		return nil
	case "notfound":
		return ErrBindNotFound
	case "conflict":
		return ErrBindStatusConflict
	default:
		return fmt.Errorf("oidc: bind CAS unexpected lua result: %v", res)
	}
}

func (s *redisBindStore) Consume(_ context.Context, jti string) (*BindSession, error) {
	if jti == "" {
		return nil, ErrBindNotFound
	}
	res, err := luaBindGetDel.Run(s.client, []string{bindSessionKey(jti)}).Result()
	if err != nil {
		if errors.Is(err, rd.Nil) {
			return nil, ErrBindNotFound
		}
		return nil, fmt.Errorf("oidc: redis consume bind: %w", err)
	}
	raw, ok := res.(string)
	if !ok || raw == "" {
		return nil, ErrBindNotFound
	}
	return decodeBindSession([]byte(raw))
}

func (s *redisBindStore) IncrAndCheck(_ context.Context, key string, limit int64, ttl time.Duration) (int64, error) {
	if key == "" {
		return 0, errors.New("oidc: bind IncrAndCheck: key required")
	}
	if limit <= 0 {
		return 0, fmt.Errorf("oidc: bind IncrAndCheck: limit must be positive, got %d", limit)
	}
	if ttl <= 0 {
		return 0, fmt.Errorf("oidc: bind IncrAndCheck: ttl must be positive, got %v", ttl)
	}
	res, err := luaBindIncrAndCheck.Run(
		s.client,
		[]string{bindCounterKey(key)},
		limit, ttl.Milliseconds(),
	).Result()
	if err != nil {
		return 0, fmt.Errorf("oidc: redis incr bind counter: %w", err)
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) != 2 {
		return 0, fmt.Errorf("oidc: bind counter unexpected lua result: %v", res)
	}
	count, _ := arr[0].(int64)
	exceededFlag, _ := arr[1].(int64)
	if exceededFlag == 1 {
		return count, ErrBindRateLimited
	}
	return count, nil
}

// Close 释放底层 Redis 连接池。模块停机时调用,与 redisStateStore.Close 同模式。
func (s *redisBindStore) Close() error {
	if s.client == nil {
		return nil
	}
	if err := s.client.Close(); err != nil {
		return fmt.Errorf("oidc: redis bind store close: %w", err)
	}
	return nil
}
