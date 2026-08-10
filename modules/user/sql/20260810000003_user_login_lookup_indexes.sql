-- +migrate Up
-- user 表登录/查重热路径补索引。
--
-- 现状（EXPLAIN 实测）：user 表除 PRIMARY(id) / uid / short_no_udx /
-- idx_user_destroy_expire 外没有任何索引，于是下面这些每次登录都要走的查询全部是
-- type: ALL 全表扫描：
--   - QueryByUsername / queryUserInfoWithNameAndPwd  → 账号登录、管理端登录、注册查重
--   - QueryByEmail                                   → 邮箱登录 / 注册 / 找回密码
--   - QueryByPhone 的明文兜底分支 / QueryByPhones     → 手机号相关流程、通讯录匹配
--
-- 用普通索引而非 UNIQUE：这三列在业务上近似唯一，但历史脏数据可能存在重复
-- （(zone,phone) 的重复已在 modules/oidc/bind_service.go 注释中记录），加 UNIQUE 会让
-- 迁移在存量库上直接失败。
--
-- MySQL 8 的 ADD INDEX 默认在线执行，不显式 pin ALGORITHM/LOCK（本仓库锁定 8.0，
-- 不为 5.7 做兼容防护）。大表上仍会有一段构建时间，按变更窗口安排。
ALTER TABLE `user`
  ADD INDEX `idx_user_username` (`username`),
  ADD INDEX `idx_user_email` (`email`),
  ADD INDEX `idx_user_zone_phone` (`zone`, `phone`);

-- +migrate Down
ALTER TABLE `user`
  DROP INDEX `idx_user_username`,
  DROP INDEX `idx_user_email`,
  DROP INDEX `idx_user_zone_phone`;
