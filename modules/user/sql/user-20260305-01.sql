-- +migrate Up

-- friend 表: 添加 vercode 索引，加速通过验证码查询好友
CREATE INDEX idx_friend_vercode ON `friend` (vercode);

-- user_setting 表: 添加 to_uid 索引，加速通过 to_uid 查询用户设置
CREATE INDEX idx_user_setting_to_uid ON `user_setting` (to_uid);
