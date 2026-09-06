-- +migrate Up

-- Project — a flat, overlappable collaboration unit INSIDE a Space.
--
-- P0 ships the member pool only. No group can belong to a Project yet
-- (`group.project_id` is P1), so invariants I2/I3 have nothing to violate here.
--
-- Invariant I1: an active octo_project_member.uid is an active space_member.uid
-- of the same Space. Enforced synchronously on every Project-side write
-- (pkg/space.CheckMembership inside the request transaction); restored
-- asynchronously on the Space-removal path via the space_member_removal_cleanup
-- outbox. The two halves have different guarantees and the difference is
-- load-bearing — see modules/project/space_member_removal.go.
--
-- Time columns are application-written UTC DATETIME(3) with NO DEFAULT and NO
-- ON UPDATE, matching octo_group_welcome_delivery rather than the older
-- space/group tables. Those use CURRENT_TIMESTAMP, i.e. the MySQL session
-- timezone, and this repo has already shipped a broken metric that way: the
-- member-removal age gauge compared a Go UTC clock against a session-timezone
-- column and read -28799 seconds under TZ=Asia/Shanghai
-- (modules/space/member_removal_metrics.go). One clock, in Go, in UTC.
--
-- Widths, charset and COLLATE are pinned to match space.space_id / user.uid /
-- space_member.uid. Those legacy tables declare no COLLATE and inherit the
-- database default; a mismatch makes every reconcile JOIN fail with MySQL 1267.
--
-- No foreign keys: nothing in this schema uses them. Cross-table integrity is
-- the cleanup outbox plus the reconcile job.
CREATE TABLE `octo_project` (
  `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `project_id`      VARCHAR(40)  NOT NULL DEFAULT ''  COMMENT '项目唯一ID（32 hex，util.GenerUUID）',
  `space_id`        VARCHAR(40)  NOT NULL DEFAULT ''  COMMENT '所属 Space（宽度/字符集/COLLATE 对齐 space.space_id）',
  `name`            VARCHAR(64)  NOT NULL DEFAULT ''  COMMENT '项目名称',
  `description`     VARCHAR(500) NOT NULL DEFAULT ''  COMMENT '项目描述',
  `logo`            VARCHAR(200) NOT NULL DEFAULT ''  COMMENT '项目图标',
  `creator`         VARCHAR(40)  NOT NULL DEFAULT ''  COMMENT '创建者 uid（对齐 user.uid）',
  `discoverability` TINYINT      NOT NULL DEFAULT 0   COMMENT '0=space_listed 1=unlisted；只过滤列表/搜索，不是安全边界',
  `join_mode`       TINYINT      NOT NULL DEFAULT 1   COMMENT '0=开放加入 1=仅邀请；自助加入是 P2，本列在 P0 无消费方',
  `max_members`     INT          NOT NULL DEFAULT 0   COMMENT '本项目成员上限；0=取全局配置',
  `is_official`     TINYINT      NOT NULL DEFAULT 0   COMMENT '官方项目徽标；P0 无任何写入方',
  `member_epoch`    BIGINT       NOT NULL DEFAULT 0   COMMENT '成员纪元；每次成员/角色写入在同一事务内 +1，只允许 +1',
  `status`          TINYINT      NOT NULL DEFAULT 1   COMMENT '1=正常 0=已解散',
  -- 生成列：只有活跃项目参与重名判定，解散即把名字腾出来。
  -- 朴素 UNIQUE (space_id, name) 会把已解散项目的名字永久占住（建 "Q3 交付" →
  -- 解散 → 再也建不了同名），与 joinPresetGroups 那个「退群后再也加不回来」的
  -- 唯一索引缺陷是同一形状（modules/space/api.go:1366）。MySQL 8.0 没有部分索引，
  -- 而唯一索引里重复的 NULL 不冲突，所以已解散行自动退出判定。
  -- ⚠️ INSERT / UPDATE 语句一律不得点名本列，否则 MySQL 报 3105；DAO 因此用
  -- 显式列清单而不是 util.AttrToUnderscore。
  `active_name`     VARCHAR(64)  GENERATED ALWAYS AS (IF(`status` = 1, `name`, NULL)) STORED
                                 COMMENT '生成列：status=1 时等于 name，否则 NULL；仅供 uk_space_active_name 使用',
  `created_at`      DATETIME(3)  NOT NULL             COMMENT 'UTC；应用侧写入，禁 NOW()',
  `updated_at`      DATETIME(3)  NOT NULL             COMMENT 'UTC；应用侧写入，禁 NOW()',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_octo_project_project_id` (`project_id`),
  UNIQUE KEY `uk_octo_project_space_active_name` (`space_id`, `active_name`),
  KEY `idx_octo_project_space_status` (`space_id`, `status`),
  KEY `idx_octo_project_space_creator` (`space_id`, `creator`, `status`),
  -- 日创建配额走 (creator, created_at) 半开区间。group 那份用
  -- WHERE creator=? AND DATE(created_at)=?（modules/group/db.go:694），列上的函数
  -- 调用让索引失效，且依赖 Go 与 MySQL 会话时区同源；抄的是 pattern，不是那条 SQL。
  KEY `idx_octo_project_creator_created` (`creator`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Space 内项目（协作单元）';

-- 主键 (project_id, uid) + status 翻转，行永不删除：重新加入是一次 UPDATE，
-- 不会出现「退出后唯一索引把人挡在外面」。
-- space_id 冗余存一份，让级联与对账可以按 (space_id, uid) 直接走索引，
-- 而不必为每一行回表查 octo_project。
CREATE TABLE `octo_project_member` (
  `project_id`  VARCHAR(40) NOT NULL DEFAULT ''  COMMENT '项目ID',
  `uid`         VARCHAR(40) NOT NULL DEFAULT ''  COMMENT '成员 uid（对齐 user.uid / space_member.uid）',
  `space_id`    VARCHAR(40) NOT NULL DEFAULT ''  COMMENT '冗余 Space：级联与对账按 (space_id, uid) 走索引',
  `role`        TINYINT     NOT NULL DEFAULT 0   COMMENT '0=普通成员 1=管理员 2=拥有者',
  `status`      TINYINT     NOT NULL DEFAULT 1   COMMENT '1=正常 0=已移除',
  `invite_uid`  VARCHAR(40) NOT NULL DEFAULT ''  COMMENT '把他加进来的人；自助加入时等于 uid',
  `created_at`  DATETIME(3) NOT NULL             COMMENT 'UTC；应用侧写入，禁 NOW()',
  `updated_at`  DATETIME(3) NOT NULL             COMMENT 'UTC；应用侧写入，禁 NOW()',
  PRIMARY KEY (`project_id`, `uid`),
  KEY `idx_octo_project_member_space_uid` (`space_id`, `uid`, `status`),
  KEY `idx_octo_project_member_uid_status` (`uid`, `status`),
  KEY `idx_octo_project_member_project_status` (`project_id`, `status`, `role`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='项目成员（I1：活跃行 ⊆ 同 Space 活跃成员）';

-- +migrate Down
DROP TABLE IF EXISTS `octo_project_member`;
DROP TABLE IF EXISTS `octo_project`;
