-- +migrate Up
-- Agent runtime 自报的托管形态 + 上报时间。与 20260417000001 的
-- agent_platform / agent_version / plugin_version 同一写入点
-- （POST /v1/bot/register 的 User Bot 分支），故并列在 robot 表上。
--
-- agent_hosting 是**客户端自报**值，服务端只校验**形状**（小写 slug）不校验取值：
-- 托管方是会增加的，把取值做成服务端枚举等于每来一个托管方发一次版。已知取值
-- self_hosted / octo_hosted 只是约定，第三方托管方按 <vendor>_hosted 自取即可。
-- 因此本列不可用于鉴权或配额：任何持有该 Bot bf_ token 的调用方都能随意填，
-- 白名单也挡不住冒充（它校验"值在集合内"，不校验"你有资格声称这个值"）。
--
-- 列宽 64 与 register 侧的 maxAgentHostingLen 严格相等：DB 开 STRICT_TRANS_TABLES，
-- 而 agent_* 全组共用一条 UPDATE，所以任何"过了校验却存不进列"的值都会连带
-- 让同一次上报的 agent_platform/version/plugin_version 一起失败。校验上界高于
-- 列宽不是余量，是延迟的失败。
--
-- agent_reported_at 语义是「最近一次收到上报」而非「值变更时间」：robot.updated_at
-- 没有 ON UPDATE（见 modules/robot/sql/20210926000001_robot_legacy01.sql），
-- 缺这一列则 agent_* 全组都是无从判断新鲜度的裸值。
ALTER TABLE `robot`
  ADD COLUMN `agent_hosting` VARCHAR(64) NOT NULL DEFAULT ''
    COMMENT 'Agent自报托管形态，小写slug如self_hosted/octo_hosted/<vendor>_hosted；空=未上报；不可用于鉴权',
  ADD COLUMN `agent_reported_at` TIMESTAMP NULL DEFAULT NULL
    COMMENT '最近一次收到Agent信息上报的时间（新鲜度判定）';

-- +migrate Down
ALTER TABLE `robot`
  DROP COLUMN `agent_reported_at`,
  DROP COLUMN `agent_hosting`;
