package user

import "sync"

// GroupMemberExternalProvider 返回群成员的外部来源/归属 Space 字段
// （对齐 group 模块 /groups/{no}/members 的 memberDetailResp 命名）。
//
// 返回值：
//   - isExternal: 1 表示外部成员；0 表示内部
//   - sourceSpaceID / sourceSpaceName: 来源 Space（仅外部成员非空）
//   - homeSpaceID / homeSpaceName: 相对视角归属 Space
//     （外部 → source space；内部 → 群自身 space）
//   - err: 底层查询失败
//
// 当 groupNo/uid 为空或成员不存在时，返回全零值和 nil error。
// 由 group 模块在 init 阶段通过 RegisterGroupMemberExternalProvider 注入，
// 避免 user 模块反向依赖 group 包，保留单模块编译、按需启用的能力。
type GroupMemberExternalProvider func(groupNo, uid string) (
	isExternal int,
	sourceSpaceID, sourceSpaceName string,
	homeSpaceID, homeSpaceName string,
	err error,
)

var (
	groupMemberExternalMu       sync.RWMutex
	groupMemberExternalProvider GroupMemberExternalProvider
)

// RegisterGroupMemberExternalProvider 注册群成员外部性字段提供者（供 group 模块调用）。
// 仅在 init 阶段调用一次，后续并发读安全由 RWMutex 提供。
func RegisterGroupMemberExternalProvider(fn GroupMemberExternalProvider) {
	setGroupMemberExternalProvider(fn)
}

func setGroupMemberExternalProvider(fn GroupMemberExternalProvider) {
	groupMemberExternalMu.Lock()
	groupMemberExternalProvider = fn
	groupMemberExternalMu.Unlock()
}

// getGroupMemberExternalProvider 返回已注册的提供者；未注册时返回 nil，
// 调用方需要显式判空以兼容未加载 group 模块的场景（例如裁剪后的单元测试）。
func getGroupMemberExternalProvider() GroupMemberExternalProvider {
	groupMemberExternalMu.RLock()
	defer groupMemberExternalMu.RUnlock()
	return groupMemberExternalProvider
}
