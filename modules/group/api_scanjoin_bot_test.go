package group

import (
	"testing"
)

// TestGroupScanJoin_BotScanerNotFlipIsExternal 本应以 HTTP 端到端方式验证：
// 当 bot 用户通过扫码入群（/v1/groups/:group_no/scanjoin）时，api.go groupScanJoin
// 不得把 is_external_group flip 到 1（保持与 DELETE / 批量 ADD 路径对称）。
//
// 但 groupScanJoin 的 handler 在入群事务内调用 ctx.EventBegin 开启
// GroupMemberScanJoin / GroupAvatarUpdate / GroupMemberAdd 事件，这依赖
// wkevent.Event 子系统。当前 testutil.NewTestServer 不初始化 ctx.Event —
// 任何触达 EventBegin 的 handler 测试都会在 context.go:136 处 nil-deref
// panic。这是仓库级测试基础设施限制，不是本 PR 引入。
//
// 因此扫码入群路径的 bot 过滤修复以以下方式覆盖：
//
//  1. 代码审查：api.go groupScanJoin 的 flip 判断已改为
//     `isExternal == 1 && scanerInfo.Robot == 0 && group.IsExternalGroup == 0`，
//     与 DELETE（service.go:1365 / api.go:2565 的 QueryExternalMemberCountTx
//     robot=0）、批量 ADD（service.go:1220 的 memberUser.Robot == 0）语义对称。
//
//  2. 同时在 scanjoin 的 memberModel 中补 `Robot: scanerInfo.Robot`，防止
//     bot 入群后被误写成 robot=0 从而在 DELETE 路径被当人类重复计数。
//
//  3. 单测覆盖整条链路的等价行为：
//     - TestQueryExternalMemberCount_ExcludesBots / OnlyBotsReturnsZero /
//     MixedMulti / DeletedHuman 证明 robot=1 + is_external=1 的行不会
//     被 QueryExternalMemberCountTx 计入，从而 DELETE 路径不会把群误判
//     成外部群。
//     - TestAddMembers_BotOnly_DoesNotFlipIsExternalGroup 覆盖批量 ADD 的
//     flip 保护，证明 bot 加入空群不 flip、human 加入仍 flip。
//
//  4. 现实触发面：正式客户端不会让 bot 调 /scanjoin（bot 通过 API 流程入群，
//     不走二维码）。本次修复属于防御性对称补齐，避免未来新入口回归。
//
// 后续若 testutil 引入事件系统 mock（例如 wkevent.NoopEvent），可把本 Skip
// 替换成真实 HTTP 驱动断言 is_external_group == 0。相关 TODO 记录在
// Mininglamp-OSS/octo-server#1184。
func TestGroupScanJoin_BotScanerNotFlipIsExternal(t *testing.T) {
	t.Skip("scanjoin handler 依赖未在 testutil 初始化的 ctx.Event（EventBegin nil-deref）；" +
		"bot 过滤修复由 api.go 代码审查 + 其它 4 个 DB/service 单测等价覆盖。详见函数注释。")
}
