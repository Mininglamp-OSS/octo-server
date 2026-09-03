-- +migrate Up
-- Agent runtime 自报的托管形态 + 上报时间。与 20260417000001 的
-- agent_platform / agent_version / plugin_version 同一写入点
-- （POST /v1/bot/register 的 User Bot 分支），故并列在 robot 表上。
--
-- agent_hosting 是**客户端自报**值，服务端只校验**形状**（小写 ASCII slug）不校验取值：
-- 托管方是会增加的，把取值做成服务端枚举等于每来一个托管方发一次版。已知取值
-- self_hosted / octo_hosted 只是约定，第三方托管方按 <vendor>_hosted 自取即可。
-- 因此本列不可用于鉴权或配额：任何持有该 Bot bf_ token 的调用方都能随意填，
-- 白名单也挡不住冒充（它校验"值在集合内"，不校验"你有资格声称这个值"）。
--
-- **空串有两种含义，靠 agent_reported_hosting_at 区分**（缺这一句会让读取方误判）：
--   ('', NULL)     = 从未上报过 hosting
--   ('', 非 NULL)  = 曾上报过，后被显式清空（runtime 主动撤回陈旧形态）
-- 两者在业务上不同：前者是"不知道"，后者是"知道它现在没有形态"。
--
-- 列宽 64 与 register 侧的 maxAgentHostingLen 严格相等：DB 开 STRICT_TRANS_TABLES，
-- 而 agent_* 全组共用一条 UPDATE，所以任何"过了校验却存不进列"的值都会连带
-- 让同一次上报的 agent_platform/version/plugin_version 一起失败。校验上界高于
-- 列宽不是余量，是延迟的失败。
--
-- agent_reported_hosting_at 语义是「最近一次收到 **agent_hosting** 上报」，只在 hosting
-- 被上报时前进（版本-only 的上报不刷新它 —— 那等于替一份该次上报从未提及的数据背书
-- 新鲜度）。缺这一列则 agent_hosting 是个无从判断可信度的裸值：robot.updated_at 没有
-- ON UPDATE（见 modules/robot/sql/20210926000001_robot_legacy01.sql），答不了这个问题。
-- 由 SQL NOW() 写入而非 Go 侧 time.Now()：本列与 bound_at（botfather/db.go 用 NOW() 写）
-- 在同一个 API 响应里并列展示，而 Go 侧写入要经驱动的 Config.Loc（默认 UTC，DSN 未设 loc）
-- 转换，应用镜像又固定 TZ=Asia/Shanghai —— MySQL session 时区非 UTC 时两个时间戳会相差
-- 8 小时且无任何标记解释。生产 MySQL 目前是 UTC，所以那是潜伏而非已发生；改用 NOW() 则
-- 彻底不再依赖这个前提。
ALTER TABLE `robot`
  ADD COLUMN `agent_hosting` VARCHAR(64) NOT NULL DEFAULT ''
    COMMENT 'Agent自报托管形态,小写slug如self_hosted/octo_hosted/<vendor>_hosted;空+时间戳NULL=从未上报,空+时间戳非NULL=已显式清空;不可用于鉴权',
  ADD COLUMN `agent_reported_hosting_at` TIMESTAMP NULL DEFAULT NULL
    COMMENT '最近一次收到agent_hosting上报的时间(SQL NOW()写入)；仅hosting上报时前进；判定agent_hosting新鲜度';

-- +migrate Down
ALTER TABLE `robot`
  DROP COLUMN `agent_reported_hosting_at`,
  DROP COLUMN `agent_hosting`;
