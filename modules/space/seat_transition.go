package space

import (
	"fmt"
	"time"

	"github.com/gocraft/dbr/v2"
)

// 席位翻转的**唯二**入口。
//
// # 为什么要有它们
//
// 在此之前，七条路径各自手写同一段五步序列：
//
//	UPDATE space_member SET status=<to> ... WHERE ... AND status=<from>
//	RowsAffected()
//	== 0 → 什么都没发生，直接返回
//	ResolveSeatTx()            ← 忘了它，标识符就是调用方的拼写
//	run<方向>TxSteps()          ← 忘了它，失效信号就没发
//
// 五步里有两步是「必须记得」，而它们分散在七处。本分支 12 轮 review 里的 8 个 P1，
// 有 6 个的机制是同一句话：**一个手工维护的枚举漏了一个元素**——漏了一个写入方
// （5/6/7 轮）、漏了一个方向（8 轮）、漏了两扇门（9 轮）、漏了一跳（12 轮）。
//
// 收进这里之后，那两步的决策点从 7 处变成 1 处。新开一条路径要么调用这两个函数
// 之一（于是自动正确），要么绕过它们直接写 space_member（于是被
// modules/project 的写入方普查抓到）。
//
// # 为什么是两个函数而不是一个带方向参数的
//
// 关席位比开席位多一件事：写清理工单（transactional outbox）。而且**顺序是有意义的**
// ——工单的 INSERT 一直在 octo_project 的写入之前，本分支已经因为锁序吃过两个 P1，
// 不值得为了少一个函数去动它。两个函数各自完成本方向的全部动作，方向由「调用了哪个」
// 决定，因此仍然不存在「只做了一半」的状态。

// openSeatTx 把一个已关闭的席位翻回活跃，并在真的翻转了的时候跑事务内步骤。
//
// role 为 nil 表示不改角色（管理端批量添加就是这个语义：重新加入不该悄悄改角色）。
//
// operatorUID 是触发这次重新打开的人，写进 rejoin 工单：自助回归时等于 uid，管理端
// 批量添加时为空（没有单一操作人），审批通过时是审批人。
//
// 返回 changed=false 表示**没有可翻转的已关闭席位**——可能这行根本不存在，也可能
// 它已经是活跃的。两种情况都不该发失效信号：前者没有存活的项目席位会因此重新可达，
// 后者是空写。调用方按自己的语义决定接下来做什么（插入新行，或直接返回）。
//
// # rejoin 工单也在这里，和 closeSeatTx 的清理工单对称
//
// 合并 main 的 #887 之前，四扇门各自在自己的 `affected == 1` 分支里调
// enqueueMemberRejoinIntentTx。那是一个手工维护的四元枚举——正是本文件开头列出的、
// 本分支 12 轮里贡献了 6 个 P1 的那种形状。收进来之后它和失效信号共享同一个判据
// （带谓词的 UPDATE + RowsAffected）和同一个标识符（ResolveSeatTx 的规范拼写），
// 所以不可能出现「工单写了、epoch 没动」或者「工单带着调用方的漂移拼写」。
//
// 顺序与 closeSeatTx 逐字一致：工单 INSERT 在事务步骤（octo_project 的写入）之前，
// 不引入新的加锁顺序。
func openSeatTx(tx *dbr.Tx, spaceID, uid string, role *int, operatorUID string) (bool, error) {
	stmt := tx.Update("space_member").
		Set("status", 1).
		Set("updated_at", time.Now())
	if role != nil {
		stmt = stmt.Set("role", *role)
	}
	result, err := stmt.Where("space_id=? AND uid=? AND status=0", spaceID, uid).Exec()
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, nil
	}
	// 判据来自写入（带谓词的 UPDATE + RowsAffected），标识符来自数据库
	// （ResolveSeatTx）。这两句是本分支第 11、12 轮各自的 P1，现在在同一个地方。
	seat, err := ResolveSeatTx(tx, spaceID, uid)
	if err != nil {
		return false, err
	}
	if err := enqueueMemberRejoinIntentForSeatTx(tx, seat, operatorUID); err != nil {
		return false, err
	}
	if err := runSeatTransitionTxSteps(tx, SeatTransition{Seat: seat, Opened: true}); err != nil {
		return false, err
	}
	return true, nil
}

// closeSeatTx 关闭一个活跃席位，写出清理工单，并跑事务内步骤。
//
// 顺序与合并前逐字一致：工单 INSERT 在事务步骤（octo_project 的写入）之前。三条
// 关席位路径原本就都是这个顺序，保持它意味着这次重构不引入任何新的加锁顺序。
//
// 返回 changed=false 表示没有活跃席位可关。调用方据此跳过（批量路径）或返回 false。
func closeSeatTx(tx *dbr.Tx, spaceID, uid, operatorUID, reason string) (bool, error) {
	result, err := tx.Update("space_member").
		Set("status", 0).
		Set("updated_at", time.Now()).
		Where("space_id=? AND uid=? AND status=1", spaceID, uid).Exec()
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, nil
	}
	seat, err := ResolveSeatTx(tx, spaceID, uid)
	if err != nil {
		return false, err
	}
	if err := enqueueMemberRemovalCleanupTx(tx, seat, operatorUID, reason); err != nil {
		return false, err
	}
	if err := runSeatTransitionTxSteps(tx, SeatTransition{Seat: seat, Opened: false}); err != nil {
		return false, err
	}
	return true, nil
}

// errSeatVanishedUnderLock 是「加锁读说这行是活跃的，紧接着的带谓词 UPDATE 却没改到它」。
//
// 在持有该行 X 锁的事务里这是不可能的，所以它是不变量被破坏而不是正常结果。报错让事务
// 回滚——席位保持原样，对端缓存的判定仍然正确，这是安全的方向。
var errSeatVanishedUnderLock = fmt.Errorf("space: locked seat did not match its own status predicate")
