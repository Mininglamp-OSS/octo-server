package oidc

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 一套行为契约,在 memory + redis 两个 impl 上跑同一组断言。
//
// redis impl 单元测试只 build 不跑(需要真 Redis);redis 路径由
// bind_store_redis_test.go 的 _Integration 测试覆盖。

func runBindStoreBehaviorSuite(t *testing.T, factory func(t *testing.T) BindStore) {
	t.Helper()

	t.Run("Save+Get roundtrip", func(t *testing.T) {
		store := factory(t)
		sess := &BindSession{
			JTI: "j-1", Issuer: "https://idp", Subject: "sub-1",
			Status: BindStatusIssued, CreatedAt: time.Now().Unix(),
			ClaimsSnapshot: []byte(`{"sub":"sub-1"}`),
			SDSnapshot:     []byte(`{"authcode":"ac-1"}`),
		}
		if err := store.Save(context.Background(), sess, time.Minute); err != nil {
			t.Fatalf("Save: %v", err)
		}
		got, err := store.Get(context.Background(), "j-1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.JTI != "j-1" || got.Issuer != "https://idp" || got.Subject != "sub-1" {
			t.Fatalf("identity mismatch: %+v", got)
		}
		if got.Status != BindStatusIssued {
			t.Fatalf("status=%v", got.Status)
		}
		if string(got.ClaimsSnapshot) != `{"sub":"sub-1"}` {
			t.Fatalf("claims snapshot lost: %q", got.ClaimsSnapshot)
		}
		if string(got.SDSnapshot) != `{"authcode":"ac-1"}` {
			t.Fatalf("sd snapshot lost: %q", got.SDSnapshot)
		}
	})

	t.Run("Get missing returns ErrBindNotFound", func(t *testing.T) {
		store := factory(t)
		_, err := store.Get(context.Background(), "j-missing")
		if !errors.Is(err, ErrBindNotFound) {
			t.Fatalf("expected ErrBindNotFound, got %v", err)
		}
	})

	t.Run("Get empty jti returns ErrBindNotFound", func(t *testing.T) {
		store := factory(t)
		if _, err := store.Get(context.Background(), ""); !errors.Is(err, ErrBindNotFound) {
			t.Fatalf("empty jti must return ErrBindNotFound, got %v", err)
		}
	})

	t.Run("Save with zero ttl rejected", func(t *testing.T) {
		store := factory(t)
		err := store.Save(context.Background(), &BindSession{JTI: "j-bad", Status: BindStatusIssued}, 0)
		if err == nil {
			t.Fatal("Save with ttl=0 must reject (caller bug)")
		}
	})

	t.Run("UpdateStatus CAS transitions then conflicts", func(t *testing.T) {
		store := factory(t)
		sess := &BindSession{JTI: "j-cas", Status: BindStatusIssued, CreatedAt: time.Now().Unix()}
		if err := store.Save(context.Background(), sess, time.Minute); err != nil {
			t.Fatalf("Save: %v", err)
		}
		// 正确的迁移
		if err := store.UpdateStatus(context.Background(), "j-cas",
			BindStatusIssued, BindStatusVerified, time.Minute); err != nil {
			t.Fatalf("UpdateStatus issued->verified: %v", err)
		}
		// 重复同样的迁移应当 CAS 失败 —— 不变式:expected != actual 必返冲突
		err := store.UpdateStatus(context.Background(), "j-cas",
			BindStatusIssued, BindStatusVerified, time.Minute)
		if !errors.Is(err, ErrBindStatusConflict) {
			t.Fatalf("expected ErrBindStatusConflict on stale expected, got %v", err)
		}
		// 现在状态是 verified,可以推进到 confirmed
		if err := store.UpdateStatus(context.Background(), "j-cas",
			BindStatusVerified, BindStatusConfirmed, time.Minute); err != nil {
			t.Fatalf("UpdateStatus verified->confirmed: %v", err)
		}
		got, err := store.Get(context.Background(), "j-cas")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != BindStatusConfirmed {
			t.Fatalf("expected confirmed, got %v", got.Status)
		}
	})

	t.Run("UpdateStatus on missing returns ErrBindNotFound", func(t *testing.T) {
		store := factory(t)
		err := store.UpdateStatus(context.Background(), "j-nope",
			BindStatusIssued, BindStatusVerified, time.Minute)
		if !errors.Is(err, ErrBindNotFound) {
			t.Fatalf("expected ErrBindNotFound, got %v", err)
		}
	})

	t.Run("Consume returns then deletes", func(t *testing.T) {
		store := factory(t)
		sess := &BindSession{JTI: "j-once", Status: BindStatusVerified, CreatedAt: time.Now().Unix()}
		if err := store.Save(context.Background(), sess, time.Minute); err != nil {
			t.Fatalf("Save: %v", err)
		}
		first, err := store.Consume(context.Background(), "j-once")
		if err != nil {
			t.Fatalf("Consume first: %v", err)
		}
		if first.Status != BindStatusVerified {
			t.Fatalf("returned session status=%v", first.Status)
		}
		// SR-1: 单次消费 —— 第二次 Consume 必须 NotFound
		_, err = store.Consume(context.Background(), "j-once")
		if !errors.Is(err, ErrBindNotFound) {
			t.Fatalf("expected ErrBindNotFound on second consume, got %v", err)
		}
		// Get 也应当 NotFound
		_, err = store.Get(context.Background(), "j-once")
		if !errors.Is(err, ErrBindNotFound) {
			t.Fatalf("expected ErrBindNotFound on Get after consume, got %v", err)
		}
	})

	t.Run("IncrAndCheck accumulates and exceeds limit", func(t *testing.T) {
		store := factory(t)
		// limit=3, 前 3 次应当成功(返回 1, 2, 3),第 4 次 ErrBindRateLimited
		for i := int64(1); i <= 3; i++ {
			n, err := store.IncrAndCheck(context.Background(), "bind:test:counter1", 3, time.Minute)
			if err != nil {
				t.Fatalf("incr #%d unexpected err: %v", i, err)
			}
			if n != i {
				t.Fatalf("incr #%d count=%d want %d", i, n, i)
			}
		}
		n, err := store.IncrAndCheck(context.Background(), "bind:test:counter1", 3, time.Minute)
		if !errors.Is(err, ErrBindRateLimited) {
			t.Fatalf("expected ErrBindRateLimited on 4th incr, got err=%v count=%d", err, n)
		}
		if n != 4 {
			t.Fatalf("count after limit hit should be 4 (so caller can audit), got %d", n)
		}
	})

	t.Run("IncrAndCheck different keys independent", func(t *testing.T) {
		store := factory(t)
		n1, err := store.IncrAndCheck(context.Background(), "bind:test:keyA", 2, time.Minute)
		if err != nil || n1 != 1 {
			t.Fatalf("keyA incr: n=%d err=%v", n1, err)
		}
		n2, err := store.IncrAndCheck(context.Background(), "bind:test:keyB", 2, time.Minute)
		if err != nil || n2 != 1 {
			t.Fatalf("keyB must not share counter with keyA: n=%d err=%v", n2, err)
		}
	})

	t.Run("IncrAndCheck invalid args rejected", func(t *testing.T) {
		store := factory(t)
		if _, err := store.IncrAndCheck(context.Background(), "", 1, time.Minute); err == nil {
			t.Fatal("empty key must reject")
		}
		if _, err := store.IncrAndCheck(context.Background(), "k", 0, time.Minute); err == nil {
			t.Fatal("limit=0 must reject (caller bug)")
		}
		if _, err := store.IncrAndCheck(context.Background(), "k", 1, 0); err == nil {
			t.Fatal("ttl=0 must reject (caller bug)")
		}
	})

	t.Run("Save respects TTL", func(t *testing.T) {
		store := factory(t)
		sess := &BindSession{JTI: "j-ttl", Status: BindStatusIssued, CreatedAt: time.Now().Unix()}
		// 1s TTL -> 2s 后 Get 应 NotFound。redis impl 用 PEXPIRE,精确到毫秒;
		// memory impl 用 time.Now 对比。sleep 1.2s 留容差。
		if err := store.Save(context.Background(), sess, time.Second); err != nil {
			t.Fatalf("Save: %v", err)
		}
		time.Sleep(1200 * time.Millisecond)
		_, err := store.Get(context.Background(), "j-ttl")
		if !errors.Is(err, ErrBindNotFound) {
			t.Fatalf("expected ErrBindNotFound after TTL, got %v", err)
		}
	})
}

// TestMemoryBindStore_Behavior memory impl 跑完整契约。
func TestMemoryBindStore_Behavior(t *testing.T) {
	runBindStoreBehaviorSuite(t, func(t *testing.T) BindStore {
		t.Helper()
		return newMemoryBindStore()
	})
}
