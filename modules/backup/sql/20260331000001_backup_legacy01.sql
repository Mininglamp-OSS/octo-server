-- +migrate Up

-- backup_config 备份配置表（存储配置复用系统 COS 配置）
CREATE TABLE `backup_config` (
    id              INTEGER         NOT NULL PRIMARY KEY AUTO_INCREMENT,
    enabled         TINYINT(1)      NOT NULL DEFAULT 0 COMMENT '是否启用备份',
    prefix          VARCHAR(128)    NOT NULL DEFAULT 'backup/' COMMENT '备份路径前缀',
    cron_expr       VARCHAR(64)     NOT NULL DEFAULT '0 2 * * *' COMMENT 'cron表达式',
    retention_count INTEGER         NOT NULL DEFAULT 7 COMMENT '保留备份数量',
    data_dir        VARCHAR(512)    NOT NULL DEFAULT '/data/wukongim' COMMENT 'WuKongIM数据目录',
    created_at      TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='备份配置表';

-- backup_history 备份历史表
CREATE TABLE `backup_history` (
    id              INTEGER         NOT NULL PRIMARY KEY AUTO_INCREMENT,
    backup_id       VARCHAR(64)     NOT NULL COMMENT '备份ID (UUID)',
    status          VARCHAR(16)     NOT NULL DEFAULT 'pending' COMMENT '状态: pending/running/success/failed',
    file_name       VARCHAR(255)    NOT NULL DEFAULT '' COMMENT '备份文件名',
    file_size       BIGINT          NOT NULL DEFAULT 0 COMMENT '文件大小 (bytes)',
    storage_path    VARCHAR(512)    NOT NULL DEFAULT '' COMMENT '存储路径',
    started_at      TIMESTAMP       NULL COMMENT '开始时间',
    finished_at     TIMESTAMP       NULL COMMENT '完成时间',
    error_message   TEXT            COMMENT '错误信息',
    created_at      TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_backup_id (backup_id),
    INDEX idx_status (status),
    INDEX idx_created_at (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='备份历史表';

-- +migrate Down
DROP TABLE IF EXISTS `backup_history`;
DROP TABLE IF EXISTS `backup_config`;
