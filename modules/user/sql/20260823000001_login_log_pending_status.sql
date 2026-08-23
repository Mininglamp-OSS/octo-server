-- +migrate Up
-- 管理台登录二次认证引入了第三种结果：密码与角色校验都通过、验证码尚未通过。
-- 它既不是成功（没签发 token），也不是失败（凭据是对的），但恰恰是最需要被审计
-- 到的一种——"有人握着正确的口令却拿不到邮箱验证码"就是口令已泄露的强信号。
-- 若把它并进 status=2，它会淹没在密码错误的噪音里；若并进 status=1，则会被统计
-- 成一次成功登录。因此单独取值 3。
--
-- 仅改列注释（元数据变更），不改类型/默认值，也不回填历史行：3 只可能由本次改动
-- 之后的管理台登录写入。queryLastLoginIP 按 status=1 过滤，不受影响。
ALTER TABLE `login_log`
  MODIFY COLUMN `status` TINYINT NOT NULL DEFAULT 1 COMMENT '1成功 2失败 3密码通过待二次认证';

-- +migrate Down
ALTER TABLE `login_log`
  MODIFY COLUMN `status` TINYINT NOT NULL DEFAULT 1 COMMENT '1成功 2失败';
