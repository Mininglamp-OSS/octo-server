-- +migrate Up

-- 用户会话扩展表（关注/最近 Tab 改版，issue #337）
CREATE TABLE IF NOT EXISTS user_conversation_ext (
  id              BIGINT       AUTO_INCREMENT PRIMARY KEY,
  uid             VARCHAR(40)  NOT NULL                    COMMENT '用户ID',
  space_id        VARCHAR(40)  NOT NULL DEFAULT ''         COMMENT '空间ID，空字符串表示全局',
  target_type     TINYINT      NOT NULL                    COMMENT '目标类型: 1私聊 2群 5子区',
  target_id       VARCHAR(100) NOT NULL                    COMMENT '目标ID（频道/群/子区）',
  followed_dm     TINYINT      NOT NULL DEFAULT 0          COMMENT '是否关注私聊: 0否 1是',
  dm_category_id  BIGINT       NULL                        COMMENT '私聊所属分类ID，NULL表示未分类',
  group_unfollowed TINYINT     NOT NULL DEFAULT 0          COMMENT '是否取消关注群: 0否 1是',
  follow_sort     INT          NOT NULL DEFAULT 0          COMMENT '关注列表内排序值',
  version         INT          NOT NULL DEFAULT 0          COMMENT '乐观锁版本号',
  created_at      DATETIME     DEFAULT CURRENT_TIMESTAMP   COMMENT '创建时间',
  updated_at      DATETIME     DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
  UNIQUE KEY uk (uid, space_id, target_type, target_id),
  KEY idx_followed (uid, space_id, followed_dm, dm_category_id, follow_sort),
  KEY idx_target (target_type, target_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户会话扩展（关注/最近 Tab，issue #337）';

-- NOTE: idx_unfollowed_group / idx_thread_sort 移到 20260513000004_conversation_ext_indexes.sql
-- 是为了让早期跑过本迁移的 dev/staging 环境在重新 migrate 时也能拿到这两个索引
-- （sql-migrate 通过 version 时间戳追踪，不会因为 SQL 文件内容变化而重跑同一 version）。

-- +migrate Down

DROP TABLE IF EXISTS user_conversation_ext;
