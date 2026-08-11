-- +migrate Up
-- 手机号加密第一阶：加密列 + 盲索引 + 后4位低敏检索列。明文 phone 列本轮保留不删
-- （第二阶单独排期切读写路径并删明文列），三列均为增量能力，双写不影响现有查询。
-- 见 modules/user/phone_crypto.go。
--
-- phone_encrypted 必须 NULLABLE：Model.PhoneEncrypted 是 []byte，无手机号的用户
-- （username/email/OAuth 注册、后台管理员）以及主密钥未配置的环境下它是 nil，而
-- util.AttrToUnderscore 的反射 INSERT 会把每一列都显式带上 —— 显式 NULL 打到
-- NOT NULL 列上不会回落到 DEFAULT，在 STRICT_TRANS_TABLES 下直接报
-- 1048 Column cannot be null，会让所有建号流程失败。phone_hash / phone_last4 是
-- string（永不为 nil，空值绑定为 ''），可以保持 NOT NULL。
ALTER TABLE `user`
  ADD COLUMN `phone_encrypted` VARBINARY(255) NULL DEFAULT NULL COMMENT '手机号密文(AES-256-GCM,phone 的加密影子列),无手机号时为 NULL',
  ADD COLUMN `phone_hash` VARCHAR(72) NOT NULL DEFAULT '' COMMENT '手机号盲索引,格式 <密钥版本>:<HMAC-SHA256 hex>',
  ADD COLUMN `phone_last4` VARCHAR(4) NOT NULL DEFAULT '' COMMENT '手机号后4位(低敏明文,供模糊检索)',
  ADD INDEX `idx_phone_hash` (`phone_hash`);

-- +migrate Down
ALTER TABLE `user`
  DROP INDEX `idx_phone_hash`,
  DROP COLUMN `phone_encrypted`,
  DROP COLUMN `phone_hash`,
  DROP COLUMN `phone_last4`;
