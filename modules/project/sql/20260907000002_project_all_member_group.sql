-- +migrate Up

-- P2 — 每个项目自带一个"全员群"。
--
-- 两列，都是 Project 自己的状态，因此都留在 modules/project/sql：只有
-- modules/project 写它们。群侧要判断"这个群是不是某项目的全员群"时走
-- pkg/project 的谓词去读这里，不需要在 group 表上加列——P1 那次
-- group.project_id 的放置错误已经把"列必须从每个读它的二进制都装得到的迁移目录
-- 发货"这条规则的代价交足了。
--
-- 不变量 I4：每个活跃项目有且仅有一个全员群，其活跃成员集合**等于**该项目的
-- 活跃成员集合（status=1 AND removing=0），系统 bot 按 P1 白名单豁免。
--
-- I2（P1）保证的是子集方向：项目群里的人一定是项目成员。I4 补上超集方向，
-- 而且**只对全员群成立**——同一个项目下用户自己建的项目群不受它约束。

-- ---------------------------------------------------------------------------
-- 1. octo_project.all_member_group_no
-- ---------------------------------------------------------------------------
--
-- 空串 = 这个项目还没有全员群，永不为 NULL。与 group.project_id 同一套哨兵约定：
-- 三值列会把 `all_member_group_no = ''` 这个谓词变成一个等着第一行 NULL 的 bug。
--
-- "有且仅有一个"由列本身保证——一个项目一行，一行一个值。被否决的替代方案是在
-- group 表上做"每个 project_id 至多一个全员群"的部分唯一索引：MySQL 8.0 没有部分
-- 索引，只能靠生成列加 NULL 技巧（octo_project.active_name 就是那个形状），而这里
-- 根本不需要——约束的自然归属地是项目行。
--
-- ADD COLUMN ... NOT NULL DEFAULT '' 在 MySQL 8.0 是 INSTANT，与表大小无关。
ALTER TABLE `octo_project`
  ADD COLUMN `all_member_group_no` VARCHAR(40) NOT NULL DEFAULT ''
  COMMENT '本项目全员群的 group_no；空串=尚未建成（哨兵值，永不为 NULL）';

-- ---------------------------------------------------------------------------
-- 2. octo_project.all_member_group_lease_until
-- ---------------------------------------------------------------------------
--
-- D4 的补建租约。
--
-- 建全员群发生在**项目事务提交之后**（钩子要在 modules/group 的表上开自己的事务，
-- 把项目行的排他锁跨到那上面会让建项目与该项目的每一次群写入互相串行）。因此
-- 项目行锁**不能**用来给补建做互斥——第一版 brief 写成"用项目行锁保证只建一个"，
-- 那句话与"钩子在提交后运行"自相矛盾，自审时改掉了。
--
-- 取而代之的是一次 CAS 认领：
--
--   UPDATE octo_project SET all_member_group_lease_until = :deadline
--    WHERE project_id = :id
--      AND all_member_group_no = ''
--      AND (all_member_group_lease_until IS NULL OR all_member_group_lease_until < :now)
--
-- 影响 1 行的那个调用者去建群，然后再 CAS 写回 group_no 并清空租约；影响 0 行的
-- 直接跳过。租约过期即可被下一个写路径重新认领，所以进程在建群中途被打死不会把
-- 这个项目永久钉在"没有全员群"上。
--
-- 时间列是应用侧写入的 UTC，无 DEFAULT 无 ON UPDATE——与本模块其余时间列同一条
-- 规矩，理由见 20260904000001_project_core.sql：CURRENT_TIMESTAMP 走 MySQL 会话
-- 时区，本仓库已经因此发过一个读数为 -28799 秒的指标。一个时钟，在 Go 里，用 UTC。
--
-- 可以为 NULL，而 all_member_group_no 不行：NULL 在这里是"从没有人认领过"，
-- 是一个真实存在的第三态，与 octo_project_member_removal_cleanup.lease_until 同义。
ALTER TABLE `octo_project`
  ADD COLUMN `all_member_group_lease_until` DATETIME(3) NULL
  COMMENT 'UTC；全员群补建的认领租约，NULL=从未被认领。见 D4 的 CAS 认领';

-- ---------------------------------------------------------------------------
-- 3. 索引
-- ---------------------------------------------------------------------------
--
-- 下面两条 CREATE INDEX **不是** INSTANT。上面那两条 ADD COLUMN 是（与表大小
-- 无关），而建索引是独立的 ONLINE / INPLACE 操作，耗时与行数成正比，需要按生产
-- 的 octo_project 行数单独估一次上线窗口。两者放在同一个文件里，容易让人把前者
-- 的结论顺手套到后者身上——PR #855 的 review 指出了这一点。
--
-- ONLINE 意味着期间读写不被阻塞，所以这不是停机窗口，是"这条语句要跑多久、
-- 什么时候能确认跑完"的问题。
--
-- I4 的对账扫描 A 要找"活跃项目里 all_member_group_no 为空的"，谓词是
-- (status, all_member_group_no)。已有的 idx_octo_project_space_status 首列是
-- space_id，扫描 A 不按 Space 过滤，用不上它。
--
-- 低基数首列在这里是对的，正如 P1 的 idx_octo_project_member_removing：分布是
-- 极度倾斜的（绝大多数活跃项目都已经有群），扫描只读它真正需要的那一段，
-- 从而满足 TestReconcileQueriesAreBounded 对"按检查行数有界"的要求。
CREATE INDEX `idx_octo_project_all_member_group`
  ON `octo_project` (`status`, `all_member_group_no`);

-- 反向点查：给定一个 group_no 问"它是不是某个项目的全员群"。D7 的四道保护每次
-- 都要问一次，而它拿到的是 group_no 和 project_id。没有这条索引，那个判定会退化
-- 成全表扫 octo_project——它挂在群退出/解散/踢人/转让四个接口上，是用户路径。
CREATE INDEX `idx_octo_project_all_member_group_no`
  ON `octo_project` (`all_member_group_no`);


-- +migrate Down
--
-- 回滚只丢掉"哪个群是全员群"这个事实，不动任何群：群本身是通过
-- POST /v1/group/create 的正常路径建出来的普通项目群，group.project_id 仍然指着
-- 它的项目，I2 照常约束它。回滚之后它就是一个"用户自己建的项目群"，
-- 不再受 D7 保护，也不再跟着项目改名——功能消失，数据不消失。
--
-- 本文件中任何注释都不得出现撇号。本模块的迁移测试按朴素方式切分语句并把引号当作
-- 字符串定界符，一个所有格撇号就会让它把下一个分号读成在字面量内部。撇号是成对
-- 抵消的，所以偶数个能过、奇数个报错，而它报错时指的是恰好夹在最后一对之间的那条
-- 语句——它已经这样咬过两次人了。不要靠数个数。
DROP INDEX `idx_octo_project_all_member_group_no` ON `octo_project`;
DROP INDEX `idx_octo_project_all_member_group` ON `octo_project`;
ALTER TABLE `octo_project` DROP COLUMN `all_member_group_lease_until`;
ALTER TABLE `octo_project` DROP COLUMN `all_member_group_no`;
