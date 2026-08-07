package message

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

// threadFeatureEnabled 与 modules/thread/1module.go 中 init() 的判定保持一致：
// DM_THREAD_ON=true 或 1 启用，其它（含未设置）禁用。
func threadFeatureEnabled() bool {
	v := strings.ToLower(os.Getenv("DM_THREAD_ON"))
	return v == "true" || v == "1"
}

// visiblesAllows 直接从原始 payload 字节解析 visibles 字段，判定 loginUID 是否允许查看。
//
// 解析行为：
//   - payload 不是 JSON 或没有 visibles → 允许（无白名单约束）
//   - visibles 是空数组 → 允许（与 from() 行为一致：仅在 len > 0 时才生效）
//   - visibles 非空 → 仅当 loginUID 命中其中一个字符串元素时允许
//
// 用 struct + RawMessage 而非 map[string]interface{}：encoding/json 仍线性扫描整个
// payload（成本 O(len(payload))），但只把 visibles 元素留下来，其它字段不建 map / 不
// 二次解码，对 1MB+ payload 的内存峰值显著低于 map 路径。
// 非字符串元素静默忽略（与 from() 的 if limitUID == loginUID 行为一致：
// 类型断言失败时不算命中）。
func visiblesAllows(rawPayload []byte, loginUID string) bool {
	var v struct {
		Visibles []json.RawMessage `json:"visibles"`
	}
	if err := json.Unmarshal(rawPayload, &v); err != nil {
		// payload 损坏或非对象时不约束（与 from() 走 placeholder 后等价：无白名单字段）
		return true
	}
	if len(v.Visibles) == 0 {
		return true
	}
	for _, raw := range v.Visibles {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			continue
		}
		if s == loginUID {
			return true
		}
	}
	return false
}

// payloadIsPlainText 从原始 payload 字节解析 type 字段，判定是否为纯文本消息
// （common.Text=1）。当前 reaction 仅对纯文本开放，其它类型（图片/卡片/系统提示/
// 通话记录等）一律拒绝。
//
// fail-closed：payload 非 JSON / 无 type / type 非整数 / type != 1 一律返回 false。
// 用 json.Number 解析以兼容整数与被序列化成数字串的场景；1.0 这类浮点写法会因
// Int64() 失败而落到 false（IM 落库的 type 恒为整数，正常文本消息不受影响）。
func payloadIsPlainText(rawPayload []byte) bool {
	var v struct {
		Type json.Number `json:"type"`
	}
	if err := json.Unmarshal(rawPayload, &v); err != nil {
		return false
	}
	n, err := v.Type.Int64()
	if err != nil {
		return false
	}
	return n == int64(common.Text.Int())
}

func msgModelToMessageResp(m *messageModel) *config.MessageResp {
	resp := &config.MessageResp{
		MessageID:   m.MessageID,
		MessageSeq:  m.MessageSeq,
		ClientMsgNo: m.ClientMsgNo,
		Setting:     m.Setting,
		FromUID:     m.FromUID,
		ChannelID:   m.ChannelID,
		ChannelType: m.ChannelType,
		Timestamp:   int32(m.Timestamp),
		Payload:     m.Payload,
		Expire:      m.Expire,
	}
	// messageModel.Header 是 IM 写入时落库的 JSON 串（no_persist/red_dot/sync_once）。
	// 不解析的话单条响应这三个字段永远是 0，与 syncChannelMessage 行为不一致。
	// 解析失败保持零值即可，不阻断响应。
	if m.Header != "" {
		_ = json.Unmarshal([]byte(m.Header), &resp.Header)
	}
	return resp
}

// getGroupMessage GET /v1/groups/:group_no/messages/:message_id
func (m *Message) getGroupMessage(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	messageIDStr := c.Param("message_id")
	loginUID := c.GetLoginUID()

	if !thread.IsValidGroupNo(groupNo) {
		respondMessageRequestInvalid(c, "group_no")
		return
	}
	messageID, ok := parsePositiveMessageID(messageIDStr)
	if !ok {
		respondMessageRequestInvalid(c, "message_id")
		return
	}
	if !m.requireGroupMember(c, groupNo, loginUID) {
		return
	}
	m.respondSingleMessage(c, groupNo, common.ChannelTypeGroup.Uint8(), groupNo, "", messageID, loginUID)
}

// getThreadMessage GET /v1/groups/:group_no/threads/:short_id/messages/:message_id
func (m *Message) getThreadMessage(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	shortID := c.Param("short_id")
	messageIDStr := c.Param("message_id")
	loginUID := c.GetLoginUID()

	if !thread.IsValidGroupNo(groupNo) {
		respondMessageRequestInvalid(c, "group_no")
		return
	}
	if !thread.IsValidShortID(shortID) {
		respondMessageRequestInvalid(c, "short_id")
		return
	}
	messageID, ok := parsePositiveMessageID(messageIDStr)
	if !ok {
		respondMessageRequestInvalid(c, "message_id")
		return
	}
	if !m.requireGroupMember(c, groupNo, loginUID) {
		return
	}
	t, err := m.threadDB.QueryByGroupNoAndShortID(groupNo, shortID)
	if err != nil {
		m.Error("查询子区失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	// 与 IM datasource (modules/thread/1module.go:87)、ExistByGroupNoAndShortID
	// (modules/thread/db.go:211)、service.GetThread (modules/thread/service.go:474)
	// 行为对齐：仅拒 ThreadStatusDeleted；归档子区允许访问历史消息（归档后仍可发
	// 消息，发完自动解档）。
	// body 用 "message not found"（与其它 404 一致）避免泄露 thread 是否存在的信号。
	if t == nil || t.Status == thread.ThreadStatusDeleted {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return
	}

	channelID := thread.BuildChannelID(groupNo, shortID)
	m.respondSingleMessage(c, channelID, common.ChannelTypeCommunityTopic.Uint8(), groupNo, "", messageID, loginUID)
}

// getPersonMessage GET /v1/messages/person/:peer_uid/:message_id
//
// 单聊(DM)单条消息直查，用于云盘转存等需要按 (会话, 消息ID) 取单条消息元数据的
// 场景。DM 消息物理存储在 fakeChannelID(= GetFakeChannelIDWith(两个uid) 对称生成的
// "uidA@uidB") 频道下、channel_type=Person。
//
// getPersonMessage GET /v1/messages/person/:peer_uid/:message_id
//
// 单聊(DM)单条消息直查，用于云盘转存等需要按 (会话, 消息ID) 取单条消息元数据的
// 场景。DM 消息物理存储在 fakeChannelID(= GetFakeChannelIDWith(两个uid) 对称生成的
// "uidA@uidB") 频道下、channel_type=Person。
//
// 鉴权与 modules/messages_search/authz.go checkP2PAccess 完整对齐（同一决策
// 分散实现的历史坑参见 checkPersonDMAccess 头注释）：
//   1. 基本参数：peer_uid 非空 / 非自聊 / 非 @ 分隔符（P1-3：GetFakeChannelIDWith
//      内部用 "A@B" 拼接，若 peer_uid 自身含 @，多方 uid 组合会产生同一 fake key
//      的跨会话碰撞，一律 400 拒）；message_id 正整数。
//   2. P2P 访问 (checkPersonDMAccess)：peer-bot 分类 + real-user same-space/friend
//      + 双向 blacklist——system bot (fileHelper/botfather 等) 和 own bot 都
//      归 bot 分支，不走 real-user Space 检查（bot 无 space_member 行）。
//   3. Space 隔离：real-user 消息按 personSpaceAllows 五规则过滤（issue #484）。
//   4. respondSingleMessage 内的 visibles / 双删 / offset 全套过滤。
//
// fakeChannelID 参数顺序：使用 (peerUID, loginUID)，与 sibling 调用(api.go:1439
// 等 sync 路径 = "req.ChannelID(=peer_uid), req.LoginUID") 严格对齐；避免碰撞对
// (CRC32 tie) 场景下同一对话在读/写两侧生成不同 key（P2, PR #708 review）。
func (m *Message) getPersonMessage(c *wkhttp.Context) {
	peerUID := c.Param("peer_uid")
	messageIDStr := c.Param("message_id")
	loginUID := c.GetLoginUID()

	if strings.TrimSpace(peerUID) == "" {
		respondMessageRequestInvalid(c, "peer_uid")
		return
	}
	if peerUID == loginUID {
		respondMessageRequestInvalid(c, "peer_uid")
		return
	}
	// P1-3: peer_uid 含 @ 会与 GetFakeChannelIDWith 的 "A@B" 拼接语义碰撞：
	//   GetFakeChannelIDWith("alice@bob", "carol") == GetFakeChannelIDWith("alice", "bob@carol")
	//   == "alice@bob@carol"
	// 允许含 @ 的 peer_uid 会让一条 DM 消息被"多种 (loginUID, peer_uid) 组合"取到，
	// 变成跨会话读。直接 400 拒。
	if strings.Contains(peerUID, "@") {
		respondMessageRequestInvalid(c, "peer_uid")
		return
	}
	messageID, ok := parsePositiveMessageID(messageIDStr)
	if !ok {
		respondMessageRequestInvalid(c, "message_id")
		return
	}

	// P1-4: P2P 访问 + 双向拉黑门禁。DM 直读绕过 IM kernel 的
	// blacklist / friend / space filter，必须在应用层自查，形成与
	// modules/messages_search/authz.go:139-207 checkP2PAccess 一致的四层门禁：
	// same-space OR friend 二选一放行，双向 blacklist 一票否决。任何一层
	// 不满足都 404 归并（防枚举）。
	if !m.checkPersonDMAccess(c, loginUID, peerUID) {
		return
	}

	fakeChannelID := common.GetFakeChannelIDWith(peerUID, loginUID)

	// Person(DM) Space 隔离：DM 是跨 Space 共享的同一物理频道，单条 DM 读必须与
	// /v1/message/channel/sync 的 filterPersonMessagesBySpace 走同一套
	// personSpaceAllows（issue #484 / YUJ-219-A / GH#1283）；不匹配一律 404 归并
	// （防枚举，与不可见同码）。SpaceMiddleware 未 opt-in（老客户端 / 内部调用不带
	// X-Space-ID）时 spaceID=="" → 跳过 Space 过滤，与 sync 向前兼容口径一致。
	// 提前查一次消息用于 Space 判定；respondSingleMessage 内部会再查一次做完整
	// 可见性校验，代价是主键单条读、可接受。
	if spaceID := spacepkg.GetSpaceID(c); spaceID != "" {
		msgModel, err := m.db.queryMessageByID(fakeChannelID, common.ChannelTypePerson.Uint8(), strconv.FormatInt(messageID, 10))
		if err != nil {
			m.Error("查询 DM 消息失败", zap.Error(err), zap.String("channel_id", fakeChannelID), zap.Int64("message_id", messageID))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return
		}
		if msgModel == nil {
			httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
			return
		}
		defaultSpaceID, derr := space.GetUserDefaultSpaceIDE(m.ctx, loginUID)
		if derr != nil {
			// 与批量 DM 同步路径逐字一致（modules/message/api.go 的 channel/sync 分支）：
			// 默认 Space 查不到时无法判定归属，按兼容口径 fail-open 保留无标签历史，
			// 避免一次 DB 抖动把合法 DM 历史静默截断。两个入口必须同口径，否则同一条
			// 无标签 DM 会出现「sync 能拉到、单条直查 404」的漂移。
			//
			// PR #713 review 有分歧：lml2468 认为这里该照 space_filter.go 用 "" sentinel
			// fail-closed；yujiawei 复核后认为现状正确——那个 sentinel 属于会话/群过滤的
			// 另一套谓词，而 DM 路径的 fail-open 是上面那条注释里写明的既有决策。保持与
			// sibling 一致，方向变更留给人来拍板。
			m.Warn("查询默认 Space 失败，DM 单条读无标签消息按兼容口径放行",
				zap.Error(derr), zap.String("loginUID", loginUID))
			defaultSpaceID = spaceID
		}
		// IsSystemBot 判定基于对端 uid（peerUID），fakeChannelID 是 loginUID+peerUID
		// 对称派生的，非 uid 直接可判。
		if !personSpaceAllows(payloadSpaceIDFromRaw(msgModel.Payload), spacepkg.IsSystemBot(peerUID), spaceID, defaultSpaceID) {
			httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
			return
		}
	}

	// personPeerUID 传 peerUID：respondSingleMessage 里的用户级 channel_offset
	// 表（清历史）用 bare peer uid 作 key，不是 fakeChannelID (P1-1a)。
	m.respondSingleMessage(c, fakeChannelID, common.ChannelTypePerson.Uint8(), "", peerUID, messageID, loginUID)
}

// personDMAccessDecision 是 checkPersonDMAccess 的纯决策核心：接收所有 DB 已
// 查出的输入，返回 allow / notFound / 具体分支（用于测试断言与日志）。抽出为
// 纯函数以便 helper 级 table-driven 测试覆盖 8 条决策路径而无需 mock 38 个方法
// 的 user.IService（PR #708 review round 3 P1-A/coverage 要求）。
//
// 决策次序（与 authz.go:99-207 checkP2PAccess 一致，多一层 SystemBot 短路，
// 详见 checkPersonDMAccess 头注释里"Step 1a"的说明）：
//
//	self==peer                     → allow (notes-to-self)
//	SystemBot(peer)                → check blacklist → allow/notFound
//	own bot(peer)                  → allow (skip blacklist)
//	other user's bot(peer)         → require friend, then check blacklist
//	real user(peer)                → require same-space OR friend, then check blacklist
//
// 每一层 fail 都 fold 成 notFound（防枚举，与 authz.go 一致）。
type personDMAccessInputs struct {
	loginUID, peerUID string
	isSystemBot       bool
	isRobot           bool
	creatorUID        string
	spaceID           string // "" 表示未 opt-in，跳过 same-space
	sameSpace         bool
	isFriend          bool
	blockedByMe       bool
	blockedByPeer     bool
}

type personDMAccessResult int

const (
	personDMAllowNotesToSelf personDMAccessResult = iota
	personDMAllowSystemBot
	personDMAllowOwnBot
	personDMAllowOtherBotFriend
	personDMAllowRealUserSameSpace
	personDMAllowRealUserFriend
	personDMDenyOtherBotNotFriend
	personDMDenyRealUserNoRelation
	personDMDenyBlacklist
)

func personDMAccessDecision(in personDMAccessInputs) personDMAccessResult {
	// Step 0: notes-to-self
	if in.peerUID == in.loginUID {
		return personDMAllowNotesToSelf
	}

	var allowed personDMAccessResult
	switch {
	case in.isSystemBot:
		// SystemBot 短路，落到 Step 3 blacklist（仍然要执行拉黑检查）
		allowed = personDMAllowSystemBot
	case in.isRobot && in.creatorUID == in.loginUID:
		// Own bot：直接放行，skip blacklist（与 authz.go:110-115 一致）
		return personDMAllowOwnBot
	case in.isRobot:
		// Other user's bot：必须是好友；否则 404
		if !in.isFriend {
			return personDMDenyOtherBotNotFriend
		}
		allowed = personDMAllowOtherBotFriend
	default:
		// Real user：same-space OR friend
		if in.spaceID != "" && in.sameSpace {
			allowed = personDMAllowRealUserSameSpace
		} else if in.isFriend {
			allowed = personDMAllowRealUserFriend
		} else {
			return personDMDenyRealUserNoRelation
		}
	}

	// Step 3: 双向 blacklist（own bot 已经在上面 return 跳过）
	if in.blockedByMe || in.blockedByPeer {
		return personDMDenyBlacklist
	}
	return allowed
}

func personDMAccessAllowed(r personDMAccessResult) bool {
	switch r {
	case personDMAllowNotesToSelf, personDMAllowSystemBot, personDMAllowOwnBot,
		personDMAllowOtherBotFriend, personDMAllowRealUserSameSpace, personDMAllowRealUserFriend:
		return true
	}
	return false
}

// checkPersonDMAccess 完整复刻 modules/messages_search/authz.go checkP2PAccess
// 的 real-user DM 访问门禁：peer-bot 分类 + same-space OR IsFriend + 双向
// blacklist。任何不满足条件都 404 归并（与不可见同码，防枚举）。DB 错误
// → 500 fail-closed。返回 false 时已写响应（handler 直接 return）。
//
// **必须与 checkP2PAccess 保持每一步一致**：DM 直读绕过 IM kernel 的
// blacklist filter，同一套访问决策要求分散实现是历史上 issue #484 / YUJ-219-A
// 一类问题的温床；本 helper 是第二份手写实现，任何时候都对齐它的口径。若
// 未来抽出跨模块公共 helper，两处一起替换。
//
// 已核实分支覆盖（决策核心在 personDMAccessDecision，被 personDMAccessDecision_test
// 覆盖 8 条路径）：
//   0. peer_uid == self（notes-to-self）：直接放行
//   1a. SystemBot 短路（fileHelper 等）：落到 Step 3 blacklist
//   1b. Step 1 peer-bot 分类（**必须先于 Space**）：own bot 放行；other bot
//       走 IsFriend + 落到 Step 3 的 blacklist
//   2. Step 2 real-user：same-space OR IsFriend
//   3. Step 3 双向 blacklist 一票否决（own-bot 分支已跳过）
func (m *Message) checkPersonDMAccess(c *wkhttp.Context, loginUID, peerUID string) bool {
	// 收集所有决策输入（DB 查询 + context 读取），然后交给纯决策函数。
	in := personDMAccessInputs{
		loginUID:    loginUID,
		peerUID:     peerUID,
		isSystemBot: spacepkg.IsSystemBot(peerUID),
	}
	if in.peerUID == in.loginUID {
		return true // 上游已拒；defense-in-depth
	}

	// 只在非 SystemBot 时才需要 QueryPeerRobotInfo（Step 1a 短路避免误 404）。
	if !in.isSystemBot {
		isRobot, creatorUID, err := m.userService.QueryPeerRobotInfo(peerUID)
		if err != nil {
			m.Error("DM 直读 QueryPeerRobotInfo 查询失败", zap.Error(err),
				zap.String("uid", loginUID), zap.String("peer", peerUID))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return false
		}
		in.isRobot = isRobot
		in.creatorUID = creatorUID
	}

	// Own bot 快通道：拿到 (isRobot=true, creatorUID==loginUID) 就直接放行，
	// 避免为 own bot 白发一次 friend / same-space / blacklist 查询。
	if in.isRobot && in.creatorUID == loginUID {
		return true
	}

	// Real-user 分支才需要 same-space / friend 查询；bot 分支只需要 friend。
	// 提前判断避免不必要的 DB 请求。
	if !in.isSystemBot && !in.isRobot {
		in.spaceID = spacepkg.GetSpaceID(c)
		if in.spaceID != "" {
			sameSpace, serr := m.userService.AreSpaceMembers(in.spaceID, loginUID, peerUID)
			if serr != nil {
				m.Error("DM 直读 same-space 查询失败", zap.Error(serr),
					zap.String("uid", loginUID), zap.String("peer", peerUID), zap.String("space_id", in.spaceID))
				httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
				return false
			}
			in.sameSpace = sameSpace
		}
	}

	// Friend 查询：real-user (若 !sameSpace 才需要) + other-user bot 需要。
	// SystemBot 与 own bot 都不需要。
	needFriend := (in.isRobot && !in.isSystemBot) ||
		(!in.isSystemBot && !in.isRobot && !in.sameSpace)
	if needFriend {
		isFriend, ferr := m.userService.IsFriend(loginUID, peerUID)
		if ferr != nil {
			m.Error("DM 直读 IsFriend 查询失败", zap.Error(ferr),
				zap.String("uid", loginUID), zap.String("peer", peerUID))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return false
		}
		in.isFriend = isFriend
	}

	// Blacklist 查询：SystemBot / other bot / real-user 都需要（own bot 已在
	// 上面 return true 跳过）。批量版一次查两向。
	blockedByMe, blockedByPeer, err := m.userService.ExistBlacklistsBoth(loginUID, []string{peerUID})
	if err != nil {
		m.Error("DM 直读 blacklist 查询失败", zap.Error(err),
			zap.String("uid", loginUID), zap.String("peer", peerUID))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return false
	}
	in.blockedByMe = blockedByMe[peerUID]
	in.blockedByPeer = blockedByPeer[peerUID]

	result := personDMAccessDecision(in)
	if !personDMAccessAllowed(result) {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return false
	}
	return true
}

// parsePositiveMessageID 校验 path 参数 message_id 为正整数。
// 注：WuKongIM 雪花算法生成的 message_id 始终为正，0 视为非法。
func parsePositiveMessageID(s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// requireGroupMember 校验登录用户是该群的活跃成员（未删除、未被拉黑），
// 并且群本身仍处于活跃状态。失败时 handler 已写入响应；返回 false 表示终止。
//
// 群不存在、群已解散、非成员、被拉黑成员一律返回 404 而不是 403/400：
// 直查接口不能泄露"资源存在但你不可达"的信号，与撤回 / 删除 / visibles 等
// "对当前用户不可见"状态保持同一返回码。
//
// 关键差异说明：
//   - 群解散后 group_member 记录不会清理（见 modules/group/api.go disband 流程），
//     仅 group.status 置为 GroupStatusDisband，所以必须额外查群状态；
//   - 黑名单（group_member.status=GroupMemberStatusBlacklist）的 is_deleted 仍为 0，
//     普通 ExistMember 返回 true。本接口绕过 WuKongIM 直查本地分表，必须用
//     ExistMemberActive 显式排除黑名单，否则被拉黑用户仍能按 ID 把消息读出来
//     （IM 路径有 datasource blacklist 拦截，本路径必须自己拦）。
func (m *Message) requireGroupMember(c *wkhttp.Context, groupNo, loginUID string) bool {
	g, err := m.groupDB.QueryWithGroupNo(groupNo)
	if err != nil {
		m.Error("查询群信息失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return false
	}
	// 读历史白名单：Normal 正常群 + Disband 已解散群都放行。
	// 企业微信式"解散"语义（产品决策 2026-06）：群解散后频道与历史保留，活跃成员
	// 仍可查看历史消息（发消息由 WuKongIM 的 disband flag 拦截，不在本读路径处理）。
	// Disabled（管理员禁用）及未来新增的非正常状态仍 fail closed。成员身份与黑名单
	// 仍由下面的 ExistMemberActive 把关。
	if g == nil || (g.Status != group.GroupStatusNormal && g.Status != group.GroupStatusDisband) {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return false
	}
	isActive, err := m.groupDB.ExistMemberActive(loginUID, groupNo)
	if err != nil {
		m.Error("检查群成员失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return false
	}
	if !isActive {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return false
	}
	return true
}

// respondSingleMessage 共用：按 (channelID, channelType, messageID) 查本地消息正文，
// 拼装 message_extra / message_user_extra / reactions，并复用 syncChannelMessage 的
// channelOffset / 群消息 enrichment（thread_created 快照、外部成员标识），
// 行为与批量同步路径保持一致。
//
// 任何"对当前用户不可见"的状态（撤回 / 双删 / 用户级删 / channel_offset 截断 /
// expire / visibles 白名单未命中）一律返回 404，避免直查接口绕过批量路径的过滤。
//
// groupNoForEnrich 仅在群消息（channelType=Group）时用作 enrichExternalMarkers 的
// 父群定位参数；子区消息这一参数无效（enrich 函数本身按消息 channel_type 分派）。
//
// personPeerUID 仅在 Person(DM) 直查时非空，用于用户级 channel_offset 表的 lookup：
// 用户清历史通过 POST /v1/message/offset 写 channel_offset，DM 场景 writer 存的
// ChannelID 是**对端 bare peer uid**（modules/message/api.go:2821，与 sync 读侧
// api.go:3575 一致），不是消息物理存储用的 fakeChannelID。因此 Person 单条直查
// 用 channelID (== fakeChannelID) 去 queryWithUIDAndChannel 恒 miss、绕过用户级
// 历史清理——必须传 peer uid 才能命中同一行 (P1-1a, PR #708 review round 2)。
// Group/Thread 无 fake，channelID 本身就是 physical id，personPeerUID 保持空即可。
func (m *Message) respondSingleMessage(c *wkhttp.Context, channelID string, channelType uint8, groupNoForEnrich, personPeerUID string, messageID int64, loginUID string) {
	// VARCHAR(20) 列必须用字符串绑定才能命中索引；详见 queryMessageByID 注释。
	messageIDStr := strconv.FormatInt(messageID, 10)
	msgModel, err := m.db.queryMessageByID(channelID, channelType, messageIDStr)
	if err != nil {
		m.Error("查询消息失败", zap.Error(err), zap.String("channel_id", channelID), zap.Int64("message_id", messageID))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	if msgModel == nil {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return
	}

	// visibles 白名单服务端判定：直接基于原始 payload 解析，不依赖 from() 截断后的 map。
	// 必要原因：from() 走 TruncatedPayload，payload > hardParsePayloadLimit (1MB) 时
	// 会替换为 placeholder（不含 visibles 字段），导致 from() 内部的 visibles 检查
	// 拿不到字段、IsDeleted 不会被置 1，依赖"from() 后兜底 IsDeleted"的策略失效，
	// 非白名单成员能拿到该消息的元数据。原始 payload 解析在所有大小下都正确。
	if !visiblesAllows(msgModel.Payload, loginUID) {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return
	}

	extra, userExtra, reactions, err := m.fetchMessageExtras(messageID, loginUID, channelID, channelType)
	if err != nil {
		m.Error("查询消息扩展失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	if extra != nil && (extra.Revoke == 1 || extra.IsDeleted == 1) {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return
	}
	if userExtra != nil && userExtra.MessageIsDeleted == 1 {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return
	}

	// 用户级历史清理偏移：/v1/message/offset 写入 channel_offset(uid, channel_id,
	// channel_type, message_seq)。syncChannelMessage 会在 newSyncChannelMessageResp
	// 内按此跳过 message_seq <= offset 的消息；单条直查必须显式比对，否则用户清理
	// 后还能按 ID 把旧消息读回来。注意这与 lookupChannelOffsetSeq 查的频道级
	// channel_setting.offset_message_seq 是两张不同的表 / 两套语义，必须都查。
	//
	// Person(DM) 场景 offset key: writer 存的 channel_id 是 bare peer uid
	// (api.go:2821 `ChannelID: req.ChannelID` 直存)，不是消息物理 fakeChannelID；
	// sync 读侧也用 bare peer uid (api.go:3575)。因此这里 Person 走 personPeerUID
	// (非空)，其它类型 channelID 本身就是 physical id (P1-1a)。
	userOffsetChannelID := channelID
	if channelType == common.ChannelTypePerson.Uint8() && personPeerUID != "" {
		userOffsetChannelID = personPeerUID
	}
	if userOffset, err := m.channelOffsetDB.queryWithUIDAndChannel(loginUID, userOffsetChannelID, channelType); err != nil {
		m.Error("查询用户清理偏移失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	} else if userOffset != nil && msgModel.MessageSeq <= userOffset.MessageSeq {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return
	}

	channelOffsetSeq, err := m.lookupChannelOffsetSeq(channelID, channelType, loginUID)
	if err != nil {
		m.Error("查询频道设置失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}

	resp := &MsgSyncResp{}
	resp.from(msgModelToMessageResp(msgModel), loginUID, extra, userExtra, reactions, channelOffsetSeq)

	// 与 syncChannelMessage 群路径保持一致：补 ThreadCreated 实时快照和外部成员标识。
	// Person/CommunityTopic 不进入这两个 enrichment（函数内部各自判断 channel_type）。
	if channelType == common.ChannelTypeGroup.Uint8() {
		msgs := []*MsgSyncResp{resp}
		m.enrichThreadCreatedMessages(msgs)
		m.enrichExternalMarkers(groupNoForEnrich, msgs)
	}

	// from() 在 visibles 白名单未命中 / expire / channelOffset 截断时会把 IsDeleted
	// 置 1。批量同步把 IsDeleted=1 留给客户端过滤；单条直查不能依赖客户端，
	// 否则相当于把"白名单消息"在 200 响应里整段下发给非授权用户。
	if resp.IsDeleted == 1 {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return
	}

	c.Response(resp)
}

// fetchMessageExtras 一次拉取 (message_extra, message_user_extra, reactions)。
// 任意一步 DB 错误时返回 wrap 后的错误，由调用方决定响应。
func (m *Message) fetchMessageExtras(messageID int64, loginUID string, channelID string, channelType uint8) (*messageExtraDetailModel, *messageUserExtraModel, []*reactionModel, error) {
	idList := []string{strconv.FormatInt(messageID, 10)}

	extras, err := m.messageExtraDB.queryWithMessageIDsAndUID(idList, loginUID)
	if err != nil {
		m.Error("查询消息扩展失败", zap.Error(err))
		return nil, nil, nil, errors.Wrap(err, "查询消息扩展失败")
	}
	var extra *messageExtraDetailModel
	if len(extras) > 0 {
		extra = extras[0]
	}

	userExtras, err := m.messageUserExtraDB.queryWithMessageIDsAndUID(idList, loginUID)
	if err != nil {
		m.Error("查询用户级扩展失败", zap.Error(err))
		return nil, nil, nil, errors.Wrap(err, "查询用户级扩展失败")
	}
	var userExtra *messageUserExtraModel
	if len(userExtras) > 0 {
		userExtra = userExtras[0]
	}

	reactions, err := m.messageReactionDB.queryWithMessageIDsInChannel(channelID, channelType, idList)
	if err != nil {
		m.Error("查询消息反应失败", zap.Error(err))
		return nil, nil, nil, errors.Wrap(err, "查询消息反应失败")
	}
	return extra, userExtra, reactions, nil
}

// lookupChannelOffsetSeq 查询 channel_setting.offset_message_seq，用于同步/直查
// 场景下过滤已被"频道清历史"截断的旧消息。
//
// 约定：调用者传入的 channelID 必须是**消息物理存储的 channel_id**
// （消息表按此分表；对 Person 而言就是 fakeChannelID，对 Group/Thread 就是
// group_no / thread channel_id）。channel_setting 表的 key 就是这个物理 id，
// 与 sync 路径 (api.go:1437-1443) 同一 fakeChannelID 一致。历史版本对
// channelType==Person 会内部再做一次 GetFakeChannelIDWith(channelID, loginUID)，
// 期望入参是 peer uid；但（a）sync 已经自己算 fake、不走此函数；
// (b) cardCanonicalVisibleToViewer 里的 Person 分支在此调用之前 return true 短路；
// (c) 直查路径 (respondSingleMessage) 传的 channelID 已经是 fakeChannelID。
// 三个 caller 没有一个会传 bare peer uid 进来。为了不再让"错误 caller 二次 fake"
// 成为隐藏坑，这里显式统一：所有 channelType 都直接用 channelID 作 lookup key。
// (P1-1b, PR #708 review round 2.)
func (m *Message) lookupChannelOffsetSeq(channelID string, channelType uint8, loginUID string) (uint32, error) {
	_ = channelType // 参数保留：签名不变，避免影响调用者；语义上不再对类型分派。
	_ = loginUID    // 参数保留：签名不变；不再需要 loginUID 二次 fake。
	settings, err := m.channelService.GetChannelSettings([]string{channelID})
	if err != nil {
		return 0, err
	}
	if len(settings) > 0 && settings[0].OffsetMessageSeq > 0 {
		return settings[0].OffsetMessageSeq, nil
	}
	return 0, nil
}
