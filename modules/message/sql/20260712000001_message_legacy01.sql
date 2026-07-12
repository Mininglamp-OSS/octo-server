-- +migrate Up

-- message-reaction 升级为多 reaction 模型：每个 (uid, message_id, channel, emoji) 独立一行、
-- 独立 toggle。写路径改用 INSERT ... ON DUPLICATE KEY UPDATE 原子 upsert，依赖此唯一键
-- 触发 upsert 并根治并发下的重复行（reaction 为新功能，reaction_users 无存量，可直接加）。
--
-- 列序 (channel_id, channel_type, message_id, uid, emoji)：既做唯一约束，又给已按频道收口
-- 的读路径 queryWithMessageIDsInChannel（channel_id=? AND channel_type=? AND message_id IN）
-- 提供左前缀。索引字节数(utf8mb4)约 722B < 3072（MySQL 8.0 默认上限）。
-- 建唯一索引前先幂等去重：旧表只有非唯一索引，历史写路径也无唯一键防并发插入，
-- 生产库若存在重复 (channel_id, channel_type, message_id, uid, emoji) 会让下面的
-- CREATE UNIQUE INDEX 直接失败、阻断发布。保留每组最大 id（最新插入）的行，删其余。
-- 无存量时该 DELETE 为 no-op。
DELETE r1 FROM reaction_users r1
JOIN reaction_users r2
  ON r1.channel_id = r2.channel_id
 AND r1.channel_type = r2.channel_type
 AND r1.message_id = r2.message_id
 AND r1.uid = r2.uid
 AND r1.emoji = r2.emoji
 AND r1.id < r2.id;

CREATE UNIQUE INDEX reaction_users_channel_msg_uid_emoji_uidx ON reaction_users (channel_id, channel_type, message_id, uid, emoji);

-- +migrate Down
-- 仅回滚本迁移新建的唯一索引（去重 DELETE 不可逆，无法在 Down 里恢复被合并的行）。
DROP INDEX reaction_users_channel_msg_uid_emoji_uidx ON reaction_users;
