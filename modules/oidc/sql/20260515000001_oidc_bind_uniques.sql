-- +migrate Up

-- 配套 OIDC 自助绑定 P0(需求 FR-5.3 / SR-5):同一 dmwork uid 在同一 IdP
-- 下只允许绑定一个 sub。confirm 路径同 (uid, issuer) 并发写入由本约束 +
-- 应用层 duplicate-key 兜底共同保证。
--
-- 上线前数据巡检(若有重复行,本迁移失败回滚):
--   SELECT uid, issuer, COUNT(*) AS n
--     FROM user_oidc_identity
--     GROUP BY uid, issuer
--     HAVING n > 1;

-- 显式 INPLACE + LOCK=NONE:MySQL 8.0 默认会选 INPLACE,但显式 pin 一是
-- 防 5.7 副本/被禁 online DDL 的环境静默退化成 LOCK=SHARED 锁表;二是让
-- "无法在线执行"的环境在 apply 时立即报错,而不是阻塞业务写。
ALTER TABLE `user_oidc_identity`
  ADD UNIQUE KEY `uk_uid_issuer` (`uid`, `issuer`),
  ALGORITHM=INPLACE, LOCK=NONE;

-- +migrate Down

-- 仅删约束,不动数据。已写入的 identity 行不回滚(本就是预期产物,与
-- 需求文档 NFR-6 一致)。

ALTER TABLE `user_oidc_identity` DROP INDEX `uk_uid_issuer`;
