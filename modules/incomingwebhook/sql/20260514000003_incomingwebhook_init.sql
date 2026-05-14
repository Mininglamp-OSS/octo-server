-- +migrate Up

-- 群入站 Webhook：外部服务通过该 URL 推送消息到指定群聊
CREATE TABLE `incoming_webhook` (
  `id`                  BIGINT       NOT NULL PRIMARY KEY AUTO_INCREMENT,
  `webhook_id`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '公开 ID，URL 路径段（iwh_ + 32 hex 或 fallback 路径下的去横线 UUID）',
  `token_hash`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'SHA-256(token) 十六进制；token 仅创建/重置时返回',
  `group_no`            VARCHAR(40)  NOT NULL DEFAULT '' COMMENT '所属群编号',
  `space_id`            VARCHAR(40)  NOT NULL DEFAULT '' COMMENT '冗余：群所属 Space',
  `name`                VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '展示名',
  `avatar`              VARCHAR(255) NOT NULL DEFAULT '' COMMENT '展示头像 URL',
  `creator_uid`         VARCHAR(40)  NOT NULL DEFAULT '' COMMENT '创建者 UID',
  `status`              SMALLINT     NOT NULL DEFAULT 1 COMMENT '0=禁用,1=启用',
  `last_used_at`        TIMESTAMP    NULL DEFAULT NULL COMMENT '最近一次成功推送时间',
  `call_count`          BIGINT       NOT NULL DEFAULT 0 COMMENT '累计成功调用次数',
  `created_at`          TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`          TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX `uk_incoming_webhook_id` ON `incoming_webhook` (`webhook_id`);
CREATE INDEX `idx_incoming_webhook_group` ON `incoming_webhook` (`group_no`, `status`);

-- 入站 Webhook 调用审计日志（仅成功调用，TTL 由清理任务/定时 DELETE 维护）
CREATE TABLE `incoming_webhook_audit` (
  `id`           BIGINT       NOT NULL PRIMARY KEY AUTO_INCREMENT,
  `webhook_id`   VARCHAR(64)  NOT NULL DEFAULT '',
  `group_no`     VARCHAR(40)  NOT NULL DEFAULT '',
  `ip`           VARCHAR(64)  NOT NULL DEFAULT '',
  `byte_size`    INT          NOT NULL DEFAULT 0,
  `message_id`   BIGINT       NOT NULL DEFAULT 0,
  `created_at`   TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX `idx_iwa_webhook_time` ON `incoming_webhook_audit` (`webhook_id`, `created_at`);

-- +migrate Down
DROP TABLE IF EXISTS `incoming_webhook_audit`;
DROP TABLE IF EXISTS `incoming_webhook`;
