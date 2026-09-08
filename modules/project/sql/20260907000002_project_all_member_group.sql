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
-- 1. 索引：一条都不加
-- ---------------------------------------------------------------------------
--
-- 本文件因此**整个**都是 INSTANT，不需要在线索引构建，也就不需要为它估上线窗口。
--
-- 这里先后写过两条索引，两条都删掉了，理由是同一条：把碰这一列的语句一句句列出来
-- 之后，没有任何一条会用上它。
--
-- 第一条 idx_octo_project_all_member_group_no (all_member_group_no)：理由写的是
-- "D7 的反向点查：给定 group_no 问它是不是某项目的全员群"。第二轮 review 查穿了
-- 它——那个判定是 IsAllMemberGroup，以 project_id 打头，走 uk_octo_project_project_id
-- 这个 UNIQUE 键，本来就是一行点查（现在它更是被拆成了两条单表读）。
--
-- 第二条 idx_octo_project_all_member_group (status, all_member_group_no)：理由写的是
-- "I4 扫描 A 要找活跃项目里 all_member_group_no 为空的，谓词是
-- (status, all_member_group_no)"。那个理由**在写下的时候是真的，后来不再是**：
-- 第三轮把 all_member_group_no 从 WHERE 移进了 violating 标志（flag-over-base-page，
-- 让 LIMIT 约束被检查的行而不是被返回的行），此后扫描 A 的 WHERE 只剩
-- p.status = ? AND p.id > ?，再 ORDER BY p.id。第五轮 review 指出了这一点。
--
-- 实测（MySQL 8.0.46，octo_project 2000 行 / octo_project_member 20000 行 /
-- group 5000 行，两种排序规则形态各跑一次）：
--
--   扫描 A：p type=range key=PRIMARY（索引在 possible_keys 里，没被选中）
--   扫描 B：优化器根本不从 p 驱动——先 pm ref idx_octo_project_member_removing，
--           再 eq_ref 回 p 走 uk_octo_project_project_id
--
-- 两种形态下结论相同，所以这不是"等排序规则转换之后它就有用了"。原因也清楚：
-- 二级索引里的行序是 (status, all_member_group_no, id)，满足不了扫描 A 的
-- ORDER BY p.id，而分页的 p.id > ? 只有主键能服务。
--
-- 索引不是免费的：CREATE INDEX 不是 INSTANT，上线要单独估一次在线构建时长，
-- 换来的是每一次 octo_project 写入都多维护一棵 B+ 树。将来真出现按这一列过滤的
-- 查询，那时连着那条查询一起加，并且带上它的 EXPLAIN。

-- ---------------------------------------------------------------------------
-- 2. octo_project.all_member_group_no
-- ---------------------------------------------------------------------------
--
-- 空串 = 这个项目还没有全员群，永不为 NULL。与 group.project_id 同一套哨兵约定：
-- 三值列会把「all_member_group_no 等于空串」这个谓词变成一个等着第一行 NULL 的 bug。
--
-- "有且仅有一个"由列本身保证——一个项目一行，一行一个值。被否决的替代方案是在
-- group 表上做"每个 project_id 至多一个全员群"的部分唯一索引：MySQL 8.0 没有部分
-- 索引，只能靠生成列加 NULL 技巧（octo_project.active_name 就是那个形状），而这里
-- 根本不需要——约束的自然归属地是项目行。
--
-- 带 NOT NULL 与空串默认值的 ADD COLUMN 在 MySQL 8.0 是 INSTANT，与表大小无关。
--
-- ALGORITHM=INSTANT 写出来，是让这句断言**被强制执行**而不是被期望：不加的话
-- MySQL 会在 INSTANT 不可用时（例如这张表的 instant 列版本用满）静默降级成
-- 一次 INPLACE 重建，而这个文件正是靠"上面两条 INSTANT、下面那条不是"来划分
-- 上线窗口的。写上之后，降级会当场报错而不是变成一段没人预料到的长耗时。
-- PR #855 第五轮 review 的 Q8。
ALTER TABLE `octo_project`
  ADD COLUMN `all_member_group_no` VARCHAR(40) NOT NULL DEFAULT ''
  COMMENT '本项目全员群的 group_no；空串=尚未建成（哨兵值，永不为 NULL）',
  ALGORITHM=INSTANT;

-- ---------------------------------------------------------------------------
-- 3. octo_project.all_member_group_lease_until
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
--      AND all_member_group_no = <空串>
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
  COMMENT 'UTC；全员群补建的认领租约，NULL=从未被认领。见 D4 的 CAS 认领',
  ALGORITHM=INSTANT;

-- +migrate Down
--
-- 回滚只丢掉"哪个群是全员群"这个事实，不动任何群：群本身是通过
-- POST /v1/group/create 的正常路径建出来的普通项目群，group.project_id 仍然指着
-- 它的项目，I2 照常约束它。回滚之后它就是一个"用户自己建的项目群"，
-- 不再受 D7 保护，也不再跟着项目改名——功能消失，数据不消失。
--
-- 上一版的 Up 段注释里还有三处成对的引号（写成谓词字面量的空串）。偶数个能过，
-- 而这正是下面那句话说的"不要靠数个数"——已经改写成中文，注释里一个引号都不留。
--
-- 回滚要在**旧二进制部署之后**执行，不是之前：新二进制的全员群查询会去读这两列，
-- 列没了就报错，D7 的守卫按设计 fail-open（计数器会响，所以不是静默），但那段时间里
-- 全员群是没有保护的。顺序是先回滚二进制，再回滚这个迁移。
--
-- 本文件中任何注释都不得出现撇号。本模块的迁移测试按朴素方式切分语句并把引号当作
-- 字符串定界符，一个所有格撇号就会让它把下一个分号读成在字面量内部。撇号是成对
-- 抵消的，所以偶数个能过、奇数个报错，而它报错时指的是恰好夹在最后一对之间的那条
-- 语句——它已经这样咬过两次人了。不要靠数个数。
ALTER TABLE `octo_project` DROP COLUMN `all_member_group_lease_until`;
ALTER TABLE `octo_project` DROP COLUMN `all_member_group_no`;
