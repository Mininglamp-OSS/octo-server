package user

import (
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
)

// derivePersonWhitelistOfUID 计算「谁被允许发给 realUID 这个人」，不做任何前缀猜测。
//
// 这是本仓库对 Person 频道白名单的**唯一**定义：
//
//	friends(uid, is_deleted=0, is_alone=0) ∪ coMembers(uid)
//
// 口径与 broker 一致：WuKongIM 的 allowSend(from, to) 查的是 **to 的频道**上有没有
// from（internal/service/permission.go），所以「X 的 Person 频道白名单」读作
// 「谁能发给 X」，而不是「X 能发给谁」。
//
// 两个调用方共用它，刻意不各写一份：
//   - 1module.go 的 IMDatasource.Whitelist（broker 回调，走下面的 channel_id 包装）；
//   - space_member_removal.go 的 reconcileRemovedMemberChannel（移除/重新加入后覆写）。
//
// 后者是**覆写**语义：这里少算一个人，就等于误摘他的授权。所以两处必须同源，
// 规则改了两边一起改。
//
// 完备性核对（决定覆写是否安全的那一条）：仓库里往裸 Person 频道写白名单的调用点
// 一共七处 —— modules/user 的好友通过 ×2、注册时绑定 botfather、modules/app_bot 的
// 机器人通过、modules/botfather 的三处 —— 每一处在 IMWhitelistAdd 之前都先写了
// **双向好友行**（bot 也不例外，见各处的 AddFriend），其余授予依据只有共处 Space。
// 因此上面这个集合是「本仓库曾经写进裸 Person 频道白名单的一切」的超集。
//
// 与旧的内联实现有一处**行为差异**：共同成员查询失败时，旧代码吞掉错误、返回
// 只含好友的结果；这里改为上抛。对覆写调用方这是必需的（截断的集合会变成一次
// 误摘），对 broker 回调而言返回残缺白名单本来也比报错更糟。
func (f *Friend) derivePersonWhitelistOfUID(ctx *config.Context, realUID string) ([]string, error) {
	if realUID == "" {
		return nil, fmt.Errorf("derive person whitelist: empty uid")
	}

	friends, err := f.userService.GetFriends(realUID)
	if err != nil {
		return nil, fmt.Errorf("query friends of %s: %w", realUID, err)
	}
	uidSet := make(map[string]struct{}, len(friends))
	for _, friend := range friends {
		// 单向好友（对方已把你删掉，is_alone=1）不进白名单，与 isMutualFriend 同口径。
		if friend.IsAlone == 0 {
			uidSet[friend.UID] = struct{}{}
		}
	}

	coMembers, err := space.GetCoMemberUIDs(ctx, realUID)
	if err != nil {
		return nil, fmt.Errorf("query co-members of %s: %w", realUID, err)
	}
	for _, uid := range coMembers {
		uidSet[uid] = struct{}{}
	}

	result := make([]string, 0, len(uidSet))
	for uid := range uidSet {
		result = append(result, uid)
	}
	return result, nil
}

// derivePersonWhitelist 是 derivePersonWhitelistOfUID 的 **channel_id** 包装，
// 供 broker 回调使用：回调拿到的可能是 s{spaceId}_{uid} 形式的频道号。
//
// **手里已经是 uid 的调用方一律不要用它**，直接调 derivePersonWhitelistOfUID。
// 下面这段剥离是启发式的（"s" 开头且含 "_" 就当成带前缀），一个形如 `s..._...`
// 的真实 uid 会被剥成另一个人，于是推导出别人的白名单：
// 对读取路径那只是错值，对覆写路径那是把别人的授权写到他头上、把他自己的摘光。
func (f *Friend) derivePersonWhitelist(ctx *config.Context, channelID string) ([]string, error) {
	// Space channel_id 格式: s{spaceId}_{uid}，提取真实 uid。
	// 用 LastIndex("_") 避免 spaceId 含下划线时解析错误。
	realUID := channelID
	if strings.HasPrefix(channelID, "s") {
		if idx := strings.LastIndex(channelID, "_"); idx >= 0 {
			realUID = channelID[idx+1:]
		}
	}
	return f.derivePersonWhitelistOfUID(ctx, realUID)
}
