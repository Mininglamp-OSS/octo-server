package incomingwebhook

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/mentionrewrite"
	"go.uber.org/zap"
)

// decodeMention 宽松解码 native 请求里的 mention 原始字节（acceptance #6）：
//   - 缺省（len==0）→ (nil,false)：无 mention。
//   - 存在但 JSON 形状非法（mention 非对象 / uids 非字符串数组 / all|bots 非布尔 等）→
//     (nil,false)：降级为「无 mention」、消息照常投递，绝不因相邻字段形状把整条推送 400。
//   - 合法 → (*mentionReq,true)。`{"mention":null}` 合法且解出零值（等价无 mention）。
func decodeMention(raw json.RawMessage) (*mentionReq, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var mr mentionReq
	if err := json.Unmarshal(raw, &mr); err != nil {
		return nil, false
	}
	return &mr, true
}

// @ 目标数量上限（去重后）。定向 @uid 是低风险能力，但仍需上限兜底：防止单条推送塞入
// 上千 uid 撑大成员闸的 IN 查询与 payload。50 与「一条消息合理 @ 的人数」量级相符，
// 可经 env 调整（与 maxContentRunes 等阈值同走 env 兜底）。
const (
	envMaxMentionUIDs     = "OCTO_INCOMINGWEBHOOK_MAX_MENTION_UIDS"
	defaultMaxMentionUIDs = 50
)

func maxMentionUIDs() int {
	if v := os.Getenv(envMaxMentionUIDs); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxMentionUIDs
}

// buildMention 把 native 推送请求里的 mentionReq 翻译成消息 payload 的 mention 子对象
// （线协议 {uids,humans,ais}），返回值可直接挂到 payload[mentionrewrite.MentionKey]。
//
// 处理（每步都对应 brief 的 acceptance）：
//  1. 定向 uids：去重 + 钳到上限 → 经群成员闸过滤（只保留本群当前成员），命中集做成
//     []interface{}（ExpandAisToBotUIDs 要求 uids 是 []interface{} 才会就地追加 bot）。
//  2. @所有人(All)：webhook 的 allow_mention_all 开【且 broadcastPermitted】则写 humans=1，否则记入 ignored。
//  3. @所有 AI(Bots)：allow_mention_bots 开【且 broadcastPermitted】则写 ais=1（稍后由调用方
//     ExpandAisToBotUIDs 展开为群内全部 bot 成员 UID），否则记入 ignored。
//
// broadcastPermitted 由调用方传入：system_setting member_can_broadcast || 创建者当前为管理员。
// 关掉该设置即可即时收回所有【成员】创建的 webhook 的广播能力，管理员创建的不受影响。
//
// mention 为空（无有效定向目标、无获批广播）时返回 nil——调用方据此决定是否挂 mention，
// 保证「无 @ 的历史 native 调用」payload 形态完全不变（向后兼容）。
//
// ignored 是非致命反馈（哪些广播位因能力位未开而被忽略），由调用方放进成功响应体；
// 定向 uids 中的非成员【静默丢弃】、不回显具体 uid——避免把推送端点变成「逐个 uid 探测
// 是否本群成员」的枚举 oracle（成员闸的反枚举取舍，与 push 路径的统一 401 同源）。
//
// best-effort：成员闸查询失败时降级为「不带定向 @」（仅记 Warn），绝不因此让整条推送失败
// ——这与 mention 其余环节的「失败即降级、不丢消息」一致。
func (w *IncomingWebhook) buildMention(m *incomingWebhookModel, req *pushPayloadReq, broadcastPermitted bool) (map[string]interface{}, []string) {
	mr, ok := decodeMention(req.Mention)
	if !ok {
		// 缺省即无 mention；【存在但畸形】按 acceptance #6 降级为无 mention（消息照投），
		// 仅此情形记 Warn 以便排障——绝不让畸形 mention 把整条推送 400。
		if len(req.Mention) > 0 {
			w.Warn("malformed mention payload ignored; delivering message without mention",
				zap.String("webhook_id", m.WebhookID))
		}
		return nil, nil
	}
	// IO 步骤：去重+钳上限后，把定向 uids 过一遍群成员闸。失败即降级为空成员集（→丢弃
	// 全部定向 @），仅记 Warn、不让整条推送失败。纯决策（能力位放行 / 装配线协议）下沉到
	// assembleMention，无 DB 依赖，便于单测穷举各分支。
	uids := dedupNonEmpty(mr.Uids, maxMentionUIDs())
	members := map[string]struct{}{}
	if len(uids) > 0 {
		got, err := w.db.filterGroupMembers(m.GroupNo, uids)
		if err != nil {
			w.Warn("filter group members for mention failed; dropping targeted @uids",
				zap.String("webhook_id", m.WebhookID), zap.Error(err))
		} else {
			members = got
		}
	}
	// 广播位有效性 = webhook 能力位 AND 策略放行（broadcastPermitted = system_setting
	// member_can_broadcast || 创建者当前为管理员，由 handlePush 计算）。关掉设置即可即时
	// 收回成员 webhook 的广播（管理员建的因 creatorIsAdmin 仍放行），无需迁移存量列。
	return assembleMention(uids, members,
		mr.All, mr.Bots,
		m.AllowMentionAll == 1 && broadcastPermitted,
		m.AllowMentionBots == 1 && broadcastPermitted)
}

// assembleMention 是 mention 装配的【纯决策核心】（无 IO，便于单测）：把已去重的定向
// uids 过成员闸、按能力位放行广播位，组装成线协议 mention 子对象 {uids,humans,ais}。
//   - uids     ：已 dedupNonEmpty 处理过的候选定向目标（保持顺序）。
//   - members  ：本群成员集（filterGroupMembers 的结果）；不在集内的 uid 被静默丢弃。
//   - wantAll/wantBots ：调用方是否请求 @所有人 / @所有 AI。
//   - allowAll/allowBots：该 webhook 是否获批对应广播能力位。
//
// kept 刻意构造为 []interface{}：ExpandAisToBotUIDs 要求 mention.uids 是 []interface{}
// 才会就地追加 bot UID（其它类型会被它当作畸形而跳过展开）。mention 为空（无有效定向、
// 无获批广播）→ 返回 nil，调用方据此不挂 mention，保证无 @ 的历史调用 payload 不变。
func assembleMention(uids []string, members map[string]struct{}, wantAll, wantBots, allowAll, allowBots bool) (map[string]interface{}, []string) {
	var ignored []string
	mention := map[string]interface{}{}

	if len(uids) > 0 {
		kept := make([]interface{}, 0, len(uids))
		for _, u := range uids {
			if _, ok := members[u]; ok {
				kept = append(kept, u)
			}
		}
		if len(kept) > 0 {
			mention[mentionrewrite.UIDsKey] = kept
		}
	}

	if wantAll {
		if allowAll {
			mention[mentionrewrite.HumansKey] = 1
		} else {
			ignored = append(ignored, "all")
		}
	}
	if wantBots {
		if allowBots {
			mention[mentionrewrite.AIsKey] = 1
		} else {
			ignored = append(ignored, "bots")
		}
	}

	if len(mention) == 0 {
		return nil, ignored
	}
	return mention, ignored
}

// dedupNonEmpty 去掉空白项并去重，保持首次出现顺序，最多保留 limit 个（limit<=0 不限）。
func dedupNonEmpty(in []string, limit int) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	if len(out) == 0 {
		// 全为空白项时与「空输入」同口径返回 nil，让调用方的 len()>0 判断一致。
		return nil
	}
	return out
}

// boolPtrTrue 报告 *bool 是否显式为 true（nil / *false 均为 false）。
func boolPtrTrue(b *bool) bool { return b != nil && *b }

// boolToInt 把布尔映射到 0/1，供能力位列写入。
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
