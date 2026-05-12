-- +migrate Up

-- 管理后台 querySpaces 的 member_count 派生表 (GROUP BY space_id WHERE status=1)
-- 以及 querySpaceIncludeDisbanded 的相关子查询、queryMembersAdmin 的成员列表
-- 都会走 (space_id, status) 维度，补复合索引让这些查询命中覆盖索引。
CREATE INDEX spacemember_spaceid_status ON `space_member` (space_id, status);
