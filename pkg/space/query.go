package space

import (
	"sort"

	"github.com/gocraft/dbr/v2"
)

// SystemBots 是所有 Space 都可见的系统级 Bot UID。
//
// 关键场景 (YUJ-216 / GH#1280)：
//   - 会话同步（POST /v1/conversation/sync）带 X-Space-ID 时，按 Space 过滤
//     普通会话。系统 Bot（botfather 等）与 Space 无关，必须始终对客户端可见，
//     哪怕用户在当前 Space 下尚未与 Bot 产生任何消息（首次进入 Space、
//     本地缓存丢失等）。Web 端在前端做兜底，移动端没有，所以后端
//     需要在 sync 响应中保底注入 SystemBot entry。
//   - 目前系统 Bot 固定为 {botfather, u_10000, fileHelper}。未来加入
//     BUILDER/ADMIN bot 时，只需在此 map 中追加，或改为从配置加载。
//     对外接口通过 SystemBotList() 暴露，便于统一遍历。
var SystemBots = map[string]bool{
	"botfather":    true,
	"u_10000":      true,
	"fileHelper":   true,
	"notification": true,
}

// SystemBotList 以稳定顺序返回所有系统 Bot UID。
//
// 稳定顺序有两个价值：
//  1. sync 响应里保底注入 SystemBot entry 时，保证跨请求顺序一致，
//     方便客户端/测试做幂等比对。
//  2. 未来系统 Bot 列表改为配置驱动时，调用方无需感知底层容器类型。
func SystemBotList() []string {
	uids := make([]string, 0, len(SystemBots))
	for uid := range SystemBots {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// IsSystemBot 判断 UID 是否为系统 Bot。语义等价于 SystemBots[uid]，
// 提供函数版本是为将来切换到配置/DB 后不破坏调用方签名。
func IsSystemBot(uid string) bool {
	return SystemBots[uid]
}

// GetBotUIDs 从给定 UID 列表中查询哪些是 Bot（robot=1），排除系统 Bot。
// 返回 Bot UID 集合。DB 查询失败时返回 error。
func GetBotUIDs(session *dbr.Session, uids []string) (map[string]bool, error) {
	result := make(map[string]bool)
	if len(uids) == 0 {
		return result, nil
	}
	var nonSystemUIDs []string
	for _, uid := range uids {
		if !SystemBots[uid] {
			nonSystemUIDs = append(nonSystemUIDs, uid)
		}
	}
	if len(nonSystemUIDs) == 0 {
		return result, nil
	}
	var botUIDs []string
	_, err := session.Select("uid").From("`user`").
		Where("uid IN ? AND robot=1", nonSystemUIDs).
		Load(&botUIDs)
	if err != nil {
		return nil, err
	}
	for _, uid := range botUIDs {
		result[uid] = true
	}
	return result, nil
}

// CheckBotsInSpace 查询给定 Bot UID 中哪些是指定 Space 的成员。
// 返回在 Space 中的 Bot UID 集合。DB 查询失败时返回 error。
func CheckBotsInSpace(session *dbr.Session, spaceID string, botUIDs map[string]bool) (map[string]bool, error) {
	result := make(map[string]bool)
	if spaceID == "" || len(botUIDs) == 0 {
		return result, nil
	}
	uids := make([]string, 0, len(botUIDs))
	for uid := range botUIDs {
		uids = append(uids, uid)
	}
	// Check space_member (User Bots and manually added bots)
	var memberUIDs []string
	_, err := session.Select("uid").From("space_member").
		Where("space_id=? AND uid IN ? AND status=1", spaceID, uids).
		Load(&memberUIDs)
	if err != nil {
		return nil, err
	}
	for _, uid := range memberUIDs {
		result[uid] = true
	}
	// Check app_bot: platform bots are visible in ALL spaces;
	// space bots are visible in their own space.
	var appBotUIDs []string
	_, err = session.Select("uid").From("app_bot").
		Where("uid IN ? AND status=1 AND (scope='platform' OR (scope='space' AND space_id=?))", uids, spaceID).
		Load(&appBotUIDs)
	if err != nil {
		return nil, err
	}
	for _, uid := range appBotUIDs {
		result[uid] = true
	}
	return result, nil
}

// GetGroupSpaceMap 批量查询群的 space_id，返回 groupNo -> spaceID 映射。
// 需要传入一个能执行 GetGroups 的回调。
func GetGroupSpaceMap(groupNos []string, getGroups func([]string) ([]GroupSpaceInfo, error)) (map[string]string, error) {
	result := make(map[string]string, len(groupNos))
	if len(groupNos) == 0 {
		return result, nil
	}
	groups, err := getGroups(groupNos)
	if err != nil {
		return result, err
	}
	for _, g := range groups {
		result[g.GroupNo] = g.SpaceID
	}
	return result, nil
}

// GroupSpaceInfo 用于 GetGroupSpaceMap 回调的最小接口。
type GroupSpaceInfo struct {
	GroupNo string
	SpaceID string
}

// GetSpaceName returns the name of an active Space by ID. Returns an empty
// string (without error) when the Space does not exist, has status=0, or
// spaceID is empty — callers can treat "no space info" uniformly.
//
// Used by H5 invite / join landing pages (YUJ-168 / GH #1243) to show
// "来自 {space_name}" below the group name so that external-group invitees
// have a trust anchor before they tap "加入群聊".
func GetSpaceName(session *dbr.Session, spaceID string) (string, error) {
	if spaceID == "" {
		return "", nil
	}
	var name string
	err := session.SelectBySql(
		"SELECT name FROM space WHERE space_id=? AND status=1",
		spaceID,
	).LoadOne(&name)
	if err != nil && err != dbr.ErrNotFound {
		return "", err
	}
	return name, nil
}

// IsSpaceActive reports whether a Space exists and is in the normal (status=1)
// state. An empty spaceID is not active. A missing row is (false, nil); only a
// real query failure returns an error, so callers can fail closed on it.
//
// Distinct from GetSpaceName, which answers the same question by proxy through a
// non-empty name. That proxy is fine for a display string but wrong for an
// existence gate: `space`.`name` carries no NOT-NULL-and-non-empty guarantee at
// the storage layer, so a Space whose name is blank would read as absent and an
// admin could never point a setting at it. This asks the question directly.
//
// Both `modules/common` (admin write-path validation) and any consumer that must
// refuse to write into a disbanded Space need this, and neither can import
// `modules/space` — it imports `modules/common`, so the dependency only runs one
// way. This leaf package is the shared floor.
func IsSpaceActive(session *dbr.Session, spaceID string) (bool, error) {
	if spaceID == "" {
		return false, nil
	}
	var count int
	if _, err := session.SelectBySql(
		"SELECT COUNT(*) FROM space WHERE space_id=? AND status=1",
		spaceID,
	).Load(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}
