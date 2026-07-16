package messages_search

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// as-user(OBO) 搜索鉴权接线（YUJ-53 / #F）。
//
// 「search as user」= 请求带 on_behalf_of，经 OBO 以 grantor（当前=创建者）身份搜索。
// 鉴权语义（决策十）= 真人分支（grantor 真人可达 + 双向 blacklist，主体=grantorUID）∩
// OBO 已授 scope。scope 校验复用发消息侧 bot_api/obo_check.go 的
// grant + scope + grantorCanReadChannel 实时权限——因此 TOCTOU 与发消息侧一致：grantor 被
// 踢出群 / 解除好友 / scope 被关后立刻搜不到（防拿旧 grant 越权）。
//
// messages_search 刻意不导入 bot_api（保持解耦，见 principal.go 顶部说明），OBO 校验以注入
// 式 seam 承载：#B 装配 bot 搜索路由时用 SetOBOChecker 注入 bot_api 支持的适配器
// （*bot_api.BotAPI.SearchOBOAllowed 结构性满足 oboChecker）。
//
// 归一化（决策九）：单频道门（oboCanReadChannel）与 global allowlist
// （oboEnumerateReadableChannels）共用同一 oboChecker，保证「单频道放行 ⇔ 频道出现在
// allowlist」双向一致，杜绝两路漂移。

// oboChecker 是 as-user(OBO) 搜索 scope 门的注入 seam。语义与发消息侧 checkOBO 完全一致。
// 返回约定（存在性隐藏 + fail-closed）：
//   - (true,  nil) → (channelID, channelType) 对 grantor 已授权 → 放行；
//   - (false, nil) → ErrOBONotAuthorized 语义（无 grant / scope disabled / grantor 已失去
//     实时访问）→ 调用方渲染 NOT_FOUND，不泄露 grant/scope 是否存在；
//   - (false, err) → 基础设施错误 → 调用方 fail-closed（INTERNAL_ERROR）。
type oboChecker interface {
	SearchOBOAllowed(botUID, grantorUID, channelID string, channelType uint8) (bool, error)
}

// errOBONoChecker as-user(OBO) 主体命中搜索但路由未注入 oboChecker（#B 装配遗漏）。
// fail-closed：绝不在缺 checker 时把 obo 搜索降级为无 scope 约束的 grantor 全量。
var errOBONoChecker = errors.New("messages_search: obo checker not wired (YUJ-49 route assembly)")

// SetOBOChecker 注入 as-user(OBO) scope 门实现（#B 在装配 bot 搜索路由时调用）。
func (h *Handler) SetOBOChecker(c oboChecker) { h.oboCheck = c }

// oboAllowed 带 nil-guard 地调用注入的 oboChecker；缺 checker → fail-closed error。
func (h *Handler) oboAllowed(botUID, grantorUID, channelID string, channelType uint8) (bool, error) {
	if h.oboCheck == nil {
		return false, errOBONoChecker
	}
	return h.oboCheck.SearchOBOAllowed(botUID, grantorUID, channelID, channelType)
}

// oboCanReadChannel 是 as-user(OBO) 的单频道门（canReadChannel 的 obo 分支）。
// 语义 = grantor 真人分支（checkChannelAccess，含双向 blacklist）∩ OBO 已授 scope。
// 任一不过 → NOT_FOUND（存在性隐藏，与真人拒绝同面）；基础设施错 → INTERNAL（fail-closed）。
//
// 真人分支在先：以 grantorUID 为主体复用现有 checkChannelAccess（#C/#D 的 user 分支），
// 覆盖群/子区成员、P2P 好友/同 Space、双向 blacklist；随后 ∩ OBO scope 二次收窄。两段的
// grantorCanReadChannel/成员校验都取实时权限，故 TOCTOU 双重兜底。
func (h *Handler) oboCanReadChannel(c *wkhttp.Context, p Principal, channelType uint8, channelID string) bool {
	op, ok := p.(oboPrincipal)
	if !ok {
		// 不应发生：dispatch 已按 Kind 分派。防御性 fail-closed。
		h.Error("messages_search: obo gate received non-obo principal",
			zap.String("kind", p.Kind().String()))
		respondInternal(c)
		return false
	}
	// 1) 真人分支（主体=grantorUID）。拒绝时 checkChannelAccess 已渲染 NOT_FOUND/INTERNAL。
	if !h.checkChannelAccess(c, channelType, channelID, op.grantorUID) {
		return false
	}
	// 2) ∩ OBO 已授 scope：grant + scope + grantorCanReadChannel 实时权限（TOCTOU）。
	allowed, err := h.oboAllowed(op.botUID, op.grantorUID, channelID, channelType)
	if err != nil {
		h.Error("messages_search: obo scope check failed",
			zap.String("bot_uid", op.botUID),
			zap.String("grantor_uid", op.grantorUID),
			zap.Uint8("channel_type", channelType),
			zap.String("channel_id", channelID),
			zap.Error(err))
		respondInternal(c)
		return false
	}
	if !allowed {
		// 存在性隐藏：与真人拒绝一致，不泄露 grant/scope 是否存在。
		respondNotFound(c, "channel")
		return false
	}
	return true
}

// oboEnumerateReadableChannels 是 as-user(OBO) 的 global allowlist
// （enumerateReadableChannels 的 obo 分支），与 oboCanReadChannel 同一谓词（决策九）：
// 先取 grantor 真人可达集（buildAllowlist），再 ∩ OBO 已授 scope 逐频道收窄。保证
// 「单频道门放行 ⇔ 频道出现在 global allowlist」双向一致。
//
// 基础设施错 → 整体 fail-closed（返回 err；上游按现有真人 allowlist 失败策略处理，不静默截断）。
func (h *Handler) oboEnumerateReadableChannels(c *wkhttp.Context, p Principal) ([]channelRef, []channelRef, []channelRef, allowlistTimings, error) {
	op, ok := p.(oboPrincipal)
	if !ok {
		h.Error("messages_search: obo allowlist received non-obo principal",
			zap.String("kind", p.Kind().String()))
		return nil, nil, nil, allowlistTimings{}, errOBONoChecker
	}
	allowGroup, allowDM, allowThread, timings, err := h.buildAllowlist(c, op.grantorUID, op.spaceID)
	if err != nil {
		return nil, nil, nil, timings, err
	}
	fg, err := h.filterChannelsByOBO(op.botUID, op.grantorUID, allowGroup)
	if err != nil {
		return nil, nil, nil, timings, err
	}
	fdm, err := h.filterChannelsByOBO(op.botUID, op.grantorUID, allowDM)
	if err != nil {
		return nil, nil, nil, timings, err
	}
	fth, err := h.filterChannelsByOBO(op.botUID, op.grantorUID, allowThread)
	if err != nil {
		return nil, nil, nil, timings, err
	}
	return fg, fdm, fth, timings, nil
}

// filterChannelsByOBO 逐频道 ∩ OBO 已授 scope，仅保留放行的频道。每个 channelRef 用自身
// WireID（DM=peer uid、群=group_no、子区=组合 id `<group>____<short>`）+ ChannelType 求值，
// 与发消息侧 checkOBO 的 per-type 语义完全一致。基础设施错 → 立即返回 err（fail-closed，
// 不部分放行）。ErrOBONotAuthorized 语义的频道被静默剔除（存在性隐藏）。
func (h *Handler) filterChannelsByOBO(botUID, grantorUID string, refs []channelRef) ([]channelRef, error) {
	if len(refs) == 0 {
		return refs, nil
	}
	out := make([]channelRef, 0, len(refs))
	for _, ref := range refs {
		allowed, err := h.oboAllowed(botUID, grantorUID, ref.WireID, ref.ChannelType)
		if err != nil {
			return nil, err
		}
		if allowed {
			out = append(out, ref)
		}
	}
	return out, nil
}
