-- +migrate Up
-- 清理"已通过但申请人已无成员资格"的加入申请（issue #683 / PR #684 review round 2）。
--
-- 背景：upsertJoinApply 现在拒绝改写 status=1 的行，以防并发重申把审批中的申请
-- 打回待审批。代价是 status=1 的语义必须真的等于"申请人当前持有成员资格"——
-- 否则一条留存的已通过申请会把退出过的用户永久挡在门外：成员校验放行、待审批
-- 查询看不到该行、upsert 被守卫拦下、读回判定"已是成员"，而 uk_space_uid 又不
-- 允许另建一行。
--
-- 移除路径（removeMemberLocked / removeMembersForce / forceDisbandSpace）已改为
-- 在同一事务里删除该行，但那只覆盖今后的移除。本次迁移处理存量：升级前退出过的
-- 用户，其申请行仍是 status=1，在旧代码下无害（无条件 upsert 会重置），升级后即
-- 变成锁死。
--
-- 判定条件是"没有活跃成员行"，同时覆盖两种存量：曾加入后被移除（status=0），
-- 以及成员行因历史原因缺失。
--
-- 竞态说明：迁移在启动时执行，理论上可能撞上一条正在审批、成员行尚未写入的申请，
-- 该行会被误删。后果仅是申请人需要重新提交一次，不会造成锁死或错误授权，且窗口
-- 是单次审批请求的长度。
DELETE ja FROM `space_join_apply` ja
LEFT JOIN `space_member` sm
  ON sm.space_id = ja.space_id AND sm.uid = ja.uid AND sm.status = 1
WHERE ja.status = 1 AND sm.uid IS NULL;

-- +migrate Down
-- 无法回滚：被删除的行是"已无意义的历史申请记录"，没有可恢复的来源，
-- 且回滚到旧代码后这些行本来就会被 upsert 无条件重置。
SELECT 1;
