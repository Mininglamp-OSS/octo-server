-- +migrate Up

-- 用户置顶频道表
CREATE TABLE IF NOT EXISTS user_pinned_channel (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  uid VARCHAR(40) NOT NULL COMMENT '用户ID',
  space_id VARCHAR(40) NOT NULL DEFAULT '' COMMENT '空间ID，空字符串表示全局',
  channel_id VARCHAR(100) NOT NULL COMMENT '频道ID',
  channel_type TINYINT NOT NULL COMMENT '频道类型: 1私聊 2群 5子区',
  sort_order INT DEFAULT 0 COMMENT '排序值',
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  UNIQUE KEY uk_user_space_channel (uid, space_id, channel_id, channel_type),
  KEY idx_uid_space_sort (uid, space_id, sort_order),
  KEY idx_channel (channel_id, channel_type)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户置顶频道（Space隔离）';

-- +migrate Down

DROP TABLE IF EXISTS user_pinned_channel;
