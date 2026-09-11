-- +migrate Up

-- octo_project_user_setting — 每个用户对某个项目的个人偏好。P2 只有置顶一项。
--
-- 为什么是新表，而不是给 octo_project_member 加一列
--
-- 两条理由，都不是风格问题：
--
--   1. 置顶偏好独立于 Project 成员事实。当前 Project 读写按有效成员授权；
--      历史偏好可在重新加入后恢复，但无成员席位的偏好不构成 Sidebar 或
--      Project 访问依据。
--   2. octo_project_member 的每一次写都在 member_epoch 递增路径上。那个纪元是
--      fleet / drive 判断「成员是否变过」的依据，只允许 +1，且由
--      TestMemberEpochOnlyEverIncrements 钉住。置顶不是成员变更，绝不能推动它；
--      而把一列放进那张表，就是在邀请下一个人在同一个事务里顺手 bump。
--
-- 表名与 octo_ 前缀、utf8mb4_general_ci 都对齐 P0 定下的规矩：本表要与
-- octo_project 做 JOIN，两侧同为 general_ci 才不需要 COLLATE（legacy 侧才需要）。
--
-- 时间列一律应用侧写入的 UTC，无 DEFAULT、无 ON UPDATE。理由与
-- octo_project_member_removal_cleanup 的表头相同：MySQL 的 CURRENT_TIMESTAMP 取
-- 会话时区，本仓已经因此发过一个读出 -28799 秒的指标。一个时钟，在 Go 里，UTC。
--
-- 注释里不要出现单引号（撇号），与 P1 那张迁移表同样的原因。真正被强制的是
-- applyOneProjectMigrationFile 里的两条检查：一是全文不得出现 sql-migrate 的显式
-- 语句块标记（连注释里提一下那个词都不行，本段就踩过一次），二是把全文按单引号成对
-- 切出来的每一段字面量里都不能有分号。DDL 的 COMMENT 字面量本身成对，不受影响；
-- 坏事的是注释里一个所有格撇号——它是单只的，会让它之后所有单引号的配对整体错位，
-- 于是本来无分号的一段被判成有分号，或者真的带分号的字面量反而检不出来。所以规则
-- 不是数单引号的个数，而是注释里一个都不写。
CREATE TABLE `octo_project_user_setting` (
  `id`         BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
  `project_id` VARCHAR(40)      NOT NULL DEFAULT ''  COMMENT '项目ID（宽度/字符集对齐 octo_project.project_id）',
  `uid`        VARCHAR(40)      NOT NULL DEFAULT ''  COMMENT '设置归属的用户（对齐 user.uid）',
  `pinned`     TINYINT UNSIGNED NOT NULL DEFAULT 0   COMMENT '1=该用户置顶了这个项目',
  -- 置顶时间，取消置顶时置空。列表排序按 (pinned DESC, pinned_at DESC, id DESC)，
  -- 让最近置顶的排在前面；末位的 octo_project.id 唯一，所以这个顺序是全序——
  -- OFFSET 分页要求全序，否则翻页会丢行和重行。
  `pinned_at`  DATETIME(3)      NULL                 COMMENT 'UTC；应用侧写入，取消置顶时置 NULL',
  `created_at` DATETIME(3)      NOT NULL             COMMENT 'UTC；应用侧写入，禁 CURRENT_TIMESTAMP',
  `updated_at` DATETIME(3)      NOT NULL             COMMENT 'UTC；应用侧写入，禁 ON UPDATE',
  PRIMARY KEY (`id`),
  -- 一个用户对一个项目只有一行。既是语义约束，也是 upsert 的幂等来源：
  -- INSERT ... ON DUPLICATE KEY UPDATE 靠它把「重复置顶」变成一次无害的改写，
  -- 而不是长出第二行。
  --
  -- 它同时就是列表读路径要的索引：列表查询以 octo_project 为驱动表，按
  -- (project_id, uid) 回查本表，正好走这条唯一键的最左前缀加等值。
  UNIQUE KEY `uk_octo_project_user_setting` (`project_id`, `uid`),
  -- 置顶配额要按 uid 数「这个用户一共置顶了几个」，而唯一键的首列是 project_id，
  -- 帮不上按 uid 的等值查找——没有这条索引，每一次置顶写入都要全表扫一张随用户
  -- 数增长的表。
  --
  -- 第二列是 pinned 而不是别的：配额只数 pinned = 1 的行，取消置顶留下的
  -- pinned = 0 行在同一个 uid 下会越积越多，让它们落在索引区间之外，扫描就只读
  -- 真正算数的那几行。
  --
  -- Space 维度不在索引里，而是靠回表 octo_project 拿 space_id 过滤。因为按
  -- (uid, pinned) 命中的行数上限就是这个用户在所有 Space 的置顶总数，本身已经
  -- 是个位数——再冗余一列 space_id 进来，是为一次已经很小的扫描增加一份要维护
  -- 的副本。
  KEY `idx_octo_project_user_setting_uid_pinned` (`uid`, `pinned`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户对项目的个人偏好（P2：置顶）';

-- +migrate Down
--
-- 回滚会丢掉所有人的置顶。这是个人偏好、不是业务数据，重新置顶即可恢复，
-- 因此不像 removal_cleanup 那张表那样需要先查未完成工单数。
DROP TABLE IF EXISTS `octo_project_user_setting`;
