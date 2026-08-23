-- +migrate Up
-- 管理台二次认证的验证码收件地址，刻意与 user.email 分开存放。
--
-- user.email 是一个登录身份：/v1/user/emaillogin 只凭邮箱验证码就能签发会话（且会
-- 带上该账号的 role），/v1/user/email/forgetpwd 更是无任何角色校验、凭验证码即可重置
-- 密码。若把管理员的二次认证地址写进 user.email，等于给管理台账号开了两条"只要控制
-- 邮箱就能进"的旁路——恰好把本功能想要的"密码 AND 邮箱"降级成"邮箱即可"。
--
-- 因此二次认证地址单列一列：它只被 /v1/manager/login 的发码路径读取，不参与任何账号
-- 解析（没有任何查询按它反查账号），所以多个管理员共用一个运维信箱也是安全的。
ALTER TABLE `user`
  ADD COLUMN `manager_two_factor_email` VARCHAR(100) NOT NULL DEFAULT ''
  COMMENT '管理台二次认证验证码收件地址(不是登录身份,与 email 分离)';

-- +migrate Down
ALTER TABLE `user`
  DROP COLUMN `manager_two_factor_email`;
