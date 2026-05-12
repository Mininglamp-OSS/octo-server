-- +migrate Up
CREATE TABLE IF NOT EXISTS `space_join_apply` (
  `id`           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `space_id`     VARCHAR(40)     NOT NULL COMMENT '空间ID',
  `uid`          VARCHAR(40)     NOT NULL COMMENT '申请人UID',
  `invite_code`  VARCHAR(20)     NOT NULL DEFAULT '' COMMENT '使用的邀请码',
  `remark`       VARCHAR(200)    NOT NULL DEFAULT '' COMMENT '申请备注',
  `status`       TINYINT         NOT NULL DEFAULT 0 COMMENT '0=待处理 1=通过 2=拒绝',
  `reviewer_uid` VARCHAR(40)     NOT NULL DEFAULT '' COMMENT '审批人UID',
  `created_at`   TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`   TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_space_uid` (`space_id`, `uid`),
  KEY `idx_space_status` (`space_id`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Space加入申请记录';
