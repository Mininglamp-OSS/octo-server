-- +migrate Up
-- 登录日志与"登录欢迎语"功能解耦，补失败记录 + 检索索引。历史上本表只在
-- sentWelcomeMsg 内部写入且只记成功，欢迎语开关关闭时登录记录完全不会落库。
-- 见 modules/user/api_login_log.go recordSuccess/recordFailure。
--
-- 账号列刻意不存明文：登录标识符对手机号注册用户就是手机号、对邮箱登录就是邮箱，
-- 直接落库等于新建一个明文 PII 存储（还会随日志采集流向下游），与"个人信息加密存储"
-- 自相矛盾。因此拆成两列：
--   account_masked — 两侧都受限的掩码形式（138****1234 / a***@x***），供人读；
--   account_hash   — 盲索引（<密钥版本>:<HMAC-SHA256 hex>），供按账号精确检索。
-- 失败记录没有 uid（账号可能根本不存在），account_hash 是唯一能定位"这个账号被爆破
-- 了多少次"的手段，所以它必须建索引；掩码值多号共享、不具备精确检索能力。
ALTER TABLE `login_log`
  ADD COLUMN `status` TINYINT NOT NULL DEFAULT 1 COMMENT '1成功 2失败',
  ADD COLUMN `login_type` VARCHAR(20) NOT NULL DEFAULT '' COMMENT '登录方式:username/email/manager/register/github/gitee/oidc/wechat',
  ADD COLUMN `account_masked` VARCHAR(100) NOT NULL DEFAULT '' COMMENT '登录尝试账号的掩码形式(不含完整PII),供人读',
  ADD COLUMN `account_hash` VARCHAR(72) NOT NULL DEFAULT '' COMMENT '登录尝试账号盲索引,格式 <密钥版本>:<HMAC-SHA256 hex>',
  ADD INDEX `idx_uid_created` (`uid`, `created_at`),
  ADD INDEX `idx_login_ip_created` (`login_ip`, `created_at`),
  ADD INDEX `idx_account_hash_created` (`account_hash`, `created_at`);

-- +migrate Down
ALTER TABLE `login_log`
  DROP INDEX `idx_uid_created`,
  DROP INDEX `idx_login_ip_created`,
  DROP INDEX `idx_account_hash_created`,
  DROP COLUMN `status`,
  DROP COLUMN `login_type`,
  DROP COLUMN `account_masked`,
  DROP COLUMN `account_hash`;
