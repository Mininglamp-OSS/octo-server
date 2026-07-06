package message

// card-message-interaction P2 D4（round-3 P1-1，spec: .octospec/tasks/
// card-message-interaction/brief.md）：card/action 的幂等 claim 存储。
//
// 去重键是业务身份 (message_id, action_id, operator_uid) —— 刻意不含
// client_token（含 token 会让「D8 超时后携新 token 重试」二次触发 bot 事件）；
// token 降级为关联 ID，只回显在 ack 与 event_data 里。
//
// 时序是契约的一部分：claim(SET NX EX 60s "pending") → 事件入队 →
// confirm(SET XX EX 24h event_id)。入队失败补偿 DEL（客户端可重试）；进程在
// claim 与 confirm 之间崩溃时键最多存活 60s —— 半途而废的请求绝不造成 24h 锁死。
//
// SetNX/SetXX 不在 octo-lib Conn 包装器上，按仓库惯例经 pkg/redis 的
// NewInstrumentedClient 构造裸 go-redis client（与 OIDC 锁 / 限流令牌桶同模式，
// TLS 与指标插桩统一）。

import (
	"fmt"
	"strconv"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
)

const (
	// cardActionIdemTTL D4：幂等窗口，与 card_action 事件可消费窗口共用一个
	// 常量（D8：actionable window == idempotency TTL）。
	cardActionIdemTTL = 24 * time.Hour
	// cardActionClaimPendingTTL claim 与 confirm 之间的 pending 存活窗口。
	cardActionClaimPendingTTL = 60 * time.Second
	cardActionClaimPending    = "pending"
)

// cardActionClaimKey D4 业务身份去重键。
func cardActionClaimKey(messageID, actionID, operatorUID string) string {
	return fmt.Sprintf("cardaction:%s:%s:%s", messageID, actionID, operatorUID)
}

type cardActionClaimStore struct {
	client *rd.Client
}

func newCardActionClaimStore(ctx *config.Context) *cardActionClaimStore {
	return &cardActionClaimStore{client: octoredis.NewInstrumentedClient(ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 3
		o.ReadTimeout = 3 * time.Second
		o.WriteTimeout = 3 * time.Second
		o.DialTimeout = 3 * time.Second
	})}
}

// Claim 原子占位（SET NX）。false = 键已存在（pending 或已 confirm）——
// 调用方返回 replay ack，绝不产生第二个 bot 事件。
func (s *cardActionClaimStore) Claim(key string) (bool, error) {
	return s.client.SetNX(key, cardActionClaimPending, cardActionClaimPendingTTL).Result()
}

// Confirm 把 claim 升格为 24h 已消费标记（值 = event_id，排障时可把去重键关联
// 回事件）。XX 语义：pending 已过期（键不在）时不写 —— 返回 false 由调用方记
// 日志即可：事件已经入队，at-least-once 语义下不回滚。
func (s *cardActionClaimStore) Confirm(key string, eventID int64) (bool, error) {
	return s.client.SetXX(key, strconv.FormatInt(eventID, 10), cardActionIdemTTL).Result()
}

// Release 入队失败的补偿删除（best-effort：删不掉也只是残留 60s pending）。
func (s *cardActionClaimStore) Release(key string) {
	_ = s.client.Del(key).Err()
}
