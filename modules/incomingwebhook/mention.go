package incomingwebhook

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-server/pkg/mentionrewrite"
	"go.uber.org/zap"
	"golang.org/x/text/unicode/norm"
)

// parseMentionUIDs 把 webhook 配置列 mention_uids（JSON 数组字符串，如 `["uid_a","bot_b"]`）
// 宽松解出 UID 列表。空串 / 解析失败一律返回 nil（降级为「无定向 @」，绝不让一行坏数据中断
// 推送）——配置在写入时已校验为合法 JSON，这里的宽松仅作纵深防御。
func parseMentionUIDs(raw string) []string {
	if raw == "" {
		return nil
	}
	var uids []string
	if err := json.Unmarshal([]byte(raw), &uids); err != nil {
		return nil
	}
	return uids
}

// @ 目标数量上限（去重后）。定向 @uid 是低风险能力，但仍需上限兜底：防止单条推送塞入
// 上千 uid 撑大成员闸的 IN 查询与 payload。50 与「一条消息合理 @ 的人数」量级相符，
// 可经 env 调整（与 maxContentRunes 等阈值同走 env 兜底）。
const (
	envMaxMentionUIDs     = "OCTO_INCOMINGWEBHOOK_MAX_MENTION_UIDS"
	defaultMaxMentionUIDs = 50

	// mentionUIDsColumnChars 是 mention_uids 落库列宽（migration 20260625000001：VARCHAR(4096)）。
	// validateMentionUIDs 写入前用它兜底：默认 50-uid 上限下 JSON ≈ 2.2KB 远未触顶，但
	// OCTO_INCOMINGWEBHOOK_MAX_MENTION_UIDS 可被运维调高到 JSON 超列宽——此时按列宽干净 400，
	// 而不是等 DB 写入才脏失败/截断。改列宽须同步此常量。
	mentionUIDsColumnChars = 4096
)

func maxMentionUIDs() int {
	if v := os.Getenv(envMaxMentionUIDs); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxMentionUIDs
}

// 三态广播的【中文 canonical】@ 字面量 + 末尾定界空格。
//
// 为什么是这两个字面量、为什么必须带空格：
//   - web/iOS/Android 三端渲染广播气泡都要求 content 文本里【存在】该字面量——没有任何一端
//     仅凭 mention.humans/ais 标志位凭空合成气泡（web 的 buildMessageMentions 合成的是「若文本
//     里出现 @所有人 就高亮」的元数据，segmentText 仍按文本匹配；iOS/Android 直接扫 content）。
//     故服务端在标志位获批时把字面量【前置】到 content，三端即可渲染气泡。
//   - 端上识别的广播 token 集是 locale-independent 的（中文 @所有人/@所有AI + 英文别名
//     @All People/@All AIs/@all）；选择器插入、服务端发出的都是【中文 canonical】，所有端都
//     识别，无需按 locale 切换。
//   - 末尾空格是【必须】的定界符：Android 高亮命中后会检查下一字符不是字母/数字/_（CJK 视作
//     字母，"@所有人执行" 会被跳过不高亮）；iOS @\S+/\b、web 正则同样需要定界。
//   - 这两个 label 与 mentionrewrite.HumansKey/AIsKey 一一对应：标志位驱动路由/红点/bot 展开，
//     label 驱动可见气泡。label 刻意留在本模块（而非 mentionrewrite）——它是「广播补文案」这个
//     render 行为的实现细节、目前仅本模块 compose，且三端各自也硬编码同一套 token；mentionrewrite
//     只拥有 wire key 词汇表（humans/ais/...），不拥有渲染 label。
const (
	broadcastTokenAll = "@所有人"  // 真人广播（mention.humans）
	broadcastTokenAIs = "@所有AI" // AI 广播（mention.ais）
	broadcastTokenSep = " "     // 定界空格（见上）
)

// broadcastLabels 是端上识别的广播标签集（@ 去前缀、小写；与 web/iOS/Android 的
// isBroadcastMentionName 同口径：中文 canonical + 英文别名）。定向 render 时昵称命中此集 → 跳过，
// 否则 "@<昵称>" 会被端上当成广播 token 渲染成 @所有人/@所有AI 气泡——伪造一次绕过
// allow_mention_* 能力位的全员广播。
var broadcastLabels = map[string]struct{}{
	"所有人": {}, "所有ai": {}, "all": {}, "all people": {}, "all ais": {},
}

// stripFormatRunes 删除 Unicode 格式字符（category Cf：零宽 ZWSP/ZWNJ/ZWJ、BOM、word-joiner
// 及 LRE/RLE/PDF/LRI… 等 bidi 控制符）。它们不可见,昵称可借此把零宽字符塞进广播标签【内部】
// （"所有<U+200B>人"）骗过 HasPrefix 比对、却渲染成视觉一模一样的 "@所有人"。guard 比对与渲染昵称
// 都先过它,确保比对不被绕过、且生成的气泡文本绝不携带不可见混淆字符。
func stripFormatRunes(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1 // 丢弃
		}
		return r
	}, s)
}

// isBroadcastLikeName 报告 name 会不会被端上当成广播 token——不止【精确】命中标签集,还包括
// 「以广播标签开头、其后紧跟非字边界」。因为 iOS 按 @\S+ 切词:"所有人 X" 会切出独立 token
// "@所有人"、"所有人:" 同理,都会渲染成广播气泡(伪造一次绕过 allow_mention_* 的全员广播)。
// 仅当标签后紧跟字母/数字/CJK 时(如 "所有人事部"/"allen")才是另一个真实词、不算广播,照常渲染。
// 单字标签(所有人/所有ai/all)即覆盖多字标签(all people/all ais)的边界,因 iOS 首个 token 止于空格。
//
// 比对前先 NFKC 折叠(全角 "ａｌｌ"→"all" 等【可见】混淆)+ stripFormatRunes(剥离【不可见】混淆)
// + 小写,把零宽/bidi/全角 confusable 都收敛到 canonical 形式再比,杜绝绕过(yujiawei/Jerry-Xin #450)。
func isBroadcastLikeName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(stripFormatRunes(norm.NFKC.String(name))))
	if n == "" {
		return false
	}
	for label := range broadcastLabels {
		if !strings.HasPrefix(n, label) {
			continue
		}
		rest := n[len(label):]
		if rest == "" {
			return true // 精确命中
		}
		// 标签后紧跟非字（字母/数字/CJK 之外）→ 端上会把 "@<label>" 切成独立广播 token。
		if r, _ := utf8.DecodeRuneInString(rest); !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// utf16Len 返回 s 的 UTF-16 码元长度（= JS String.length / NSString length / Kotlin
// String length），与端上 mention.entities 的 offset/length 单位一致——【绝不能】用字节
// len() 或 rune 数（含 emoji 时三者分叉）。
func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
}

// composeMentionContent 把服务端生成的 @ 前缀补到 content 文首，使三端渲染气泡。两类前缀按
// 固定顺序拼成一段、一次性前置（广播在前、定向在后、原 content 最后）：
//
//   - 广播字面量（@所有人/@所有AI，#448 ②）：want* && allow*（= AllowMention*==1 &&
//     broadcastPermitted）且 content 未含该 canonical 时前置。广播气泡由端上据 humans/ais
//     标志位 + 文本里的字面量渲染，故【不】生成 entity。
//   - 定向昵称（render，#448 ① b）：把 renderUIDs（已是本群成员、按调用方顺序）逐个解析昵称、
//     前置 "@<昵称> "，并【生成】对应线协议 entity（offset/length 为 UTF-16 码元、指向前缀里该
//     @ 段）——让只传 uid 的调用方也能渲染可点击 @气泡。
//
// 返回（改写后的 content，前缀的 UTF-16 码元长度，定向 render 生成的 entities）。前缀为空 →
// (content, 0, nil)，保证无补文案的历史调用 payload 字节不变（向后兼容）。
//
// 不变量与边界：
//   - prefixU16 供调用方把【调用方自带】的 entities(#449) 整体右移（前置改变了 content）；render
//     与自带 entities 互斥，故二者不会同时出现。
//   - 防伪造广播：昵称会被端上当成广播 token（isBroadcastLikeName：精确命中标签集，或以标签开头且
//     后接非字边界如 "所有人 X"/"所有人:"——iOS @\S+ 会切出独立 "@所有人"）或含 '@'（WeChat 昵称
//     路径不过滤 @）时【跳过】render——否则 "@<昵称>" 会被渲染成 @所有人/@所有AI 气泡，伪造一次绕过
//     allow_mention_* 能力位的全员广播。命中者仍按 uid 路由、只是不出气泡。
//   - 幂等：广播按 canonical 字面量、定向按 "@<昵称>" 在【原始 content】里 Contains 去重（避免双
//     气泡）。子串误判（名是另一段文本的前缀，如名 "张" 撞 "@张三"）会保守跳过该气泡、uid 仍路由——可接受。
//   - 空昵称（非成员/未 join 到 user.name）跳过——绝不补 "@ "。
//   - 含空格的昵称（如 "Bob Smith"）：web/Android 用 entity 精确绑定整段；iOS 忽略 entity、按 @\S+
//     定位，气泡文本会截到首个空格（点击仍按位次绑定到正确 uid）——与 #449 的 iOS 已知行为一致、非错绑。
//   - 预算 maxRunes>0：增量维护 prefixRunes（含已前置的广播段，故定向段按剩余额度收敛），补每个
//     @昵称前估算「前缀+原文」总 rune 数，超限即停止再补（剩余 uid 仍由 mention.uids 路由）,保证
//     补文案后 content 不破 maxContentRunes。rune 数有界（≤maxContentRunes）；utf8mb4 昵称下字节
//     最坏约 4×，仍远低于下游序列化上限。
func composeMentionContent(content string, wantAll, wantBots, allowAll, allowBots, render bool, renderUIDs []string, namesByUID map[string]string, maxRunes int) (string, int, []interface{}) {
	var prefix strings.Builder
	prefixU16, prefixRunes := 0, 0 // 增量维护，避免每轮重算 prefix.String()（O(n²)）
	appendToken := func(tok string) {
		prefix.WriteString(tok)
		prefixU16 += utf16Len(tok)
		prefixRunes += utf8.RuneCountInString(tok)
	}
	if wantAll && allowAll && !strings.Contains(content, broadcastTokenAll) {
		appendToken(broadcastTokenAll + broadcastTokenSep)
	}
	if wantBots && allowBots && !strings.Contains(content, broadcastTokenAIs) {
		appendToken(broadcastTokenAIs + broadcastTokenSep)
	}
	var genEntities []interface{}
	if render {
		contentRunes := utf8.RuneCountInString(content)
		seen := make(map[string]struct{}, len(renderUIDs))
		for _, uid := range renderUIDs {
			if _, dup := seen[uid]; dup {
				continue
			}
			// 先剥离不可见格式字符（零宽/bidi）再 trim：渲染出的气泡不带不可见混淆字符，且下面
			// 的 broadcast-like 比对看到的是 canonical 形式（"所有<U+200B>人"→"所有人"，无法夹零宽绕过）。
			name := strings.TrimSpace(stripFormatRunes(namesByUID[uid]))
			if name == "" {
				continue // 非成员 / 未解析到昵称 / 仅由格式字符构成 → 不渲染（绝不补 "@ "）
			}
			if isBroadcastLikeName(name) || strings.Contains(name, "@") {
				continue // 防伪造广播 / 嵌入式 @：昵称会被端上当成广播 token 或破坏 @ 分词
			}
			atName := "@" + name
			if strings.Contains(content, atName) {
				continue // 幂等：调用方已把该 @昵称写进 content
			}
			seg := atName + broadcastTokenSep
			// 预算：补这一段后「前缀+原文」rune 数不得超过 maxContentRunes（prefixRunes 已含广播段）。
			if maxRunes > 0 && prefixRunes+utf8.RuneCountInString(seg)+contentRunes > maxRunes {
				break // 余下 uid 不再出气泡，仍由 mention.uids 路由
			}
			seen[uid] = struct{}{}
			genEntities = append(genEntities, map[string]interface{}{
				entityKeyUID:    uid,
				entityKeyOffset: prefixU16, // 该 @ 段在前缀里的 UTF-16 起点（append 前捕获）
				entityKeyLength: utf16Len(atName),
			})
			appendToken(seg)
		}
	}
	if prefix.Len() == 0 {
		return content, 0, nil
	}
	return prefix.String() + content, prefixU16, genEntities
}

// buildMention 据 webhook 配置（创建/修改时指定的 MentionUids + AllowMention* 广播开关）构造
// 消息 payload 的 mention 子对象（线协议 {uids,humans,ais,entities}）。push body 已不再携带
// mention（model 层移除该字段），故本函数【完全从 webhook 配置】构造，不读 req 的 @ 信息——
// req 仅提供正文 content 与 msg_type。返回 (mention, content, ignored)：
//   - mention 可直接挂到 payload[mentionrewrite.MentionKey]；无定向目标且无获批广播时返回 nil，
//     保证「无 @ 配置」的 webhook payload 形态完全不变（向后兼容）。
//   - content 是【可能前置了 @ 补文案的】正文（仅 text 路径；richtext 原样返回 req.Content）。
//   - ignored 是非致命反馈（哪些广播位因策略未放行被忽略），由调用方放进成功响应体。
//
// 处理（仅 text 路径补文案/生成 entity；richtext 只装配标志位 + uids 路由，content 原样）：
//  1. 定向 uids：解析配置列 → 去重 + 钳到上限 → 过【当前】群成员闸（配置后成员可能退群，故每次
//     推送都重过）→ 命中集进 mention.uids 路由；并把成员昵称按配置顺序前置 "@<昵称> " 生成 entity
//     （定向 render 默认开，让只配 uid 的 webhook 也渲染出可点击 @气泡）。非成员静默丢弃（反枚举）。
//  2. @所有人(humans)：AllowMentionAll==1 且 broadcastPermitted → humans=1 + 前置 @所有人，否则 ignored。
//  3. @所有 AI(ais)：AllowMentionBots==1 且 broadcastPermitted → ais=1（稍后由调用方 ExpandAisToBotUIDs
//     展开为群内全部 bot 成员 UID）+ 前置 @所有AI，否则 ignored。
//
// broadcastPermitted 由调用方传入：system_setting member_can_broadcast || 创建者当前为管理员。
// 关掉该设置即可即时收回所有【成员】创建的 webhook 的广播能力，管理员创建的不受影响。定向 @uid
// 不走此闸（不是广播、风险低），仅受群成员闸 + 去重 + 上限约束。
//
// best-effort：成员闸查询失败时降级为「不带定向 @」（仅记 Warn），绝不因此让整条推送失败
// ——这与 mention 其余环节的「失败即降级、不丢消息」一致。
func (w *IncomingWebhook) buildMention(m *incomingWebhookModel, req *pushPayloadReq, broadcastPermitted bool) (map[string]interface{}, string, []string) {
	uids := dedupNonEmpty(parseMentionUIDs(m.MentionUids), maxMentionUIDs())
	wantAll := m.AllowMentionAll == 1
	wantBots := m.AllowMentionBots == 1

	// 无定向目标、无广播位 → 无 mention，content 原样返回，payload 字节不变（向后兼容无 @ 的 webhook）。
	if len(uids) == 0 && !wantAll && !wantBots {
		return nil, req.Content, nil
	}

	// 广播位有效性 = webhook 能力位 AND 策略放行。compose（补文案）与 assemble（置 humans/ais
	// 标志位）共用这对布尔，保证二者严格同条件触发。
	allowAll := wantAll && broadcastPermitted
	allowBots := wantBots && broadcastPermitted

	// 成员闸：把配置 uids 过【当前】群成员（key=成员归属，value=昵称供 render）。失败即降级为
	// 空成员集（→丢弃全部定向 @），仅记 Warn、不让整条推送失败。
	membersByName := map[string]string{}
	if len(uids) > 0 {
		got, err := w.db.filterGroupMembers(m.GroupNo, uids)
		if err != nil {
			w.Warn("filter group members for mention failed; dropping targeted @uids",
				zap.String("webhook_id", m.WebhookID), zap.Error(err))
		} else {
			membersByName = got
		}
	}
	members := make(map[string]struct{}, len(membersByName))
	for u := range membersByName {
		members[u] = struct{}{}
	}

	// 定向 @ 昵称渲染（仅 text 路径、默认开）：配置 uids 里的本群成员，按配置顺序由
	// composeMentionContent 逐个解析昵称、前置 "@<昵称> " 并生成对应 entity。
	isText := isTextMention(req)
	var renderUIDs []string
	if isText {
		for _, u := range uids {
			if _, ok := membersByName[u]; ok {
				renderUIDs = append(renderUIDs, u)
			}
		}
	}

	// 服务端补 @ 前缀【仅 text 路径】：广播字面量（获批的 all/bots）+ 定向 @昵称。content 为改写
	// 后的正文；genEntities 是渲染生成的定向 entities（offset 已含全部前缀长度）。richtext
	// （isText=false）不补、content 原样、payload 字节不变。前缀长度 prefixU16 在此无下游用途
	// （调用方自带 entities 已移除），故丢弃。
	content := req.Content
	var genEntities []interface{}
	if isText {
		content, _, genEntities = composeMentionContent(
			req.Content, wantAll, wantBots, allowAll, allowBots,
			true, renderUIDs, membersByName, maxContentRunes())
	}

	mention, ignored := assembleMention(uids, members, wantAll, wantBots, allowAll, allowBots)
	if len(genEntities) > 0 {
		if mention == nil {
			mention = map[string]interface{}{}
		}
		mention[mentionrewrite.EntitiesKey] = genEntities
	}
	return mention, content, ignored
}

// entity 线协议字段名（与 web/Android 解析、用户给的 native 示例一致：{uid,offset,length}）。
// 服务端据 webhook 配置的定向 @ 目标解析昵称、生成这些 entity（见 composeMentionContent）；
// 调用方不再自带 entities（push body 已不接受 mention）。
const (
	entityKeyUID    = "uid"
	entityKeyOffset = "offset"
	entityKeyLength = "length"
)

// isTextMention 报告本次推送是否走纯文本路径（@ 补文案/生成 entity 仅在该路径进行）。
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
