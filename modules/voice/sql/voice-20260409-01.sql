-- +migrate Up
CREATE TABLE IF NOT EXISTS `user_voice_context` (
    `id`                   BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
    `uid`                  VARCHAR(100) NOT NULL COMMENT 'bot owner uid',
    `space_id`             VARCHAR(100) NOT NULL COMMENT 'Space ID',
    `asr_correct_context`  TEXT         NOT NULL COMMENT '纠错上下文内容（最大10000字符）',
    `created_at`           TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at`           TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    `updated_by`           VARCHAR(100) NOT NULL COMMENT '设置该上下文的 bot id 或 user uid',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_uid_space` (`uid`, `space_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户语音纠错上下文';

-- +migrate Down
DROP TABLE IF EXISTS `user_voice_context`;
