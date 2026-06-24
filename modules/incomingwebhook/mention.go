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

// isBroadcastLikeName 报告 name（trim+小写后）会不会被端上当成广播 token——不止【精确】命中标签集,
// 还包括「以广播标签开头、其后紧跟非字边界」。因为 iOS 按 @\S+ 切词:名 "所有人 X" 会切出独立
// token "@所有人"、名 "所有人:" 同理,都会渲染成广播气泡(伪造一次绕过 allow_mention_* 的全员广播)。
// 仅当标签后紧跟字母/数字/CJK 时(如 "所有人事部"/"allen")才是另一个真实词、不算广播,照常渲染定向气泡。
// 单字标签(所有人/所有ai/all)即覆盖多字标签(all people/all ais)的边界,因 iOS 首个 @\S+ token 止于空格。
func isBroadcastLikeName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
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
			name := strings.TrimSpace(namesByUID[uid])
			if name == "" {
				continue // 非成员 / 未解析到昵称 → 不渲染（绝不补 "@ "）
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

// buildMention 把 native 推送请求里的 mentionReq 翻译成消息 payload 的 mention 子对象
// （线协议 {uids,humans,ais,entities}）。返回 (mention, content, ignored)：mention 可直接挂到
// payload[mentionrewrite.MentionKey]；content 是【可能前置了广播补文案的】正文，调用方据此
// 覆盖 payload 的 content（无补文案时与 req.Content 全等，payload 字节不变）。
//
// 处理（每步都对应 brief 的 acceptance）：
//  1. 定向 uids：去重 + 钳到上限 → 经群成员闸过滤（只保留本群当前成员），命中集做成
//     []interface{}（ExpandAisToBotUIDs 要求 uids 是 []interface{} 才会就地追加 bot）。
//  2. @所有人(All)：webhook 的 allow_mention_all 开【且 broadcastPermitted】则写 humans=1，否则记入 ignored。
//  3. @所有 AI(Bots)：allow_mention_bots 开【且 broadcastPermitted】则写 ais=1（稍后由调用方
//     ExpandAisToBotUIDs 展开为群内全部 bot 成员 UID），否则记入 ignored。
//  4. 广播补文案（仅 text 路径）：获批的 all/bots 把 canonical 字面量(@所有人/@所有AI)前置到
//     content，使三端渲染出可见广播气泡（见 composeMentionContent）；未获批不前置。
//  5. 定向昵称渲染（mention.render，opt-in，仅 text 路径）：把【本群成员】uids 解析成展示昵称、
//     前置 "@<昵称> " 并生成对应 entities，让只传 uid 的调用方也渲染出 @气泡；与调用方自带
//     entities 互斥（后者权威，见 composeMentionContent）。
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
func (w *IncomingWebhook) buildMention(m *incomingWebhookModel, req *pushPayloadReq, broadcastPermitted bool) (map[string]interface{}, string, []string) {
	mr, ok := decodeMention(req.Mention)
	if !ok {
		// 缺省即无 mention；【存在但畸形】按 acceptance #6 降级为无 mention（消息照投），
		// 仅此情形记 Warn 以便排障——绝不让畸形 mention 把整条推送 400。
		if len(req.Mention) > 0 {
			w.Warn("malformed mention payload ignored; delivering message without mention",
				zap.String("webhook_id", m.WebhookID))
		}
		return nil, req.Content, nil
	}
	// 广播位有效性 = webhook 能力位 AND 策略放行（broadcastPermitted = system_setting
	// member_can_broadcast || 创建者当前为管理员，由 handlePush 计算）。compose（补文案）与
	// assemble（置 humans/ais 标志位）共用这对布尔，保证二者严格同条件触发。
	allowAll := m.AllowMentionAll == 1 && broadcastPermitted
	allowBots := m.AllowMentionBots == 1 && broadcastPermitted
	// IO 步骤：去重+钳上限后，把定向 uids 过一遍群成员闸。失败即降级为空成员集（→丢弃
	// 全部定向 @），仅记 Warn、不让整条推送失败。纯决策（能力位放行 / 装配线协议）下沉到
	// assembleMention，无 DB 依赖，便于单测穷举各分支。
	uids := dedupNonEmpty(mr.Uids, maxMentionUIDs())
	// 渲染层 entities（调用方传入的 @ 区间）【仅 text 路径】处理：offset/length 是对纯文本
	// content 的 UTF-16 偏移，richtext 的块结构参考系不同（且跨端已知 caption/plain 错位），
	// 本期不碰。逐条宽松解码后，其 uid 也并入下面同一次成员闸查询，避免二次查询。
	isText := isTextMention(req)
	var ents []mentionEntity
	if isText {
		ents = decodeEntities(mr.Entities, maxMentionUIDs())
	}
	gateUIDs := uids
	if len(ents) > 0 {
		// 上限 2*maxMentionUIDs：uids 与 entity uids 各自已先钳到 maxMentionUIDs，合并去重后
		// 成员闸 IN 查询最多约 2N 个 uid（仍有界、量级与单查询无异），换取「定向 uids + entities
		// 一次查询」而非两次。
		gateUIDs = dedupNonEmpty(append(append([]string{}, uids...), entityUIDsOf(ents)...), maxMentionUIDs()*2)
	}
	// 成员闸：返回 uid→展示昵称（key 即成员归属判定；昵称仅 render 取用）。失败即降级为空集
	// （→丢弃全部定向 @），仅记 Warn、不让整条推送失败。
	membersByName := map[string]string{}
	if len(gateUIDs) > 0 {
		got, err := w.db.filterGroupMembers(m.GroupNo, gateUIDs)
		if err != nil {
			w.Warn("filter group members for mention failed; dropping targeted @uids",
				zap.String("webhook_id", m.WebhookID), zap.Error(err))
		} else {
			membersByName = got
		}
	}
	// 成员集（key 即成员）供 assembleMention / finalizeEntities 判定归属；昵称仅 render 取用。
	members := make(map[string]struct{}, len(membersByName))
	for u := range membersByName {
		members[u] = struct{}{}
	}

	// 定向昵称渲染（opt-in、仅 text 路径、且与调用方自带 entities 互斥——后者权威）。renderUIDs 是
	// uids 里的本群成员、保持调用方顺序，由 composeMentionContent 逐个解析昵称、前置 "@<昵称> "
	// 并生成对应 entity。
	renderEffective := mr.Render && isText && len(ents) == 0
	var renderUIDs []string
	if renderEffective {
		for _, u := range uids {
			if _, ok := membersByName[u]; ok {
				renderUIDs = append(renderUIDs, u)
			}
		}
	}

	// 服务端补 @ 前缀【仅 text 路径】：广播字面量（获批的 all/bots）+ 定向 @昵称（render）。
	// content 为改写后的正文；prefixU16 供下面把调用方自带 entities 右移；genEntities 是 render
	// 生成的定向 entities。richtext（isText=false）不补、content 原样、payload 字节不变。
	content := req.Content
	prefixU16 := 0
	var genEntities []interface{}
	if isText {
		content, prefixU16, genEntities = composeMentionContent(
			req.Content, mr.All, mr.Bots, allowAll, allowBots,
			renderEffective, renderUIDs, membersByName, maxContentRunes())
	}

	mention, ignored := assembleMention(uids, members, mr.All, mr.Bots, allowAll, allowBots)

	// mention.entities 二选一（render 与调用方自带 entities 互斥）：
	//   - render 生成的定向 entities：offset 已含全部前缀长度（compose 内按前缀位置算），无需再移；
	//   - 调用方自带 entities（#449）：相对【原始 req.Content】校验（offset/'@' 锚点按未前置文本），
	//     再整体右移 prefixU16——前置只在文首插入，故 offset+prefixU16 仍精确指向同一 '@'。
	// 与 uids 正交，ExpandAisToBotUIDs 只动 uids、不碰 entities。
	switch {
	case len(genEntities) > 0:
		if mention == nil {
			mention = map[string]interface{}{}
		}
		mention[mentionrewrite.EntitiesKey] = genEntities
	case len(ents) > 0:
		if validEnts := finalizeEntities(ents, members, req.Content); len(validEnts) > 0 {
			shiftEntityOffsets(validEnts, prefixU16)
			if mention == nil {
				mention = map[string]interface{}{}
			}
			mention[mentionrewrite.EntitiesKey] = validEnts
		}
	}
	return mention, content, ignored
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

// shiftEntityOffsets 把 finalizeEntities 产出的线协议 entities 的 offset 整体右移 by 个
// UTF-16 码元（就地）。广播补文案在文首前置了前缀、改变了 content 时，定向 entities 的 offset
// 必须右移同样长度，否则 web/Android 会按旧 offset 把气泡错绑到前缀文本上。by<=0 为空操作；
// 只移 offset、不动 length（区间长度不变），uid 不变。
func shiftEntityOffsets(ents []interface{}, by int) {
	if by <= 0 || len(ents) == 0 {
		return
	}
	for _, e := range ents {
		m, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		if off, ok := m[entityKeyOffset].(int); ok {
			m[entityKeyOffset] = off + by
		}
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
