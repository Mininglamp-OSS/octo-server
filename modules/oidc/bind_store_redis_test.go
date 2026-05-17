package oidc

import (
	"context"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
)

// TestRedisBindStore_Behavior_Integration redis impl 跑同一组行为契约,
// 走真 Redis(testutil.NewTestServer 接的 127.0.0.1:6379)。
//
// 与 memory impl 共用 runBindStoreBehaviorSuite,任何只在某一边过的
// 测试都揭示了 impl 偏差(尤其是 CAS lua / TTL 行为)。
func TestRedisBindStore_Behavior_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	runBindStoreBehaviorSuite(t, func(t *testing.T) BindStore {
		t.Helper()
		store := newRedisBindStore(ctx)
		// 每个 subtest 拿到的 store 共享同一 redis 后端,但 keys 用 jti
		// 隔离(测试已选独立 JTI/counter key)。这里只为 Close 注册 cleanup。
		t.Cleanup(func() {
			cleanupRedisBindStore(t, store)
			if err := store.Close(); err != nil {
				t.Logf("close: %v", err)
			}
		})
		return store
	})
}

// cleanupRedisBindStore 清掉本次 subtest 用到的所有 bind session + counter key,
// 防止 testutil 不 flush Redis 时跨 subtest 串扰(同 binary 多次跑同一 jti
// 会让 Save 之前残留状态干扰 CAS / IncrAndCheck 断言)。
//
// 用 SCAN 而非 KEYS 是怕 prod Redis 复用——但这里是 _Integration,直接用 KEYS
// 也行;统一保守一点。
func cleanupRedisBindStore(t *testing.T, store *redisBindStore) {
	t.Helper()
	ctx := context.Background()
	for _, prefix := range []string{bindSessionKeyPrefix + "*", bindCounterKeyPrefix + "*"} {
		var cursor uint64
		for {
			keys, next, err := store.client.Scan(cursor, prefix, 100).Result()
			if err != nil {
				t.Logf("scan %s: %v", prefix, err)
				break
			}
			if len(keys) > 0 {
				if err := store.client.Del(keys...).Err(); err != nil {
					t.Logf("del %d keys: %v", len(keys), err)
				}
			}
			if next == 0 {
				break
			}
			cursor = next
		}
	}
	// 给 Redis 一个被命令送达的 ack 时间 —— Scan 是流式,Del 是异步可见;
	// 紧跟一个 Get 全清场,避免下个 subtest 撞旧 key。
	_ = ctx
	time.Sleep(20 * time.Millisecond)
}
