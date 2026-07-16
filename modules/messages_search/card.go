package messages_search

// card-message-protocol P1（spec: .octospec/tasks/card-message-protocol/
// brief.md）：InteractiveCard(=17) 的搜索命中投影支撑。响应侧只投影 plain
// （镜像 buildRichTextDetail 的响应侧投影定位），且必须过 Decision-2
// residual-risk 的单一执法点 —— 命中文档的存储 sender 不是 bot/webhook 身份时
// 投影 [卡片]（round-3 P1-2）。sender 身份判定复用共享的 modules/cardtrust
// （带 LRU 缓存），此处只声明 Handler 依赖的最小接口便于测试替换。
// 索引侧 searchText 物化是 wukongim-message-indexer 的跨仓 follow-up
// （携同一 sender 约束），本仓不闭合。

// payloadTypeCard InteractiveCard 的 payload.type（Decision 1；≠ 名片 Card=7）。
const payloadTypeCard = 17

// cardSenderTruster 是 singleMessageHit 卡片分支依赖的最小判定接口
// （生产实现 = *cardtrust.Resolver；测试可注入 stub）。
type cardSenderTruster interface {
	Trusted(fromUID string) bool
}
