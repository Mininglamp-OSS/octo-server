package incomingwebhook

import (
	"encoding/json"

	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/gocraft/dbr/v2"
)

// incoming_webhook.status 取值。SQL 列为 SMALLINT，仅 0/1 由 init 迁移注释覆盖；
// 2 是 #254 引入的软删除标记，复用同一列、无需迁移。
//
//   - statusEnabled  正常可推送（push 闸要求 status == statusEnabled）。
//   - statusDisabled 管理员主动关停，或群解散级联禁用（handleGroupDisband）。
//   - statusDeleted  软删除：保留行供历史消息渲染（display datasource 不按 status
//     过滤），同时 push 自动失效、从管理列表隐藏、释放每群配额，且不可经 update 复活。
const (
	statusDisabled = 0
	statusEnabled  = 1
	statusDeleted  = 2
)

// incomingWebhookModel 对应 incoming_webhook 表。
type incomingWebhookModel struct {
	WebhookID  string
	TokenHash  string
	GroupNo    string
	SpaceID    string
	Name       string
	Avatar     string
	CreatorUID string
	Status     int
	// AllowMentionAll / AllowMentionBots 是【广播型 @】的 per-webhook 能力位
	// （0=否[默认] / 1=是）：分别授权该 webhook 推送 @所有人(真人广播,→mention.humans)
	// 与 @所有 AI(bot 广播,→mention.ais)。广播会刷全群红点 / 唤起全部 bot，是高噪声能力，
	// 故默认关闭、需显式打开；由 webhook 的合法成员（创建者，或群管理员）在 create/update
	// 时开关——与「成员可自建/自管 webhook」一致，能力位本身即该广播能力的唯一闸。
	// 定向 @uid（指定一个/多个成员）不受这两个开关约束，但受「群成员闸 + 去重 + 上限」。
	AllowMentionAll  int
	AllowMentionBots int
	LastUsedAt       dbr.NullTime
	CallCount        int64
	db.BaseModel
}

// 审计投递结果（auditModel.Status）。1/2 而非 0/1：避免「未设置即默认 0」被误读为
// 一个有效结果——历史行与新成功行都应是 auditSuccess(1)，迁移默认值即 1。
const (
	auditSuccess = 1
	auditFailed  = 2
	// auditSkipped 「已接收、刻意不投递」：GitHub ping / 渲染子集之外的事件等，响应
	// 200（平台侧显示投递成功）但没有消息进群。单列一种状态而非伪装成成功（message_id=0
	// 的"成功"会误导排障）或失败（调用方无需修复任何东西）。
	auditSkipped = 3
)

// 投递来源/适配器（auditModel.Adapter）。
const (
	adapterNative  = "native"  // 原生推送端点
	adapterTest    = "test"    // 管理端「测试推送」
	adapterGitHub  = "github"  // GitHub 事件适配器（#297 Phase 3）
	adapterWeCom   = "wecom"   // 企业微信群机器人格式适配器（#297 Phase 3）
	adapterMultica = "multica" // Multica 出站 webhook 适配器（#426）
	adapterGitLab  = "gitlab"  // GitLab 事件适配器（#297 Phase 4）
	adapterFeishu  = "feishu"  // 飞书自定义机器人格式适配器（#297 Phase 4）
)

// auditModel 对应 incoming_webhook_audit 表，记录【鉴权通过后】的每次投递结果
// （成功+失败）。鉴权失败（未知/错 token）不落本表，仅进 IP 失败预算，维持反枚举。
type auditModel struct {
	WebhookID  string
	GroupNo    string
	IP         string
	ByteSize   int
	MessageID  int64
	Status     int    // auditSuccess / auditFailed / auditSkipped
	Reason     string // 失败/跳过原因码；成功为空
	HTTPStatus int    // 返回给调用方的 HTTP 状态码
	Adapter    string // 来源/适配器：adapterNative / adapterTest / adapterGitHub / adapterWeCom / adapterMultica
	db.BaseModel
}

// pushPayloadReq 推送端点的请求体。
//
// 兼容两种消息形态，由 MsgType 选择（缺省/"text" 走纯文本，与历史行为一致）：
//   - 纯文本：Content 必填，客户端按 markdown 渲染（历史契约不变）。
//   - 富文本/图文混排（MsgType=="richtext"）：Blocks 承载有序图文块，服务端翻译为
//     octo 原生 RichText(=14) 后走 richtext.Validate/Finalize。对外刻意不暴露内部
//     ContentType 数字与 plain 等服务端字段——调用方只需描述「文本块 / 图片块」。
type pushPayloadReq struct {
	MsgType string `json:"msg_type,omitempty"`
	Content string `json:"content"`
	// Text 是 Content 的别名（Slack/部分平台用 "text"）：Content 为空时回退到 Text，
	// 降低从既有集成迁移的改造成本。两者都填以 Content 为准。
	Text      string                 `json:"text,omitempty"`
	Blocks    []webhookBlock         `json:"blocks,omitempty"`
	Username  string                 `json:"username,omitempty"`
	AvatarURL string                 `json:"avatar_url,omitempty"`
	Extra     map[string]interface{} `json:"extra,omitempty"`
	// Mention 让调用方 @ 群成员（用户/bot 同一 UID 命名空间，一个或多个）并可选广播。
	// 仅 native 端点解析（pushAdapter.allowMention），适配器(企业微信/飞书/GitHub…)不支持。
	// 服务端把它翻译成消息 payload 的 mention 子对象（buildMention），定向 uids 经群成员闸
	// 过滤、广播位经 per-webhook 能力位放行；其余 mention 语义（红点/唤起 bot）全由下游
	// 既有 message 监听器完成。绝不透传到 payload，与 Extra 被丢弃同源（见 buildPayload）。
	//
	// 类型刻意是 json.RawMessage 而非 *mentionReq：mention 是【可选】字段，其形状非法
	// （如 uids 传成字符串）不能把整条推送 400 掉——否则相邻的合法 content 也被连累丢弃，
	// 违反 acceptance #6「malformed mention.uids → no panic, no mention key, 消息照投」。
	// 故此处只做惰性捕获，真正的宽松解码在 buildMention/decodeMention：解码失败即降级为
	// 「无 mention」、消息照常投递。
	Mention json.RawMessage `json:"mention,omitempty"`
}

// mentionReq 是 native 推送请求里的 @ 描述（对外契约）。两个广播位刻意不暴露内部线协议的
// humans/ais 字段名：调用方只写「@所有人(all) / @所有 AI(bots)」，由服务端 buildMention 翻译
// 为 mention.{humans,ais} 并做能力位校验。Entities 则【直接以线协议字段名 entities 接收】——
// 它是渲染层 [{uid,offset,length}]，本就是端上要消费的形态、无可翻译语义，服务端只做成员闸 +
// 越界/锚点校验后原样透传（见 Entities 字段注释）。
//   - Uids     ：要 @ 的成员 UID（用户或 bot），去重后受上限约束，且必须是本群当前成员。
//   - All      ：@所有人（真人广播），受 webhook 的 allow_mention_all 能力位约束。
//   - Bots     ：@所有 AI（bot 广播），受 webhook 的 allow_mention_bots 能力位约束。
//   - Entities ：渲染层 @ 区间（详见字段注释）；定向渲染、不受广播能力位约束。
type mentionReq struct {
	Uids []string `json:"uids,omitempty"`
	All  bool     `json:"all,omitempty"`
	Bots bool     `json:"bots,omitempty"`
	// Entities 是【渲染层】的 @ 区间（线协议 mention.entities：[{uid,offset,length}]），
	// 与 uids（路由层）正交：调用方自带 content 文本与每个 @ 的 offset/length，服务端只校验
	// 每条 entity 的 uid 是本群成员、offset/length 落在 content 的 UTF-16 码元范围内且 offset
	// 处确为 '@'，合法者原样透传到线协议供端上权威渲染气泡（web 用 entities 精确绑定、Android
	// 校验后保留、iOS 忽略改按位置解析）。offset/length 单位是 UTF-16 码元（= JS .length /
	// Java/Kotlin String / NSString），不是字节、也不是 rune。
	//
	// 类型是 []json.RawMessage 以做【逐条】宽松解码：单条形状非法（如 offset 传字符串）只丢
	// 该条，不连累其余 entity，也不影响 mention 的 uids/all/bots——与 decodeMention 的宽松
	// 契约（malformed → 降级、不 400、不丢消息）一致。
	Entities []json.RawMessage `json:"entities,omitempty"`
}

// webhookBlock 是富文本消息的单个有序块（对外契约）。字段刻意与 octo-lib
// common.RichTextBlock 对齐但独立声明：对外只暴露 text/image 两类块所需的字段，
// 不让调用方感知内部 ContentType 编号或 plain/size 等服务端派生字段。
//   - type=="text"  ：Text 必填且非空。
//   - type=="image" ：URL（仅 http/https）+ Width/Height（>0）必填，供端上占位排版。
//
// ⚠️ 新增块类型时务必与 pkg/richtext（及其底层 common.RichTextBlock 支持的类型）保持
// 同步：buildRichTextPayload 按白名单逐类翻译，未在此显式支持的块类型会被 Validate 拒绝。
type webhookBlock struct {
	Type   string `json:"type"`
	Text   string `json:"text,omitempty"`
	URL    string `json:"url,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

// createReq 管理端创建 webhook 的请求体。
// AllowMention* 为广播能力位，任意合法成员均可设置；缺省/false 即默认关闭。
type createReq struct {
	Name             string `json:"name"`
	Avatar           string `json:"avatar,omitempty"`
	AllowMentionAll  *bool  `json:"allow_mention_all,omitempty"`
	AllowMentionBots *bool  `json:"allow_mention_bots,omitempty"`
}

// updateReq 修改 webhook 的请求体。零值字段不更新。
// AllowMention* 由 webhook 的合法成员（创建者/管理员）可改。
type updateReq struct {
	Name             *string `json:"name,omitempty"`
	Avatar           *string `json:"avatar,omitempty"`
	Status           *int    `json:"status,omitempty"`
	AllowMentionAll  *bool   `json:"allow_mention_all,omitempty"`
	AllowMentionBots *bool   `json:"allow_mention_bots,omitempty"`
}

// webhookResp 对外暴露的 webhook 元信息（不含 token / token_hash）。
// creator_uid 与登录 uid 对比可判断"是否我创建的"（成员仅可管理自己创建的）。
type webhookResp struct {
	WebhookID  string `json:"webhook_id"`
	GroupNo    string `json:"group_no"`
	Name       string `json:"name"`
	Avatar     string `json:"avatar"`
	CreatorUID string `json:"creator_uid"`
	Status     int    `json:"status"`
	// AllowMentionAll / AllowMentionBots 回显广播能力位（0/1），便于管理端 UI 渲染开关。
	AllowMentionAll  int   `json:"allow_mention_all"`
	AllowMentionBots int   `json:"allow_mention_bots"`
	LastUsedAt       int64 `json:"last_used_at"`
	CallCount        int64 `json:"call_count"`
	CreatedAt        int64 `json:"created_at"`
}

// createResp 创建/重置返回；token 仅此一次出现。
type createResp struct {
	webhookResp
	Token string `json:"token"`
	URL   string `json:"url"`
	// URLs 各推送形态的路径（native / github / wecom / multica），与 URL 一样不含 host、由前端
	// 拼接基础域名。token 仅在 create/regenerate 时可见，故完整推送路径也只在这两处
	// 下发（list 不回显 token，自然也不回推送 URL）。
	URLs map[string]string `json:"urls"`
}

// deliveryResp 是 deliveries 排障端点返回的单条投递记录（成功+失败）。绝不含 token。
//
// 刻意【不】下发调用方 IP：审计表仍存 ip（限流/排查上下文），但向群管理员暴露来源 IP
// 是隐私取舍，按 review 决定收敛——deliveries 只回投递结果元数据，不回 IP（PR #299 review）。
type deliveryResp struct {
	Status     int    `json:"status"`      // auditSuccess / auditFailed / auditSkipped
	Reason     string `json:"reason"`      // 失败/跳过原因码；成功为空
	HTTPStatus int    `json:"http_status"` // 返回给调用方的 HTTP 状态码
	Adapter    string `json:"adapter"`     // 来源/适配器
	ByteSize   int    `json:"byte_size"`
	MessageID  int64  `json:"message_id"`
	CreatedAt  int64  `json:"created_at"`
}
