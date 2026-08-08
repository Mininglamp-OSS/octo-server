package message

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/pkg/authtree"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// requireBoundSpaceGroup 把 uk 树上的群 / 子区单条消息读钉在 API Key 绑定的那个 Space。
//
// 为什么需要它：getGroupMessage / getThreadMessage 的授权只有 requireGroupMember，那道门
// 校验的是「群仍活跃」+「调用者是活跃成员」，从不比较群的 Space 与凭据绑定的 Space；两个
// handler 也都不读 pkg/space 的 GetSpaceID，所以 enforceKeySpace 的 space_id 注入对它们
// 是 no-op。结果是绑定 A 空间的 key，只要主人同时也是 B 空间某群的成员，就能用这把 A 的
// key 把该 B 群的消息读出来——暴露面被主人的真实群成员资格限住，但仍然破坏了「每把 key
// 封闭在一个租户内」这个本 PR 对外声明的属性。
//
// 归属判定复刻 space_filter.go 里 decideConvKeepInSpace 的群分支，避免读侧与会话列表侧
// 的口径漂移（同一个群不能在会话列表里被隐藏、却能被单条直查读到）：
//
//	group.space_id == bound                → 放行（精确匹配）
//	调用者是该群外部成员，其 source_space_id
//	（空则回落默认 Space）== bound          → 放行（外部成员的归属空间是来源 Space）
//	group.space_id == "" && bound == 默认Space → 放行（未回填 space_id 的存量群归属
//	                                        用户默认 Space，同 issue #484 口径）
//	其余                                    → 404
//
// 这里比 decideConvKeepInSpace **严格一格**，方向是 fail-closed：那边在外部成员分支不中
// 之后还会落到 `spaceID == ""` 的兜底，于是「外部成员 + 群 space_id 未回填 + source_space_id
// 非空且不等于 bound」这一角，会话列表在 bound == 默认 Space 时仍保留，而这里 404。上面那条
// 不变量（列表隐藏的群不得可直查）依然成立，反过来不成立——差异只出现在这一角，且偏向隐藏。
//
// 一律 404 而不是 403，与 requireGroupMember 的所有拒绝分支同码：直查接口不能泄露
// 「这个群存在但不属于你这把 key 的空间」这一信号。
//
// 默认 Space 查询失败时置空串 sentinel（不是置 bound），理由与 space_filter.go 的
// defaultSpaceID 完全相同：置 bound 会让上面第三条门对任意 bound 恒真，把所有未回填
// space_id 的存量群向每个 Space 放开。bound 在此恒非空，故 "" 是「永不等于 bound」的
// fail-closed sentinel。
func (m *Message) requireBoundSpaceGroup() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		bound := authtree.BoundSpaceID(c)
		// 绑定为空说明没经过 uk 树的 enforceKeySpace（那里已对无绑定 key fail-closed）
		// 或装配有误；本 guard 的约束全部依赖绑定值，拿不到就 fail-closed。
		if bound == "" {
			httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
			c.Abort()
			return
		}

		groupNo := c.Param("group_no")
		loginUID := c.GetLoginUID()
		allowed, err := m.groupInBoundSpace(groupNo, loginUID, bound)
		if err != nil {
			m.Error("校验群归属 Space 失败", zap.String("group_no", groupNo), zap.String("space_id", bound), zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			c.Abort()
			return
		}
		if !allowed {
			httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
			c.Abort()
			return
		}
		c.Next()
	}
}

// groupInBoundSpace 判定 groupNo 这个群对 loginUID 是否归属 bound 空间。
// 决策规则见 requireBoundSpaceGroup 的头注释；DB 故障返回 error（由调用方转 5xx），
// 「查不到群」与「不归属」都返回 false。
func (m *Message) groupInBoundSpace(groupNo, loginUID, bound string) (bool, error) {
	g, err := m.groupDB.QueryWithGroupNo(groupNo)
	if err != nil {
		return false, err
	}
	// 群不存在时交给 requireGroupMember 去回 404，这里只需判定「不归属」。
	if g == nil {
		return false, nil
	}
	if g.SpaceID == bound {
		return true, nil
	}

	// 外部成员的归属空间是自己的 source_space_id，不是群的 space_id。用与 space_filter
	// 同一个数据来源（QueryExternalGroupNosForUser）取，map 是否命中即「是否外部成员」——
	// 单查 source_space_id 无法区分「不是外部成员」与「是外部成员但来源空间为空」，而这
	// 两种情况的归属结论不同。
	externalMap, err := m.groupDB.QueryExternalGroupNosForUser(loginUID)
	if err != nil {
		return false, err
	}
	sourceSpaceID, isExternal := externalMap[groupNo]

	// 默认 Space 只在前两个信号都为空时才需要，放到这里查避免给主路径多一次查询。
	defaultSpaceID := func() string {
		if (isExternal && sourceSpaceID != "") || (!isExternal && g.SpaceID != "") {
			return ""
		}
		id, derr := space.GetUserDefaultSpaceIDE(m.ctx, loginUID)
		if derr != nil {
			m.Warn("查询默认 Space 失败，无归属空间的群按 fail-closed 隐藏",
				zap.Error(derr), zap.String("loginUID", loginUID))
			return ""
		}
		return id
	}()

	return groupSpaceAllows(g.SpaceID, isExternal, sourceSpaceID, bound, defaultSpaceID), nil
}

// groupSpaceAllows 是 groupInBoundSpace 的纯决策核心：接收 DB 已查出的全部输入，
// 判定该群对调用者是否归属 bound。抽成纯函数以便 table-driven 测试覆盖全部分支而
// 不需要 DB（成例见 personDMAccessDecision）。
//
// 归属空间的取法与 decideConvKeepInSpace 的群分支一致：外部成员看自己的
// source_space_id，普通成员看群的 space_id，两者为空则回落调用者默认 Space。
// bound 恒非空（调用方已 fail-closed 掉空绑定），所以任何一个空的归属信号都不会
// 意外匹配上。
func groupSpaceAllows(groupSpaceID string, isExternal bool, sourceSpaceID, bound, defaultSpaceID string) bool {
	if bound == "" {
		return false
	}
	if groupSpaceID == bound {
		return true
	}
	effective := groupSpaceID
	if isExternal {
		effective = sourceSpaceID
	}
	if effective == "" {
		effective = defaultSpaceID
	}
	return effective != "" && effective == bound
}
