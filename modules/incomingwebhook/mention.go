package incomingwebhook

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"unicode/utf16"

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
	// 渲染层 entities（调用方传入的 @ 区间）【仅 text 路径】处理：offset/length 是对纯文本
	// content 的 UTF-16 偏移，richtext 的块结构参考系不同（且跨端已知 caption/plain 错位），
	// 本期不碰。逐条宽松解码后，其 uid 也并入下面同一次成员闸查询，避免二次查询。
	var ents []mentionEntity
	if isTextMention(req) {
		ents = decodeEntities(mr.Entities, maxMentionUIDs())
	}
	gateUIDs := uids
	if len(ents) > 0 {
		// 上限 2*maxMentionUIDs：uids 与 entity uids 各自已先钳到 maxMentionUIDs，合并去重后
		// 成员闸 IN 查询最多约 2N 个 uid（仍有界、量级与单查询无异），换取「定向 uids + entities
		// 一次查询」而非两次。
		gateUIDs = dedupNonEmpty(append(append([]string{}, uids...), entityUIDsOf(ents)...), maxMentionUIDs()*2)
	}
	members := map[string]struct{}{}
	if len(gateUIDs) > 0 {
		got, err := w.db.filterGroupMembers(m.GroupNo, gateUIDs)
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
	mention, ignored := assembleMention(uids, members,
		mr.All, mr.Bots,
		m.AllowMentionAll == 1 && broadcastPermitted,
		m.AllowMentionBots == 1 && broadcastPermitted)

	// 校验通过的 entities 原样挂到 mention.entities（与 uids 正交，ExpandAisToBotUIDs 只动
	// uids、不碰 entities）。ents 仅在 text 路径非空，故此处无需再判 msg_type；content 在
	// text 路径原样透传（PR-1 不改写 content），offset 与落地一致。
	if len(ents) > 0 {
		if validEnts := finalizeEntities(ents, members, req.Content); len(validEnts) > 0 {
			if mention == nil {
				mention = map[string]interface{}{}
			}
			mention[mentionrewrite.EntitiesKey] = validEnts
		}
	}
	return mention, ignored
}

// mentionEntity 是调用方传入的单条【渲染层】@ 区间（线协议 mention.entities 的元素）：
// offset/length 单位是 UTF-16 码元、相对消息文本，uid 必须是本群成员。
type mentionEntity struct {
	UID    string `json:"uid"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

// entity 线协议字段名（与 web/Android 解析、用户给的 native 示例一致：{uid,offset,length}）。
const (
	entityKeyUID    = "uid"
	entityKeyOffset = "offset"
	entityKeyLength = "length"
)

// decodeEntities 逐条宽松解码 mention.entities：单条 JSON 形状非法 / uid 空 / offset<0 /
// length<=0 只丢该条，绝不影响其余 entity 或 mention 的 uids/all/bots（acceptance #6 的延伸）。
// 这里只做结构 + 基本数值 sanity；成员闸与「offset 越界 / 指向 '@'」校验在 finalizeEntities
// （那两步需要成员集 + content）。limit<=0 不限；>0 时按出现顺序最多保留 limit 条（兜底膨胀）。
func decodeEntities(raw []json.RawMessage, limit int) []mentionEntity {
	if len(raw) == 0 {
		return nil
	}
	out := make([]mentionEntity, 0, len(raw))
	for _, r := range raw {
		if limit > 0 && len(out) >= limit {
			break
		}
		var e mentionEntity
		if err := json.Unmarshal(r, &e); err != nil {
			continue
		}
		if e.UID == "" || e.Offset < 0 || e.Length <= 0 {
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// entityUIDsOf 抽出 entities 的 uid 列表，用于并入成员闸查询（顺序无关，后续会去重）。
func entityUIDsOf(ents []mentionEntity) []string {
	if len(ents) == 0 {
		return nil
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.UID)
	}
	return out
}

// finalizeEntities 把已基本解码的 entities 做【权威校验】并构造线协议形态。每条须满足：
//   - uid 是本群成员（复用成员闸结果；非成员【静默丢弃】，与定向 uids 反枚举同源）；
//   - offset/length 落在 content 的 UTF-16 码元范围内（offset 指向真实码元、length 不越界）；
//   - content 在 offset 处确为 '@'（与 Android plain[offset]=='@' 对齐，挡掉调用方算错的偏移、
//     避免把气泡错绑到非 @ 文本）；
//   - 区间不与已接受的 entity 重叠（首条占位者胜）——去重/防重叠，与 uids 的 dedupNonEmpty
//     对称，且与端上 dedup（web parseMentionWithEntities 的 lastEnd 跳过、Android claimed[]）
//     一致，避免同一段文本叠多个气泡。
//
// 单位刻意用 UTF-16 码元——与 web(String.substring/.length)/Android(Kotlin String)/iOS(NSRange)
// 一致；故用 utf16.Encode 量度，【绝不能】用字节 len() 或 rune 数（含 emoji 时三者分叉）。
// 返回 []interface{}（每项 map{uid,offset,length}），可直接挂到 mention[EntitiesKey]；无合法项 → nil。
func finalizeEntities(ents []mentionEntity, members map[string]struct{}, content string) []interface{} {
	if len(ents) == 0 {
		return nil
	}
	u16 := utf16.Encode([]rune(content))
	claimed := make([]bool, len(u16)) // 已被先前 entity 覆盖的码元位，用于防重叠/重复
	out := make([]interface{}, 0, len(ents))
	for _, e := range ents {
		if e.UID == "" {
			continue
		}
		if _, ok := members[e.UID]; !ok {
			continue
		}
		if e.Offset < 0 || e.Length <= 0 {
			continue
		}
		// 越界判断写成减法形式，避免 offset+length 在异常大入参下整型溢出。
		if e.Offset >= len(u16) || e.Length > len(u16)-e.Offset {
			continue
		}
		if u16[e.Offset] != '@' {
			continue
		}
		if rangeClaimed(claimed, e.Offset, e.Length) {
			continue // 与已接受区间重叠 / 完全重复 → 丢弃
		}
		markClaimed(claimed, e.Offset, e.Length)
		out = append(out, map[string]interface{}{
			entityKeyUID:    e.UID,
			entityKeyOffset: e.Offset,
			entityKeyLength: e.Length,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// rangeClaimed 报告 [offset, offset+length) 内是否有任一码元已被占用。调用前提：区间已通过
// finalizeEntities 的越界校验，故下标恒在 claimed 范围内。
func rangeClaimed(claimed []bool, offset, length int) bool {
	for i := offset; i < offset+length; i++ {
		if claimed[i] {
			return true
		}
	}
	return false
}

// markClaimed 把 [offset, offset+length) 标记为已占用。
func markClaimed(claimed []bool, offset, length int) {
	for i := offset; i < offset+length; i++ {
		claimed[i] = true
	}
}

// isTextMention 报告本次推送是否走纯文本路径（mention.entities 仅在该路径校验/透传）。
// 与 handlePush 的 msg_type 分发同口径：缺省 / "text" 即文本。
func isTextMention(req *pushPayloadReq) bool {
	switch strings.ToLower(strings.TrimSpace(req.MsgType)) {
	case "", msgTypeText:
		return true
	default:
		return false
	}
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
