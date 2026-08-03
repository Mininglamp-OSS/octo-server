-- +migrate Up

-- space_member.is_bot：该成员是不是 bot 账号，从 user.robot 冗余而来。
--
-- 动机是通讯录目录 (GET /v1/space/{space_id}/directory) 的 LIMIT 下推。目录的
-- 分类条件此前只能写成 `u.robot = 0` 这类引用 JOIN 表的谓词，优化器因此必须把
-- 整个 Space 的成员 join 完、过滤完才能排序取前 N——实测 6000 成员的 Space 上
-- limit=50 与 limit=6000 几乎同价（47ms vs 43ms）。把判定挪到 space_member 自己
-- 的列上，WHERE / ORDER BY / LIMIT 就都落在单表，分页只需读实际要的行数。
--
-- 这份冗余不会失效：user.robot 只在账号创建时写入（modules/user/service.go 与
-- modules/botfather/command.go 的 Robot: 1），全仓库没有任何一处 UPDATE 它——
-- bot 账号不会变成真人，反之亦然。因此插入时抄一次即可，无需同步钩子。
--
-- 列名刻意不叫 robot：queryMembers / searchMembers 用 `SELECT sm.*` 再叠加自己的
-- `CASE ... as robot` 别名，同名列会在结果集里撞车，改变这两个（已标记废弃、
-- 禁止改动的）接口的取值。is_bot 只是给它们多带一个无人读取的列，行为不变。
--
-- 注意区分：本列表达「是不是 bot 账号」，与「bot 是否处于启用状态」
-- （robot.status）是两回事，后者可变，仍由查询时 JOIN robot 判定。
ALTER TABLE `space_member` ADD COLUMN `is_bot` smallint NOT NULL DEFAULT 0 COMMENT '是否机器人账号 0.否 1.是（冗余自 user.robot，不可变）';

-- 回填存量数据。user 行缺失时保守留 0（当作真人），与查询侧此前
-- IFNULL(u.robot,0) 的兜底一致。
UPDATE `space_member` sm
  JOIN `user` u ON u.uid = sm.uid
  SET sm.is_bot = u.robot
  WHERE u.robot = 1;

-- 目录查询的覆盖索引。WHERE 的三列在前作等值前缀，随后是三个排序列，
-- 末位 uid 与 directoryOrderBy 的唯一 tiebreaker 对齐以保证翻页稳定。
-- role DESC 与 created_at ASC 是混合方向，依赖 MySQL 8.0 的降序索引。
--
-- 单独加这个索引对旧查询形态无效（此前验证过：排序只占总耗时约 2ms，瓶颈在
-- 逐行 JOIN）。它只有在 is_bot 让谓词变成单表之后才真正发挥作用——两者必须
-- 一起上，缺一个都不会有收益。
CREATE INDEX spacemember_directory ON `space_member` (space_id, status, is_bot, role DESC, created_at, uid);

-- +migrate Down

DROP INDEX spacemember_directory ON `space_member`;
ALTER TABLE `space_member` DROP COLUMN `is_bot`;
