package oidc

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/redis"
	"go.uber.org/zap"
)

const (
	defaultCallbackFailThreshold = 10
	defaultCallbackFailWindow    = 5 * time.Minute
	callbackFailKeyPrefix        = "oidc:cb:fail:"
	callbackUnknownIPKey         = "__unknown_ip__"
)

// ErrCallbackBlocked /callback 同 IP 失败次数过多,临时锁定。
var ErrCallbackBlocked = errors.New("oidc callback rate limit exceeded")

// CallbackGuard 按客户端 IP 给 OIDC /callback 限流。
//
// 与 user.LoginGuard 同形:Redis INCR + EXPIRE 滑动窗口。
//
// 维度选 IP 而非 account/uid,因为 callback 阶段尚未拿到 IdP claims;state 32 字节
// 随机不可猜,合法用户失败率 ≈ 0,失败计数低门槛(默认 10 / 5min)对正常用户
// 极宽松,对扫描类攻击足够紧。Redis 不可用 fail-open。
//
// 一致性:INCR + EXPIRE 非原子,首次 Incr 成功而 Expire 失败会让 key 永不过期 →
// IP 永久锁定。RecordFailure 每次都重设 TTL,后续失败修复缺失的 TTL。代价是
// 滑动窗口语义(攻击者持续尝试时窗口续期),对防扫描更严格,符合安全预期。
type CallbackGuard struct {
	redis     *redis.Conn
	threshold int64
	window    time.Duration
}

// NewCallbackGuard 构造 CallbackGuard。threshold/window <=0 时回落默认值。
func NewCallbackGuard(r *redis.Conn, threshold int64, window time.Duration) *CallbackGuard {
	if threshold <= 0 {
		threshold = defaultCallbackFailThreshold
	}
	if window <= 0 {
		window = defaultCallbackFailWindow
	}
	return &CallbackGuard{redis: r, threshold: threshold, window: window}
}

func normalizeIP(ip string) string {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		// wkhttp.ClientIP 对畸形或歧义的代理头 fail-closed 返回空串。与共享
		// IP limiter 保持同一语义：归入稳定的 unknown-IP 桶，不能跳过保护。
		return callbackUnknownIPKey
	}
	return ip
}

func (g *CallbackGuard) key(ip string) string { return callbackFailKeyPrefix + ip }

// Check 失败计数已达阈值返回 ErrCallbackBlocked,否则放行。空 IP 归入共享的
// unknown-IP 桶，避免畸形代理头绕过保护。
//
// fail-open:Redis 抖动时不锁,与 LoginGuard 一致。基础设施层网关 cap 兜底 DoS。
//
// nil receiver 安全:测试/配置 disabled 时直接放行,避免 callee 各自加 nil 判断。
func (g *CallbackGuard) Check(ip string) error {
	if g == nil {
		return nil
	}
	ip = normalizeIP(ip)
	s, err := g.redis.GetString(g.key(ip))
	if err != nil {
		log.Warn("CallbackGuard Check 读取失败,fail-open", zap.String("ip", ip), zap.Error(err))
		return nil
	}
	if s == "" {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	if n >= g.threshold {
		return ErrCallbackBlocked
	}
	return nil
}

// RecordFailure 失败 +1,每次重设 TTL(滑动窗口;详见 CallbackGuard 文档)。
// nil receiver 安全。
func (g *CallbackGuard) RecordFailure(ip string) error {
	if g == nil {
		return nil
	}
	ip = normalizeIP(ip)
	key := g.key(ip)
	if _, err := g.redis.Incr(key); err != nil {
		return fmt.Errorf("incr callback failure: %w", err)
	}
	if err := g.redis.Expire(key, g.window); err != nil {
		return fmt.Errorf("expire callback failure: %w", err)
	}
	return nil
}

// Reset 删除 IP 的失败计数(成功路径调用,清场避免长尾误锁)。
// nil receiver 安全(测试 / disabled 路径)。
func (g *CallbackGuard) Reset(ip string) error {
	if g == nil {
		return nil
	}
	ip = normalizeIP(ip)
	if err := g.redis.Del(g.key(ip)); err != nil {
		return fmt.Errorf("del callback failure: %w", err)
	}
	return nil
}

// RecordFailureLogged 包装 RecordFailure,失败仅 warn 不向上扩散。
func (g *CallbackGuard) RecordFailureLogged(ip string) {
	if err := g.RecordFailure(ip); err != nil {
		log.Warn("CallbackGuard RecordFailure 失败", zap.String("ip", ip), zap.Error(err))
	}
}

// ResetLogged 包装 Reset,失败仅 warn。
func (g *CallbackGuard) ResetLogged(ip string) {
	if err := g.Reset(ip); err != nil {
		log.Warn("CallbackGuard Reset 失败", zap.String("ip", ip), zap.Error(err))
	}
}

// callbackGuardThresholdFromEnv 读 DM_OIDC_CALLBACK_FAIL_THRESHOLD;非法/缺省返 0。
func callbackGuardThresholdFromEnv() int64 {
	if v := os.Getenv("DM_OIDC_CALLBACK_FAIL_THRESHOLD"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// callbackGuardWindowFromEnv 读 DM_OIDC_CALLBACK_FAIL_WINDOW_SEC(秒);非法/缺省返 0。
func callbackGuardWindowFromEnv() time.Duration {
	if v := os.Getenv("DM_OIDC_CALLBACK_FAIL_WINDOW_SEC"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 0
}
