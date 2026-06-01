package oidc

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
)

// idTokenKeyPrefix RP-Initiated Logout 所需 id_token_hint 的 Redis key 前缀。
// 按 uid 存一份(最近一次登录的 id_token);logout 原子取出并删除。
const idTokenKeyPrefix = "oidc:idtoken:"

// idTokenStore 存取 RP-Initiated Logout 所需的 id_token_hint。
//
// 生产实现把 id_token 加密后写 Redis,TTL 到期自动清理;测试注入内存 fake。
// id_token 含 PII(email/phone/name)+ 是有效的 IdP 签名 JWT,因此必须加密落盘,
// 与 refresh_token 在 DB 中加密存储的安全等级保持一致。
type idTokenStore interface {
	// Save 覆盖写 uid 当前的 id_token(空 uid / 空 token 视作 no-op,返回 nil)。
	Save(ctx context.Context, uid, idToken string, ttl time.Duration) error
	// Take 原子取出并删除 uid 的 id_token(一次性消费);未命中返回 ("", nil)。
	Take(ctx context.Context, uid string) (string, error)
}

// redisIDTokenStore 生产实现。
//
// 与 redisStateStore 同构:持独立 *redis.Client(go-redis v6),用 Lua GETDEL 保证
// "取出即作废"的原子性 —— 避免并发双击 logout 时同一 id_token 被取两次。
// Read/WriteTimeout 提供命令级超时,网络分区时不阻塞 logout 路径。
// 值为 base64(AES-256-GCM 密文),避免 id_token 在 Redis 明文落盘。
type redisIDTokenStore struct {
	client *rd.Client
	enc    *Encryptor
}

func newRedisIDTokenStore(ctx *config.Context, enc *Encryptor) *redisIDTokenStore {
	client := rd.NewClient(octoredis.MustBuildOptions(ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 3
		o.ReadTimeout = 3 * time.Second
		o.WriteTimeout = 3 * time.Second
		o.DialTimeout = 3 * time.Second
	}))
	return &redisIDTokenStore{client: client, enc: enc}
}

// Save 接受 context.Context 满足接口契约,但 go-redis v6 的命令 API 不支持 context
// 取消;cancellation 由 Read/WriteTimeout 替代(与 redisStateStore 一致)。
func (s *redisIDTokenStore) Save(_ context.Context, uid, idToken string, ttl time.Duration) error {
	if uid == "" || idToken == "" {
		return nil
	}
	ct, err := s.enc.Encrypt([]byte(idToken))
	if err != nil {
		return fmt.Errorf("oidc: encrypt id_token: %w", err)
	}
	val := base64.StdEncoding.EncodeToString(ct)
	if err := s.client.Set(idTokenKey(uid), val, ttl).Err(); err != nil {
		return fmt.Errorf("oidc: redis set id_token: %w", err)
	}
	return nil
}

func (s *redisIDTokenStore) Take(_ context.Context, uid string) (string, error) {
	if uid == "" {
		return "", nil
	}
	// 复用 state_store_redis.go 的 luaGetDel:原子 GET+DEL,消除并发双取窗口。
	res, err := luaGetDel.Run(s.client, []string{idTokenKey(uid)}).Result()
	if err != nil {
		if errors.Is(err, rd.Nil) {
			return "", nil // 未命中:非 OIDC 登录 / 已过期 / 已消费
		}
		return "", fmt.Errorf("oidc: redis getdel id_token: %w", err)
	}
	raw, ok := res.(string)
	if !ok || raw == "" {
		return "", nil
	}
	ct, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("oidc: decode id_token: %w", err)
	}
	pt, err := s.enc.Decrypt(ct)
	if err != nil {
		return "", fmt.Errorf("oidc: decrypt id_token: %w", err)
	}
	return string(pt), nil
}

// Close 释放底层 Redis 连接池,在模块/进程优雅关闭时调用。
func (s *redisIDTokenStore) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	if err := s.client.Close(); err != nil {
		return fmt.Errorf("oidc: redis id_token store close: %w", err)
	}
	return nil
}

func idTokenKey(uid string) string {
	return idTokenKeyPrefix + uid
}

// bindIDTokenKey 自助绑定接管阶段(bind_pending)的暂存键。
//
// bind 路径的 callback 还不知道最终 uid,无法按 uid 存;先按 bind token(jti)暂存,
// confirm/create 拿到 uid 后再迁移(见 OIDC.promoteBindIDToken)。"bind:" 前缀与正常
// 的 uid 键命名空间隔离 —— uid 不会以 "bind:" 开头。jti 本身已是 32B base64,无注入风险。
func bindIDTokenKey(jti string) string {
	return "bind:" + jti
}
