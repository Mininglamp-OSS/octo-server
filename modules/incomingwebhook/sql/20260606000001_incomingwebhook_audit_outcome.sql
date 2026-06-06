-- +migrate Up

-- Phase 2 失败可观测性：审计表原本只记成功推送，发送方调试失败（限流/payload 非法/
-- 投递失败）时是黑盒。扩展为记录「鉴权通过后」的全部投递结果（成功+失败），供管理端
-- deliveries 端点排障。鉴权失败（未知/错 token）仍只进 IP 失败预算、不落本表，维持
-- push 路径的反枚举不变量。
--
-- 目标库 MySQL 8.0：在表末尾 ADD COLUMN 走 INSTANT 算法、瞬时无锁，无需显式 pin
-- ALGORITHM/LOCK。历史行均为成功推送：status 默认 1(成功) 即正确回填，http_status
-- 历史值未知留 0，adapter 回填 'native'。
ALTER TABLE `incoming_webhook_audit`
  ADD COLUMN `status`      SMALLINT     NOT NULL DEFAULT 1        COMMENT '投递结果：1=成功,2=失败',
  ADD COLUMN `reason`      VARCHAR(32)  NOT NULL DEFAULT ''       COMMENT '失败原因码（body/json/content/blocks/msg_type/too_large/delivery_failed）；成功为空。限流429不入审计',
  ADD COLUMN `http_status` SMALLINT     NOT NULL DEFAULT 0        COMMENT '返回给调用方的 HTTP 状态码；0=未知（迁移前的历史成功行，刻意不伪造成 200）',
  ADD COLUMN `adapter`     VARCHAR(16)  NOT NULL DEFAULT 'native' COMMENT '消息来源/适配器：native/test（Phase 3/4 扩展 github/gitlab/wecom/feishu）';

-- +migrate Down
ALTER TABLE `incoming_webhook_audit`
  DROP COLUMN `status`,
  DROP COLUMN `reason`,
  DROP COLUMN `http_status`,
  DROP COLUMN `adapter`;
