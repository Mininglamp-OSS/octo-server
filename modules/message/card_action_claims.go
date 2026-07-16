package message

// card-message-interaction P2 D4（round-3 P1-1，spec: .octospec/tasks/
// card-message-interaction/brief.md）：card/action 的幂等 claim 存储。
//
// 去重键是业务身份 (message_id, action_id, operator_uid) —— 刻意不含
// client_token（含 token 会让「D8 超时后携新 token 重试」二次触发 bot 事件）；
// token 降级为关联 ID，只回显在 ack 与 event_data 里。
//
// 时序是契约的一部分：claim(SET NX EX 60s "pending") → 事件入队 →
// confirm(SET XX EX <可操作窗口> event_id)。入队失败补偿 DEL（客户端可重试）；进程
// 在 claim 与 confirm 之间崩溃时键最多存活 60s —— 半途而废的请求绝不造成长时锁死。
//
// SetNX/SetXX 不在 octo-lib Conn 包装器上，按仓库惯例经 pkg/redis 的
// NewInstrumentedClient 构造裸 go-redis client（与 OIDC 锁 / 限流令牌桶同模式，
// TLS 与指标插桩统一）。
//
// 这是业务身份幂等（rate-limit 规则的例外条款明确允许），不是请求频率限流 ——
// 频率限流是 SharedUIDRateLimiter 的职责。

import (
	"fmt"
	"strconv"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
)

const (
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
	// idemTTL D4 幂等键存活时长 —— 取自 card_action 事件的可消费窗口
	// Robot.MessageExpire（robot/api.go EnqueueBotTypedEvent 以同值把事件入队 bot
	// 事件 ZSET）。D8：幂等窗口 == 可操作窗口，同一来源、同一常量 —— 去重键与事件
	// 同生共死，一次点击在事件仍可消费的整个窗口内都被去重，杜绝「去重键先于事件过期
	// → 窗口内 re-tap 造出第二条事件」（PR#548 review：两值原先解耦为 24h/7d）。
	idemTTL time.Duration
}

func newCardActionClaimStore(ctx *config.Context) *cardActionClaimStore {
	idemTTL := ctx.GetConfig().Robot.MessageExpire
	if idemTTL <= 0 {
		// 兜底：与 octo-lib config.New 的 Robot.MessageExpire 默认一致，避免 0 TTL
		// 被 Redis 当作「永不过期」。
		idemTTL = 7 * 24 * time.Hour
	}
	return &cardActionClaimStore{
		client: octoredis.NewInstrumentedClient(ctx.GetConfig(), func(o *rd.Options) {
			o.MaxRetries = 3
			o.ReadTimeout = 3 * time.Second
			o.WriteTimeout = 3 * time.Second
			o.DialTimeout = 3 * time.Second
		}),
		idemTTL: idemTTL,
	}
}

// Claim 原子占位（SET NX）。false = 键已存在（pending 或已 confirm）—— 调用方据
// Confirmed 区分：已 confirm 回 replay:true，仅 pending 则视为可重试（PR#548 review
// P2），两种情况都绝不产生第二个 bot 事件。
func (s *cardActionClaimStore) Claim(key string) (bool, error) {
	return s.client.SetNX(key, cardActionClaimPending, cardActionClaimPendingTTL).Result()
}

// Confirm 把 claim 升格为「已消费」标记，存活 idemTTL（= 可操作窗口；值 = event_id，
// 排障时可把去重键关联回事件）。XX 语义：pending 已过期（键不在）时不写 —— 返回 false
// 由调用方记日志即可：事件已经入队，at-least-once 语义下不回滚。
func (s *cardActionClaimStore) Confirm(key string, eventID int64) (bool, error) {
	return s.client.SetXX(key, strconv.FormatInt(eventID, 10), s.idemTTL).Result()
}

// Confirmed 报告 key 是否已 confirm（值 != pending）。并发下区分「已确认的重复提交」
// （回 replay:true）与「首请求尚在处理、只占了 pending 位」（可重试，不回虚假成功）——
// PR#548 review P2：只对 confirmed claim 回 replay，避免 A 校验失败释放后 B 拿着 A 的
// pending 得到成功 ack 却无事件入队而丢动作。key 不存在（已过期/已释放）视为未确认。
func (s *cardActionClaimStore) Confirmed(key string) (bool, error) {
	v, err := s.client.Get(key).Result()
	if err == rd.Nil {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v != cardActionClaimPending, nil
}

// releaseIfPendingScript 仅当键仍是 pending 时删除（GET==pending 才 DEL，EVAL 内
// GET+DEL 原子）。避免「首请求 stall >60s → pending 过期 → 重试 claim+confirm(idemTTL)
// 后，原请求补偿 Release 若无条件 DEL 会误删已 confirm 键 → 去重窗口重开、同一动作
// 二次入队」（PR#548 review P2-c）。返回被删的键数（1/0），调用方只关心 err。
var releaseIfPendingScript = rd.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0
`)

// Release 入队失败的补偿删除 —— 仅当键仍是 pending 时删（CAS-del，见 releaseIfPendingScript）：
// 键已被并发重试 confirm 时不动它，杜绝误删已确认 claim 而重开去重窗口。返回 Redis 错误
// 供调用方记日志（删不掉只是残留至多 60s pending，不影响正确性，但便于发现系统性 Redis 故障）。
func (s *cardActionClaimStore) Release(key string) error {
	return releaseIfPendingScript.Run(s.client, []string{key}, cardActionClaimPending).Err()
}
